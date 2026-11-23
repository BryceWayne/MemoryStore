package nats

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func startTestNATSServer(t *testing.T, enableJS bool) *server.Server {
	t.Helper()

	opts := &server.Options{
		Host:                   "127.0.0.1",
		Port:                   -1,
		NoLog:                  true,
		NoSigs:                 true,
		JetStream:              enableJS,
		DisableJetStreamBanner: true,
	}
	if enableJS {
		opts.StoreDir = t.TempDir()
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("server.NewServer() error = %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded nats-server did not become ready in time")
	}

	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	return ns
}

func assertNoGoroutineLeaks(t *testing.T) {
	t.Helper()

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "github.com/BryceWayne/MemoryStore/pubsub/nats") ||
			strings.Contains(out, "github.com/nats-io/nats.go") {
			t.Errorf("detected leaked goroutine:\n%s", out)
		}
	}
}

func receiveOne(t *testing.T, ch <-chan []byte, timeout time.Duration) string {
	t.Helper()
	select {
	case msg, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly while waiting for message")
		}
		return string(msg)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for message")
		return ""
	}
}

func assertChannelClosed(t *testing.T, ch <-chan []byte, timeout time.Duration) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed, received unexpected message")
		}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for channel close")
	}
}

func TestConstructorValidation(t *testing.T) {
	if _, err := NewClient(""); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("NewClient(empty) = %v, want ErrInvalidURL", err)
	}
	if _, err := NewClient("   "); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("NewClient(spaces) = %v, want ErrInvalidURL", err)
	}
	if _, err := NewClient("nats://127.0.0.1:1", WithNATSOptions(nats.Timeout(50*time.Millisecond))); err == nil {
		t.Error("NewClient(unreachable) expected error, got nil")
	}

	if _, err := NewClientFromConn(nil); !errors.Is(err, ErrInvalidConn) {
		t.Errorf("NewClientFromConn(nil) = %v, want ErrInvalidConn", err)
	}

	ns := startTestNATSServer(t, false)
	ncClosed, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	ncClosed.Close()
	if _, err := NewClientFromConn(ncClosed); !errors.Is(err, ErrConnClosed) {
		t.Errorf("NewClientFromConn(closed) = %v, want ErrConnClosed", err)
	}

	ncDraining, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	if err := ncDraining.Drain(); err != nil {
		t.Fatalf("ncDraining.Drain() error = %v", err)
	}
	if _, err := NewClientFromConn(ncDraining); !errors.Is(err, ErrConnClosed) {
		t.Errorf("NewClientFromConn(draining) = %v, want ErrConnClosed", err)
	}
	ncDraining.Close()

	if _, err := NewClient(ns.ClientURL(), WithJetStream("")); !errors.Is(err, ErrInvalidJetStream) {
		t.Errorf("WithJetStream(empty) = %v, want ErrInvalidJetStream", err)
	}
	if _, err := NewClient(ns.ClientURL(), WithJetStream("bad.stream")); !errors.Is(err, ErrInvalidJetStream) {
		t.Errorf("WithJetStream(bad.stream) = %v, want ErrInvalidJetStream", err)
	}
	if _, err := NewClient(ns.ClientURL(), WithJetStreamDurable("svc")); !errors.Is(err, ErrInvalidJetStream) {
		t.Errorf("WithJetStreamDurable without WithJetStream = %v, want ErrInvalidJetStream", err)
	}
	if _, err := NewClient(ns.ClientURL(), WithJetStream("OK_STREAM"), WithJetStreamDurable("")); !errors.Is(err, ErrInvalidDurablePrefix) {
		t.Errorf("WithJetStreamDurable(empty) = %v, want ErrInvalidDurablePrefix", err)
	}
	if _, err := NewClient(ns.ClientURL(), WithJetStream("OK_STREAM"), WithJetStreamDurable("bad/prefix")); !errors.Is(err, ErrInvalidDurablePrefix) {
		t.Errorf("WithJetStreamDurable(bad/prefix) = %v, want ErrInvalidDurablePrefix", err)
	}
	if _, err := NewClient(ns.ClientURL(), WithQueueGroup("bad group")); !errors.Is(err, nats.ErrBadQueueName) {
		t.Errorf("WithQueueGroup(bad group) = %v, want ErrBadQueueName", err)
	}

	// Non-positive buffer / drain timeout and nil options preserve defaults.
	c, err := NewClient(
		ns.ClientURL(),
		nil,
		WithChannelBuffer(0),
		WithChannelBuffer(-5),
		WithDrainTimeout(0),
		WithNATSOptions(nil),
	)
	if err != nil {
		t.Fatalf("NewClient(defaults) error = %v", err)
	}
	if c.cfg.channelBuffer != defaultChannelBuffer {
		t.Errorf("channelBuffer = %d, want %d", c.cfg.channelBuffer, defaultChannelBuffer)
	}
	if c.cfg.drainTimeout != defaultDrainTimeout {
		t.Errorf("drainTimeout = %v, want %v", c.cfg.drainTimeout, defaultDrainTimeout)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestClient_CorePubSubLifecycle(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, false)

	c, err := NewClient(ns.ClientURL(), WithChannelBuffer(16))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	// Setup-only context: canceling subCtx right after Subscribe returns must not stop delivery.
	subCtx, cancelSub := context.WithCancel(ctx)
	ch, err := c.Subscribe(subCtx, "cache.users")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	cancelSub()

	if err := c.Publish(ctx, "cache.users", []byte("invalidate-1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := receiveOne(t, ch, 2*time.Second); got != "invalidate-1" {
		t.Errorf("received %q, want invalidate-1", got)
	}

	// Empty topic checks.
	if _, err := c.Subscribe(ctx, ""); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Subscribe(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Publish(ctx, "", []byte("x")); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Publish(empty) = %v, want ErrInvalidTopic", err)
	}

	// Idempotent Unsubscribe on missing topic.
	if err := c.Unsubscribe(ctx, "does.not.exist"); err != nil {
		t.Errorf("Unsubscribe(missing) = %v, want nil", err)
	}

	// Canceled ctx guards.
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

	// Unsubscribe closes channel.
	if err := c.Unsubscribe(ctx, "cache.users"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}
	assertChannelClosed(t, ch, 2*time.Second)

	// Close is idempotent and post-Close calls return ErrClosed.
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, err := c.Subscribe(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Publish(ctx, "t", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish(closed) = %v, want ErrClosed", err)
	}
	if err := c.Unsubscribe(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Unsubscribe(closed) = %v, want ErrClosed", err)
	}
}

func TestClient_Wildcards(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, false)

	c, err := NewClient(ns.ClientURL())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	singleCh, err := c.Subscribe(ctx, "cache.*")
	if err != nil {
		t.Fatalf("Subscribe(cache.*) error = %v", err)
	}
	multiCh, err := c.Subscribe(ctx, "cache.>")
	if err != nil {
		t.Fatalf("Subscribe(cache.>) error = %v", err)
	}

	if err := c.Publish(ctx, "cache.users", []byte("m1")); err != nil {
		t.Fatalf("Publish(cache.users) error = %v", err)
	}
	if err := c.Publish(ctx, "cache.users.42", []byte("m2")); err != nil {
		t.Fatalf("Publish(cache.users.42) error = %v", err)
	}

	if got := receiveOne(t, singleCh, 2*time.Second); got != "m1" {
		t.Errorf("singleCh got %q, want m1", got)
	}
	// Single-token wildcard must NOT receive multi-token cache.users.42.
	select {
	case extra := <-singleCh:
		t.Errorf("singleCh unexpectedly received %q for multi-token subject", string(extra))
	case <-time.After(100 * time.Millisecond):
	}

	if got := receiveOne(t, multiCh, 2*time.Second); got != "m1" {
		t.Errorf("multiCh first got %q, want m1", got)
	}
	if got := receiveOne(t, multiCh, 2*time.Second); got != "m2" {
		t.Errorf("multiCh second got %q, want m2", got)
	}
}

func TestClient_QueueGroup(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, false)

	w1, err := NewClient(ns.ClientURL(), WithQueueGroup("workers"))
	if err != nil {
		t.Fatalf("NewClient w1 error = %v", err)
	}
	defer func() { _ = w1.Close() }()

	w2, err := NewClient(ns.ClientURL(), WithQueueGroup("workers"))
	if err != nil {
		t.Fatalf("NewClient w2 error = %v", err)
	}
	defer func() { _ = w2.Close() }()

	pub, err := NewClient(ns.ClientURL())
	if err != nil {
		t.Fatalf("NewClient pub error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	ch1, err := w1.Subscribe(ctx, "jobs.process")
	if err != nil {
		t.Fatalf("w1.Subscribe() error = %v", err)
	}
	ch2, err := w2.Subscribe(ctx, "jobs.process")
	if err != nil {
		t.Fatalf("w2.Subscribe() error = %v", err)
	}

	const total = 20
	for i := range total {
		if err := pub.Publish(ctx, "jobs.process", fmt.Appendf(nil, "job-%d", i)); err != nil {
			t.Fatalf("Publish(%d) error = %v", i, err)
		}
	}

	seen := make(map[string]int, total)
	deadline := time.After(3 * time.Second)
	for len(seen) < total {
		select {
		case msg := <-ch1:
			seen[string(msg)]++
		case msg := <-ch2:
			seen[string(msg)]++
		case <-deadline:
			t.Fatalf("timed out waiting for %d queue messages, got %d", total, len(seen))
		}
	}

	// Ensure no duplicates arrived.
	select {
	case dup := <-ch1:
		t.Errorf("unexpected duplicate on ch1: %q", string(dup))
	case dup := <-ch2:
		t.Errorf("unexpected duplicate on ch2: %q", string(dup))
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClient_ConnOwnership(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, false)

	// 1. NewClient closes its owned connection by default.
	c1, err := NewClient(ns.ClientURL())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("c1.Close() error = %v", err)
	}
	if !c1.conn.IsClosed() {
		t.Error("expected NewClient default Close() to close underlying *nats.Conn")
	}

	// 2. NewClient with WithCloseConn(false) leaves connection open.
	c2, err := NewClient(ns.ClientURL(), WithCloseConn(false))
	if err != nil {
		t.Fatalf("NewClient(WithCloseConn(false)) error = %v", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("c2.Close() error = %v", err)
	}
	if c2.conn.IsClosed() {
		t.Error("expected WithCloseConn(false) to leave *nats.Conn open")
	}
	c2.conn.Close()

	// 3. NewClientFromConn leaves caller-owned connection open by default.
	nc3, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	defer nc3.Close()

	c3, err := NewClientFromConn(nc3)
	if err != nil {
		t.Fatalf("NewClientFromConn() error = %v", err)
	}
	if _, err := c3.Subscribe(ctx, "own.test"); err != nil {
		t.Fatalf("c3.Subscribe() error = %v", err)
	}
	if err := c3.Close(); err != nil {
		t.Fatalf("c3.Close() error = %v", err)
	}
	if nc3.IsClosed() {
		t.Error("expected NewClientFromConn default Close() to leave caller *nats.Conn open")
	}

	// 4. NewClientFromConn with WithCloseConn(true) closes connection without overwriting ClosedHandler.
	var closedHandlerFired atomic.Bool
	nc4, err := nats.Connect(ns.ClientURL(), nats.ClosedHandler(func(_ *nats.Conn) {
		closedHandlerFired.Store(true)
	}))
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	c4, err := NewClientFromConn(nc4, WithCloseConn(true))
	if err != nil {
		t.Fatalf("NewClientFromConn(WithCloseConn(true)) error = %v", err)
	}
	if err := c4.Close(); err != nil {
		t.Fatalf("c4.Close() error = %v", err)
	}
	if !nc4.IsClosed() {
		t.Error("expected WithCloseConn(true) to close *nats.Conn")
	}
	deadline := time.Now().Add(time.Second)
	for !closedHandlerFired.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !closedHandlerFired.Load() {
		t.Error("expected existing nats.ClosedHandler to remain intact and fire on Close")
	}
}

func TestClient_DuplicateSubscribe(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, false)

	c, err := NewClient(ns.ClientURL())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	ch1, err := c.Subscribe(ctx, "dup.topic")
	if err != nil {
		t.Fatalf("first Subscribe() error = %v", err)
	}
	ch2, err := c.Subscribe(ctx, "dup.topic")
	if err != nil {
		t.Fatalf("second Subscribe() error = %v", err)
	}

	assertChannelClosed(t, ch1, 2*time.Second)

	if err := c.Publish(ctx, "dup.topic", []byte("after-resub")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := receiveOne(t, ch2, 2*time.Second); got != "after-resub" {
		t.Errorf("ch2 got %q, want after-resub", got)
	}
}

func TestClient_SlowConsumerAndConcurrentTeardown(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, false)

	c, err := NewClient(ns.ClientURL(), WithChannelBuffer(2), WithDrainTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	ch, err := c.Subscribe(ctx, "burst.topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Burst far more messages than channelBuffer without reading.
	for i := range 50 {
		if err := c.Publish(ctx, "burst.topic", fmt.Appendf(nil, "msg-%d", i)); err != nil {
			t.Fatalf("Publish(%d) error = %v", i, err)
		}
	}

	// Unsubscribe must not deadlock or panic on full outCh.
	if err := c.Unsubscribe(ctx, "burst.topic"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}
	// Drain any buffered items and verify ch closes.
	for range ch {
	}

	// Concurrent Subscribe/Publish/Unsubscribe/Close stress under -race.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			topic := fmt.Sprintf("stress.%d", i%3)
			_, _ = c.Subscribe(ctx, topic)
			_ = c.Publish(ctx, topic, []byte("payload"))
			_ = c.Unsubscribe(ctx, topic)
		})
	}
	time.Sleep(10 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	wg.Wait()
}

func TestClient_JetStreamBroadcastQueueAndDurable(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, true)

	// 1. Auto-create disabled returns ErrStreamNotFound when stream does not exist.
	noCreateClient, err := NewClient(
		ns.ClientURL(),
		WithJetStream("MISSING_STREAM"),
		WithJetStreamAutoCreate(false),
	)
	if err != nil {
		t.Fatalf("NewClient(noCreate) error = %v", err)
	}
	if _, err := noCreateClient.Subscribe(ctx, "js.missing"); !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("Subscribe(missing stream, autoCreate=false) = %v, want ErrStreamNotFound", err)
	}
	_ = noCreateClient.Close()

	// 2. Broadcast mode via OrderedConsumer across two clients + multi-subject expansion.
	b1, err := NewClient(ns.ClientURL(), WithJetStream("BROADCAST_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(b1) error = %v", err)
	}
	defer func() { _ = b1.Close() }()

	b2, err := NewClient(ns.ClientURL(), WithJetStream("BROADCAST_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(b2) error = %v", err)
	}
	defer func() { _ = b2.Close() }()

	ch1, err := b1.Subscribe(ctx, "events.alpha")
	if err != nil {
		t.Fatalf("b1.Subscribe() error = %v", err)
	}
	ch2, err := b2.Subscribe(ctx, "events.alpha")
	if err != nil {
		t.Fatalf("b2.Subscribe() error = %v", err)
	}

	if err := b1.Publish(ctx, "events.alpha", []byte("broadcast-1")); err != nil {
		t.Fatalf("b1.Publish() error = %v", err)
	}
	if got := receiveOne(t, ch1, 3*time.Second); got != "broadcast-1" {
		t.Errorf("b1 got %q, want broadcast-1", got)
	}
	if got := receiveOne(t, ch2, 3*time.Second); got != "broadcast-1" {
		t.Errorf("b2 got %q, want broadcast-1", got)
	}

	// Second distinct subject updates stream subjects seamlessly.
	chBeta, err := b1.Subscribe(ctx, "other.beta")
	if err != nil {
		t.Fatalf("b1.Subscribe(other.beta) error = %v", err)
	}
	if err := b2.Publish(ctx, "other.beta", []byte("beta-1")); err != nil {
		t.Fatalf("b2.Publish(other.beta) error = %v", err)
	}
	if got := receiveOne(t, chBeta, 3*time.Second); got != "beta-1" {
		t.Errorf("chBeta got %q, want beta-1", got)
	}

	// 3. Queue group mode load-balances across workers.
	q1, err := NewClient(ns.ClientURL(), WithJetStream("QUEUE_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage), WithQueueGroup("js-workers"))
	if err != nil {
		t.Fatalf("NewClient(q1) error = %v", err)
	}
	defer func() { _ = q1.Close() }()

	q2, err := NewClient(ns.ClientURL(), WithJetStream("QUEUE_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage), WithQueueGroup("js-workers"))
	if err != nil {
		t.Fatalf("NewClient(q2) error = %v", err)
	}
	defer func() { _ = q2.Close() }()

	qch1, err := q1.Subscribe(ctx, "work.item")
	if err != nil {
		t.Fatalf("q1.Subscribe() error = %v", err)
	}
	qch2, err := q2.Subscribe(ctx, "work.item")
	if err != nil {
		t.Fatalf("q2.Subscribe() error = %v", err)
	}

	const queueTotal = 10
	for i := range queueTotal {
		if err := q1.Publish(ctx, "work.item", fmt.Appendf(nil, "q-%d", i)); err != nil {
			t.Fatalf("q1.Publish(%d) error = %v", i, err)
		}
	}
	qSeen := make(map[string]int, queueTotal)
	qDeadline := time.After(4 * time.Second)
	for len(qSeen) < queueTotal {
		select {
		case msg := <-qch1:
			qSeen[string(msg)]++
		case msg := <-qch2:
			qSeen[string(msg)]++
		case <-qDeadline:
			t.Fatalf("timed out waiting for JetStream queue messages, got %d/%d", len(qSeen), queueTotal)
		}
	}

	// 4. Durable mode resumes from ack floor across Close / re-Subscribe.
	d1, err := NewClient(ns.ClientURL(), WithJetStream("DURABLE_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage), WithJetStreamDurable("svc"))
	if err != nil {
		t.Fatalf("NewClient(d1) error = %v", err)
	}
	dch1, err := d1.Subscribe(ctx, "orders.created")
	if err != nil {
		t.Fatalf("d1.Subscribe() error = %v", err)
	}
	if err := d1.Publish(ctx, "orders.created", []byte("order-1")); err != nil {
		t.Fatalf("d1.Publish(order-1) error = %v", err)
	}
	if got := receiveOne(t, dch1, 3*time.Second); got != "order-1" {
		t.Errorf("d1 got %q, want order-1", got)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("d1.Close() error = %v", err)
	}

	// Publish while subscriber is offline using a separate publisher client.
	pub, err := NewClient(ns.ClientURL(), WithJetStream("DURABLE_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(pub) error = %v", err)
	}
	defer func() { _ = pub.Close() }()
	if err := pub.Publish(ctx, "orders.created", []byte("order-2")); err != nil {
		t.Fatalf("pub.Publish(order-2) error = %v", err)
	}

	// Re-subscribe with same durable prefix and receive only order-2.
	d2, err := NewClient(ns.ClientURL(), WithJetStream("DURABLE_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage), WithJetStreamDurable("svc"))
	if err != nil {
		t.Fatalf("NewClient(d2) error = %v", err)
	}
	defer func() { _ = d2.Close() }()
	dch2, err := d2.Subscribe(ctx, "orders.created")
	if err != nil {
		t.Fatalf("d2.Subscribe() error = %v", err)
	}
	if got := receiveOne(t, dch2, 3*time.Second); got != "order-2" {
		t.Errorf("d2 got %q, want order-2 (acked order-1 must not replay)", got)
	}
}

func TestClient_JetStreamDisabledOnServer(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, false)

	c, err := NewClient(ns.ClientURL(), WithJetStream("NO_JS"))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Subscribe(ctx, "t"); !errors.Is(err, jetstream.ErrJetStreamNotEnabled) {
		t.Errorf("Subscribe(JS disabled) = %v, want ErrJetStreamNotEnabled", err)
	}
	if err := c.Publish(ctx, "t", []byte("x")); !errors.Is(err, jetstream.ErrJetStreamNotEnabled) {
		t.Errorf("Publish(JS disabled) = %v, want ErrJetStreamNotEnabled", err)
	}
}

func TestSanitizeConsumerNameAndSubjectMatches(t *testing.T) {
	names := []string{
		sanitizeConsumerName("svc", "", "orders.*.created"),
		sanitizeConsumerName("svc", "workers", "events.>"),
		sanitizeConsumerName("svc", "", "a/b c"),
		sanitizeConsumerName("svc", "", "a.b"),
		sanitizeConsumerName("svc", "", "a_b"),
		sanitizeConsumerName(strings.Repeat("longprefix", 10), "group", "subject.with.dots"),
	}
	for _, name := range names {
		if !isValidJSName(name) {
			t.Errorf("sanitizeConsumerName produced invalid JS name %q", name)
		}
		if len(name) > 65 {
			t.Errorf("sanitizeConsumerName(%q) len = %d, want <= 65", name, len(name))
		}
	}
	if names[3] == names[4] {
		t.Errorf("expected distinct sanitized names for a.b and a_b, both got %q", names[3])
	}

	if !subjectMatches("orders.*", "orders.created") {
		t.Error("expected orders.* to match orders.created")
	}
	if subjectMatches("orders.*", "orders.a.b") {
		t.Error("expected orders.* NOT to match orders.a.b")
	}
	if !subjectMatches("orders.>", "orders.a.b") {
		t.Error("expected orders.> to match orders.a.b")
	}
	if subjectMatches("orders.created", "orders.*") {
		t.Error("expected orders.created NOT to cover orders.*")
	}
	if subjectMatches("orders.*", "orders.>") {
		t.Error("expected orders.* NOT to cover orders.>")
	}
}

func TestClient_NoGoroutineLeaks(t *testing.T) {
	ctx := t.Context()
	nsCore := startTestNATSServer(t, false)
	nsJS := startTestNATSServer(t, true)

	// Core NATS owned client.
	cCore, err := NewClient(nsCore.ClientURL())
	if err != nil {
		t.Fatalf("NewClient(core) error = %v", err)
	}
	chCore, err := cCore.Subscribe(ctx, "leak.core")
	if err != nil {
		t.Fatalf("cCore.Subscribe() error = %v", err)
	}
	if err := cCore.Publish(ctx, "leak.core", []byte("ok")); err != nil {
		t.Fatalf("cCore.Publish() error = %v", err)
	}
	if got := receiveOne(t, chCore, 2*time.Second); got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
	if err := cCore.Close(); err != nil {
		t.Fatalf("cCore.Close() error = %v", err)
	}

	// Core NATS caller-owned connection (WithCloseConn(false) and WithCloseConn(true)).
	ncExt, err := nats.Connect(nsCore.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	cExtFalse, err := NewClientFromConn(ncExt)
	if err != nil {
		t.Fatalf("NewClientFromConn(false) error = %v", err)
	}
	chExt, err := cExtFalse.Subscribe(ctx, "leak.ext")
	if err != nil {
		t.Fatalf("cExtFalse.Subscribe() error = %v", err)
	}
	if err := cExtFalse.Unsubscribe(ctx, "leak.ext"); err != nil {
		t.Fatalf("cExtFalse.Unsubscribe() error = %v", err)
	}
	assertChannelClosed(t, chExt, 2*time.Second)
	if err := cExtFalse.Close(); err != nil {
		t.Fatalf("cExtFalse.Close() error = %v", err)
	}

	cExtTrue, err := NewClientFromConn(ncExt, WithCloseConn(true))
	if err != nil {
		t.Fatalf("NewClientFromConn(true) error = %v", err)
	}
	if _, err := cExtTrue.Subscribe(ctx, "leak.ext2"); err != nil {
		t.Fatalf("cExtTrue.Subscribe() error = %v", err)
	}
	if err := cExtTrue.Close(); err != nil {
		t.Fatalf("cExtTrue.Close() error = %v", err)
	}

	// JetStream broadcast + durable clients.
	cJS, err := NewClient(nsJS.ClientURL(), WithJetStream("LEAK_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(js) error = %v", err)
	}
	chJS, err := cJS.Subscribe(ctx, "leak.js")
	if err != nil {
		t.Fatalf("cJS.Subscribe() error = %v", err)
	}
	if err := cJS.Publish(ctx, "leak.js", []byte("js-ok")); err != nil {
		t.Fatalf("cJS.Publish() error = %v", err)
	}
	if got := receiveOne(t, chJS, 3*time.Second); got != "js-ok" {
		t.Fatalf("got %q, want js-ok", got)
	}
	if err := cJS.Close(); err != nil {
		t.Fatalf("cJS.Close() error = %v", err)
	}

	// Shut down servers before inspecting goroutineleak profile.
	nsCore.Shutdown()
	nsCore.WaitForShutdown()
	nsJS.Shutdown()
	nsJS.WaitForShutdown()

	assertNoGoroutineLeaks(t)
}

func TestMergeStreamSubjectAndOverlap(t *testing.T) {
	overlapCases := []struct {
		a, b string
		want bool
	}{
		{"events.a", "events.*", true},
		{"events.a", "events.>", true},
		{"events.*", "events.>", true},
		{"a.*", "*.b", true},
		{"a.>", "*.b", true},
		{"a.>", "*.b.>", true},
		{"a.*", "a.b.>", false},
		{"a.>", "a", false},
		{"a.b.c", "a.b.d", false},
		{"", "a.b", false},
	}
	for _, tc := range overlapCases {
		if got := subjectsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("subjectsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}

	mergeCases := []struct {
		name        string
		existing    []string
		topic       string
		wantChanged bool
		want        []string
	}{
		{
			name:        "already covered exact",
			existing:    []string{"events.*"},
			topic:       "events.a",
			wantChanged: false,
			want:        []string{"events.*"},
		},
		{
			name:        "prune exact covered by single wildcard",
			existing:    []string{"events.a", "other.b"},
			topic:       "events.*",
			wantChanged: true,
			want:        []string{"other.b", "events.*"},
		},
		{
			name:        "prune exact and single wildcard covered by gt",
			existing:    []string{"events.a", "events.*", "other.b"},
			topic:       "events.>",
			wantChanged: true,
			want:        []string{"other.b", "events.>"},
		},
		{
			name:        "partial cross overlap widens cleanly",
			existing:    []string{"a.*"},
			topic:       "*.b",
			wantChanged: true,
			want:        []string{"*.*"},
		},
		{
			name:        "cascading merge prunes newly covered subjects",
			existing:    []string{"x.y", "a.*"},
			topic:       "*.b",
			wantChanged: true,
			want:        []string{"*.*"},
		},
		{
			name:        "partial overlap with multi-token wildcard",
			existing:    []string{"a.b.>"},
			topic:       "*.*.c",
			wantChanged: true,
			want:        []string{"*.*.>"},
		},
		{
			name:        "non-overlapping different lengths",
			existing:    []string{"a.*"},
			topic:       "a.b.>",
			wantChanged: true,
			want:        []string{"a.*", "a.b.>"},
		},
	}
	for _, tc := range mergeCases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := mergeStreamSubject(tc.existing, tc.topic)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v (subjects=%v)", changed, tc.wantChanged, got)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("subjects = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClient_JetStreamOverlappingSubjects(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, true)

	// 1. Concrete publish followed by progressively broader wildcards (* then >).
	c, err := NewClient(ns.ClientURL(), WithJetStream("OVERLAP_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Publish(ctx, "events.a", []byte("seed")); err != nil {
		t.Fatalf("Publish(events.a) error = %v", err)
	}

	starCh, err := c.Subscribe(ctx, "events.*")
	if err != nil {
		t.Fatalf("Subscribe(events.*) after Publish(events.a) error = %v", err)
	}
	gtCh, err := c.Subscribe(ctx, "events.>")
	if err != nil {
		t.Fatalf("Subscribe(events.>) after Subscribe(events.*) error = %v", err)
	}

	if err := c.Publish(ctx, "events.a", []byte("msg-a")); err != nil {
		t.Fatalf("Publish(events.a) error = %v", err)
	}
	if err := c.Publish(ctx, "events.b.c", []byte("msg-bc")); err != nil {
		t.Fatalf("Publish(events.b.c) error = %v", err)
	}

	if got := receiveOne(t, starCh, 3*time.Second); got != "msg-a" {
		t.Errorf("starCh got %q, want msg-a", got)
	}
	if got := receiveOne(t, gtCh, 3*time.Second); got != "msg-a" {
		t.Errorf("gtCh first got %q, want msg-a", got)
	}
	if got := receiveOne(t, gtCh, 3*time.Second); got != "msg-bc" {
		t.Errorf("gtCh second got %q, want msg-bc", got)
	}

	// 2. Partial cross-overlap ("cross.a.*" then "cross.*.b") widens to "cross.*.*" without err_code=10052.
	cCross, err := NewClient(ns.ClientURL(), WithJetStream("CROSS_OVERLAP"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(cross) error = %v", err)
	}
	defer func() { _ = cCross.Close() }()

	chA, err := cCross.Subscribe(ctx, "cross.a.*")
	if err != nil {
		t.Fatalf("Subscribe(cross.a.*) error = %v", err)
	}
	chB, err := cCross.Subscribe(ctx, "cross.*.b")
	if err != nil {
		t.Fatalf("Subscribe(cross.*.b) after Subscribe(cross.a.*) error = %v", err)
	}
	if err := cCross.Publish(ctx, "cross.a.b", []byte("ab")); err != nil {
		t.Fatalf("Publish(cross.a.b) error = %v", err)
	}
	if err := cCross.Publish(ctx, "cross.a.x", []byte("ax")); err != nil {
		t.Fatalf("Publish(cross.a.x) error = %v", err)
	}
	if err := cCross.Publish(ctx, "cross.y.b", []byte("yb")); err != nil {
		t.Fatalf("Publish(cross.y.b) error = %v", err)
	}
	if got := receiveOne(t, chA, 3*time.Second); got != "ab" {
		t.Errorf("chA first got %q, want ab", got)
	}
	if got := receiveOne(t, chA, 3*time.Second); got != "ax" {
		t.Errorf("chA second got %q, want ax", got)
	}
	if got := receiveOne(t, chB, 3*time.Second); got != "ab" {
		t.Errorf("chB first got %q, want ab", got)
	}
	if got := receiveOne(t, chB, 3*time.Second); got != "yb" {
		t.Errorf("chB second got %q, want yb", got)
	}
}

func TestClient_JetStreamConcurrentEnsureStream(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, true)

	// 1. Concurrent expansion on an existing stream across two Clients.
	c1, err := NewClient(ns.ClientURL(), WithJetStream("CONC_EXPAND"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(c1) error = %v", err)
	}
	defer func() { _ = c1.Close() }()

	c2, err := NewClient(ns.ClientURL(), WithJetStream("CONC_EXPAND"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(c2) error = %v", err)
	}
	defer func() { _ = c2.Close() }()

	if err := c1.Publish(ctx, "seed.topic", []byte("init")); err != nil {
		t.Fatalf("seed Publish() error = %v", err)
	}

	var (
		chAlpha, chBeta <-chan []byte
		err1, err2      error
		wg              sync.WaitGroup
	)
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		chAlpha, err1 = c1.Subscribe(ctx, "alpha.topic")
	})
	wg.Go(func() {
		<-start
		chBeta, err2 = c2.Subscribe(ctx, "beta.topic")
	})
	close(start)
	wg.Wait()

	if err1 != nil {
		t.Fatalf("c1.Subscribe(alpha.topic) error = %v", err1)
	}
	if err2 != nil {
		t.Fatalf("c2.Subscribe(beta.topic) error = %v", err2)
	}

	stream, err := c1.js.Stream(ctx, "CONC_EXPAND")
	if err != nil {
		t.Fatalf("js.Stream() error = %v", err)
	}
	subjects := stream.CachedInfo().Config.Subjects
	for _, want := range []string{"seed.topic", "alpha.topic", "beta.topic"} {
		if !slices.Contains(subjects, want) {
			t.Errorf("stream subjects = %v, missing %q", subjects, want)
		}
	}

	if err := c2.Publish(ctx, "alpha.topic", []byte("to-alpha")); err != nil {
		t.Fatalf("Publish(alpha.topic) error = %v", err)
	}
	if err := c1.Publish(ctx, "beta.topic", []byte("to-beta")); err != nil {
		t.Fatalf("Publish(beta.topic) error = %v", err)
	}
	if got := receiveOne(t, chAlpha, 3*time.Second); got != "to-alpha" {
		t.Errorf("chAlpha got %q, want to-alpha", got)
	}
	if got := receiveOne(t, chBeta, 3*time.Second); got != "to-beta" {
		t.Errorf("chBeta got %q, want to-beta", got)
	}

	// 2. Concurrent initial stream creation race across two Clients.
	n1, err := NewClient(ns.ClientURL(), WithJetStream("CONC_CREATE"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(n1) error = %v", err)
	}
	defer func() { _ = n1.Close() }()
	n2, err := NewClient(ns.ClientURL(), WithJetStream("CONC_CREATE"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(n2) error = %v", err)
	}
	defer func() { _ = n2.Close() }()

	var (
		nErr1, nErr2 error
		createWG     sync.WaitGroup
	)
	createStart := make(chan struct{})
	createWG.Go(func() {
		<-createStart
		_, nErr1 = n1.Subscribe(ctx, "new.one")
	})
	createWG.Go(func() {
		<-createStart
		_, nErr2 = n2.Subscribe(ctx, "new.two")
	})
	close(createStart)
	createWG.Wait()

	if nErr1 != nil {
		t.Fatalf("n1.Subscribe(new.one) error = %v", nErr1)
	}
	if nErr2 != nil {
		t.Fatalf("n2.Subscribe(new.two) error = %v", nErr2)
	}
	createdStream, err := n1.js.Stream(ctx, "CONC_CREATE")
	if err != nil {
		t.Fatalf("js.Stream(CONC_CREATE) error = %v", err)
	}
	createdSubjects := createdStream.CachedInfo().Config.Subjects
	for _, want := range []string{"new.one", "new.two"} {
		if !slices.Contains(createdSubjects, want) {
			t.Errorf("CONC_CREATE subjects = %v, missing %q", createdSubjects, want)
		}
	}
}

func TestClient_JetStreamDurableImmediateClose(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, true)

	pub, err := NewClient(ns.ClientURL(), WithJetStream("DURABLE_IMMEDIATE"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(pub) error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	// Repeatedly receive one message and immediately Close() with zero sleep.
	for i := range 5 {
		subClient, err := NewClient(
			ns.ClientURL(),
			WithJetStream("DURABLE_IMMEDIATE"),
			WithJetStreamStorage(jetstream.MemoryStorage),
			WithJetStreamDurable("imm"),
		)
		if err != nil {
			t.Fatalf("iter %d NewClient() error = %v", i, err)
		}
		ch, err := subClient.Subscribe(ctx, "items.ack")
		if err != nil {
			_ = subClient.Close()
			t.Fatalf("iter %d Subscribe() error = %v", i, err)
		}
		want := fmt.Sprintf("item-%d", i)
		if err := pub.Publish(ctx, "items.ack", []byte(want)); err != nil {
			_ = subClient.Close()
			t.Fatalf("iter %d Publish() error = %v", i, err)
		}
		if got := receiveOne(t, ch, 3*time.Second); got != want {
			_ = subClient.Close()
			t.Fatalf("iter %d got %q, want %q (acked message must not replay)", i, got, want)
		}
		if err := subClient.Close(); err != nil {
			t.Fatalf("iter %d Close() error = %v", i, err)
		}
	}
}

func TestClient_JetStreamAutoCreateFalseBehavior(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, true)

	// Pre-provision stream with Subjects: ["pre.*"].
	admin, err := NewClient(ns.ClientURL(), WithJetStream("PRE_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(admin) error = %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "PRE_STREAM",
		Subjects: []string{"pre.*"},
		Storage:  jetstream.MemoryStorage,
	}); err != nil {
		t.Fatalf("admin CreateStream() error = %v", err)
	}

	c, err := NewClient(
		ns.ClientURL(),
		WithJetStream("PRE_STREAM"),
		WithJetStreamAutoCreate(false),
	)
	if err != nil {
		t.Fatalf("NewClient(autoCreate=false) error = %v", err)
	}
	defer func() { _ = c.Close() }()

	ch, err := c.Subscribe(ctx, "pre.one")
	if err != nil {
		t.Fatalf("Subscribe(pre.one) error = %v", err)
	}
	if err := c.Publish(ctx, "pre.one", []byte("covered")); err != nil {
		t.Fatalf("Publish(pre.one) error = %v", err)
	}
	if got := receiveOne(t, ch, 3*time.Second); got != "covered" {
		t.Errorf("got %q, want covered", got)
	}

	// Publish to uncovered subject fails because stream is not auto-expanded.
	if err := c.Publish(ctx, "other.uncovered", []byte("fail")); err == nil {
		t.Error("expected Publish(other.uncovered) with autoCreate=false to fail, got nil")
	}

	// Subscribe to uncovered subject requires stream existence and does not modify stream Subjects.
	if _, err := c.Subscribe(ctx, "other.uncovered"); err != nil {
		t.Fatalf("Subscribe(other.uncovered) on existing stream error = %v", err)
	}
	stream, err := admin.js.Stream(ctx, "PRE_STREAM")
	if err != nil {
		t.Fatalf("admin.js.Stream() error = %v", err)
	}
	if got := stream.CachedInfo().Config.Subjects; !slices.Equal(got, []string{"pre.*"}) {
		t.Errorf("PRE_STREAM subjects mutated to %v, want [pre.*]", got)
	}
}

func TestStreamRetryHelpersAndExternalCreateRace(t *testing.T) {
	if !isRetryableStreamErr(jetstream.ErrStreamNameAlreadyInUse) {
		t.Error("expected ErrStreamNameAlreadyInUse to be retryable")
	}
	if !isRetryableStreamErr(&jetstream.APIError{ErrorCode: 10052, Description: `subject "events.a" overlaps with "events.*"`}) {
		t.Error("expected 10052 overlaps-with error to be retryable")
	}
	if isRetryableStreamErr(&jetstream.APIError{ErrorCode: 10052, Description: "subjects that overlap with jetstream api require no-ack to be true"}) {
		t.Error("expected 10052 jetstream api overlap error NOT to be retryable")
	}
	if isRetryableStreamErr(errors.New("other")) {
		t.Error("expected generic error NOT to be retryable")
	}

	if err := waitEnsureStreamJitter(t.Context()); err != nil {
		t.Errorf("waitEnsureStreamJitter(active) = %v, want nil", err)
	}
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitEnsureStreamJitter(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Errorf("waitEnsureStreamJitter(canceled) = %v, want context.Canceled", err)
	}

	// Race Client.Subscribe against raw jetstream.CreateStream (bypassing process lock).
	ctx := t.Context()
	ns := startTestNATSServer(t, true)
	rawNC, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	defer rawNC.Close()
	rawJS, err := jetstream.New(rawNC)
	if err != nil {
		t.Fatalf("jetstream.New() error = %v", err)
	}

	for i := range 3 {
		streamName := fmt.Sprintf("EXT_RACE_%d", i)
		clientTopic := fmt.Sprintf("client.topic.%d", i)
		rawTopic := fmt.Sprintf("raw.topic.%d", i)
		c, err := NewClient(ns.ClientURL(), WithJetStream(streamName), WithJetStreamStorage(jetstream.MemoryStorage))
		if err != nil {
			t.Fatalf("NewClient(%s) error = %v", streamName, err)
		}

		var (
			subErr error
			wg     sync.WaitGroup
		)
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			_, subErr = c.Subscribe(ctx, clientTopic)
		})
		wg.Go(func() {
			<-start
			_, _ = rawJS.CreateStream(ctx, jetstream.StreamConfig{
				Name:     streamName,
				Subjects: []string{rawTopic},
				Storage:  jetstream.MemoryStorage,
			})
		})
		close(start)
		wg.Wait()

		if subErr != nil {
			_ = c.Close()
			t.Fatalf("iter %d Subscribe() error = %v", i, subErr)
		}
		infoStream, err := rawJS.Stream(ctx, streamName)
		if err != nil {
			_ = c.Close()
			t.Fatalf("iter %d rawJS.Stream() error = %v", i, err)
		}
		if !slices.Contains(infoStream.CachedInfo().Config.Subjects, clientTopic) {
			_ = c.Close()
			t.Fatalf("iter %d stream subjects = %v, missing %s", i, infoStream.CachedInfo().Config.Subjects, clientTopic)
		}
		_ = c.Close()
	}
}

type retryOnceMsgsCtx struct {
	jetstream.MessagesContext
	failOnce atomic.Bool
}

func (r *retryOnceMsgsCtx) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	if r.failOnce.CompareAndSwap(true, false) {
		return nil, errors.New("transient iterator error")
	}
	return r.MessagesContext.Next(opts...)
}

func TestClient_JetStreamSlowConsumerNakAndRedeliver(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, true)

	pub, err := NewClient(ns.ClientURL(), WithJetStream("NAK_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
	if err != nil {
		t.Fatalf("NewClient(pub) error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	// Seed stream first so durable subscriber starts cleanly.
	d1, err := NewClient(
		ns.ClientURL(),
		WithJetStream("NAK_STREAM"),
		WithJetStreamStorage(jetstream.MemoryStorage),
		WithJetStreamDurable("nak"),
		WithChannelBuffer(1),
	)
	if err != nil {
		t.Fatalf("NewClient(d1) error = %v", err)
	}
	ch1, err := d1.Subscribe(ctx, "nak.topic")
	if err != nil {
		_ = d1.Close()
		t.Fatalf("d1.Subscribe() error = %v", err)
	}

	// Publish one at a time so msgsCtx does not prefetch extra un-Nak'ed messages:
	// 1. m-0 is read and acked.
	// 2. m-1 fills the 1-slot buffer (ch1) and is acked.
	// 3. m-2 is pulled by runJetStreamLoop and blocks on ch1 <- msg.Data().
	if err := pub.Publish(ctx, "nak.topic", []byte("m-0")); err != nil {
		_ = d1.Close()
		t.Fatalf("Publish(m-0) error = %v", err)
	}
	if got := receiveOne(t, ch1, 3*time.Second); got != "m-0" {
		_ = d1.Close()
		t.Fatalf("got %q, want m-0", got)
	}

	if err := pub.Publish(ctx, "nak.topic", []byte("m-1")); err != nil {
		_ = d1.Close()
		t.Fatalf("Publish(m-1) error = %v", err)
	}
	bufDeadline := time.Now().Add(2 * time.Second)
	for len(ch1) == 0 && time.Now().Before(bufDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(ch1) == 0 {
		_ = d1.Close()
		t.Fatal("timed out waiting for m-1 to saturate ch1 buffer")
	}
	// Allow DoubleAck for m-1 to complete and loop to block on m-2.
	time.Sleep(30 * time.Millisecond)
	if err := pub.Publish(ctx, "nak.topic", []byte("m-2")); err != nil {
		_ = d1.Close()
		t.Fatalf("Publish(m-2) error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := d1.Unsubscribe(ctx, "nak.topic"); err != nil {
		_ = d1.Close()
		t.Fatalf("d1.Unsubscribe() error = %v", err)
	}
	delivered := []string{"m-0"}
	for msg := range ch1 {
		delivered = append(delivered, string(msg))
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("d1.Close() error = %v", err)
	}

	const total = 3
	if len(delivered) >= total {
		t.Fatalf("expected slow durable consumer d1 to deliver < %d messages before Unsubscribe, got %v", total, delivered)
	}

	// Re-subscribe with same durable prefix; Nak'ed/unacked messages must all arrive.
	d2, err := NewClient(
		ns.ClientURL(),
		WithJetStream("NAK_STREAM"),
		WithJetStreamStorage(jetstream.MemoryStorage),
		WithJetStreamDurable("nak"),
	)
	if err != nil {
		t.Fatalf("NewClient(d2) error = %v", err)
	}
	ch2, err := d2.Subscribe(ctx, "nak.topic")
	if err != nil {
		_ = d2.Close()
		t.Fatalf("d2.Subscribe() error = %v", err)
	}

	seen := make(map[string]bool, total)
	for _, m := range delivered {
		seen[m] = true
	}
	wantRem := total - len(delivered)
	for range wantRem {
		msg := receiveOne(t, ch2, 3*time.Second)
		seen[msg] = true
	}
	for i := range total {
		key := fmt.Sprintf("m-%d", i)
		if !seen[key] {
			t.Errorf("missing redelivered message %q (d1=%v)", key, delivered)
		}
	}
	if err := d2.Close(); err != nil {
		t.Fatalf("d2.Close() error = %v", err)
	}

	// Exercise runJetStreamLoop Next() transient error retry branch (nats.go:691-698).
	cRetry, err := NewClient(ns.ClientURL(), WithJetStream("NAK_STREAM"))
	if err != nil {
		t.Fatalf("NewClient(cRetry) error = %v", err)
	}
	stream, err := cRetry.js.Stream(ctx, "NAK_STREAM")
	if err != nil {
		_ = cRetry.Close()
		t.Fatalf("Stream() error = %v", err)
	}
	cons, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"nak.retry"},
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	})
	if err != nil {
		_ = cRetry.Close()
		t.Fatalf("OrderedConsumer() error = %v", err)
	}
	realMsgsCtx, err := cons.Messages()
	if err != nil {
		_ = cRetry.Close()
		t.Fatalf("cons.Messages() error = %v", err)
	}
	wrapMsgs := &retryOnceMsgsCtx{MessagesContext: realMsgsCtx}
	wrapMsgs.failOnce.Store(true)

	recvCtx, cancelRecv := context.WithCancel(ctx)
	outCh := make(chan []byte, 4)
	var loopWG sync.WaitGroup
	loopWG.Go(func() {
		runJetStreamLoop(recvCtx, wrapMsgs, false, outCh)
	})

	// Wait past one jsReceiveRetryDelay so Next() retries and recovers.
	time.Sleep(jsReceiveRetryDelay + 50*time.Millisecond)
	if err := pub.Publish(ctx, "nak.retry", []byte("recovered-after-next-err")); err != nil {
		cancelRecv()
		loopWG.Wait()
		_ = cRetry.Close()
		t.Fatalf("Publish(nak.retry) error = %v", err)
	}
	if got := receiveOne(t, outCh, 3*time.Second); got != "recovered-after-next-err" {
		t.Errorf("got %q, want recovered-after-next-err", got)
	}
	cancelRecv()
	loopWG.Wait()
	_ = cRetry.Close()

	ns.Shutdown()
	ns.WaitForShutdown()
	assertNoGoroutineLeaks(t)
}

func TestClient_CloseWhileReconnectingAndShortUnsubscribe(t *testing.T) {
	ctx := t.Context()

	// 1. Close() while *nats.Conn is in RECONNECTING state (nats.go:845-850).
	nsRecon := startTestNATSServer(t, false)
	cRecon, err := NewClient(
		nsRecon.ClientURL(),
		WithNATSOptions(
			nats.ReconnectWait(50*time.Millisecond),
			nats.MaxReconnects(-1),
		),
	)
	if err != nil {
		t.Fatalf("NewClient(recon) error = %v", err)
	}
	chRecon, err := cRecon.Subscribe(ctx, "recon.topic")
	if err != nil {
		_ = cRecon.Close()
		t.Fatalf("Subscribe(recon.topic) error = %v", err)
	}

	nsRecon.Shutdown()
	nsRecon.WaitForShutdown()

	reconDeadline := time.Now().Add(2 * time.Second)
	for cRecon.conn.Status() != nats.RECONNECTING && time.Now().Before(reconDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status := cRecon.conn.Status(); status != nats.RECONNECTING {
		_ = cRecon.Close()
		t.Fatalf("expected connection status RECONNECTING, got %v", status)
	}
	if err := cRecon.Close(); err != nil {
		t.Fatalf("Close() while RECONNECTING returned error = %v", err)
	}
	assertChannelClosed(t, chRecon, 2*time.Second)

	// 2. Unsubscribe with short / expired deadline clamps drain timeout and forces Unsubscribe.
	nsShort := startTestNATSServer(t, false)
	cShort, err := NewClient(nsShort.ClientURL(), WithDrainTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient(short) error = %v", err)
	}
	chShort, err := cShort.Subscribe(ctx, "short.topic")
	if err != nil {
		_ = cShort.Close()
		t.Fatalf("Subscribe(short.topic) error = %v", err)
	}

	// Pass an entry to stopSubscription with a tiny deadline (< drainTimeout) and
	// verify Unsubscribe(shortCtx, ...) also returns cleanly.
	shortCtx, cancelShort := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancelShort()
	if err := cShort.Unsubscribe(shortCtx, "short.topic"); err != nil {
		_ = cShort.Close()
		t.Fatalf("Unsubscribe(shortCtx) error = %v", err)
	}
	assertChannelClosed(t, chShort, 2*time.Second)

	// Also exercise timeout <= 0 branch in stopSubscription directly.
	chZero, err := cShort.Subscribe(ctx, "zero.topic")
	if err != nil {
		_ = cShort.Close()
		t.Fatalf("Subscribe(zero.topic) error = %v", err)
	}
	cShort.mu.Lock()
	zeroEntry := cShort.subscriptions["zero.topic"]
	delete(cShort.subscriptions, "zero.topic")
	cShort.mu.Unlock()
	expiredCtx, cancelExpired := context.WithDeadline(ctx, time.Now().Add(-time.Millisecond))
	defer cancelExpired()
	if err := cShort.stopSubscription(expiredCtx, zeroEntry); err != nil {
		_ = cShort.Close()
		t.Fatalf("stopSubscription(expiredCtx) error = %v", err)
	}
	assertChannelClosed(t, chZero, 2*time.Second)

	if err := cShort.Close(); err != nil {
		t.Fatalf("cShort.Close() error = %v", err)
	}

	nsShort.Shutdown()
	nsShort.WaitForShutdown()
	assertNoGoroutineLeaks(t)
}

type flakyStream struct {
	jetstream.Stream
	failInfoOnce      atomic.Bool
	overrideSubjects  []string
	forceUncoveredSub bool
}

func (f *flakyStream) Info(ctx context.Context, opts ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if f.failInfoOnce.CompareAndSwap(true, false) {
		return nil, jetstream.ErrStreamNameAlreadyInUse
	}
	return f.Stream.Info(ctx, opts...)
}

func (f *flakyStream) CachedInfo() *jetstream.StreamInfo {
	if f.forceUncoveredSub {
		return &jetstream.StreamInfo{
			Config: jetstream.StreamConfig{
				Name:     "FLAKY_STREAM",
				Subjects: f.overrideSubjects,
			},
		}
	}
	return f.Stream.CachedInfo()
}

type flakyJetStream struct {
	jetstream.JetStream
	streamEntered   chan struct{}
	streamRelease   chan struct{}
	streamOnce      sync.Once
	failCreateOnce  atomic.Bool
	failUpdateOnce  atomic.Bool
	failVerifyAfter atomic.Bool
	alwaysOverwrite bool
}

func (f *flakyJetStream) Stream(ctx context.Context, stream string) (jetstream.Stream, error) {
	if f.streamEntered != nil {
		f.streamOnce.Do(func() { close(f.streamEntered) })
		<-f.streamRelease
	}
	if f.failVerifyAfter.CompareAndSwap(true, false) {
		return nil, errors.New("transient verify stream lookup error")
	}
	s, err := f.JetStream.Stream(ctx, stream)
	if err != nil {
		return nil, err
	}
	if f.alwaysOverwrite {
		return &flakyStream{
			Stream:            s,
			forceUncoveredSub: true,
			overrideSubjects:  []string{"unrelated.subject"},
		}, nil
	}
	return s, nil
}

func (f *flakyJetStream) CreateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	if f.failCreateOnce.CompareAndSwap(true, false) {
		return nil, jetstream.ErrStreamNameAlreadyInUse
	}
	s, err := f.JetStream.CreateStream(ctx, cfg)
	if err == nil {
		// Arm post-create verification error for the immediate verify lookup.
		f.failVerifyAfter.Store(true)
	}
	return s, err
}

func (f *flakyJetStream) CreateOrUpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	if f.failUpdateOnce.CompareAndSwap(true, false) {
		return nil, jetstream.ErrStreamNameAlreadyInUse
	}
	return f.JetStream.CreateOrUpdateStream(ctx, cfg)
}

func TestClient_JetStreamConcurrentLifecycleAndLeaks(t *testing.T) {
	ctx := t.Context()
	ns := startTestNATSServer(t, true)

	// 1. Concurrent JetStream Subscribe/Publish/Unsubscribe/Close under -race + goroutineleak.
	t.Run("ConcurrentJetStreamStress", func(t *testing.T) {
		c, err := NewClient(
			ns.ClientURL(),
			WithJetStream("JS_STRESS"),
			WithJetStreamStorage(jetstream.MemoryStorage),
			WithChannelBuffer(4),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		topics := []string{"js.stress.0", "js.stress.1", "js.stress.2"}
		var wg sync.WaitGroup
		var drainers sync.WaitGroup
		for g := range 4 {
			wg.Go(func() {
				for i := range 6 {
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
						_ = c.Publish(ctx, topic, []byte("js-payload"))
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
	})

	// 2. Subscribe racing with Close exercises cleanupPendingSub for both Core and JetStream.
	t.Run("SubscribeCloseCleanupPendingSub", func(t *testing.T) {
		// JetStream: use caller-owned conn (WithCloseConn(false)) so Close()
		// sets c.closed=true without closing the underlying NATS connection
		// mid-setup, hitting nats.go:610-613 and cleanupPendingSub cleanly.
		cJS, err := NewClient(ns.ClientURL(), WithJetStream("PENDING_JS"), WithJetStreamStorage(jetstream.MemoryStorage), WithCloseConn(false))
		if err != nil {
			t.Fatalf("NewClient(js) error = %v", err)
		}
		defer cJS.conn.Close()
		barrierJS := &flakyJetStream{
			JetStream:     cJS.js,
			streamEntered: make(chan struct{}),
			streamRelease: make(chan struct{}),
		}
		cJS.js = barrierJS

		subErrCh := make(chan error, 1)
		go func() {
			_, subErr := cJS.Subscribe(ctx, "pending.js")
			subErrCh <- subErr
		}()

		<-barrierJS.streamEntered
		if err := cJS.Close(); err != nil {
			t.Fatalf("cJS.Close() error = %v", err)
		}
		close(barrierJS.streamRelease)

		select {
		case subErr := <-subErrCh:
			if !errors.Is(subErr, ErrClosed) {
				t.Fatalf("cJS.Subscribe() error = %v, want ErrClosed", subErr)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for cJS.Subscribe after Close")
		}

		// Also verify cleanupPendingSub directly with a real Core *nats.Subscription.
		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatalf("nats.Connect() error = %v", err)
		}
		sub, err := nc.SubscribeSync("pending.core")
		if err != nil {
			nc.Close()
			t.Fatalf("SubscribeSync() error = %v", err)
		}
		cleanupPendingSub(sub, nil)
		if sub.IsValid() {
			t.Error("expected cleanupPendingSub to unsubscribe core *nats.Subscription")
		}
		nc.Close()
	})

	// 3. ensureStream retryable CreateStream, CreateOrUpdateStream, verify error,
	// Stream.Info retry, and concurrent-overwrite exhaustion branches.
	t.Run("EnsureStreamRetryAndOverwriteBranches", func(t *testing.T) {
		c, err := NewClient(ns.ClientURL(), WithJetStream("RETRY_STREAM"), WithJetStreamStorage(jetstream.MemoryStorage))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		defer func() { _ = c.Close() }()

		realJS := c.js
		fjs := &flakyJetStream{JetStream: realJS}
		fjs.failCreateOnce.Store(true)
		c.js = fjs

		// CreateStream fails once with ErrStreamNameAlreadyInUse -> retries and succeeds;
		// post-create verify lookup fails once and retries cleanly.
		if _, err := c.ensureStream(ctx, "retry.one"); err != nil {
			t.Fatalf("ensureStream(create retry) error = %v", err)
		}

		// CreateOrUpdateStream fails once with ErrStreamNameAlreadyInUse -> retries and succeeds.
		fjs.failUpdateOnce.Store(true)
		if _, err := c.ensureStream(ctx, "retry.two"); err != nil {
			t.Fatalf("ensureStream(update retry) error = %v", err)
		}

		// Stream.Info returns nil CachedInfo and fails once with retryable error -> succeeds.
		cInfo, err := NewClient(ns.ClientURL(), WithJetStream("RETRY_STREAM"))
		if err != nil {
			t.Fatalf("NewClient(cInfo) error = %v", err)
		}
		defer func() { _ = cInfo.Close() }()
		origStream, err := realJS.Stream(ctx, "RETRY_STREAM")
		if err != nil {
			t.Fatalf("realJS.Stream() error = %v", err)
		}
		var onceInfoErr atomic.Bool
		onceInfoErr.Store(true)
		_ = origStream
		cInfo.js = &retryStreamInfoJS{JetStream: realJS, failInfoOnce: &onceInfoErr}
		if _, err := cInfo.ensureStream(ctx, "retry.three"); err != nil {
			t.Fatalf("ensureStream(Info retry) error = %v", err)
		}

		// Concurrent overwrite on every attempt returns overwritten error after max attempts.
		fjsOverwrite := &flakyJetStream{
			JetStream:       realJS,
			alwaysOverwrite: true,
		}
		c.js = fjsOverwrite
		if _, err := c.ensureStream(ctx, "retry.overwrite"); err == nil || !strings.Contains(err.Error(), "overwritten concurrently") {
			t.Errorf("ensureStream(overwrite) = %v, want overwritten concurrently error", err)
		}
	})

	ns.Shutdown()
	ns.WaitForShutdown()
	assertNoGoroutineLeaks(t)
}

type retryStreamInfoJS struct {
	jetstream.JetStream
	failInfoOnce *atomic.Bool
}

func (r *retryStreamInfoJS) Stream(ctx context.Context, stream string) (jetstream.Stream, error) {
	s, err := r.JetStream.Stream(ctx, stream)
	if err != nil {
		return nil, err
	}
	return &retryStreamInfoWrapper{Stream: s, failInfoOnce: r.failInfoOnce}, nil
}

type retryStreamInfoWrapper struct {
	jetstream.Stream
	failInfoOnce *atomic.Bool
}

func (w *retryStreamInfoWrapper) CachedInfo() *jetstream.StreamInfo {
	return nil
}

func (w *retryStreamInfoWrapper) Info(ctx context.Context, opts ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if w.failInfoOnce.CompareAndSwap(true, false) {
		return nil, jetstream.ErrStreamNameAlreadyInUse
	}
	return w.Stream.Info(ctx, opts...)
}

// Compile-time structural check that Client satisfies memorystore.PubSubClient.
type pubSubContract interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}

var _ pubSubContract = (*Client)(nil)
