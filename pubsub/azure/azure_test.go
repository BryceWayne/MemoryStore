package azure

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
)

// --- Fake Azure Service Bus backends for deterministic unit tests ---

type fakeBroker struct {
	mu            sync.Mutex
	topics        map[string]map[string]*fakeReceiver
	senderErr     error
	receiverErr   error
	closed        atomic.Bool
	sendersCount  atomic.Int32
	onNewReceiver func()
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
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[string]*fakeReceiver)
	}
	r := &fakeReceiver{
		queue: make(chan *azservicebus.ReceivedMessage, 32),
		mode:  opts.ReceiveMode,
	}
	b.topics[topic][subscription] = r
	hook := b.onNewReceiver
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return r, nil
}

func (b *fakeBroker) Close(_ context.Context) error {
	b.closed.Store(true)
	return nil
}

type fakeSender struct {
	broker   *fakeBroker
	topic    string
	closed   atomic.Bool
	closeErr error
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
	return s.closeErr
}

type fakeReceiver struct {
	queue       chan *azservicebus.ReceivedMessage
	mode        azservicebus.ReceiveMode
	completed   atomic.Int32
	abandoned   atomic.Int32
	closed      atomic.Bool
	failOnce    atomic.Bool
	alwaysFail  atomic.Bool
	onFail      func()
	completeErr error
	abandonErr  error
	closeErr    error
}

func (r *fakeReceiver) ReceiveMessages(ctx context.Context, _ int) ([]*azservicebus.ReceivedMessage, error) {
	if r.alwaysFail.Load() || r.failOnce.Swap(false) {
		if r.onFail != nil {
			r.onFail()
		}
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
	return r.completeErr
}

func (r *fakeReceiver) AbandonMessage(_ context.Context, _ *azservicebus.ReceivedMessage) error {
	r.abandoned.Add(1)
	return r.abandonErr
}

func (r *fakeReceiver) Close(_ context.Context) error {
	r.closed.Store(true)
	return r.closeErr
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if recv.completed.Load() == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
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

// --- Offline HTTP Transporter & TokenCredential for realAdminClient / constructors ---

const (
	testDummySASConnectionString = "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=dGVzdC1rZXktMTIzNDU2Nzg5MDEyMzQ1Njc4OTA="
	testTopicATOMXML             = `<entry xmlns="http://www.w3.org/2005/Atom"><title>orders</title><content type="application/xml"><TopicDescription xmlns="http://schemas.microsoft.com/netservices/2010/10/servicebus/connect"></TopicDescription></content></entry>`
	testSubscriptionATOMXML      = `<entry xmlns="http://www.w3.org/2005/Atom"><title>sub1</title><content type="application/xml"><SubscriptionDescription xmlns="http://schemas.microsoft.com/netservices/2010/10/servicebus/connect"></SubscriptionDescription></content></entry>`
)

type fakeTokenCredential struct{}

func (fakeTokenCredential) GetToken(_ context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{
		Token:     "fake-token",
		ExpiresOn: time.Now().Add(time.Hour),
	}, nil
}

type recordedHTTPReq struct {
	method string
	path   string
	body   string
}

type mockAdminTransport struct {
	mu       sync.Mutex
	requests []recordedHTTPReq
	handler  func(req *http.Request, body string) *http.Response
}

func (m *mockAdminTransport) Do(req *http.Request) (*http.Response, error) {
	var bodyStr string
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		bodyStr = string(b)
	}
	m.mu.Lock()
	m.requests = append(m.requests, recordedHTTPReq{
		method: req.Method,
		path:   req.URL.Path,
		body:   bodyStr,
	})
	m.mu.Unlock()
	return m.handler(req, bodyStr), nil
}

func newTestAdminClient(t *testing.T, handler func(req *http.Request, body string) *http.Response) (*realAdminClient, *mockAdminTransport) {
	t.Helper()
	tr := &mockAdminTransport{handler: handler}
	ac, err := admin.NewClient("test.servicebus.windows.net", fakeTokenCredential{}, &admin.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: tr,
			Retry: policy.RetryOptions{
				MaxRetries: -1,
			},
		},
	})
	if err != nil {
		t.Fatalf("admin.NewClient() error = %v", err)
	}
	return &realAdminClient{inner: ac}, tr
}

func assertNoAzureGoroutineLeaks(t *testing.T) {
	t.Helper()
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

func TestRealAdminClient_EnsureTopicAndSubscriptionLifecycle(t *testing.T) {
	ctx := t.Context()

	t.Run("EnsureTopic existing 200 skips PUT", func(t *testing.T) {
		adm, tr := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(testTopicATOMXML)),
				Request:    req,
			}
		})
		if err := adm.EnsureTopic(ctx, "orders"); err != nil {
			t.Fatalf("EnsureTopic() error = %v", err)
		}
		if len(tr.requests) != 1 || tr.requests[0].method != http.MethodGet {
			t.Errorf("expected single GET request, got %+v", tr.requests)
		}
	})

	t.Run("EnsureTopic missing 404 triggers PUT 201", func(t *testing.T) {
		adm, tr := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			if req.Method == http.MethodGet {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(strings.NewReader(testTopicATOMXML)),
				Request:    req,
			}
		})
		if err := adm.EnsureTopic(ctx, "orders"); err != nil {
			t.Fatalf("EnsureTopic() error = %v", err)
		}
		if len(tr.requests) != 2 || tr.requests[0].method != http.MethodGet || tr.requests[1].method != http.MethodPut {
			t.Errorf("expected GET then PUT, got %+v", tr.requests)
		}
	})

	t.Run("EnsureTopic 404 then PUT 409 conflict succeeds idempotently", func(t *testing.T) {
		adm, tr := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			if req.Method == http.MethodGet {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}
			}
			return &http.Response{
				StatusCode: http.StatusConflict,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		})
		if err := adm.EnsureTopic(ctx, "orders"); err != nil {
			t.Fatalf("EnsureTopic() error = %v", err)
		}
		if len(tr.requests) != 2 || tr.requests[1].method != http.MethodPut {
			t.Errorf("expected GET then PUT, got %+v", tr.requests)
		}
	})

	t.Run("EnsureTopic GET non-404 error returns wrapped error", func(t *testing.T) {
		for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
			adm, tr := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
				return &http.Response{
					StatusCode: status,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}
			})
			err := adm.EnsureTopic(ctx, "orders")
			if err == nil || !strings.Contains(err.Error(), "failed to check topic existence") {
				t.Errorf("status %d: EnsureTopic() error = %v, want failed to check topic existence", status, err)
			}
			if !isHTTPStatus(err, status) {
				t.Errorf("status %d: expected wrapped ResponseError, got %v", status, err)
			}
			if len(tr.requests) != 1 {
				t.Errorf("status %d: expected only GET request, got %+v", status, tr.requests)
			}
		}
	})

	t.Run("EnsureTopic 404 then PUT 500 returns create error", func(t *testing.T) {
		adm, _ := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			if req.Method == http.MethodGet {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}
			}
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		})
		err := adm.EnsureTopic(ctx, "orders")
		if err == nil || !strings.Contains(err.Error(), "failed to create topic") {
			t.Errorf("EnsureTopic() error = %v, want failed to create topic", err)
		}
	})

	t.Run("CreateSubscription 201 409 500", func(t *testing.T) {
		adm, tr := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			return &http.Response{
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(strings.NewReader(testSubscriptionATOMXML)),
				Request:    req,
			}
		})
		if err := adm.CreateSubscription(ctx, "orders", "sub1", "PT1H"); err != nil {
			t.Fatalf("CreateSubscription(201) error = %v", err)
		}
		if len(tr.requests) != 1 || !strings.Contains(tr.requests[0].body, "<AutoDeleteOnIdle>PT1H</AutoDeleteOnIdle>") {
			t.Errorf("expected AutoDeleteOnIdle=PT1H in PUT body, got %+v", tr.requests)
		}

		admConflict, _ := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			return &http.Response{
				StatusCode: http.StatusConflict,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		})
		if err := admConflict.CreateSubscription(ctx, "orders", "sub1", ""); err != nil {
			t.Errorf("CreateSubscription(409) error = %v, want nil", err)
		}

		admErr, _ := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		})
		if err := admErr.CreateSubscription(ctx, "orders", "sub1", ""); err == nil || !strings.Contains(err.Error(), "failed to create subscription") {
			t.Errorf("CreateSubscription(500) error = %v, want failed to create subscription", err)
		}
	})

	t.Run("DeleteSubscription 200 404 500", func(t *testing.T) {
		for _, status := range []int{http.StatusOK, http.StatusNotFound} {
			adm, _ := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
				return &http.Response{
					StatusCode: status,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}
			})
			if err := adm.DeleteSubscription(ctx, "orders", "sub1"); err != nil {
				t.Errorf("DeleteSubscription(%d) error = %v, want nil", status, err)
			}
		}

		admErr, _ := newTestAdminClient(t, func(req *http.Request, _ string) *http.Response {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		})
		if err := admErr.DeleteSubscription(ctx, "orders", "sub1"); err == nil || !strings.Contains(err.Error(), "failed to delete subscription") {
			t.Errorf("DeleteSubscription(500) error = %v, want failed to delete subscription", err)
		}
	})
}

func TestRealConstructorsAndDataWrappersOffline(t *testing.T) {
	tr := &mockAdminTransport{
		handler: func(req *http.Request, _ string) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}
		},
	}
	clientOpts := &azservicebus.ClientOptions{
		ApplicationID: "memorystore-test",
		RetryOptions: azservicebus.RetryOptions{
			MaxRetries: -1,
		},
	}
	adminOpts := &admin.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: tr,
			Retry: policy.RetryOptions{
				MaxRetries: -1,
			},
		},
	}

	cCred, err := NewClient(
		"test.servicebus.windows.net",
		fakeTokenCredential{},
		WithClientOptions(clientOpts),
		WithAdminClientOptions(adminOpts),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if cCred.admin == nil {
		t.Error("expected auto-managed NewClient to initialize admin client")
	}
	if err := cCred.Close(); err != nil {
		t.Fatalf("NewClient.Close() error = %v", err)
	}

	cNoAdmin, err := NewClient(
		"test.servicebus.windows.net",
		fakeTokenCredential{},
		WithAutoManage(false),
	)
	if err != nil {
		t.Fatalf("NewClient(no admin) error = %v", err)
	}
	if cNoAdmin.admin != nil {
		t.Error("expected admin client to be nil when auto-manage is disabled")
	}
	if err := cNoAdmin.Close(); err != nil {
		t.Fatalf("NewClient(no admin).Close() error = %v", err)
	}

	cConn, err := NewClientFromConnectionString(
		testDummySASConnectionString,
		WithClientOptions(clientOpts),
		WithAdminClientOptions(adminOpts),
	)
	if err != nil {
		t.Fatalf("NewClientFromConnectionString() error = %v", err)
	}
	if cConn.admin == nil {
		t.Error("expected NewClientFromConnectionString to initialize admin client")
	}

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	sender, err := cConn.data.NewSender("offline-topic")
	if err != nil {
		t.Fatalf("data.NewSender() error = %v", err)
	}
	if err := sender.SendMessage(canceledCtx, &azservicebus.Message{Body: []byte("offline")}); err == nil {
		t.Error("expected SendMessage with canceled context to return error")
	}
	if err := sender.Close(context.Background()); err != nil {
		t.Errorf("sender.Close() error = %v", err)
	}

	recv, err := cConn.data.NewReceiverForSubscription("offline-topic", "offline-sub", &azservicebus.ReceiverOptions{
		ReceiveMode: azservicebus.ReceiveModeReceiveAndDelete,
	})
	if err != nil {
		t.Fatalf("data.NewReceiverForSubscription() error = %v", err)
	}
	if _, err := recv.ReceiveMessages(canceledCtx, 1); err == nil {
		t.Error("expected ReceiveMessages with canceled context to return error")
	}
	if err := recv.CompleteMessage(canceledCtx, &azservicebus.ReceivedMessage{}); err == nil {
		t.Error("expected CompleteMessage on ReceiveAndDelete receiver to return error")
	}
	if err := recv.AbandonMessage(canceledCtx, &azservicebus.ReceivedMessage{}); err == nil {
		t.Error("expected AbandonMessage on ReceiveAndDelete receiver to return error")
	}
	if err := recv.Close(context.Background()); err != nil {
		t.Errorf("recv.Close() error = %v", err)
	}

	if err := cConn.Close(); err != nil {
		t.Fatalf("NewClientFromConnectionString.Close() error = %v", err)
	}
	assertNoAzureGoroutineLeaks(t)
}

func TestClient_MissingBranchesAndEdgeCases(t *testing.T) {
	ctx := t.Context()

	t.Run("getOrCreateSender error and concurrent Publish", func(t *testing.T) {
		broker := newFakeBroker()
		wantErr := errors.New("sender init failed")
		broker.senderErr = wantErr
		c := newClientWithBackends(broker, nil, defaultConfig())
		if err := c.Publish(ctx, "t1", []byte("x")); !errors.Is(err, wantErr) {
			t.Errorf("Publish() error = %v, want %v", err, wantErr)
		}
		broker.senderErr = nil

		// Concurrent Publish on a fresh topic exercises sender creation & caching under -race.
		const workers = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := c.Publish(ctx, "concurrent-topic", []byte("msg")); err != nil {
					t.Errorf("concurrent Publish() error = %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := broker.sendersCount.Load(); got != 1 {
			t.Errorf("sendersCount = %d, want 1", got)
		}
		_ = c.Close()
	})

	t.Run("Subscribe duplicate replaces previous subscription", func(t *testing.T) {
		broker := newFakeBroker()
		adm := &fakeAdmin{}
		c := newClientWithBackends(broker, adm, defaultConfig())

		ch1, err := c.Subscribe(ctx, "orders")
		if err != nil {
			t.Fatalf("Subscribe 1 error = %v", err)
		}
		ch2, err := c.Subscribe(ctx, "orders")
		if err != nil {
			t.Fatalf("Subscribe 2 error = %v", err)
		}

		select {
		case _, ok := <-ch1:
			if ok {
				t.Error("expected ch1 to be closed on duplicate Subscribe")
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for ch1 close")
		}

		adm.mu.Lock()
		deletedCount := len(adm.deletedSubs)
		adm.mu.Unlock()
		if deletedCount != 1 {
			t.Errorf("expected old ephemeral subscription to be deleted, deletedSubs=%v", adm.deletedSubs)
		}

		if err := c.Publish(ctx, "orders", []byte("second-sub")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		select {
		case got := <-ch2:
			if string(got) != "second-sub" {
				t.Errorf("got %q, want second-sub", string(got))
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for ch2 message")
		}

		_ = c.Close()
		assertNoAzureGoroutineLeaks(t)
	})

	t.Run("Subscribe propagates EnsureTopic and CreateSubscription errors", func(t *testing.T) {
		broker := newFakeBroker()
		adm := &fakeAdmin{ensureErr: errors.New("ensure topic boom")}
		c := newClientWithBackends(broker, adm, defaultConfig())

		if _, err := c.Subscribe(ctx, "orders"); !errors.Is(err, adm.ensureErr) {
			t.Errorf("Subscribe(ensureErr) = %v, want %v", err, adm.ensureErr)
		}

		adm.ensureErr = nil
		adm.createSubErr = errors.New("create sub boom")
		if _, err := c.Subscribe(ctx, "orders"); !errors.Is(err, adm.createSubErr) {
			t.Errorf("Subscribe(createSubErr) = %v, want %v", err, adm.createSubErr)
		}
		_ = c.Close()
	})

	t.Run("Subscribe racing with Close rolls back receiver and ephemeral subscription", func(t *testing.T) {
		broker := newFakeBroker()
		adm := &fakeAdmin{}
		c := newClientWithBackends(broker, adm, defaultConfig())

		broker.onNewReceiver = func() {
			_ = c.Close()
		}

		if _, err := c.Subscribe(ctx, "race-topic"); !errors.Is(err, ErrClosed) {
			t.Errorf("Subscribe() during Close = %v, want ErrClosed", err)
		}

		broker.mu.Lock()
		var recv *fakeReceiver
		for _, r := range broker.topics["race-topic"] {
			recv = r
		}
		broker.mu.Unlock()
		if recv == nil || !recv.closed.Load() {
			t.Error("expected newly created receiver to be closed during rollback")
		}

		adm.mu.Lock()
		deletedCount := len(adm.deletedSubs)
		adm.mu.Unlock()
		if deletedCount != 1 {
			t.Errorf("expected ephemeral subscription rollback deletion, deletedSubs=%v", adm.deletedSubs)
		}
		assertNoAzureGoroutineLeaks(t)
	})

	t.Run("Cancel during receiveRetryDelay exits immediately", func(t *testing.T) {
		broker := newFakeBroker()
		failedOnce := make(chan struct{})
		var once sync.Once
		broker.onNewReceiver = func() {
			broker.mu.Lock()
			recv := broker.topics["retry-cancel-topic"]["cancel-retry-sub"]
			broker.mu.Unlock()
			recv.onFail = func() {
				once.Do(func() { close(failedOnce) })
			}
			recv.alwaysFail.Store(true)
		}

		cfg := defaultConfig()
		WithAutoManage(false)(&cfg)
		WithSubscriptionName("cancel-retry-sub")(&cfg)
		c := newClientWithBackends(broker, nil, cfg)

		ch, err := c.Subscribe(ctx, "retry-cancel-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		select {
		case <-failedOnce:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for ReceiveMessages failure")
		}
		// Give the loop a brief moment to enter the retry timer select before canceling.
		time.Sleep(15 * time.Millisecond)

		start := time.Now()
		if err := c.Unsubscribe(ctx, "retry-cancel-topic"); err != nil {
			t.Fatalf("Unsubscribe() error = %v", err)
		}
		if elapsed := time.Since(start); elapsed >= receiveRetryDelay {
			t.Errorf("Unsubscribe took %v, expected immediate exit (< %v)", elapsed, receiveRetryDelay)
		}
		if _, ok := <-ch; ok {
			t.Error("expected channel to be closed after Unsubscribe")
		}
		_ = c.Close()
		assertNoAzureGoroutineLeaks(t)
	})

	t.Run("Slow consumer abandons unbuffered PeekLock message on cancel", func(t *testing.T) {
		broker := newFakeBroker()
		cfg := defaultConfig()
		WithAutoManage(false)(&cfg)
		WithSubscriptionName("slow-sub")(&cfg)
		WithChannelBuffer(1)(&cfg)
		WithReceiveMode(azservicebus.ReceiveModePeekLock)(&cfg)
		c := newClientWithBackends(broker, nil, cfg)

		ch, err := c.Subscribe(ctx, "slow-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		broker.mu.Lock()
		recv := broker.topics["slow-topic"]["slow-sub"]
		broker.mu.Unlock()

		// Enqueue 2 messages without draining ch (buffer=1):
		// msg 1 enters ch and completes; msg 2 blocks on ch <- msg.Body.
		recv.queue <- &azservicebus.ReceivedMessage{Body: []byte("msg-1")}
		recv.queue <- &azservicebus.ReceivedMessage{Body: []byte("msg-2")}

		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if recv.completed.Load() == 1 && len(recv.queue) == 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if recv.completed.Load() != 1 || len(recv.queue) != 0 {
			t.Fatalf("unexpected state before Unsubscribe: completed=%d queueLen=%d", recv.completed.Load(), len(recv.queue))
		}
		// Ensure receive goroutine is blocked in select on ch <- msg.Body.
		time.Sleep(15 * time.Millisecond)

		if err := c.Unsubscribe(ctx, "slow-topic"); err != nil {
			t.Fatalf("Unsubscribe() error = %v", err)
		}
		if got := recv.abandoned.Load(); got != 1 {
			t.Errorf("AbandonMessage count = %d, want 1", got)
		}
		// Drain buffered msg-1 and verify channel closes.
		if msg := <-ch; string(msg) != "msg-1" {
			t.Errorf("drained %q, want msg-1", string(msg))
		}
		if _, ok := <-ch; ok {
			t.Error("expected channel to be closed")
		}
		_ = c.Close()
		assertNoAzureGoroutineLeaks(t)
	})
}

func TestClient_ConcurrentSubscribeSameTopic(t *testing.T) {
	t.Run("StopsSupersededSubscriptionAndPreservesActive", func(t *testing.T) {
		ctx := t.Context()
		broker := newFakeBroker()
		adm := &fakeAdmin{}

		var arrived atomic.Int32
		bothArrived := make(chan struct{})
		broker.onNewReceiver = func() {
			if arrived.Add(1) == 2 {
				close(bothArrived)
			} else {
				<-bothArrived
			}
		}

		c := newClientWithBackends(broker, adm, defaultConfig())

		type subRes struct {
			ch  <-chan []byte
			err error
		}
		resCh := make(chan subRes, 2)
		for range 2 {
			go func() {
				ch, err := c.Subscribe(ctx, "race-same-topic")
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
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for superseded Subscribe channel to close")
		}

		adm.mu.Lock()
		createdCount := len(adm.createdSubs)
		deletedCount := len(adm.deletedSubs)
		adm.mu.Unlock()
		if createdCount != 2 || deletedCount != 1 {
			t.Errorf("created=%d deleted=%d, want created=2 deleted=1", createdCount, deletedCount)
		}

		broker.mu.Lock()
		closedCount := 0
		for _, r := range broker.topics["race-same-topic"] {
			if r.closed.Load() {
				closedCount++
			}
		}
		broker.mu.Unlock()
		if closedCount != 1 {
			t.Errorf("closed receivers = %d, want 1", closedCount)
		}

		if err := c.Publish(ctx, "race-same-topic", []byte("winner")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		select {
		case got := <-activeCh:
			if string(got) != "winner" {
				t.Errorf("received %q, want winner", string(got))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for active channel message")
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		if _, ok := <-activeCh; ok {
			t.Error("expected active channel to be closed after Close")
		}
		adm.mu.Lock()
		finalDeleted := len(adm.deletedSubs)
		adm.mu.Unlock()
		if finalDeleted != 2 {
			t.Errorf("final deletedSubs = %d, want 2", finalDeleted)
		}
		assertNoAzureGoroutineLeaks(t)
	})

	t.Run("LogsWarningWhenSupersededStopFails", func(t *testing.T) {
		ctx := t.Context()
		handler := &azureCaptureHandler{}
		broker := newFakeBroker()
		adm := &fakeAdmin{deleteSubErr: errors.New("superseded delete failed")}

		var arrived atomic.Int32
		bothArrived := make(chan struct{})
		broker.onNewReceiver = func() {
			if arrived.Add(1) == 2 {
				close(bothArrived)
			} else {
				<-bothArrived
			}
		}

		cfg := defaultConfig()
		WithLogger(slog.New(handler))(&cfg)
		c := newClientWithBackends(broker, adm, cfg)

		type subRes struct {
			ch  <-chan []byte
			err error
		}
		resCh := make(chan subRes, 2)
		for range 2 {
			go func() {
				ch, err := c.Subscribe(ctx, "race-warn-topic")
				resCh <- subRes{ch: ch, err: err}
			}()
		}

		r1 := <-resCh
		r2 := <-resCh
		if r1.err != nil || r2.err != nil {
			t.Fatalf("Subscribe errors: r1=%v r2=%v", r1.err, r2.err)
		}

		if !handler.hasWarn("failed to stop superseded subscription") {
			t.Errorf("expected superseded subscription stop warn log, got %+v", handler.snapshot())
		}

		_ = c.Close()
		for range r1.ch {
		}
		for range r2.ch {
		}
		assertNoAzureGoroutineLeaks(t)
	})
}

type azureLogRecord struct {
	level   slog.Level
	message string
}

type azureCaptureHandler struct {
	mu      sync.Mutex
	records []azureLogRecord
}

func (h *azureCaptureHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelDebug
}

func (h *azureCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, azureLogRecord{
		level:   r.Level,
		message: r.Message,
	})
	h.mu.Unlock()
	return nil
}

func (h *azureCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *azureCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *azureCaptureHandler) hasWarn(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.level == slog.LevelWarn && strings.Contains(r.message, substr) {
			return true
		}
	}
	return false
}

func (h *azureCaptureHandler) snapshot() []azureLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]azureLogRecord(nil), h.records...)
}

func TestClient_WithLogger(t *testing.T) {
	t.Run("NilLoggerIsSafe", func(t *testing.T) {
		cfg := defaultConfig()
		WithLogger(nil)(&cfg)
		c := newClientWithBackends(newFakeBroker(), &fakeAdmin{}, cfg)
		c.log().Warn("should be discarded")
	})

	t.Run("ReceiveCompleteSlowConsumerAbandonAndCleanupWarnings", func(t *testing.T) {
		ctx := t.Context()
		handler := &azureCaptureHandler{}
		broker := newFakeBroker()
		var setupOnce sync.Once
		broker.onNewReceiver = func() {
			setupOnce.Do(func() {
				broker.mu.Lock()
				for _, r := range broker.topics["log-topic"] {
					r.completeErr = errors.New("complete failed")
					r.abandonErr = errors.New("abandon failed")
					r.closeErr = errors.New("receiver close failed")
					r.failOnce.Store(true)
				}
				broker.mu.Unlock()
			})
		}
		adm := &fakeAdmin{}
		cfg := defaultConfig()
		WithChannelBuffer(1)(&cfg)
		WithLogger(slog.New(handler))(&cfg)
		c := newClientWithBackends(broker, adm, cfg)

		ch, err := c.Subscribe(ctx, "log-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		broker.mu.Lock()
		var recv *fakeReceiver
		for _, r := range broker.topics["log-topic"] {
			recv = r
		}
		broker.mu.Unlock()

		// First message triggers ReceiveMessages retry warning, then delivers into buffer=1
		// and logs CompleteMessage warning. Second message blocks on full buffer, logs
		// slow-consumer warning, and on Unsubscribe/resubscribe logs AbandonMessage warning.
		recv.queue <- &azservicebus.ReceivedMessage{Body: []byte("m1")}
		recv.queue <- &azservicebus.ReceivedMessage{Body: []byte("m2")}

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if handler.hasWarn("subscriber buffer full") && recv.completed.Load() >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !handler.hasWarn("subscriber buffer full") {
			t.Fatalf("expected slow consumer warning, got %+v", handler.snapshot())
		}
		if !handler.hasWarn("receive failed") {
			t.Fatalf("expected receive failed warning, got %+v", handler.snapshot())
		}
		if !handler.hasWarn("complete message failed") {
			t.Fatalf("expected complete message failed warning, got %+v", handler.snapshot())
		}

		// Force DeleteSubscription error during resubscribe & Close to cover stopSubscription warnings.
		adm.deleteSubErr = errors.New("delete sub failed")
		ch2, err := c.Subscribe(ctx, "log-topic")
		if err != nil {
			t.Fatalf("second Subscribe() error = %v", err)
		}
		// Drain ch so goroutine exits cleanly.
		for range ch {
		}

		if err := c.Publish(ctx, "log-topic", []byte("after-resub")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		select {
		case got := <-ch2:
			if string(got) != "after-resub" {
				t.Fatalf("received %q, want after-resub", string(got))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for after-resub")
		}

		// Inject sender close error via a wrapped sender in c.senders.
		c.mu.Lock()
		for k, s := range c.senders {
			if fs, ok := s.(*fakeSender); ok {
				fs.closeErr = errors.New("sender close failed")
				c.senders[k] = fs
			}
		}
		c.mu.Unlock()

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		for _, want := range []string{
			"abandon message failed",
			"failed to close receiver",
			"failed to stop previous subscription on resubscribe",
			"failed to stop subscription during close",
			"failed to close sender during close",
		} {
			if !handler.hasWarn(want) {
				t.Errorf("expected warning containing %q, records=%+v", want, handler.snapshot())
			}
		}
		assertNoAzureGoroutineLeaks(t)
	})

	t.Run("RollbackDeleteAndCloseRaceWarnings", func(t *testing.T) {
		ctx := t.Context()
		handler := &azureCaptureHandler{}
		broker := newFakeBroker()
		broker.receiverErr = errors.New("receiver creation failed")
		adm := &fakeAdmin{deleteSubErr: errors.New("rollback delete failed")}
		cfg := defaultConfig()
		WithLogger(slog.New(handler))(&cfg)
		c := newClientWithBackends(broker, adm, cfg)

		if _, err := c.Subscribe(ctx, "rollback-topic"); err == nil {
			t.Fatal("expected Subscribe error")
		}
		if !handler.hasWarn("failed to delete subscription during receiver rollback") {
			t.Errorf("expected receiver rollback warning, got %+v", handler.snapshot())
		}

		// Now test close race rollback warnings.
		broker.receiverErr = nil
		broker.onNewReceiver = func() {
			broker.mu.Lock()
			for _, r := range broker.topics["race-warn"] {
				r.closeErr = errors.New("close race receiver error")
			}
			broker.mu.Unlock()
			_ = c.Close()
		}
		if _, err := c.Subscribe(ctx, "race-warn"); !errors.Is(err, ErrClosed) {
			t.Fatalf("Subscribe() = %v, want ErrClosed", err)
		}
		if !handler.hasWarn("failed to close receiver during close rollback") {
			t.Errorf("expected close race receiver warning, got %+v", handler.snapshot())
		}
		if !handler.hasWarn("failed to delete subscription during close rollback") {
			t.Errorf("expected close race delete warning, got %+v", handler.snapshot())
		}
		assertNoAzureGoroutineLeaks(t)
	})
}
