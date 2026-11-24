package redis

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func receiveWithTimeout(t *testing.T, ch <-chan []byte, timeout time.Duration) []byte {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return msg
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for message", timeout)
		return nil
	}
}

func assertNoMessage(t *testing.T, ch <-chan []byte, wait time.Duration) {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if ok {
			t.Fatalf("unexpected message received: %q", string(msg))
		}
		t.Fatal("channel closed unexpectedly while asserting no message")
	case <-time.After(wait):
	}
}

func assertChannelClosed(t *testing.T, ch <-chan []byte, timeout time.Duration) {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if ok {
			t.Fatalf("expected channel to be closed, got message %q", string(msg))
		}
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for channel close", timeout)
	}
}

func TestPubSubContract(t *testing.T) {
	// Verified at compile time via var _ pubSubContract = (*Client)(nil).
	var _ pubSubContract = &Client{}
}

func TestConstructorValidation(t *testing.T) {
	if _, err := NewClient(nil); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("NewClient(nil) = %v, want ErrInvalidOptions", err)
	}
	if _, err := NewClient(&redis.UniversalOptions{}); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("NewClient(empty Addrs) = %v, want ErrInvalidOptions", err)
	}
	if _, err := NewClient(&redis.UniversalOptions{Addrs: []string{""}}); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("NewClient(blank addr) = %v, want ErrInvalidOptions", err)
	}
	if _, err := NewClient(&redis.UniversalOptions{Addrs: []string{"   "}}); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("NewClient(whitespace addr) = %v, want ErrInvalidOptions", err)
	}
	if _, err := NewClientFromClient(nil); !errors.Is(err, ErrInvalidClient) {
		t.Errorf("NewClientFromClient(nil) = %v, want ErrInvalidClient", err)
	}

	var typedNil *redis.Client
	if _, err := NewClientFromClient(typedNil); !errors.Is(err, ErrInvalidClient) {
		t.Errorf("NewClientFromClient(typed nil) = %v, want ErrInvalidClient", err)
	}

	mr := miniredis.RunT(t)
	c1, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}}, nil)
	if err != nil {
		t.Fatalf("NewClient with nil option error = %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	raw := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	defer func() { _ = raw.Close() }()
	c2, err := NewClientFromClient(raw, nil)
	if err != nil {
		t.Fatalf("NewClientFromClient with nil option error = %v", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestOptions_DefaultsAndOverrides(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	// Positive WithChannelBuffer overrides default 100.
	cBuf, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}}, WithChannelBuffer(8))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	chBuf, err := cBuf.Subscribe(ctx, "buf-custom")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if got := cap(chBuf); got != 8 {
		t.Errorf("cap(chBuf) = %d, want 8", got)
	}
	if err := cBuf.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if pingErr := cBuf.client.Ping(ctx).Err(); !errors.Is(pingErr, redis.ErrClosed) {
		t.Errorf("owned client Ping after Close = %v, want redis.ErrClosed", pingErr)
	}

	// Non-positive WithChannelBuffer falls back to default 100; WithCloseClient(false) keeps client open.
	cKeep, err := NewClient(
		&redis.UniversalOptions{Addrs: []string{mr.Addr()}},
		WithChannelBuffer(0),
		WithChannelBuffer(-10),
		WithCloseClient(false),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = cKeep.client.Close() }()

	chDefault, err := cKeep.Subscribe(ctx, "buf-default")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if got := cap(chDefault); got != defaultChannelBuffer {
		t.Errorf("cap(chDefault) = %d, want %d", got, defaultChannelBuffer)
	}
	if err := cKeep.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if pingErr := cKeep.client.Ping(ctx).Err(); pingErr != nil {
		t.Errorf("client Ping after Close with WithCloseClient(false) = %v, want nil", pingErr)
	}

	// NewClientFromClient defaults to not closing injected client.
	shared := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	defer func() { _ = shared.Close() }()

	cShared, err := NewClientFromClient(shared)
	if err != nil {
		t.Fatalf("NewClientFromClient() error = %v", err)
	}
	if err := cShared.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if pingErr := shared.Ping(ctx).Err(); pingErr != nil {
		t.Errorf("shared client Ping after Close = %v, want nil", pingErr)
	}

	// NewClientFromClient with WithCloseClient(true) closes injected client.
	ownedViaOpt := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	defer func() { _ = ownedViaOpt.Close() }()

	cOwnedViaOpt, err := NewClientFromClient(ownedViaOpt, WithCloseClient(true))
	if err != nil {
		t.Fatalf("NewClientFromClient(WithCloseClient(true)) error = %v", err)
	}
	if err := cOwnedViaOpt.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if pingErr := ownedViaOpt.Ping(ctx).Err(); !errors.Is(pingErr, redis.ErrClosed) {
		t.Errorf("injected client Ping after Close with WithCloseClient(true) = %v, want redis.ErrClosed", pingErr)
	}
}

func TestClient_ExactPubSubLifecycle(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	ch, err := c.Subscribe(ctx, "orders")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := c.Publish(ctx, "orders", []byte("order-1")); err != nil {
		t.Fatalf("Publish 1 error = %v", err)
	}
	if err := c.Publish(ctx, "orders", []byte("order-2")); err != nil {
		t.Fatalf("Publish 2 error = %v", err)
	}

	for _, want := range []string{"order-1", "order-2"} {
		got := receiveWithTimeout(t, ch, time.Second)
		if string(got) != want {
			t.Errorf("received %q, want %q", string(got), want)
		}
	}

	if err := c.Unsubscribe(ctx, "orders"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}
	assertChannelClosed(t, ch, time.Second)

	// Close is idempotent and closes the underlying client.
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if pingErr := c.client.Ping(ctx).Err(); !errors.Is(pingErr, redis.ErrClosed) {
		t.Errorf("Ping after Close = %v, want redis.ErrClosed", pingErr)
	}
}

func TestClient_WildcardRouting(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	patternCh, err := c.Subscribe(ctx, "user:*")
	if err != nil {
		t.Fatalf("Subscribe(user:*) error = %v", err)
	}
	exactCh, err := c.Subscribe(ctx, "user:123")
	if err != nil {
		t.Fatalf("Subscribe(user:123) error = %v", err)
	}
	qCh, err := c.Subscribe(ctx, "item:?")
	if err != nil {
		t.Fatalf("Subscribe(item:?) error = %v", err)
	}
	bracketCh, err := c.Subscribe(ctx, "grp:[ab]")
	if err != nil {
		t.Fatalf("Subscribe(grp:[ab]) error = %v", err)
	}

	// Publish to "user:123": both exact and wildcard subscriptions receive it.
	if err := c.Publish(ctx, "user:123", []byte("evt-1")); err != nil {
		t.Fatalf("Publish(user:123) error = %v", err)
	}
	if got := string(receiveWithTimeout(t, exactCh, time.Second)); got != "evt-1" {
		t.Errorf("exactCh got %q, want evt-1", got)
	}
	if got := string(receiveWithTimeout(t, patternCh, time.Second)); got != "evt-1" {
		t.Errorf("patternCh got %q, want evt-1", got)
	}

	// Publish to multi-segment "user:123:profile": Redis PSUBSCRIBE matches across ':'.
	if err := c.Publish(ctx, "user:123:profile", []byte("evt-multi")); err != nil {
		t.Fatalf("Publish(user:123:profile) error = %v", err)
	}
	if got := string(receiveWithTimeout(t, patternCh, time.Second)); got != "evt-multi" {
		t.Errorf("patternCh got %q, want evt-multi", got)
	}
	assertNoMessage(t, exactCh, 40*time.Millisecond)

	// Non-matching topic must not reach user:*.
	if err := c.Publish(ctx, "order:1", []byte("ignore")); err != nil {
		t.Fatalf("Publish(order:1) error = %v", err)
	}
	assertNoMessage(t, patternCh, 40*time.Millisecond)

	// '?' and '[...]' glob routing.
	if err := c.Publish(ctx, "item:7", []byte("q-match")); err != nil {
		t.Fatalf("Publish(item:7) error = %v", err)
	}
	if err := c.Publish(ctx, "item:77", []byte("q-miss")); err != nil {
		t.Fatalf("Publish(item:77) error = %v", err)
	}
	if got := string(receiveWithTimeout(t, qCh, time.Second)); got != "q-match" {
		t.Errorf("qCh got %q, want q-match", got)
	}
	assertNoMessage(t, qCh, 40*time.Millisecond)

	if err := c.Publish(ctx, "grp:b", []byte("b-match")); err != nil {
		t.Fatalf("Publish(grp:b) error = %v", err)
	}
	if err := c.Publish(ctx, "grp:c", []byte("c-miss")); err != nil {
		t.Fatalf("Publish(grp:c) error = %v", err)
	}
	if got := string(receiveWithTimeout(t, bracketCh, time.Second)); got != "b-match" {
		t.Errorf("bracketCh got %q, want b-match", got)
	}
	assertNoMessage(t, bracketCh, 40*time.Millisecond)

	// Unsubscribing pattern uses PUNSUBSCRIBE cleanly without affecting exact sub.
	if err := c.Unsubscribe(ctx, "user:*"); err != nil {
		t.Fatalf("Unsubscribe(user:*) error = %v", err)
	}
	assertChannelClosed(t, patternCh, time.Second)

	if err := c.Publish(ctx, "user:123", []byte("still-exact")); err != nil {
		t.Fatalf("Publish(user:123) after PUnsubscribe error = %v", err)
	}
	if got := string(receiveWithTimeout(t, exactCh, time.Second)); got != "still-exact" {
		t.Errorf("exactCh after pattern unsubscribe got %q, want still-exact", got)
	}
}

func TestClient_BinaryPayloads(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	ch, err := c.Subscribe(ctx, "binary-topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	payloads := [][]byte{
		{0x00, 0xff, '\r', '\n', 0x7f, 0x80, 'A', 'B'},
		[]byte(`{"o":"node-1","k":["k1","k2"]}`),
		{},
		nil,
	}

	for i, p := range payloads {
		if err := c.Publish(ctx, "binary-topic", p); err != nil {
			t.Fatalf("Publish[%d] error = %v", i, err)
		}
		got := receiveWithTimeout(t, ch, time.Second)
		if !bytes.Equal(got, p) {
			t.Errorf("payload[%d] = %x, want %x", i, got, p)
		}
	}
}

func TestClient_ShortLivedSubscribeContext(t *testing.T) {
	mr := miniredis.RunT(t)

	c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	// Mirror MemoryStore.Subscribe and startInvalidationBackplane: cancel the
	// Subscribe context immediately after Subscribe returns.
	subCtx, subCancel := context.WithCancel(t.Context())
	ch, err := c.Subscribe(subCtx, "memorystore:invalidation")
	subCancel()
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := c.Publish(t.Context(), "memorystore:invalidation", []byte(`{"o":"a","k":["x"]}`)); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	got := receiveWithTimeout(t, ch, time.Second)
	if string(got) != `{"o":"a","k":["x"]}` {
		t.Errorf("got %q, want invalidation payload", string(got))
	}
}

func TestClient_ErrorsAndEdgeCases(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := c.Subscribe(ctx, ""); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Subscribe(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Publish(ctx, "", []byte("x")); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Publish(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Unsubscribe(ctx, "never-subscribed"); err != nil {
		t.Errorf("Unsubscribe(unknown) = %v, want nil", err)
	}

	// Pre-canceled context is rejected upfront.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Subscribe(canceledCtx, "t"); !errors.Is(err, context.Canceled) {
		t.Errorf("Subscribe(canceled) = %v, want context.Canceled", err)
	}
	if err := c.Publish(canceledCtx, "t", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Publish(canceled) = %v, want context.Canceled", err)
	}
	if err := c.Unsubscribe(canceledCtx, "t"); !errors.Is(err, context.Canceled) {
		t.Errorf("Unsubscribe(canceled) = %v, want context.Canceled", err)
	}

	// Server error surfaced on Publish.
	mr.SetError("ERR simulated failure")
	if err := c.Publish(ctx, "t", []byte("x")); err == nil || !strings.Contains(err.Error(), "simulated failure") {
		t.Errorf("Publish with mr.SetError = %v, want simulated failure error", err)
	}
	mr.SetError("")

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Operations after Close return ErrClosed.
	if _, err := c.Subscribe(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Publish(ctx, "t", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish(closed) = %v, want ErrClosed", err)
	}
	if err := c.Unsubscribe(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Unsubscribe(closed) = %v, want ErrClosed", err)
	}

	// Unreachable server fails Subscribe cleanly without leaking goroutines.
	deadMr := miniredis.RunT(t)
	deadAddr := deadMr.Addr()
	deadMr.Close()

	cDead, err := NewClient(&redis.UniversalOptions{
		Addrs:       []string{deadAddr},
		DialTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient(deadAddr) error = %v", err)
	}
	if _, err := cDead.Subscribe(ctx, "unreachable"); err == nil {
		t.Error("expected Subscribe to unreachable address to fail")
	}
	if err := cDead.Close(); err != nil {
		t.Fatalf("cDead.Close() error = %v", err)
	}

	// Stalled handshake cancellation via context.AfterFunc (no deadline on ctx).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	cStall, err := NewClient(&redis.UniversalOptions{Addrs: []string{ln.Addr().String()}})
	if err != nil {
		t.Fatalf("NewClient(stall) error = %v", err)
	}
	defer func() { _ = cStall.Close() }()

	stallCtx, stallCancel := context.WithCancel(ctx)
	go func() {
		conn := <-accepted
		defer func() { _ = conn.Close() }()
		time.Sleep(30 * time.Millisecond)
		stallCancel()
		// Keep connection open briefly until test finishes or client closes it.
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)
	}()

	if _, err := cStall.Subscribe(stallCtx, "stalled"); !errors.Is(err, context.Canceled) {
		t.Errorf("Subscribe(stalled handshake canceled) = %v, want context.Canceled", err)
	}
}

func TestClient_ResubscribeSameTopic(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	ch1, err := c.Subscribe(ctx, "orders")
	if err != nil {
		t.Fatalf("first Subscribe() error = %v", err)
	}
	if err := c.Publish(ctx, "orders", []byte("first")); err != nil {
		t.Fatalf("Publish 1 error = %v", err)
	}
	if got := string(receiveWithTimeout(t, ch1, time.Second)); got != "first" {
		t.Errorf("ch1 got %q, want first", got)
	}

	ch2, err := c.Subscribe(ctx, "orders")
	if err != nil {
		t.Fatalf("second Subscribe() error = %v", err)
	}
	assertChannelClosed(t, ch1, time.Second)

	if err := c.Publish(ctx, "orders", []byte("second")); err != nil {
		t.Fatalf("Publish 2 error = %v", err)
	}
	if got := string(receiveWithTimeout(t, ch2, time.Second)); got != "second" {
		t.Errorf("ch2 got %q, want second", got)
	}
}

func TestClient_SlowConsumerAndReconnect(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	// Slow consumer with buffer 1: fill buffer without draining, then Unsubscribe cleanly.
	c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}}, WithChannelBuffer(1))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	chSlow, err := c.Subscribe(ctx, "slow-topic")
	if err != nil {
		t.Fatalf("Subscribe(slow-topic) error = %v", err)
	}
	for i := range 5 {
		if err := c.Publish(ctx, "slow-topic", []byte{byte('0' + i)}); err != nil {
			t.Fatalf("Publish slow[%d] error = %v", i, err)
		}
	}

	// Give delivery goroutine a moment to fill the 1-slot buffer and block on send.
	time.Sleep(30 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		done <- c.Unsubscribe(ctx, "slow-topic")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Unsubscribe(slow-topic) error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unsubscribe deadlocked on slow consumer")
	}

	// Drain any buffered message and confirm channel is closed.
	drainDeadline := time.After(time.Second)
drainLoop:
	for {
		select {
		case _, ok := <-chSlow:
			if !ok {
				break drainLoop
			}
		case <-drainDeadline:
			t.Fatal("timed out waiting for slow-topic channel to close")
		}
	}

	// Reconnect/retry path: close miniredis, let ReceiveMessage retry, restart miniredis.
	chRecon, err := c.Subscribe(ctx, "reconnect-topic")
	if err != nil {
		t.Fatalf("Subscribe(reconnect-topic) error = %v", err)
	}
	mr.Close()
	time.Sleep(150 * time.Millisecond)
	if err := mr.Restart(); err != nil {
		t.Fatalf("mr.Restart() error = %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var recovered bool
	for time.Now().Before(deadline) {
		_ = c.Publish(ctx, "reconnect-topic", []byte("after-restart"))
		select {
		case got, ok := <-chRecon:
			if !ok {
				t.Fatal("reconnect-topic channel closed unexpectedly")
			}
			if string(got) == "after-restart" {
				recovered = true
			}
		case <-time.After(50 * time.Millisecond):
		}
		if recovered {
			break
		}
	}
	if !recovered {
		t.Fatal("subscription did not recover after server restart")
	}
}

func TestClient_ConcurrentLifecycleAndGoroutineLeaks(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	shared := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	c, err := NewClientFromClient(shared, WithChannelBuffer(16))
	if err != nil {
		t.Fatalf("NewClientFromClient() error = %v", err)
	}

	topics := []string{"alpha", "beta", "evt:*", "item:?"}
	var wg sync.WaitGroup
	var drainers sync.WaitGroup
	for g := range 6 {
		wg.Go(func() {
			for i := range 10 {
				topic := topics[(g+i)%len(topics)]
				switch (g + i) % 3 {
				case 0:
					if ch, subErr := c.Subscribe(ctx, topic); subErr == nil {
						drainers.Go(func() {
							for range ch {
							}
						})
					}
				case 1:
					pubTopic := strings.ReplaceAll(strings.ReplaceAll(topic, "*", "x"), "?", "1")
					_ = c.Publish(ctx, pubTopic, []byte("concurrent-msg"))
				case 2:
					_ = c.Unsubscribe(ctx, topic)
				}
			}
		})
	}
	wg.Wait()

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	drainers.Wait()
	if err := shared.Close(); err != nil {
		t.Fatalf("shared.Close() error = %v", err)
	}

	// Give runtime a moment to settle exited goroutines before inspecting leak profile.
	time.Sleep(50 * time.Millisecond)

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/redis") {
			t.Errorf("detected leaked goroutine in pubsub/redis:\n%s", buf.String())
		}
	}
}

func TestClient_CloseUnblocksStalledSubscribe(t *testing.T) {
	runCase := func(t *testing.T, topic string, makeClient func(addr string) (*Client, func())) {
		t.Helper()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen() error = %v", err)
		}

		accepted := make(chan struct{})
		var acceptOnce sync.Once
		var connsMu sync.Mutex
		var conns []net.Conn
		var acceptWG sync.WaitGroup
		acceptWG.Go(func() {
			for {
				conn, acceptErr := ln.Accept()
				if acceptErr != nil {
					return
				}
				connsMu.Lock()
				conns = append(conns, conn)
				connsMu.Unlock()
				acceptOnce.Do(func() { close(accepted) })
			}
		})

		cleanupListener := func() {
			_ = ln.Close()
			acceptWG.Wait()
			connsMu.Lock()
			for _, conn := range conns {
				_ = conn.Close()
			}
			connsMu.Unlock()
		}
		defer cleanupListener()

		c, extraCleanup := makeClient(ln.Addr().String())
		if extraCleanup != nil {
			defer extraCleanup()
		}

		type subResult struct {
			ch  <-chan []byte
			err error
		}
		subDone := make(chan subResult, 1)
		go func() {
			ch, subErr := c.Subscribe(context.Background(), topic)
			subDone <- subResult{ch: ch, err: subErr}
		}()

		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for stalled TCP listener to accept connection")
		}

		closeDone := make(chan error, 1)
		go func() {
			closeDone <- c.Close()
		}()

		select {
		case closeErr := <-closeDone:
			if closeErr != nil {
				t.Fatalf("Close() error = %v", closeErr)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("Close() did not return within 1s")
		}

		select {
		case res := <-subDone:
			if !errors.Is(res.err, ErrClosed) {
				t.Fatalf("Subscribe() error = %v, want ErrClosed", res.err)
			}
			if res.ch != nil {
				t.Fatalf("Subscribe() returned non-nil channel on ErrClosed")
			}
		case <-time.After(1 * time.Second):
			t.Fatal("Subscribe() did not unblock within 1s after Close()")
		}

		cleanupListener()
		if extraCleanup != nil {
			extraCleanup()
		}
		time.Sleep(50 * time.Millisecond)

		if prof := pprof.Lookup("goroutineleak"); prof != nil {
			var buf bytes.Buffer
			if err := prof.WriteTo(&buf, 1); err != nil {
				t.Fatalf("goroutineleak WriteTo() error = %v", err)
			}
			if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/redis") {
				t.Errorf("detected leaked goroutine in pubsub/redis:\n%s", buf.String())
			}
		}
	}

	t.Run("NewClient", func(t *testing.T) {
		runCase(t, "stalled", func(addr string) (*Client, func()) {
			c, err := NewClient(&redis.UniversalOptions{Addrs: []string{addr}})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			return c, nil
		})
	})

	t.Run("NewClientFromClient", func(t *testing.T) {
		runCase(t, "stalled:*", func(addr string) (*Client, func()) {
			shared := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{addr}})
			c, err := NewClientFromClient(shared)
			if err != nil {
				_ = shared.Close()
				t.Fatalf("NewClientFromClient() error = %v", err)
			}
			return c, func() { _ = shared.Close() }
		})
	})
}

type interceptPubSubConn struct {
	net.Conn
	subMatch       string
	unsubMatch     string
	subSentCh      chan struct{}
	subOnce        *sync.Once
	allowSubWrite  chan struct{}
	unsubSentCount *int32
	mu             *sync.Mutex
}

func (c *interceptPubSubConn) Write(p []byte) (int, error) {
	lower := strings.ToLower(string(p))
	if strings.Contains(lower, c.subMatch) && !strings.Contains(lower, c.unsubMatch) {
		c.subOnce.Do(func() {
			close(c.subSentCh)
		})
		<-c.allowSubWrite
	}
	if strings.Contains(lower, c.unsubMatch) {
		c.mu.Lock()
		*c.unsubSentCount++
		c.mu.Unlock()
	}
	return c.Conn.Write(p)
}

func TestClient_CloseUnusedPubSubOnLateCancel(t *testing.T) {
	cases := []struct {
		name       string
		topic      string
		isPattern  bool
		subMatch   string
		unsubMatch string
		lateClose  bool
		wantErr    error
	}{
		{
			name:       "ExactLateCtxCancel",
			topic:      "late:exact",
			isPattern:  false,
			subMatch:   "subscribe",
			unsubMatch: "unsubscribe",
			lateClose:  false,
			wantErr:    context.Canceled,
		},
		{
			name:       "PatternLateCtxCancel",
			topic:      "late:*",
			isPattern:  true,
			subMatch:   "psubscribe",
			unsubMatch: "punsubscribe",
			lateClose:  false,
			wantErr:    context.Canceled,
		},
		{
			name:       "ExactLateClientClose",
			topic:      "close:exact",
			isPattern:  false,
			subMatch:   "subscribe",
			unsubMatch: "unsubscribe",
			lateClose:  true,
			wantErr:    ErrClosed,
		},
		{
			name:       "PatternLateClientClose",
			topic:      "close:*",
			isPattern:  true,
			subMatch:   "psubscribe",
			unsubMatch: "punsubscribe",
			lateClose:  true,
			wantErr:    ErrClosed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			subSentCh := make(chan struct{})
			allowSubWrite := make(chan struct{})
			var subOnce sync.Once
			var unsubSentCount int32
			var unsubMu sync.Mutex

			shared := redis.NewUniversalClient(&redis.UniversalOptions{
				Addrs: []string{mr.Addr()},
				Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					return &interceptPubSubConn{
						Conn:           conn,
						subMatch:       tc.subMatch,
						unsubMatch:     tc.unsubMatch,
						subSentCh:      subSentCh,
						subOnce:        &subOnce,
						allowSubWrite:  allowSubWrite,
						unsubSentCount: &unsubSentCount,
						mu:             &unsubMu,
					}, nil
				},
			})
			defer func() { _ = shared.Close() }()

			c, err := NewClientFromClient(shared)
			if err != nil {
				t.Fatalf("NewClientFromClient() error = %v", err)
			}

			subCtx, subCancel := context.WithCancel(t.Context())
			defer subCancel()

			type subRes struct {
				ch  <-chan []byte
				err error
			}
			resCh := make(chan subRes, 1)
			go func() {
				ch, subErr := c.Subscribe(subCtx, tc.topic)
				resCh <- subRes{ch: ch, err: subErr}
			}()

			// Wait until SUBSCRIBE/PSUBSCRIBE is being sent (Subscribe has unlocked c.mu),
			// then hold c.mu while letting the handshake complete and reach line 303.
			<-subSentCh
			c.mu.Lock()
			close(allowSubWrite)
			time.Sleep(25 * time.Millisecond)

			if tc.lateClose {
				c.closed = true
				c.closeCancel()
			} else {
				subCancel()
			}
			c.mu.Unlock()

			select {
			case res := <-resCh:
				if !errors.Is(res.err, tc.wantErr) {
					t.Fatalf("Subscribe() error = %v, want %v", res.err, tc.wantErr)
				}
				if res.ch != nil {
					t.Fatalf("expected nil channel on aborted Subscribe, got %v", res.ch)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for Subscribe to complete")
			}

			c.handshakeWG.Wait()
			c.wg.Wait()
			if !tc.lateClose {
				if err := c.Close(); err != nil {
					t.Fatalf("c.Close() error = %v", err)
				}
			}

			unsubMu.Lock()
			gotUnsub := unsubSentCount
			unsubMu.Unlock()
			if gotUnsub == 0 {
				t.Errorf("expected %q to be sent on wire by closeUnusedPubSub, got 0", tc.unsubMatch)
			}

			// Verify server has zero remaining subscriptions for the topic/pattern.
			if tc.isPattern {
				numPat, err := shared.PubSubNumPat(t.Context()).Result()
				if err != nil {
					t.Fatalf("PubSubNumPat() error = %v", err)
				}
				if numPat != 0 {
					t.Errorf("PubSubNumPat = %d, want 0", numPat)
				}
			} else {
				numSub, err := shared.PubSubNumSub(t.Context(), tc.topic).Result()
				if err != nil {
					t.Fatalf("PubSubNumSub() error = %v", err)
				}
				if numSub[tc.topic] != 0 {
					t.Errorf("PubSubNumSub[%q] = %d, want 0", tc.topic, numSub[tc.topic])
				}
			}
		})
	}

	time.Sleep(50 * time.Millisecond)
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/redis") {
			t.Errorf("detected leaked goroutine in pubsub/redis:\n%s", buf.String())
		}
	}
}

func TestClient_SlowConsumerCloseAndConcurrentSmallBuffer(t *testing.T) {
	ctx := t.Context()
	mr := miniredis.RunT(t)

	// 1. Slow consumer (WithChannelBuffer(1)) racing directly with Close().
	cSlow, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}}, WithChannelBuffer(1))
	if err != nil {
		t.Fatalf("NewClient(slow) error = %v", err)
	}
	chSlow, err := cSlow.Subscribe(ctx, "slow-close-topic")
	if err != nil {
		_ = cSlow.Close()
		t.Fatalf("Subscribe(slow-close-topic) error = %v", err)
	}
	for i := range 10 {
		if err := cSlow.Publish(ctx, "slow-close-topic", []byte{byte('a' + i)}); err != nil {
			_ = cSlow.Close()
			t.Fatalf("Publish(%d) error = %v", i, err)
		}
	}
	// Let delivery goroutine fill the 1-slot channel and block on ch <- payload.
	bufDeadline := time.Now().Add(2 * time.Second)
	for len(chSlow) == 0 && time.Now().Before(bufDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- cSlow.Close()
	}()
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatalf("cSlow.Close() error = %v", closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cSlow.Close() deadlocked on full subscriber channel")
	}
	for range chSlow {
	}

	// 2. Concurrent Subscribe/Publish/Unsubscribe/Close stress with WithChannelBuffer(1).
	cStress, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}}, WithChannelBuffer(1))
	if err != nil {
		t.Fatalf("NewClient(stress) error = %v", err)
	}
	topics := []string{"s:alpha", "s:beta", "s:*"}
	var wg sync.WaitGroup
	var drainers sync.WaitGroup
	for g := range 6 {
		wg.Go(func() {
			for i := range 8 {
				topic := topics[(g+i)%len(topics)]
				switch (g + i) % 3 {
				case 0:
					if ch, subErr := cStress.Subscribe(ctx, topic); subErr == nil {
						drainers.Go(func() {
							for range ch {
							}
						})
					}
				case 1:
					pubTopic := strings.ReplaceAll(topic, "*", "x")
					_ = cStress.Publish(ctx, pubTopic, []byte("burst-msg"))
				case 2:
					_ = cStress.Unsubscribe(ctx, topic)
				}
			}
		})
	}
	time.Sleep(25 * time.Millisecond)
	if err := cStress.Close(); err != nil {
		t.Fatalf("cStress.Close() error = %v", err)
	}
	wg.Wait()
	drainers.Wait()

	time.Sleep(50 * time.Millisecond)
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/redis") {
			t.Errorf("detected leaked goroutine in pubsub/redis:\n%s", buf.String())
		}
	}
}

// Compile-time structural check that Client satisfies memorystore.PubSubClient.
type pubSubContract interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}

var _ pubSubContract = (*Client)(nil)

type fakePubSubReceiver struct {
	mu      sync.Mutex
	results []fakeReceiveResult
	idx     int
	blockCh chan struct{}
}

type fakeReceiveResult struct {
	msg *redis.Message
	err error
}

func (f *fakePubSubReceiver) ReceiveMessage(ctx context.Context) (*redis.Message, error) {
	f.mu.Lock()
	if f.idx < len(f.results) {
		res := f.results[f.idx]
		f.idx++
		f.mu.Unlock()
		return res.msg, res.err
	}
	blockCh := f.blockCh
	f.mu.Unlock()

	if blockCh != nil {
		select {
		case <-blockCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestReceiveLoop_ExponentialBackoffAndReset(t *testing.T) {
	simErr := errors.New("redis connection lost")
	ps := &fakePubSubReceiver{
		results: []fakeReceiveResult{
			{err: simErr},
			{err: simErr},
			{err: simErr},
			{err: simErr},
			{err: simErr},
			{err: simErr},
			{err: simErr},
			{err: simErr},
			{msg: &redis.Message{Payload: "recovered"}},
			{err: simErr},
			{err: simErr},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch := make(chan []byte, 4)
	var delaysMu sync.Mutex
	var delays []time.Duration
	reachedBlocking := make(chan struct{})
	var notifyOnce sync.Once

	waitFn := func(_ context.Context, d time.Duration) bool {
		delaysMu.Lock()
		delays = append(delays, d)
		count := len(delays)
		delaysMu.Unlock()
		if count == 10 {
			notifyOnce.Do(func() { close(reachedBlocking) })
		}
		return true
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(ch)
		receiveLoop(ctx, "backoff-topic", ps, ch, waitFn, nil)
	})

	gotMsg := receiveWithTimeout(t, ch, time.Second)
	if string(gotMsg) != "recovered" {
		t.Fatalf("received %q, want %q", string(gotMsg), "recovered")
	}

	select {
	case <-reachedBlocking:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for post-reset backoff sequence")
	}

	cancel()
	wg.Wait()
	assertChannelClosed(t, ch, time.Second)

	wantDelays := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		3200 * time.Millisecond,
		5 * time.Second,
		5 * time.Second,
		100 * time.Millisecond,
		200 * time.Millisecond,
	}
	delaysMu.Lock()
	gotDelays := slices.Clone(delays)
	delaysMu.Unlock()
	if !slices.Equal(gotDelays, wantDelays) {
		t.Fatalf("backoff delays = %v, want %v", gotDelays, wantDelays)
	}

	time.Sleep(50 * time.Millisecond)
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/redis") {
			t.Errorf("detected leaked goroutine in pubsub/redis:\n%s", buf.String())
		}
	}
}

func TestReceiveLoop_CancelDuringBackoff(t *testing.T) {
	ps := &fakePubSubReceiver{
		results: []fakeReceiveResult{
			{err: errors.New("transient error")},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch := make(chan []byte, 1)
	done := make(chan struct{})
	start := time.Now()

	go func() {
		defer close(done)
		defer close(ch)
		receiveLoop(ctx, "cancel-backoff-topic", ps, ch, waitRetry, nil)
	}()

	time.Sleep(15 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(80 * time.Millisecond):
		t.Fatalf("receiveLoop did not exit promptly after cancel during backoff (elapsed %v)", time.Since(start))
	}
	assertChannelClosed(t, ch, time.Second)

	time.Sleep(50 * time.Millisecond)
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/redis") {
			t.Errorf("detected leaked goroutine in pubsub/redis:\n%s", buf.String())
		}
	}
}

type redisLogEntry struct {
	level   slog.Level
	message string
	attrs   map[string]string
}

type redisCaptureHandler struct {
	mu      sync.Mutex
	entries []redisLogEntry
}

func (h *redisCaptureHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelDebug
}

func (h *redisCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, redisLogEntry{
		level:   r.Level,
		message: r.Message,
		attrs:   attrs,
	})
	h.mu.Unlock()
	return nil
}

func (h *redisCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *redisCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *redisCaptureHandler) find(lvl slog.Level, substr string) (redisLogEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.level == lvl && strings.Contains(e.message, substr) {
			return e, true
		}
	}
	return redisLogEntry{}, false
}

func (h *redisCaptureHandler) snapshot() []redisLogEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.entries)
}

type failWriteConn struct {
	net.Conn
	failWrite *atomic.Bool
	writeErr  error
}

func (c *failWriteConn) Write(p []byte) (int, error) {
	if c.failWrite != nil && c.failWrite.Load() {
		return 0, c.writeErr
	}
	return c.Conn.Write(p)
}

type fakePubSubHandle struct {
	unsubErr   error
	punsubErr  error
	closeErr   error
	unsubCalls atomic.Int32
	closeCalls atomic.Int32
}

func (f *fakePubSubHandle) ReceiveMessage(ctx context.Context) (*redis.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakePubSubHandle) Unsubscribe(context.Context, ...string) error {
	f.unsubCalls.Add(1)
	return f.unsubErr
}

func (f *fakePubSubHandle) PUnsubscribe(context.Context, ...string) error {
	f.unsubCalls.Add(1)
	return f.punsubErr
}

func (f *fakePubSubHandle) Close() error {
	f.closeCalls.Add(1)
	return f.closeErr
}

func TestWithLogger(t *testing.T) {
	t.Run("NilLoggerIsSafe", func(t *testing.T) {
		mr := miniredis.RunT(t)
		c, err := NewClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}}, WithLogger(nil))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		defer func() { _ = c.Close() }()
		if c.log() == nil {
			t.Fatal("expected non-nil discard logger")
		}
		c.log().Warn("discarded")

		zeroClient := &Client{}
		zeroClient.log().Warn("discarded")
	})

	t.Run("ReceiveLoopLogsBackoffAndRecovery", func(t *testing.T) {
		handler := &redisCaptureHandler{}
		ps := &fakePubSubReceiver{
			results: []fakeReceiveResult{
				{err: errors.New("temporary read failure")},
				{msg: &redis.Message{Payload: "ok"}},
			},
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ch := make(chan []byte, 2)
		var wg sync.WaitGroup
		wg.Go(func() {
			defer close(ch)
			receiveLoop(ctx, "orders", ps, ch, func(context.Context, time.Duration) bool { return true }, slog.New(handler))
		})

		got := receiveWithTimeout(t, ch, time.Second)
		if string(got) != "ok" {
			t.Fatalf("received %q, want ok", string(got))
		}
		cancel()
		wg.Wait()

		warnEntry, ok := handler.find(slog.LevelWarn, "redis receive failed; retrying with backoff")
		if !ok {
			t.Fatalf("expected backoff warn log, got %+v", handler.entries)
		}
		if warnEntry.attrs["topic"] != "orders" {
			t.Errorf("warn topic attr = %q, want orders", warnEntry.attrs["topic"])
		}
		if warnEntry.attrs["retry_delay"] == "" || warnEntry.attrs["err"] == "" {
			t.Errorf("expected retry_delay and err attrs, got %+v", warnEntry.attrs)
		}

		infoEntry, ok := handler.find(slog.LevelInfo, "redis receive loop recovered")
		if !ok {
			t.Fatalf("expected recovery info log, got %+v", handler.entries)
		}
		if infoEntry.attrs["topic"] != "orders" {
			t.Errorf("info topic attr = %q, want orders", infoEntry.attrs["topic"])
		}
	})

	t.Run("StopSubscriptionLogsUnsubscribeError", func(t *testing.T) {
		mr := miniredis.RunT(t)
		unsubErr := errors.New("injected unsubscribe write failure")
		closeErr := errors.New("injected pubsub close failure")

		var failWrite atomic.Bool

		newFaultClient := func(handler *redisCaptureHandler) *Client {
			c, err := NewClient(
				&redis.UniversalOptions{
					Addrs: []string{mr.Addr()},
					Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
						conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, addr)
						if dialErr != nil {
							return nil, dialErr
						}
						return &failWriteConn{
							Conn:      conn,
							failWrite: &failWrite,
							writeErr:  unsubErr,
						}, nil
					},
				},
				WithLogger(slog.New(handler)),
			)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			return c
		}

		ctx := t.Context()

		// 1. Exact topic Unsubscribe write error via real Dialer + *redis.PubSub.
		exactHandler := &redisCaptureHandler{}
		cExact := newFaultClient(exactHandler)
		defer func() { _ = cExact.Close() }()

		chExact, err := cExact.Subscribe(ctx, "disconnect-topic")
		if err != nil {
			t.Fatalf("Subscribe(exact) error = %v", err)
		}
		failWrite.Store(true)
		err = cExact.Unsubscribe(ctx, "disconnect-topic")
		failWrite.Store(false)
		if err != nil {
			t.Fatalf("Unsubscribe(exact) error = %v", err)
		}
		assertChannelClosed(t, chExact, 2*time.Second)

		exactWarn, ok := exactHandler.find(slog.LevelWarn, "redis unsubscribe failed")
		if !ok {
			t.Fatalf("expected warn log on exact unsubscribe error, got %+v", exactHandler.snapshot())
		}
		if exactWarn.attrs["topic"] != "disconnect-topic" || exactWarn.attrs["pattern"] != "false" || !strings.Contains(exactWarn.attrs["err"], unsubErr.Error()) {
			t.Errorf("unexpected exact unsubscribe warn attrs: %+v", exactWarn.attrs)
		}

		// 2. Pattern topic PUnsubscribe write error via real Dialer + *redis.PubSub.
		patHandler := &redisCaptureHandler{}
		cPat := newFaultClient(patHandler)
		defer func() { _ = cPat.Close() }()

		chPat, err := cPat.Subscribe(ctx, "disconnect-*")
		if err != nil {
			t.Fatalf("Subscribe(pattern) error = %v", err)
		}
		failWrite.Store(true)
		err = cPat.Unsubscribe(ctx, "disconnect-*")
		failWrite.Store(false)
		if err != nil {
			t.Fatalf("Unsubscribe(pattern) error = %v", err)
		}
		assertChannelClosed(t, chPat, 2*time.Second)

		patWarn, ok := patHandler.find(slog.LevelWarn, "redis unsubscribe failed")
		if !ok {
			t.Fatalf("expected warn log on pattern punsubscribe error, got %+v", patHandler.snapshot())
		}
		if patWarn.attrs["topic"] != "disconnect-*" || patWarn.attrs["pattern"] != "true" || !strings.Contains(patWarn.attrs["err"], unsubErr.Error()) {
			t.Errorf("unexpected pattern punsubscribe warn attrs: %+v", patWarn.attrs)
		}

		// 3. PubSub.Close error in stopSubscription (go-redis standalone Client.pubSub
		// swallows cn.Close() errors in redis.go, so exercise stopSubscription directly
		// with a failing pubsubHandle, and verify redis.ErrClosed/context.Canceled suppression).
		closeHandler := &redisCaptureHandler{}
		cClose := newClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), config{
			channelBuffer: 1,
			closeClient:   true,
			logger:        slog.New(closeHandler),
		})
		defer func() { _ = cClose.Close() }()

		psErr := &fakePubSubHandle{closeErr: closeErr}
		_, cancelErr := context.WithCancel(ctx)
		entryErr := &redisSubscription{
			topic:  "close-err-topic",
			ps:     psErr,
			cancel: cancelErr,
		}
		if err := cClose.stopSubscription(ctx, entryErr); err != nil {
			t.Fatalf("stopSubscription(closeErr) error = %v", err)
		}
		closeWarn, ok := closeHandler.find(slog.LevelWarn, "redis pubsub close failed")
		if !ok {
			t.Fatalf("expected warn log on pubsub close error, got %+v", closeHandler.snapshot())
		}
		if closeWarn.attrs["topic"] != "close-err-topic" || !strings.Contains(closeWarn.attrs["err"], closeErr.Error()) {
			t.Errorf("unexpected pubsub close warn attrs: %+v", closeWarn.attrs)
		}

		suppressedHandler := &redisCaptureHandler{}
		cSuppressed := newClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), config{
			channelBuffer: 1,
			closeClient:   true,
			logger:        slog.New(suppressedHandler),
		})
		defer func() { _ = cSuppressed.Close() }()

		_, cancelSup := context.WithCancel(ctx)
		entrySup := &redisSubscription{
			topic:  "suppressed-topic",
			ps:     &fakePubSubHandle{unsubErr: redis.ErrClosed, closeErr: redis.ErrClosed},
			cancel: cancelSup,
		}
		if err := cSuppressed.stopSubscription(ctx, entrySup); err != nil {
			t.Fatalf("stopSubscription(suppressed) error = %v", err)
		}
		if entries := suppressedHandler.snapshot(); len(entries) != 0 {
			t.Errorf("expected redis.ErrClosed errors to be suppressed, got %+v", entries)
		}
	})
}
