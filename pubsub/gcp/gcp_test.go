package gcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Verify Client satisfies the standard PubSubClient contract structurally.
type pubSubContract interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}

var _ pubSubContract = (*Client)(nil)
