package azure

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

// --- Fake Azure Service Bus backends for deterministic unit tests ---

type fakeBroker struct {
	mu           sync.Mutex
	topics       map[string]map[string]*fakeReceiver
	senderErr    error
	receiverErr  error
	closed       atomic.Bool
	sendersCount atomic.Int32
}

func newFakeBroker() *fakeBroker {
	return &fakeBroker{
		topics: make(map[string]map[string]*fakeReceiver),
	}
}

func (b *fakeBroker) NewSender(topic string) (sbSender, error) {
	if b.senderErr != nil {
		return nil, b.senderErr
	}
	b.sendersCount.Add(1)
	return &fakeSender{broker: b, topic: topic}, nil
}

func (b *fakeBroker) NewReceiverForSubscription(topic, subscription string, opts *azservicebus.ReceiverOptions) (sbReceiver, error) {
	if b.receiverErr != nil {
		return nil, b.receiverErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[string]*fakeReceiver)
	}
	r := &fakeReceiver{
		queue: make(chan *azservicebus.ReceivedMessage, 32),
		mode:  opts.ReceiveMode,
	}
	b.topics[topic][subscription] = r
	return r, nil
}

func (b *fakeBroker) Close(_ context.Context) error {
	b.closed.Store(true)
	return nil
}

type fakeSender struct {
	broker *fakeBroker
	topic  string
	closed atomic.Bool
}

func (s *fakeSender) SendMessage(_ context.Context, msg *azservicebus.Message) error {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	for _, r := range s.broker.topics[s.topic] {
		r.queue <- &azservicebus.ReceivedMessage{Body: msg.Body}
	}
	return nil
}

func (s *fakeSender) Close(_ context.Context) error {
	s.closed.Store(true)
	return nil
}

type fakeReceiver struct {
	queue     chan *azservicebus.ReceivedMessage
	mode      azservicebus.ReceiveMode
	completed atomic.Int32
	abandoned atomic.Int32
	closed    atomic.Bool
	failOnce  atomic.Bool
}

func (r *fakeReceiver) ReceiveMessages(ctx context.Context, _ int) ([]*azservicebus.ReceivedMessage, error) {
	if r.failOnce.Swap(false) {
		return nil, errors.New("transient amqp error")
	}
	select {
	case msg := <-r.queue:
		return []*azservicebus.ReceivedMessage{msg}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *fakeReceiver) CompleteMessage(_ context.Context, _ *azservicebus.ReceivedMessage) error {
	r.completed.Add(1)
	return nil
}

func (r *fakeReceiver) AbandonMessage(_ context.Context, _ *azservicebus.ReceivedMessage) error {
	r.abandoned.Add(1)
	return nil
}

func (r *fakeReceiver) Close(_ context.Context) error {
	r.closed.Store(true)
	return nil
}

type fakeAdmin struct {
	mu            sync.Mutex
	ensuredTopics []string
	createdSubs   []string
	deletedSubs   []string
	lastIdle      string
	ensureErr     error
	createSubErr  error
	deleteSubErr  error
}

func (a *fakeAdmin) EnsureTopic(_ context.Context, topic string) error {
	if a.ensureErr != nil {
		return a.ensureErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ensuredTopics = append(a.ensuredTopics, topic)
	return nil
}

func (a *fakeAdmin) CreateSubscription(_ context.Context, topic, subscription, autoDeleteOnIdle string) error {
	if a.createSubErr != nil {
		return a.createSubErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.createdSubs = append(a.createdSubs, topic+"/"+subscription)
	a.lastIdle = autoDeleteOnIdle
	return nil
}

func (a *fakeAdmin) DeleteSubscription(_ context.Context, topic, subscription string) error {
	if a.deleteSubErr != nil {
		return a.deleteSubErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deletedSubs = append(a.deletedSubs, topic+"/"+subscription)
	return nil
}

// --- Tests ---

func TestConstructorValidation(t *testing.T) {
	if _, err := NewClient("", nil); !errors.Is(err, ErrInvalidNamespace) {
		t.Errorf("NewClient(empty ns) = %v, want ErrInvalidNamespace", err)
	}
	if _, err := NewClient("ns.servicebus.windows.net", nil); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("NewClient(nil cred) = %v, want ErrInvalidCredential", err)
	}
	if _, err := NewClientFromConnectionString(""); !errors.Is(err, ErrInvalidConnectionString) {
		t.Errorf("NewClientFromConnectionString(empty) = %v, want ErrInvalidConnectionString", err)
	}
	if _, err := NewClientFromConnectionString("not-a-valid-connection-string"); err == nil {
		t.Error("expected error for malformed connection string")
	}
}

func TestClient_AutoManagedPubSubLifecycle(t *testing.T) {
	ctx := t.Context()
	broker := newFakeBroker()
	adm := &fakeAdmin{}
	cfg := defaultConfig()
	WithAutoDeleteOnIdle("PT2H")(&cfg)
	WithMaxMessages(5)(&cfg)
	WithChannelBuffer(16)(&cfg)

	c := newClientWithBackends(broker, adm, cfg)

	ch, err := c.Subscribe(ctx, "orders")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Verify admin created topic & ephemeral subscription <= 50 chars with idle TTL.
	adm.mu.Lock()
	if len(adm.ensuredTopics) != 1 || adm.ensuredTopics[0] != "orders" {
		t.Errorf("ensuredTopics = %v, want [orders]", adm.ensuredTopics)
	}
	if len(adm.createdSubs) != 1 || !strings.HasPrefix(adm.createdSubs[0], "orders/ms-") {
		t.Errorf("createdSubs = %v, want orders/ms-<uuid>", adm.createdSubs)
	}
	if adm.lastIdle != "PT2H" {
		t.Errorf("lastIdle = %q, want PT2H", adm.lastIdle)
	}
	adm.mu.Unlock()

	// Publish two messages and confirm sender is cached (created only once).
	if err := c.Publish(ctx, "orders", []byte("order-1")); err != nil {
		t.Fatalf("Publish 1 error = %v", err)
	}
	if err := c.Publish(ctx, "orders", []byte("order-2")); err != nil {
		t.Fatalf("Publish 2 error = %v", err)
	}
	if got := broker.sendersCount.Load(); got != 1 {
		t.Errorf("sendersCount = %d, want 1 (cached)", got)
	}

	for _, want := range []string{"order-1", "order-2"} {
		select {
		case got := <-ch:
			if string(got) != want {
				t.Errorf("received %q, want %q", string(got), want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}

	// Verify PeekLock settlement called CompleteMessage.
	broker.mu.Lock()
	var recv *fakeReceiver
	for _, r := range broker.topics["orders"] {
		recv = r
	}
	broker.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if got := recv.completed.Load(); got != 2 {
		t.Errorf("CompleteMessage count = %d, want 2", got)
	}

	// Unsubscribe should close channel, close receiver, and delete ephemeral subscription.
	if err := c.Unsubscribe(ctx, "orders"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after Unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close")
	}
	if !recv.closed.Load() {
		t.Error("expected receiver to be closed on Unsubscribe")
	}

	adm.mu.Lock()
	if len(adm.deletedSubs) != 1 {
		t.Errorf("deletedSubs = %v, want 1 deletion", adm.deletedSubs)
	}
	adm.mu.Unlock()

	// Close is idempotent.
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if !broker.closed.Load() {
		t.Error("expected underlying broker to be closed")
	}
}

func TestClient_PreProvisionedModeSkipsAdmin(t *testing.T) {
	ctx := t.Context()
	broker := newFakeBroker()
	adm := &fakeAdmin{}
	cfg := defaultConfig()
	WithSubscriptionName("prod-sub-1")(&cfg)
	WithReceiveMode(azservicebus.ReceiveModeReceiveAndDelete)(&cfg)

	c := newClientWithBackends(broker, adm, cfg)
	defer func() { _ = c.Close() }()

	ch, err := c.Subscribe(ctx, "events")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if err := c.Publish(ctx, "events", []byte("ping")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := string(<-ch); got != "ping" {
		t.Errorf("got %q, want ping", got)
	}

	// ReceiveAndDelete must not call CompleteMessage.
	broker.mu.Lock()
	recv := broker.topics["events"]["prod-sub-1"]
	broker.mu.Unlock()
	if recv == nil {
		t.Fatal("expected receiver on pre-provisioned subscription prod-sub-1")
	}
	if recv.completed.Load() != 0 {
		t.Errorf("ReceiveAndDelete should not call CompleteMessage, got %d", recv.completed.Load())
	}

	if err := c.Unsubscribe(ctx, "events"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}

	// Admin client must not have been invoked at all.
	adm.mu.Lock()
	if len(adm.ensuredTopics) != 0 || len(adm.createdSubs) != 0 || len(adm.deletedSubs) != 0 {
		t.Errorf("expected zero admin calls in pre-provisioned mode, got ensured=%v created=%v deleted=%v",
			adm.ensuredTopics, adm.createdSubs, adm.deletedSubs)
	}
	adm.mu.Unlock()
}

func TestClient_ErrorsAndEdgeCases(t *testing.T) {
	ctx := t.Context()
	broker := newFakeBroker()
	adm := &fakeAdmin{}

	// Subscription name > 50 chars fails validation.
	cfgLong := defaultConfig()
	WithSubscriptionNameFunc(func(topic string) string {
		return strings.Repeat("a", 51)
	})(&cfgLong)
	cLong := newClientWithBackends(broker, adm, cfgLong)
	if _, err := cLong.Subscribe(ctx, "t"); !errors.Is(err, ErrInvalidSubscriptionName) {
		t.Errorf("Subscribe(51-char sub) = %v, want ErrInvalidSubscriptionName", err)
	}

	c := newClientWithBackends(broker, adm, defaultConfig())
	if _, err := c.Subscribe(ctx, ""); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Subscribe(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Publish(ctx, "", []byte("x")); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Publish(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Unsubscribe(ctx, "nonexistent"); err != nil {
		t.Errorf("Unsubscribe(nonexistent) = %v, want nil", err)
	}

	// Canceled context rejected immediately.
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

	// Receiver creation error triggers ephemeral subscription rollback.
	broker.receiverErr = errors.New("amqp connect failed")
	if _, err := c.Subscribe(ctx, "rollback-topic"); err == nil {
		t.Error("expected error when NewReceiverForSubscription fails")
	}
	broker.receiverErr = nil
	adm.mu.Lock()
	if len(adm.deletedSubs) != 1 {
		t.Errorf("expected failed Subscribe to roll back ephemeral subscription, deletedSubs=%v", adm.deletedSubs)
	}
	adm.mu.Unlock()

	// Closed client rejects operations.
	_ = c.Close()
	if _, err := c.Subscribe(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Publish(ctx, "t", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish(closed) = %v, want ErrClosed", err)
	}
	if err := c.Unsubscribe(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Unsubscribe(closed) = %v, want ErrClosed", err)
	}

	// Verify isHTTPStatus helper.
	if !isHTTPStatus(&azcore.ResponseError{StatusCode: http.StatusConflict}, http.StatusConflict) {
		t.Error("expected isHTTPStatus to match StatusConflict")
	}
	if isHTTPStatus(errors.New("plain"), http.StatusConflict) {
		t.Error("expected isHTTPStatus to return false for non-ResponseError")
	}
}

func TestClient_TransientReceiveRetryAndNoGoroutineLeaks(t *testing.T) {
	ctx := t.Context()
	broker := newFakeBroker()
	cfg := defaultConfig()
	WithAutoManage(false)(&cfg)
	WithSubscriptionName("retry-sub")(&cfg)

	c := newClientWithBackends(broker, nil, cfg)
	ch, err := c.Subscribe(ctx, "retry-topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	broker.mu.Lock()
	recv := broker.topics["retry-topic"]["retry-sub"]
	broker.mu.Unlock()
	recv.failOnce.Store(true)

	if err := c.Publish(ctx, "retry-topic", []byte("after-retry")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case got := <-ch:
		if string(got) != "after-retry" {
			t.Errorf("got %q, want after-retry", string(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message after transient error")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/azure") {
			t.Errorf("detected leaked goroutine in pubsub/azure:\n%s", buf.String())
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
