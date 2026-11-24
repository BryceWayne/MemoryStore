package gcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const testProjectID = "test-project"

func newTestClient(t *testing.T, reactorOpts ...pstest.ServerReactorOption) (*Client, *pstest.Server) {
	t.Helper()

	srv := pstest.NewServer(reactorOpts...)
	t.Cleanup(func() {
		_ = srv.Close()
	})

	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}

	c, err := NewClient(
		t.Context(),
		testProjectID,
		option.WithGRPCConn(conn),
		option.WithoutAuthentication(),
	)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
	})

	return c, srv
}

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

func assertChannelClosed(t *testing.T, ch <-chan []byte, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("timed out after %v waiting for channel close", timeout)
		}
	}
}

type onceErrorReactor struct {
	fired atomic.Bool
	err   error
}

func (r *onceErrorReactor) React(_ any) (bool, any, error) {
	if r.fired.CompareAndSwap(false, true) {
		return true, nil, r.err
	}
	return false, nil, nil
}

func TestNewClient_EmptyProject(t *testing.T) {
	ctx := t.Context()
	_, err := NewClient(ctx, "")
	if err == nil {
		t.Error("expected error for empty project ID")
	}
}

func TestClient_ValidationErrors(t *testing.T) {
	c := &Client{
		projectID:     testProjectID,
		subscriptions: make(map[string]*gcpSubscription),
	}

	if _, err := c.Subscribe(t.Context(), ""); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Subscribe(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Publish(t.Context(), "", []byte("msg")); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Publish(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Unsubscribe(t.Context(), "nonexistent"); err != nil {
		t.Errorf("Unsubscribe(nonexistent) = %v, want nil", err)
	}

	// Mark closed and verify operations return ErrClosed.
	c.closed = true
	if _, err := c.Subscribe(t.Context(), "topic"); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Publish(t.Context(), "topic", []byte("msg")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish(closed) = %v, want ErrClosed", err)
	}
	if err := c.Unsubscribe(t.Context(), "topic"); !errors.Is(err, ErrClosed) {
		t.Errorf("Unsubscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close(already closed) = %v, want nil", err)
	}

	// Verify helper path formatting.
	if got, want := c.topicPath("updates"), "projects/test-project/topics/updates"; got != want {
		t.Errorf("topicPath() = %q, want %q", got, want)
	}
	if got, want := c.subscriptionPath("sub-1"), "projects/test-project/subscriptions/sub-1"; got != want {
		t.Errorf("subscriptionPath() = %q, want %q", got, want)
	}
}

func TestClient_SubscribePublishUnsubscribe(t *testing.T) {
	ctx := t.Context()
	c, _ := newTestClient(t)

	ch, err := c.Subscribe(ctx, "updates")
	if err != nil {
		t.Fatalf("Subscribe(updates) error = %v", err)
	}

	// Record the server-side subscription name so we can verify deletion on Unsubscribe.
	c.mu.RLock()
	subName := c.subscriptions["updates"].subName
	c.mu.RUnlock()

	// First publish creates and caches the publisher; second publish reuses it.
	payloads := [][]byte{[]byte("first-message"), []byte("second-message")}
	for i, p := range payloads {
		if err := c.Publish(ctx, "updates", p); err != nil {
			t.Fatalf("Publish(%d) error = %v", i, err)
		}
	}

	received := make(map[string]bool, len(payloads))
	for range payloads {
		msg := receiveWithTimeout(t, ch, 5*time.Second)
		received[string(msg)] = true
	}
	for _, p := range payloads {
		if !received[string(p)] {
			t.Errorf("missing expected payload %q, got %v", string(p), received)
		}
	}

	if err := c.Unsubscribe(ctx, "updates"); err != nil {
		t.Fatalf("Unsubscribe(updates) error = %v", err)
	}
	assertChannelClosed(t, ch, 5*time.Second)

	// Subscription should have been deleted on the server.
	_, err = c.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: subName,
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after Unsubscribe = %v, want NotFound", err)
	}

	// Unsubscribing again is a no-op.
	if err := c.Unsubscribe(ctx, "updates"); err != nil {
		t.Errorf("second Unsubscribe(updates) = %v, want nil", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
}

func TestClient_SubscribeExistingTopicAndCloseCleanup(t *testing.T) {
	ctx := t.Context()
	c, _ := newTestClient(t)

	// Pre-create the topic so Subscribe takes the existing-topic branch.
	_, err := c.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{
		Name: c.topicPath("preexisting"),
	})
	if err != nil {
		t.Fatalf("CreateTopic() error = %v", err)
	}

	ch, err := c.Subscribe(ctx, "preexisting")
	if err != nil {
		t.Fatalf("Subscribe(preexisting) error = %v", err)
	}

	if err := c.Publish(ctx, "preexisting", []byte("ping")); err != nil {
		t.Fatalf("Publish(preexisting) error = %v", err)
	}
	if got := string(receiveWithTimeout(t, ch, 5*time.Second)); got != "ping" {
		t.Errorf("received = %q, want %q", got, "ping")
	}

	// Close should stop cached publishers, cancel subscriptions, delete them, and close channels.
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	assertChannelClosed(t, ch, 5*time.Second)
}

func TestClient_SubscribeErrors(t *testing.T) {
	t.Run("GetTopicError", func(t *testing.T) {
		c, _ := newTestClient(t, pstest.WithErrorInjection("GetTopic", codes.PermissionDenied, "denied"))
		_, err := c.Subscribe(t.Context(), "topic")
		if err == nil || !strings.Contains(err.Error(), "failed to check topic existence") {
			t.Errorf("Subscribe() error = %v, want wrapped GetTopic error", err)
		}
	})

	t.Run("CreateTopicError", func(t *testing.T) {
		c, _ := newTestClient(t, pstest.WithErrorInjection("CreateTopic", codes.Internal, "create boom"))
		_, err := c.Subscribe(t.Context(), "topic")
		if err == nil || !strings.Contains(err.Error(), "failed to create topic") {
			t.Errorf("Subscribe() error = %v, want wrapped CreateTopic error", err)
		}
	})

	t.Run("CreateTopicAlreadyExistsRace", func(t *testing.T) {
		reactor := &onceErrorReactor{err: status.Error(codes.NotFound, "simulated race")}
		c, _ := newTestClient(t, pstest.ServerReactorOption{
			FuncName: "GetTopic",
			Reactor:  reactor,
		})

		// Pre-create topic so CreateTopic naturally returns codes.AlreadyExists after GetTopic's NotFound.
		ctx := t.Context()
		_, err := c.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{
			Name: c.topicPath("raced-topic"),
		})
		if err != nil {
			t.Fatalf("CreateTopic() error = %v", err)
		}

		ch, err := c.Subscribe(ctx, "raced-topic")
		if err != nil {
			t.Fatalf("Subscribe(raced-topic) error = %v, want nil on AlreadyExists", err)
		}

		if err := c.Publish(ctx, "raced-topic", []byte("ok")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		if got := string(receiveWithTimeout(t, ch, 5*time.Second)); got != "ok" {
			t.Errorf("received = %q, want %q", got, "ok")
		}
	})

	t.Run("CreateSubscriptionError", func(t *testing.T) {
		c, _ := newTestClient(t, pstest.WithErrorInjection("CreateSubscription", codes.PermissionDenied, "sub denied"))
		_, err := c.Subscribe(t.Context(), "topic")
		if err == nil || !strings.Contains(err.Error(), "failed to create subscription") {
			t.Errorf("Subscribe() error = %v, want wrapped CreateSubscription error", err)
		}
	})
}

func TestClient_PublishErrors(t *testing.T) {
	t.Run("NonexistentTopic", func(t *testing.T) {
		c, _ := newTestClient(t)
		if err := c.Publish(t.Context(), "does-not-exist", []byte("msg")); err == nil {
			t.Error("Publish(nonexistent topic) = nil, want error")
		}
	})

	t.Run("InjectedPublishError", func(t *testing.T) {
		c, _ := newTestClient(t, pstest.WithErrorInjection("Publish", codes.PermissionDenied, "publish denied"))
		ctx := t.Context()
		_, err := c.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{
			Name: c.topicPath("denied-topic"),
		})
		if err != nil {
			t.Fatalf("CreateTopic() error = %v", err)
		}
		if err := c.Publish(ctx, "denied-topic", []byte("msg")); err == nil {
			t.Error("Publish() = nil, want error")
		}
	})
}

func TestClient_UnsubscribeDeleteErrors(t *testing.T) {
	t.Run("DeleteSubscriptionNotFound", func(t *testing.T) {
		c, _ := newTestClient(t, pstest.WithErrorInjection("DeleteSubscription", codes.NotFound, "already deleted"))
		ctx := t.Context()
		ch, err := c.Subscribe(ctx, "gone-sub")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		if err := c.Unsubscribe(ctx, "gone-sub"); err != nil {
			t.Errorf("Unsubscribe() = %v, want nil when DeleteSubscription returns NotFound", err)
		}
		assertChannelClosed(t, ch, 5*time.Second)
	})

	t.Run("DeleteSubscriptionInternalError", func(t *testing.T) {
		c, _ := newTestClient(t, pstest.WithErrorInjection("DeleteSubscription", codes.Internal, "delete failed"))
		ctx := t.Context()
		ch, err := c.Subscribe(ctx, "err-sub")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		err = c.Unsubscribe(ctx, "err-sub")
		if err == nil || !strings.Contains(err.Error(), "failed to delete subscription") {
			t.Errorf("Unsubscribe() = %v, want wrapped DeleteSubscription error", err)
		}
		assertChannelClosed(t, ch, 5*time.Second)
	})
}

func TestClient_SlowConsumerNackOnCancel(t *testing.T) {
	ctx := t.Context()
	c, srv := newTestClient(t)

	ch, err := c.Subscribe(ctx, "slow-topic")
	if err != nil {
		t.Fatalf("Subscribe(slow-topic) error = %v", err)
	}

	topicPath := c.topicPath("slow-topic")
	for i := range defaultChannelBuffer + 25 {
		srv.Publish(topicPath, []byte(fmt.Sprintf("msg-%d", i)), nil)
	}

	// Wait for the output channel buffer to fill completely so subsequent
	// messages block inside the Receive callback select until cancel.
	deadline := time.Now().Add(5 * time.Second)
	for len(ch) < defaultChannelBuffer && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(ch) < defaultChannelBuffer {
		t.Fatalf("expected channel buffer to fill to %d, got %d", defaultChannelBuffer, len(ch))
	}
	// Give extra messages a moment to enter the select before canceling.
	time.Sleep(50 * time.Millisecond)

	if err := c.Unsubscribe(ctx, "slow-topic"); err != nil {
		t.Fatalf("Unsubscribe(slow-topic) error = %v", err)
	}
	assertChannelClosed(t, ch, 5*time.Second)
}

func TestClient_ReceiveNonCanceledError(t *testing.T) {
	srv := pstest.NewServer()
	t.Cleanup(func() {
		_ = srv.Close()
	})

	streamErrInterceptor := func(
		_ context.Context,
		_ *grpc.StreamDesc,
		_ *grpc.ClientConn,
		_ string,
		_ grpc.Streamer,
		_ ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		return nil, status.Error(codes.PermissionDenied, "stream denied")
	}

	conn, err := grpc.NewClient(
		srv.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStreamInterceptor(streamErrInterceptor),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}

	ctx := t.Context()
	c, err := NewClient(
		ctx,
		testProjectID,
		option.WithGRPCConn(conn),
		option.WithoutAuthentication(),
	)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
	})

	ch, err := c.Subscribe(ctx, "stream-err")
	if err != nil {
		t.Fatalf("Subscribe(stream-err) error = %v", err)
	}

	// StreamingPull immediately returns PermissionDenied; Receive exits and closes ch.
	assertChannelClosed(t, ch, 5*time.Second)

	if err := c.Unsubscribe(ctx, "stream-err"); err != nil {
		t.Fatalf("Unsubscribe(stream-err) error = %v", err)
	}
}

func TestClient_ConcurrentPublishAndClose(t *testing.T) {
	ctx := t.Context()
	c, _ := newTestClient(t)

	_, err := c.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{
		Name: c.topicPath("race-close"),
	})
	if err != nil {
		t.Fatalf("CreateTopic() error = %v", err)
	}

	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			<-start
			topic := fmt.Sprintf("race-close-%d", i%4)
			_ = c.Publish(ctx, topic, []byte("msg"))
		})
	}
	wg.Go(func() {
		<-start
		_ = c.Close()
	})
	close(start)
	wg.Wait()

	if err := c.Publish(ctx, "race-close", []byte("after-close")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish after Close = %v, want ErrClosed", err)
	}
}

func TestClient_ConcurrentPublish(t *testing.T) {
	ctx := t.Context()
	c, _ := newTestClient(t)

	ch, err := c.Subscribe(ctx, "concurrent-topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	const workers = 16
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			<-start
			if pubErr := c.Publish(ctx, "concurrent-topic", []byte(fmt.Sprintf("m-%d", i))); pubErr != nil {
				errCh <- pubErr
			}
		})
	}
	close(start)
	wg.Wait()
	close(errCh)

	for pubErr := range errCh {
		t.Errorf("concurrent Publish() error = %v", pubErr)
	}

	for range workers {
		_ = receiveWithTimeout(t, ch, 5*time.Second)
	}
}

func TestClient_CloseNoGoroutineLeak(t *testing.T) {
	ctx := t.Context()
	c, _ := newTestClient(t)

	topics := []string{"leak-a", "leak-b"}
	channels := make([]<-chan []byte, 0, len(topics))
	for _, topic := range topics {
		ch, err := c.Subscribe(ctx, topic)
		if err != nil {
			t.Fatalf("Subscribe(%s) error = %v", topic, err)
		}
		channels = append(channels, ch)
		if err := c.Publish(ctx, topic, []byte("hello-"+topic)); err != nil {
			t.Fatalf("Publish(%s) error = %v", topic, err)
		}
		got := string(receiveWithTimeout(t, ch, 5*time.Second))
		if got != "hello-"+topic {
			t.Errorf("received on %s = %q, want %q", topic, got, "hello-"+topic)
		}
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	for _, ch := range channels {
		assertChannelClosed(t, ch, 5*time.Second)
	}

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/gcp") {
			t.Errorf("detected leaked goroutine in pubsub/gcp:\n%s", buf.String())
		}
	}
}

func newTestClientWithGRPCOpts(
	t *testing.T,
	dialOpts []grpc.DialOption,
	reactorOpts ...pstest.ServerReactorOption,
) (*Client, *pstest.Server) {
	t.Helper()

	srv := pstest.NewServer(reactorOpts...)
	t.Cleanup(func() {
		_ = srv.Close()
	})

	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dialOpts...)
	conn, err := grpc.NewClient(srv.Addr, opts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}

	c, err := NewClient(
		t.Context(),
		testProjectID,
		option.WithGRPCConn(conn),
		option.WithoutAuthentication(),
	)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
	})

	return c, srv
}

func assertNoGoroutineLeak(t *testing.T) {
	t.Helper()
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/gcp") {
			t.Errorf("detected leaked goroutine in pubsub/gcp:\n%s", buf.String())
		}
	}
}

func assertValidGCPSubID(t *testing.T, id string) {
	t.Helper()
	if len(id) < 52 || len(id) > maxSubscriptionIDLen {
		t.Errorf("subscription ID %q len = %d, want in [52, %d]", id, len(id), maxSubscriptionIDLen)
	}
	if !strings.HasPrefix(id, subIDPrefix) {
		t.Errorf("subscription ID %q does not start with %q", id, subIDPrefix)
	}
	if id[0] < 'A' || (id[0] > 'Z' && id[0] < 'a') || id[0] > 'z' {
		t.Errorf("subscription ID %q must start with ASCII letter", id)
	}
	if strings.HasPrefix(id, "goog") {
		t.Errorf("subscription ID %q must not start with goog", id)
	}
	for _, r := range id {
		if !isValidGCPSubIDChar(r) {
			t.Errorf("subscription ID %q contains invalid char %q", id, r)
		}
	}
}

func TestSanitizeSubscriptionID(t *testing.T) {
	cases := []struct {
		name  string
		topic string
	}{
		{name: "simple", topic: "updates"},
		{name: "colons_slashes_wildcards", topic: "cache:users/*/profile?a=[1]"},
		{name: "leading_digits", topic: "123-topic"},
		{name: "leading_dash", topic: "-dash"},
		{name: "non_ascii_only", topic: "日本語"},
		{name: "mixed_non_ascii", topic: "café:über"},
		{name: "all_disallowed", topic: "///***"},
		{name: "empty", topic: ""},
		{name: "over_255_chars", topic: strings.Repeat("a", 300)},
		{name: "boundary_trailing_disallowed", topic: strings.Repeat("a", 201) + ":bbb"},
		{name: "all_dashes_over_cap", topic: strings.Repeat("-", 250) + "abc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id1 := sanitizeSubscriptionID(tc.topic)
			id2 := sanitizeSubscriptionID(tc.topic)
			assertValidGCPSubID(t, id1)
			assertValidGCPSubID(t, id2)
			if id1 == id2 {
				t.Errorf("expected distinct UUID suffixes for %q, got identical %q", tc.topic, id1)
			}
		})
	}

	t.Run("EndToEndSubscribeLongAndSpecialTopics", func(t *testing.T) {
		ctx := t.Context()
		c, _ := newTestClient(t)

		cases := []struct {
			topic      string
			canPublish bool
		}{
			{topic: strings.Repeat("a", 240), canPublish: true},
			{topic: "cache:users:*", canPublish: true},
			{topic: "cache:users/*/profile", canPublish: false},
			{topic: "日本語:topic/1", canPublish: false},
		}
		for _, tc := range cases {
			ch, err := c.Subscribe(ctx, tc.topic)
			if err != nil {
				t.Fatalf("Subscribe(%q) error = %v", tc.topic, err)
			}

			c.mu.RLock()
			subName := c.subscriptions[tc.topic].subName
			c.mu.RUnlock()

			parts := strings.Split(subName, "/")
			subID := parts[len(parts)-1]
			assertValidGCPSubID(t, subID)

			if tc.canPublish {
				payload := []byte("msg-" + tc.topic)
				if err := c.Publish(ctx, tc.topic, payload); err != nil {
					t.Fatalf("Publish(%q) error = %v", tc.topic, err)
				}
				if got := receiveWithTimeout(t, ch, 5*time.Second); !bytes.Equal(got, payload) {
					t.Errorf("received on %q = %q, want %q", tc.topic, got, payload)
				}
			}
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		assertNoGoroutineLeak(t)
	})
}

func TestClient_DuplicateSubscribeCleanup(t *testing.T) {
	ctx := t.Context()
	c, _ := newTestClient(t)

	ch1, err := c.Subscribe(ctx, "dup-topic")
	if err != nil {
		t.Fatalf("first Subscribe() error = %v", err)
	}
	c.mu.RLock()
	sub1Name := c.subscriptions["dup-topic"].subName
	c.mu.RUnlock()

	ch2, err := c.Subscribe(ctx, "dup-topic")
	if err != nil {
		t.Fatalf("second Subscribe() error = %v", err)
	}
	assertChannelClosed(t, ch1, 5*time.Second)

	c.mu.RLock()
	sub2Name := c.subscriptions["dup-topic"].subName
	c.mu.RUnlock()

	if sub2Name == sub1Name {
		t.Errorf("expected new subscription name, got identical %q", sub2Name)
	}

	_, err = c.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: sub1Name,
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription(%q) = %v, want NotFound", sub1Name, err)
	}

	if err := c.Publish(ctx, "dup-topic", []byte("after-resub")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := string(receiveWithTimeout(t, ch2, 5*time.Second)); got != "after-resub" {
		t.Errorf("received = %q, want %q", got, "after-resub")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	assertChannelClosed(t, ch2, 5*time.Second)
	assertNoGoroutineLeak(t)
}

func TestClient_CanceledAndExpiredContext(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	expiredCtx, expireCancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expireCancel()

	t.Run("PreCanceledAndExpiredNoRPC", func(t *testing.T) {
		c, _ := newTestClient(
			t,
			pstest.WithErrorInjection("GetTopic", codes.Internal, "should not run"),
			pstest.WithErrorInjection("CreateTopic", codes.Internal, "should not run"),
			pstest.WithErrorInjection("CreateSubscription", codes.Internal, "should not run"),
			pstest.WithErrorInjection("Publish", codes.Internal, "should not run"),
			pstest.WithErrorInjection("DeleteSubscription", codes.Internal, "should not run"),
		)

		for _, tc := range []struct {
			name string
			ctx  context.Context
			want error
		}{
			{name: "canceled", ctx: canceledCtx, want: context.Canceled},
			{name: "expired", ctx: expiredCtx, want: context.DeadlineExceeded},
		} {
			if _, err := c.Subscribe(tc.ctx, "t"); !errors.Is(err, tc.want) {
				t.Errorf("%s Subscribe() = %v, want %v", tc.name, err, tc.want)
			}
			if err := c.Publish(tc.ctx, "t", []byte("msg")); !errors.Is(err, tc.want) {
				t.Errorf("%s Publish() = %v, want %v", tc.name, err, tc.want)
			}
			if err := c.Unsubscribe(tc.ctx, "t"); !errors.Is(err, tc.want) {
				t.Errorf("%s Unsubscribe() = %v, want %v", tc.name, err, tc.want)
			}
		}
	})

	t.Run("UninitializedClient", func(t *testing.T) {
		c := &Client{
			projectID:     testProjectID,
			subscriptions: make(map[string]*gcpSubscription),
		}
		if _, err := c.Subscribe(canceledCtx, ""); !errors.Is(err, context.Canceled) {
			t.Errorf("Subscribe(canceled, empty) = %v, want context.Canceled", err)
		}
		if err := c.Publish(expiredCtx, "", []byte("msg")); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Publish(expired, empty) = %v, want context.DeadlineExceeded", err)
		}
		if err := c.Unsubscribe(canceledCtx, "t"); !errors.Is(err, context.Canceled) {
			t.Errorf("Unsubscribe(canceled) = %v, want context.Canceled", err)
		}
	})

	t.Run("CanceledAfterCreateSubscriptionRollsBack", func(t *testing.T) {
		subCtx, subCancel := context.WithCancel(t.Context())
		interceptor := func(
			ctx context.Context,
			method string,
			req, reply any,
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			if err == nil && strings.HasSuffix(method, "/CreateSubscription") {
				subCancel()
			}
			return err
		}

		c, srv := newTestClientWithGRPCOpts(t, []grpc.DialOption{grpc.WithUnaryInterceptor(interceptor)})
		if _, err := c.Subscribe(subCtx, "cancel-rollback"); !errors.Is(err, context.Canceled) {
			t.Errorf("Subscribe() = %v, want context.Canceled", err)
		}

		listResp, err := srv.GServer.ListSubscriptions(t.Context(), &pubsubpb.ListSubscriptionsRequest{
			Project: "projects/" + testProjectID,
		})
		if err != nil {
			t.Fatalf("ListSubscriptions() error = %v", err)
		}
		if len(listResp.Subscriptions) != 0 {
			t.Errorf("expected 0 subscriptions after rollback, got %d", len(listResp.Subscriptions))
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		assertNoGoroutineLeak(t)
	})
}

func TestClient_ConcurrentSubscribeAndClose(t *testing.T) {
	t.Run("ConcurrentSubscribeRaceSameTopic", func(t *testing.T) {
		var arrived atomic.Int32
		bothArrived := make(chan struct{})
		interceptor := func(
			ctx context.Context,
			method string,
			req, reply any,
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			if strings.HasSuffix(method, "/CreateSubscription") {
				if arrived.Add(1) == 2 {
					close(bothArrived)
				} else {
					select {
					case <-bothArrived:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		ctx := t.Context()
		c, srv := newTestClientWithGRPCOpts(t, []grpc.DialOption{grpc.WithUnaryInterceptor(interceptor)})

		// Pre-create topic so both goroutines race directly on CreateSubscription.
		if _, err := c.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{
			Name: c.topicPath("race-sub"),
		}); err != nil {
			t.Fatalf("CreateTopic() error = %v", err)
		}

		type subRes struct {
			ch  <-chan []byte
			err error
		}
		resCh := make(chan subRes, 2)
		for range 2 {
			go func() {
				ch, err := c.Subscribe(ctx, "race-sub")
				resCh <- subRes{ch: ch, err: err}
			}()
		}

		r1 := <-resCh
		r2 := <-resCh
		if r1.err != nil {
			t.Fatalf("Subscribe 1 error = %v", r1.err)
		}
		if r2.err != nil {
			t.Fatalf("Subscribe 2 error = %v", r2.err)
		}

		var activeCh <-chan []byte
		select {
		case _, ok := <-r1.ch:
			if ok {
				t.Fatal("unexpected message on losing channel r1")
			}
			activeCh = r2.ch
		case _, ok := <-r2.ch:
			if ok {
				t.Fatal("unexpected message on losing channel r2")
			}
			activeCh = r1.ch
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for losing Subscribe channel to close")
		}

		listResp, err := srv.GServer.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{
			Project: "projects/" + testProjectID,
		})
		if err != nil {
			t.Fatalf("ListSubscriptions() error = %v", err)
		}
		if len(listResp.Subscriptions) != 1 {
			t.Errorf("active subscriptions = %d, want 1", len(listResp.Subscriptions))
		}

		if err := c.Publish(ctx, "race-sub", []byte("winner")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		if got := string(receiveWithTimeout(t, activeCh, 5*time.Second)); got != "winner" {
			t.Errorf("received = %q, want %q", got, "winner")
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		assertChannelClosed(t, activeCh, 5*time.Second)

		afterClose, err := srv.GServer.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{
			Project: "projects/" + testProjectID,
		})
		if err != nil {
			t.Fatalf("ListSubscriptions() after Close error = %v", err)
		}
		if len(afterClose.Subscriptions) != 0 {
			t.Errorf("subscriptions after Close = %d, want 0", len(afterClose.Subscriptions))
		}
		assertNoGoroutineLeak(t)
	})

	t.Run("CloseDuringInFlightCreateSubscription", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once

		interceptor := func(
			ctx context.Context,
			method string,
			req, reply any,
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			if err == nil && strings.HasSuffix(method, "/CreateSubscription") {
				once.Do(func() {
					close(entered)
					<-release
				})
			}
			return err
		}

		ctx := t.Context()
		c, srv := newTestClientWithGRPCOpts(t, []grpc.DialOption{grpc.WithUnaryInterceptor(interceptor)})

		subErrCh := make(chan error, 1)
		go func() {
			_, err := c.Subscribe(ctx, "close-race")
			subErrCh <- err
		}()

		<-entered

		closeErrCh := make(chan error, 1)
		go func() {
			closeErrCh <- c.Close()
		}()

		// Wait until Close() has set c.closed under c.mu before releasing Subscribe.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			c.mu.RLock()
			closed := c.closed
			c.mu.RUnlock()
			if closed {
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(release)

		if err := <-subErrCh; !errors.Is(err, ErrClosed) {
			t.Errorf("Subscribe() during Close = %v, want ErrClosed", err)
		}
		if err := <-closeErrCh; err != nil {
			t.Errorf("Close() error = %v", err)
		}

		listResp, err := srv.GServer.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{
			Project: "projects/" + testProjectID,
		})
		if err != nil {
			t.Fatalf("ListSubscriptions() error = %v", err)
		}
		if len(listResp.Subscriptions) != 0 {
			t.Errorf("expected 0 subscriptions after Close rollback, got %d", len(listResp.Subscriptions))
		}
		assertNoGoroutineLeak(t)
	})
}

type testLogEntry struct {
	level   slog.Level
	message string
	attrs   map[string]string
}

type captureLogHandler struct {
	mu      sync.Mutex
	entries []testLogEntry
	minLvl  slog.Level
}

func (h *captureLogHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.minLvl
}

func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, testLogEntry{
		level:   r.Level,
		message: r.Message,
		attrs:   attrs,
	})
	h.mu.Unlock()
	return nil
}

func (h *captureLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureLogHandler) hasMessage(lvl slog.Level, substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.level == lvl && strings.Contains(e.message, substr) {
			return true
		}
	}
	return false
}

func newTestClientWithOptions(
	t *testing.T,
	dialOpts []grpc.DialOption,
	reactorOpts []pstest.ServerReactorOption,
	opts ...Option,
) (*Client, *pstest.Server) {
	t.Helper()

	srv := pstest.NewServer(reactorOpts...)
	t.Cleanup(func() {
		_ = srv.Close()
	})

	dOpts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dialOpts...)
	conn, err := grpc.NewClient(srv.Addr, dOpts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}

	allOpts := append([]Option{
		WithClientOptions(nil, option.WithGRPCConn(conn), option.WithoutAuthentication()),
	}, opts...)
	c, err := NewClientWithOptions(t.Context(), testProjectID, allOpts...)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("NewClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
	})
	return c, srv
}

func TestNewClientWithOptions_ChannelBufferAndNilLogger(t *testing.T) {
	ctx := t.Context()
	c, _ := newTestClientWithOptions(
		t,
		nil,
		nil,
		nil,
		WithChannelBuffer(0),
		WithChannelBuffer(-5),
		WithChannelBuffer(7),
		WithLogger(nil),
	)
	if got := c.channelBuffer(); got != 7 {
		t.Fatalf("channelBuffer() = %d, want 7", got)
	}

	// Zero-value Client must be nil-logger and buffer safe.
	zeroClient := &Client{}
	if got := zeroClient.channelBuffer(); got != defaultChannelBuffer {
		t.Fatalf("zeroClient.channelBuffer() = %d, want %d", got, defaultChannelBuffer)
	}
	zeroClient.log().Info("discarded")

	ch, err := c.Subscribe(ctx, "buf-topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if cap(ch) != 7 {
		t.Fatalf("cap(ch) = %d, want 7", cap(ch))
	}
	if err := c.Publish(ctx, "buf-topic", []byte("hello")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := string(receiveWithTimeout(t, ch, 5*time.Second)); got != "hello" {
		t.Fatalf("received = %q, want %q", got, "hello")
	}
}

func TestClient_PreProvisionedSubscriptions(t *testing.T) {
	forbidAdminRPCs := func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		for _, blocked := range []string{"/GetTopic", "/CreateTopic", "/CreateSubscription", "/DeleteSubscription"} {
			if strings.HasSuffix(method, blocked) {
				return status.Errorf(codes.Internal, "unexpected admin RPC %s", method)
			}
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}

	t.Run("StaticSubscriptionID_SkipsAdminRPCs", func(t *testing.T) {
		ctx := t.Context()
		c, srv := newTestClientWithOptions(
			t,
			[]grpc.DialOption{grpc.WithUnaryInterceptor(forbidAdminRPCs)},
			nil,
			WithSubscriptionID("preprovisioned-sub"),
		)

		topicPath := c.topicPath("pre-topic")
		subPath := c.subscriptionPath("preprovisioned-sub")
		if _, err := srv.GServer.CreateTopic(ctx, &pubsubpb.Topic{Name: topicPath}); err != nil {
			t.Fatalf("GServer.CreateTopic() error = %v", err)
		}
		if _, err := srv.GServer.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name:  subPath,
			Topic: topicPath,
		}); err != nil {
			t.Fatalf("GServer.CreateSubscription() error = %v", err)
		}

		ch, err := c.Subscribe(ctx, "pre-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		if err := c.Publish(ctx, "pre-topic", []byte("pre-msg")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		if got := string(receiveWithTimeout(t, ch, 5*time.Second)); got != "pre-msg" {
			t.Fatalf("received = %q, want %q", got, "pre-msg")
		}

		if err := c.Unsubscribe(ctx, "pre-topic"); err != nil {
			t.Fatalf("Unsubscribe() error = %v", err)
		}
		assertChannelClosed(t, ch, 5*time.Second)

		// Verify pre-provisioned subscription still exists on server.
		if _, err := srv.GServer.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subPath}); err != nil {
			t.Fatalf("pre-provisioned subscription was deleted or missing: %v", err)
		}
	})

	t.Run("SubscriptionIDFunc_WithAutoManageFalse", func(t *testing.T) {
		ctx := t.Context()
		c, srv := newTestClientWithOptions(
			t,
			[]grpc.DialOption{grpc.WithUnaryInterceptor(forbidAdminRPCs)},
			nil,
			WithSubscriptionIDFunc(func(topic string) string {
				return "sub-" + topic
			}),
			WithAutoManage(false),
		)

		for _, topic := range []string{"alpha", "beta"} {
			topicPath := c.topicPath(topic)
			subPath := c.subscriptionPath("sub-" + topic)
			if _, err := srv.GServer.CreateTopic(ctx, &pubsubpb.Topic{Name: topicPath}); err != nil {
				t.Fatalf("CreateTopic(%s) error = %v", topic, err)
			}
			if _, err := srv.GServer.CreateSubscription(ctx, &pubsubpb.Subscription{
				Name:  subPath,
				Topic: topicPath,
			}); err != nil {
				t.Fatalf("CreateSubscription(%s) error = %v", topic, err)
			}
		}

		chA, err := c.Subscribe(ctx, "alpha")
		if err != nil {
			t.Fatalf("Subscribe(alpha) error = %v", err)
		}
		chB, err := c.Subscribe(ctx, "beta")
		if err != nil {
			t.Fatalf("Subscribe(beta) error = %v", err)
		}

		if err := c.Publish(ctx, "alpha", []byte("a1")); err != nil {
			t.Fatalf("Publish(alpha) error = %v", err)
		}
		if err := c.Publish(ctx, "beta", []byte("b1")); err != nil {
			t.Fatalf("Publish(beta) error = %v", err)
		}
		if got := string(receiveWithTimeout(t, chA, 5*time.Second)); got != "a1" {
			t.Fatalf("alpha = %q, want a1", got)
		}
		if got := string(receiveWithTimeout(t, chB, 5*time.Second)); got != "b1" {
			t.Fatalf("beta = %q, want b1", got)
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		assertChannelClosed(t, chA, 5*time.Second)
		assertChannelClosed(t, chB, 5*time.Second)
	})

	t.Run("AutoManageTrueOverridesStaticSubscriptionID", func(t *testing.T) {
		ctx := t.Context()
		c, srv := newTestClientWithOptions(
			t,
			nil,
			nil,
			WithSubscriptionID("custom-auto-sub"),
			WithAutoManage(true),
		)

		ch, err := c.Subscribe(ctx, "auto-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		if err := c.Publish(ctx, "auto-topic", []byte("auto-msg")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		if got := string(receiveWithTimeout(t, ch, 5*time.Second)); got != "auto-msg" {
			t.Fatalf("received = %q, want auto-msg", got)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		_, err = srv.GServer.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
			Subscription: c.subscriptionPath("custom-auto-sub"),
		})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("expected auto-managed subscription to be deleted on Close, got %v", err)
		}
	})

	t.Run("ValidationErrors", func(t *testing.T) {
		cMissing, _ := newTestClientWithOptions(t, nil, nil, WithAutoManage(false))
		if _, err := cMissing.Subscribe(t.Context(), "topic"); !errors.Is(err, ErrMissingSubscriptionID) {
			t.Fatalf("Subscribe() = %v, want ErrMissingSubscriptionID", err)
		}

		badIDs := []string{
			"",
			"ab",
			strings.Repeat("a", 256),
			"1starts-with-digit",
			"goog-reserved",
			"GOOG-reserved",
			"projects/p/subscriptions/s",
			"has space",
		}
		for _, bad := range badIDs {
			cBad, _ := newTestClientWithOptions(t, nil, nil, WithSubscriptionIDFunc(func(string) string {
				return bad
			}))
			if _, err := cBad.Subscribe(t.Context(), "topic"); !errors.Is(err, ErrInvalidSubscriptionID) {
				t.Errorf("Subscribe(bad=%q) = %v, want ErrInvalidSubscriptionID", bad, err)
			}
		}
	})
}

func TestClient_WithLogger(t *testing.T) {
	t.Run("LogsReceiveError", func(t *testing.T) {
		handler := &captureLogHandler{minLvl: slog.LevelDebug}
		streamErrInterceptor := func(
			_ context.Context,
			_ *grpc.StreamDesc,
			_ *grpc.ClientConn,
			_ string,
			_ grpc.Streamer,
			_ ...grpc.CallOption,
		) (grpc.ClientStream, error) {
			return nil, status.Error(codes.PermissionDenied, "stream denied")
		}

		c, _ := newTestClientWithOptions(
			t,
			[]grpc.DialOption{grpc.WithStreamInterceptor(streamErrInterceptor)},
			nil,
			WithLogger(slog.New(handler)),
		)

		ch, err := c.Subscribe(t.Context(), "log-stream-err")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		assertChannelClosed(t, ch, 5*time.Second)

		if !handler.hasMessage(slog.LevelError, "gcp pubsub receive stopped with error") {
			t.Errorf("expected error log for receive failure, got %+v", handler.entries)
		}
	})

	t.Run("LogsCleanupDeleteErrorsAndNackDebug", func(t *testing.T) {
		handler := &captureLogHandler{minLvl: slog.LevelDebug}
		c, srv := newTestClientWithOptions(
			t,
			nil,
			[]pstest.ServerReactorOption{
				pstest.WithErrorInjection("DeleteSubscription", codes.Internal, "delete boom"),
			},
			WithChannelBuffer(1),
			WithLogger(slog.New(handler)),
		)

		ctx := t.Context()
		ch, err := c.Subscribe(ctx, "log-cleanup")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		topicPath := c.topicPath("log-cleanup")
		for i := range 5 {
			srv.Publish(topicPath, []byte(fmt.Sprintf("m-%d", i)), nil)
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(ch) < 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)

		// Resubscribe logs warning when previous DeleteSubscription fails.
		ch2, err := c.Subscribe(ctx, "log-cleanup")
		if err != nil {
			t.Fatalf("second Subscribe() error = %v", err)
		}
		assertChannelClosed(t, ch, 5*time.Second)

		// Close logs warning when DeleteSubscription fails.
		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		assertChannelClosed(t, ch2, 5*time.Second)

		if !handler.hasMessage(slog.LevelWarn, "failed to stop previous subscription") {
			t.Errorf("expected warn log on resubscribe delete failure, got %+v", handler.entries)
		}
		if !handler.hasMessage(slog.LevelWarn, "failed to stop subscription during close") {
			t.Errorf("expected warn log on close delete failure, got %+v", handler.entries)
		}
		if !handler.hasMessage(slog.LevelDebug, "nacking message on context cancellation") {
			t.Errorf("expected debug log for Nack on cancellation, got %+v", handler.entries)
		}
	})

	t.Run("LogsRollbackDeleteError", func(t *testing.T) {
		handler := &captureLogHandler{minLvl: slog.LevelDebug}
		subCtx, subCancel := context.WithCancel(t.Context())
		interceptor := func(
			ctx context.Context,
			method string,
			req, reply any,
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			if err == nil && strings.HasSuffix(method, "/CreateSubscription") {
				subCancel()
			}
			return err
		}

		c, _ := newTestClientWithOptions(
			t,
			[]grpc.DialOption{grpc.WithUnaryInterceptor(interceptor)},
			[]pstest.ServerReactorOption{
				pstest.WithErrorInjection("DeleteSubscription", codes.Internal, "rollback delete boom"),
			},
			WithLogger(slog.New(handler)),
		)

		if _, err := c.Subscribe(subCtx, "rollback-log"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Subscribe() = %v, want context.Canceled", err)
		}
		if !handler.hasMessage(slog.LevelWarn, "failed to delete temporary subscription during rollback") {
			t.Errorf("expected warn log on rollback delete error, got %+v", handler.entries)
		}
	})
}

// Verify Client satisfies the standard PubSubClient contract structurally.
type pubSubContract interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}

var _ pubSubContract = (*Client)(nil)
