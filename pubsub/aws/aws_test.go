package aws

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// --- Deterministic fake SNS & SQS backends for unit tests ---

type fakeSNSState struct {
	mu           sync.Mutex
	topics       map[string]string             // topicName -> topicARN
	subsByTopic  map[string][]fakeSubscription // topicARN -> subscriptions
	sqs          *fakeSQSState
	subSeq       int
	msgSeq       int
	createErr    error
	publishErr   error
	subscribeErr error
	unsubErr     error

	createTopicCalls int
	subscribeCalls   int
	unsubscribeCalls int
	unsubscribedARNs []string
}

type fakeSubscription struct {
	subARN             string
	queueARN           string
	rawMessageDelivery bool
}

func newFakeSNSState(sqsState *fakeSQSState) *fakeSNSState {
	return &fakeSNSState{
		topics:      make(map[string]string),
		subsByTopic: make(map[string][]fakeSubscription),
		sqs:         sqsState,
	}
}

func (s *fakeSNSState) CreateTopic(_ context.Context, params *sns.CreateTopicInput, _ ...func(*sns.Options)) (*sns.CreateTopicOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createTopicCalls++
	if s.createErr != nil {
		return nil, s.createErr
	}
	name := aws.ToString(params.Name)
	topicARN, ok := s.topics[name]
	if !ok {
		topicARN = "arn:aws:sns:us-east-1:123456789012:" + name
		s.topics[name] = topicARN
	}
	return &sns.CreateTopicOutput{TopicArn: aws.String(topicARN)}, nil
}

func (s *fakeSNSState) Publish(_ context.Context, params *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
	s.mu.Lock()
	if s.publishErr != nil {
		err := s.publishErr
		s.mu.Unlock()
		return nil, err
	}
	s.msgSeq++
	msgID := fmt.Sprintf("msg-%d", s.msgSeq)
	topicARN := aws.ToString(params.TopicArn)
	subs := slices.Clone(s.subsByTopic[topicARN])
	s.mu.Unlock()

	body := aws.ToString(params.Message)
	for _, sub := range subs {
		if sub.rawMessageDelivery {
			attrs := make(map[string]sqstypes.MessageAttributeValue, len(params.MessageAttributes))
			for k, v := range params.MessageAttributes {
				attrs[k] = sqstypes.MessageAttributeValue{
					DataType:    v.DataType,
					StringValue: v.StringValue,
				}
			}
			s.sqs.deliverToARN(sub.queueARN, body, attrs)
		} else {
			envAttrs := make(map[string]snsEnvelopeAttr, len(params.MessageAttributes))
			for k, v := range params.MessageAttributes {
				envAttrs[k] = snsEnvelopeAttr{
					Type:  aws.ToString(v.DataType),
					Value: aws.ToString(v.StringValue),
				}
			}
			env := snsNotificationEnvelope{
				Type:              "Notification",
				MessageID:         msgID,
				TopicARN:          topicARN,
				Message:           aws.String(body),
				MessageAttributes: envAttrs,
			}
			raw, _ := json.Marshal(env)
			s.sqs.deliverToARN(sub.queueARN, string(raw), nil)
		}
	}

	return &sns.PublishOutput{MessageId: aws.String(msgID)}, nil
}

func (s *fakeSNSState) Subscribe(_ context.Context, params *sns.SubscribeInput, _ ...func(*sns.Options)) (*sns.SubscribeOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribeCalls++
	if s.subscribeErr != nil {
		return nil, s.subscribeErr
	}
	s.subSeq++
	topicARN := aws.ToString(params.TopicArn)
	subARN := fmt.Sprintf("%s:sub-%d", topicARN, s.subSeq)
	raw := params.Attributes["RawMessageDelivery"] == "true"
	s.subsByTopic[topicARN] = append(s.subsByTopic[topicARN], fakeSubscription{
		subARN:             subARN,
		queueARN:           aws.ToString(params.Endpoint),
		rawMessageDelivery: raw,
	})
	return &sns.SubscribeOutput{SubscriptionArn: aws.String(subARN)}, nil
}

func (s *fakeSNSState) Unsubscribe(_ context.Context, params *sns.UnsubscribeInput, _ ...func(*sns.Options)) (*sns.UnsubscribeOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribeCalls++
	if s.unsubErr != nil {
		return nil, s.unsubErr
	}
	target := aws.ToString(params.SubscriptionArn)
	s.unsubscribedARNs = append(s.unsubscribedARNs, target)
	for topicARN, subs := range s.subsByTopic {
		for i, sub := range subs {
			if sub.subARN == target {
				s.subsByTopic[topicARN] = append(subs[:i], subs[i+1:]...)
				return &sns.UnsubscribeOutput{}, nil
			}
		}
	}
	return &sns.UnsubscribeOutput{}, nil
}

type fakeSQSQueue struct {
	url      string
	arn      string
	policy   string
	waitTime string
	messages chan sqstypes.Message
}

type fakeSQSState struct {
	mu              sync.Mutex
	queuesByURL     map[string]*fakeSQSQueue
	queuesByARN     map[string]*fakeSQSQueue
	deletedQueues   []string
	deletedHandles  []string
	createCalls     int
	getAttrCalls    int
	setAttrCalls    int
	deleteCalls     int
	receiveCalls    atomic.Int32
	failReceiveOnce atomic.Bool
	getAttrErr      error
	setAttrErr      error
	deleteQueueErr  error
	receiptSeq      atomic.Int64
}

func newFakeSQSState() *fakeSQSState {
	return &fakeSQSState{
		queuesByURL: make(map[string]*fakeSQSQueue),
		queuesByARN: make(map[string]*fakeSQSQueue),
	}
}

func (q *fakeSQSState) ensurePreProvisionedQueue(queueURL, queueARN string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := &fakeSQSQueue{
		url:      queueURL,
		arn:      queueARN,
		messages: make(chan sqstypes.Message, 64),
	}
	q.queuesByURL[queueURL] = entry
	q.queuesByARN[queueARN] = entry
}

func (q *fakeSQSState) deliverToARN(queueARN, body string, attrs map[string]sqstypes.MessageAttributeValue) {
	q.mu.Lock()
	entry := q.queuesByARN[queueARN]
	q.mu.Unlock()
	if entry == nil {
		return
	}
	id := q.receiptSeq.Add(1)
	entry.messages <- sqstypes.Message{
		MessageId:         aws.String(fmt.Sprintf("sqs-msg-%d", id)),
		ReceiptHandle:     aws.String(fmt.Sprintf("receipt-%d", id)),
		Body:              aws.String(body),
		MessageAttributes: attrs,
	}
}

func (q *fakeSQSState) deliverRawToURL(queueURL, body string, attrs map[string]sqstypes.MessageAttributeValue) {
	q.mu.Lock()
	entry := q.queuesByURL[queueURL]
	q.mu.Unlock()
	if entry == nil {
		return
	}
	id := q.receiptSeq.Add(1)
	entry.messages <- sqstypes.Message{
		MessageId:         aws.String(fmt.Sprintf("sqs-msg-%d", id)),
		ReceiptHandle:     aws.String(fmt.Sprintf("receipt-%d", id)),
		Body:              aws.String(body),
		MessageAttributes: attrs,
	}
}

func (q *fakeSQSState) CreateQueue(_ context.Context, params *sqs.CreateQueueInput, _ ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.createCalls++
	name := aws.ToString(params.QueueName)
	queueURL := "https://sqs.us-east-1.amazonaws.com/123456789012/" + name
	queueARN := "arn:aws:sqs:us-east-1:123456789012:" + name
	entry := &fakeSQSQueue{
		url:      queueURL,
		arn:      queueARN,
		waitTime: params.Attributes[string(sqstypes.QueueAttributeNameReceiveMessageWaitTimeSeconds)],
		messages: make(chan sqstypes.Message, 64),
	}
	q.queuesByURL[queueURL] = entry
	q.queuesByARN[queueARN] = entry
	return &sqs.CreateQueueOutput{QueueUrl: aws.String(queueURL)}, nil
}

func (q *fakeSQSState) GetQueueAttributes(_ context.Context, params *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.getAttrCalls++
	if q.getAttrErr != nil {
		return nil, q.getAttrErr
	}
	entry := q.queuesByURL[aws.ToString(params.QueueUrl)]
	if entry == nil {
		return nil, &sqstypes.QueueDoesNotExist{Message: aws.String("missing")}
	}
	return &sqs.GetQueueAttributesOutput{
		Attributes: map[string]string{
			string(sqstypes.QueueAttributeNameQueueArn): entry.arn,
		},
	}, nil
}

func (q *fakeSQSState) SetQueueAttributes(_ context.Context, params *sqs.SetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.SetQueueAttributesOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.setAttrCalls++
	if q.setAttrErr != nil {
		return nil, q.setAttrErr
	}
	entry := q.queuesByURL[aws.ToString(params.QueueUrl)]
	if entry == nil {
		return nil, &sqstypes.QueueDoesNotExist{Message: aws.String("missing")}
	}
	if p, ok := params.Attributes[string(sqstypes.QueueAttributeNamePolicy)]; ok {
		entry.policy = p
	}
	return &sqs.SetQueueAttributesOutput{}, nil
}

func (q *fakeSQSState) DeleteQueue(_ context.Context, params *sqs.DeleteQueueInput, _ ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deleteCalls++
	if q.deleteQueueErr != nil {
		return nil, q.deleteQueueErr
	}
	queueURL := aws.ToString(params.QueueUrl)
	q.deletedQueues = append(q.deletedQueues, queueURL)
	if entry := q.queuesByURL[queueURL]; entry != nil {
		delete(q.queuesByURL, queueURL)
		delete(q.queuesByARN, entry.arn)
	}
	return &sqs.DeleteQueueOutput{}, nil
}

func (q *fakeSQSState) ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	q.receiveCalls.Add(1)
	if q.failReceiveOnce.Swap(false) {
		return nil, errors.New("transient sqs receive error")
	}
	q.mu.Lock()
	entry := q.queuesByURL[aws.ToString(params.QueueUrl)]
	q.mu.Unlock()
	if entry == nil {
		return nil, &sqstypes.QueueDoesNotExist{Message: aws.String("missing")}
	}

	select {
	case msg := <-entry.messages:
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{msg}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (q *fakeSQSState) DeleteMessage(_ context.Context, params *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deletedHandles = append(q.deletedHandles, aws.ToString(params.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, nil
}

// --- Tests ---

func TestConstructorAndOptionValidation(t *testing.T) {
	if _, err := NewClient(aws.Config{}); !errors.Is(err, ErrMissingRegion) {
		t.Errorf("NewClient(empty config) = %v, want ErrMissingRegion", err)
	}

	if _, err := NewClient(aws.Config{}, WithSNSOptions(func(o *sns.Options) {
		o.BaseEndpoint = aws.String("http://localhost:4566")
	})); !errors.Is(err, ErrMissingRegion) {
		t.Errorf("NewClient(only SNS BaseEndpoint) = %v, want ErrMissingRegion", err)
	}

	c, err := NewClient(aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewClient(region) error = %v", err)
	}
	_ = c.Close()

	cEndpoint, err := NewClient(
		aws.Config{},
		WithSNSOptions(func(o *sns.Options) { o.BaseEndpoint = aws.String("http://127.0.0.1:4566") }),
		WithSQSOptions(func(o *sqs.Options) { o.BaseEndpoint = aws.String("http://127.0.0.1:4566") }),
	)
	if err != nil {
		t.Fatalf("NewClient(BaseEndpoints only) error = %v", err)
	}
	_ = cEndpoint.Close()

	for _, badURL := range []string{"not-a-url", "://bad", "ftp://example.com/q", "https://"} {
		if _, err := NewClient(aws.Config{Region: "us-east-1"}, WithQueueURL(badURL)); !errors.Is(err, ErrInvalidQueueURL) {
			t.Errorf("NewClient(WithQueueURL(%q)) = %v, want ErrInvalidQueueURL", badURL, err)
		}
	}

	for _, badARN := range []string{"not-an-arn", "arn:aws:sqs:us-east-1:123456789012:q", "arn:aws:sns:us-east-1:123456789012:"} {
		if _, err := NewClient(aws.Config{Region: "us-east-1"}, WithTopicARN(badARN)); !errors.Is(err, ErrInvalidTopicARN) {
			t.Errorf("NewClient(WithTopicARN(%q)) = %v, want ErrInvalidTopicARN", badARN, err)
		}
	}

	// Verify option defaults, bounds clamping, and option ordering independence.
	cfg := defaultConfig()
	if cfg.channelBuffer != 100 || cfg.waitTimeSeconds != 20 || cfg.maxNumberOfMessages != 10 || !cfg.shouldAutoManage() {
		t.Fatalf("unexpected defaultConfig: %+v", cfg)
	}

	WithChannelBuffer(0)(&cfg)
	WithChannelBuffer(-10)(&cfg)
	WithWaitTimeSeconds(-1)(&cfg)
	WithWaitTimeSeconds(21)(&cfg)
	WithMaxNumberOfMessages(0)(&cfg)
	WithMaxNumberOfMessages(11)(&cfg)
	if cfg.channelBuffer != 100 || cfg.waitTimeSeconds != 20 || cfg.maxNumberOfMessages != 10 {
		t.Fatalf("invalid option values were not ignored: %+v", cfg)
	}

	WithChannelBuffer(32)(&cfg)
	WithWaitTimeSeconds(0)(&cfg)
	WithMaxNumberOfMessages(4)(&cfg)
	if cfg.channelBuffer != 32 || cfg.waitTimeSeconds != 0 || cfg.maxNumberOfMessages != 4 {
		t.Fatalf("valid option values were not applied: %+v", cfg)
	}

	// QueueURL auto-disables autoManage regardless of option order unless WithAutoManage(true) is set.
	cfgQueueOnly := defaultConfig()
	WithQueueURL("https://sqs.us-east-1.amazonaws.com/123456789012/q")(&cfgQueueOnly)
	if cfgQueueOnly.shouldAutoManage() {
		t.Error("expected WithQueueURL alone to disable autoManage")
	}

	cfgExplicitFirst := defaultConfig()
	WithAutoManage(true)(&cfgExplicitFirst)
	WithQueueURL("https://sqs.us-east-1.amazonaws.com/123456789012/q")(&cfgExplicitFirst)
	if !cfgExplicitFirst.shouldAutoManage() {
		t.Error("expected WithAutoManage(true) before WithQueueURL to stay enabled")
	}

	cfgExplicitLast := defaultConfig()
	WithQueueURL("https://sqs.us-east-1.amazonaws.com/123456789012/q")(&cfgExplicitLast)
	WithAutoManage(true)(&cfgExplicitLast)
	if !cfgExplicitLast.shouldAutoManage() {
		t.Error("expected WithAutoManage(true) after WithQueueURL to stay enabled")
	}

	if _, err := newClientWithBackends(nil, newFakeSQSState(), defaultConfig()); err == nil {
		t.Error("expected error when snsBackend is nil")
	}
}

func TestClient_AutoManagedLifecycleAndPolicy(t *testing.T) {
	ctx := t.Context()
	sqsState := newFakeSQSState()
	snsState := newFakeSNSState(sqsState)

	cfg := defaultConfig()
	WithChannelBuffer(8)(&cfg)
	WithWaitTimeSeconds(5)(&cfg)
	WithMaxNumberOfMessages(3)(&cfg)

	c, err := newClientWithBackends(snsState, sqsState, cfg)
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}

	ch, err := c.Subscribe(ctx, "cache:invalidation")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Inspect created SQS queue name constraints and IAM policy JSON.
	sqsState.mu.Lock()
	if len(sqsState.queuesByURL) != 1 {
		t.Fatalf("queuesByURL len = %d, want 1", len(sqsState.queuesByURL))
	}
	var qEntry *fakeSQSQueue
	for _, entry := range sqsState.queuesByURL {
		qEntry = entry
	}
	sqsState.mu.Unlock()

	qName := strings.TrimPrefix(qEntry.url, "https://sqs.us-east-1.amazonaws.com/123456789012/")
	if len(qName) > 80 || !strings.HasPrefix(qName, "ms-invalidation-") {
		t.Errorf("unexpected ephemeral queue name %q (len=%d)", qName, len(qName))
	}
	for i := 0; i < len(qName); i++ {
		chByte := qName[i]
		valid := (chByte >= 'a' && chByte <= 'z') || (chByte >= 'A' && chByte <= 'Z') ||
			(chByte >= '0' && chByte <= '9') || chByte == '_' || chByte == '-'
		if !valid {
			t.Errorf("queue name %q contains invalid SQS character %q", qName, chByte)
		}
	}
	if qEntry.waitTime != "5" {
		t.Errorf("queue ReceiveMessageWaitTimeSeconds = %q, want 5", qEntry.waitTime)
	}

	var policyDoc sqsPolicyDocument
	if err := json.Unmarshal([]byte(qEntry.policy), &policyDoc); err != nil {
		t.Fatalf("unmarshal queue policy %q error = %v", qEntry.policy, err)
	}
	if len(policyDoc.Statement) != 1 ||
		policyDoc.Statement[0].Principal["Service"] != "sns.amazonaws.com" ||
		policyDoc.Statement[0].Action != "sqs:SendMessage" ||
		policyDoc.Statement[0].Resource != qEntry.arn ||
		policyDoc.Statement[0].Condition.ArnEquals["aws:SourceArn"] != "arn:aws:sns:us-east-1:123456789012:cache:invalidation" {
		t.Errorf("unexpected SQS policy document: %+v", policyDoc)
	}

	// Publish two messages; verify CreateTopic is cached and called only once.
	msg1 := []byte(`{"o":"node-1","k":["user:42"]}`)
	msg2 := []byte(`{"o":"node-1","k":["user:99"]}`)
	if err := c.Publish(ctx, "cache:invalidation", msg1); err != nil {
		t.Fatalf("Publish 1 error = %v", err)
	}
	if err := c.Publish(ctx, "cache:invalidation", msg2); err != nil {
		t.Fatalf("Publish 2 error = %v", err)
	}

	snsState.mu.Lock()
	createCalls := snsState.createTopicCalls
	snsState.mu.Unlock()
	if createCalls != 1 {
		t.Errorf("CreateTopic calls = %d, want 1 (cached)", createCalls)
	}

	for _, want := range [][]byte{msg1, msg2} {
		select {
		case got := <-ch:
			if !bytes.Equal(got, want) {
				t.Errorf("received %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}

	// Wait briefly for DeleteMessage after channel delivery.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sqsState.mu.Lock()
		deletedCount := len(sqsState.deletedHandles)
		sqsState.mu.Unlock()
		if deletedCount == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sqsState.mu.Lock()
	if len(sqsState.deletedHandles) != 2 {
		t.Errorf("DeleteMessage calls = %d, want 2", len(sqsState.deletedHandles))
	}
	sqsState.mu.Unlock()

	if err := c.Unsubscribe(ctx, "cache:invalidation"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after Unsubscribe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close")
	}

	snsState.mu.Lock()
	unsubCount := snsState.unsubscribeCalls
	snsState.mu.Unlock()
	if unsubCount != 1 {
		t.Errorf("sns.Unsubscribe calls = %d, want 1", unsubCount)
	}
	sqsState.mu.Lock()
	delQueues := len(sqsState.deletedQueues)
	sqsState.mu.Unlock()
	if delQueues != 1 {
		t.Errorf("sqs.DeleteQueue calls = %d, want 1", delQueues)
	}

	// Idempotent Close.
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestClient_SubscribeCanceledContextResilience(t *testing.T) {
	sqsState := newFakeSQSState()
	snsState := newFakeSNSState(sqsState)
	c, err := newClientWithBackends(snsState, sqsState, defaultConfig())
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	subCtx, cancel := context.WithCancel(t.Context())
	ch, err := c.Subscribe(subCtx, "orders")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	// Mirrors MemoryStore.Subscribe / startInvalidationBackplane immediate cancel.
	cancel()

	if err := c.Publish(t.Context(), "orders", []byte("still-alive")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case got := <-ch:
		if string(got) != "still-alive" {
			t.Errorf("got %q, want still-alive", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background receive loop died when Subscribe ctx was canceled")
	}
}

func TestClient_DuplicateSubscribeReplacement(t *testing.T) {
	ctx := t.Context()
	sqsState := newFakeSQSState()
	snsState := newFakeSNSState(sqsState)
	c, err := newClientWithBackends(snsState, sqsState, defaultConfig())
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	ch1, err := c.Subscribe(ctx, "events")
	if err != nil {
		t.Fatalf("first Subscribe() error = %v", err)
	}
	ch2, err := c.Subscribe(ctx, "events")
	if err != nil {
		t.Fatalf("second Subscribe() error = %v", err)
	}

	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("expected prior subscription channel to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first subscription channel close")
	}

	if err := c.Publish(ctx, "events", []byte("second-sub")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case got := <-ch2:
		if string(got) != "second-sub" {
			t.Errorf("got %q, want second-sub", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message on replacement subscription")
	}
}

func TestClient_PreProvisionedLeastPrivilegeIAMMode(t *testing.T) {
	ctx := t.Context()
	sqsState := newFakeSQSState()
	snsState := newFakeSNSState(sqsState)

	topicARN := "arn:aws:sns:us-east-1:123456789012:prod-invalidation"
	queueURL := "https://sqs.us-east-1.amazonaws.com/123456789012/pod-1-queue"
	queueARN := "arn:aws:sqs:us-east-1:123456789012:pod-1-queue"

	sqsState.ensurePreProvisionedQueue(queueURL, queueARN)
	// Simulate pre-provisioned SNS->SQS subscription with RawMessageDelivery=false (envelope mode).
	snsState.subsByTopic[topicARN] = []fakeSubscription{
		{subARN: topicARN + ":pre-sub", queueARN: queueARN, rawMessageDelivery: false},
	}

	cfg := defaultConfig()
	WithQueueURL(queueURL)(&cfg)
	WithTopicARN(topicARN)(&cfg)

	c, err := newClientWithBackends(snsState, sqsState, cfg)
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}

	ch, err := c.Subscribe(ctx, "invalidation")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	wantPayload := []byte(`{"o":"pod-a","k":["k1","k2"]}`)
	if err := c.Publish(ctx, "invalidation", wantPayload); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case got := <-ch:
		if !bytes.Equal(got, wantPayload) {
			t.Errorf("unwrapped envelope = %q, want %q", got, wantPayload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for unwrapped SNS envelope")
	}

	// Plain JSON that happens to contain Type=="Notification" without TopicArn/MessageId must NOT be unwrapped.
	plainJSON := `{"Type":"Notification","Message":"inner-not-sns"}`
	sqsState.deliverRawToURL(queueURL, plainJSON, nil)
	select {
	case got := <-ch:
		if string(got) != plainJSON {
			t.Errorf("plain JSON was altered: got %q, want %q", got, plainJSON)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for plain JSON passthrough")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Verify zero management RPCs were invoked.
	snsState.mu.Lock()
	if snsState.createTopicCalls != 0 || snsState.subscribeCalls != 0 || snsState.unsubscribeCalls != 0 {
		t.Errorf("unexpected SNS admin calls in pre-provisioned mode: create=%d sub=%d unsub=%d",
			snsState.createTopicCalls, snsState.subscribeCalls, snsState.unsubscribeCalls)
	}
	snsState.mu.Unlock()
	sqsState.mu.Lock()
	if sqsState.createCalls != 0 || sqsState.getAttrCalls != 0 || sqsState.setAttrCalls != 0 || sqsState.deleteCalls != 0 {
		t.Errorf("unexpected SQS admin calls in pre-provisioned mode: create=%d get=%d set=%d del=%d",
			sqsState.createCalls, sqsState.getAttrCalls, sqsState.setAttrCalls, sqsState.deleteCalls)
	}
	sqsState.mu.Unlock()
}

func TestClient_BinaryPayloadBase64RoundTrip(t *testing.T) {
	ctx := t.Context()
	binaryPayload := []byte{0xff, 0xfe, 0x00, 0x01, 'A', 0x80, 0x99}

	for _, rawMode := range []bool{true, false} {
		name := "raw"
		if !rawMode {
			name = "envelope"
		}
		t.Run(name, func(t *testing.T) {
			sqsState := newFakeSQSState()
			snsState := newFakeSNSState(sqsState)
			topicARN := "arn:aws:sns:us-east-1:123456789012:bin-topic"
			queueURL := "https://sqs.us-east-1.amazonaws.com/123456789012/bin-queue"
			queueARN := "arn:aws:sqs:us-east-1:123456789012:bin-queue"

			sqsState.ensurePreProvisionedQueue(queueURL, queueARN)
			snsState.subsByTopic[topicARN] = []fakeSubscription{
				{subARN: topicARN + ":sub", queueARN: queueARN, rawMessageDelivery: rawMode},
			}

			cfg := defaultConfig()
			WithQueueURL(queueURL)(&cfg)
			WithTopicARN(topicARN)(&cfg)
			c, err := newClientWithBackends(snsState, sqsState, cfg)
			if err != nil {
				t.Fatalf("newClientWithBackends() error = %v", err)
			}
			defer func() { _ = c.Close() }()

			ch, err := c.Subscribe(ctx, topicARN)
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			if err := c.Publish(ctx, topicARN, binaryPayload); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			select {
			case got := <-ch:
				if !bytes.Equal(got, binaryPayload) {
					t.Errorf("got %x, want %x", got, binaryPayload)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for binary payload")
			}
		})
	}
}

func TestClient_ErrorsRollbackAndClose(t *testing.T) {
	ctx := t.Context()

	// Pre-provisioned missing QueueURL / missing TopicARN.
	cfgNoQueue := defaultConfig()
	WithAutoManage(false)(&cfgNoQueue)
	cNoQueue, err := newClientWithBackends(newFakeSNSState(newFakeSQSState()), newFakeSQSState(), cfgNoQueue)
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}
	if _, err := cNoQueue.Subscribe(ctx, "t"); !errors.Is(err, ErrMissingQueueURL) {
		t.Errorf("Subscribe(no queueURL) = %v, want ErrMissingQueueURL", err)
	}
	if err := cNoQueue.Publish(ctx, "t", []byte("x")); !errors.Is(err, ErrMissingTopicARN) {
		t.Errorf("Publish(no topicARN) = %v, want ErrMissingTopicARN", err)
	}
	// Passing full ARN directly works even without WithTopicARN.
	if err := cNoQueue.Publish(ctx, "arn:aws:sns:us-east-1:123456789012:direct", []byte("ok")); err != nil {
		t.Errorf("Publish(full ARN) error = %v", err)
	}
	_ = cNoQueue.Close()

	// Rollback on GetQueueAttributes, SetQueueAttributes, and sns.Subscribe failures.
	for _, tc := range []struct {
		name  string
		setup func(*fakeSNSState, *fakeSQSState)
	}{
		{
			name:  "GetQueueAttributes",
			setup: func(_ *fakeSNSState, q *fakeSQSState) { q.getAttrErr = errors.New("get attr boom") },
		},
		{
			name:  "SetQueueAttributes",
			setup: func(_ *fakeSNSState, q *fakeSQSState) { q.setAttrErr = errors.New("set policy boom") },
		},
		{
			name:  "sns.Subscribe",
			setup: func(s *fakeSNSState, _ *fakeSQSState) { s.subscribeErr = errors.New("sns sub boom") },
		},
	} {
		t.Run("rollback_"+tc.name, func(t *testing.T) {
			sqsState := newFakeSQSState()
			snsState := newFakeSNSState(sqsState)
			tc.setup(snsState, sqsState)
			c, err := newClientWithBackends(snsState, sqsState, defaultConfig())
			if err != nil {
				t.Fatalf("newClientWithBackends() error = %v", err)
			}
			defer func() { _ = c.Close() }()

			if _, err := c.Subscribe(ctx, "rollback-topic"); err == nil {
				t.Fatalf("expected Subscribe error for %s", tc.name)
			}
			sqsState.mu.Lock()
			deleted := len(sqsState.deletedQueues)
			sqsState.mu.Unlock()
			if deleted != 1 {
				t.Errorf("expected queue rollback (DeleteQueue count=1), got %d", deleted)
			}
		})
	}

	// Close still calls DeleteQueue when sns.Unsubscribe fails, joining errors.
	sqsState := newFakeSQSState()
	snsState := newFakeSNSState(sqsState)
	cJoin, err := newClientWithBackends(snsState, sqsState, defaultConfig())
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}
	if _, err := cJoin.Subscribe(ctx, "join-topic"); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	snsState.mu.Lock()
	snsState.unsubErr = errors.New("unsub failed")
	snsState.mu.Unlock()
	if err := cJoin.Close(); err == nil {
		t.Error("expected Close() to return joined unsubscribe error")
	}
	sqsState.mu.Lock()
	if len(sqsState.deletedQueues) != 1 {
		t.Errorf("expected DeleteQueue to still run after sns.Unsubscribe failure, deleted=%d", len(sqsState.deletedQueues))
	}
	sqsState.mu.Unlock()

	// NotFound / QueueDoesNotExist during cleanup are ignored cleanly.
	sqsNotFound := newFakeSQSState()
	snsNotFound := newFakeSNSState(sqsNotFound)
	cNF, err := newClientWithBackends(snsNotFound, sqsNotFound, defaultConfig())
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}
	if _, err := cNF.Subscribe(ctx, "nf-topic"); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	snsNotFound.mu.Lock()
	snsNotFound.unsubErr = &snstypes.NotFoundException{Message: aws.String("gone")}
	snsNotFound.mu.Unlock()
	sqsNotFound.mu.Lock()
	sqsNotFound.deleteQueueErr = &sqstypes.QueueDoesNotExist{Message: aws.String("gone")}
	sqsNotFound.mu.Unlock()
	if err := cNF.Close(); err != nil {
		t.Errorf("expected NotFound/QueueDoesNotExist cleanup errors to be ignored, got %v", err)
	}

	// Standard validation, canceled ctx, unknown Unsubscribe, and ErrClosed checks.
	c, err := newClientWithBackends(newFakeSNSState(newFakeSQSState()), newFakeSQSState(), defaultConfig())
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}
	if _, err := c.Subscribe(ctx, ""); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Subscribe(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Publish(ctx, "", []byte("x")); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Publish(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Unsubscribe(ctx, "nonexistent"); err != nil {
		t.Errorf("Unsubscribe(nonexistent) = %v, want nil", err)
	}

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
}

func TestClient_TransientReceiveRetryAndNoGoroutineLeaks(t *testing.T) {
	ctx := t.Context()
	sqsState := newFakeSQSState()
	snsState := newFakeSNSState(sqsState)
	topicARN := "arn:aws:sns:us-east-1:123456789012:retry-topic"
	queueURL := "https://sqs.us-east-1.amazonaws.com/123456789012/retry-queue"
	queueARN := "arn:aws:sqs:us-east-1:123456789012:retry-queue"

	sqsState.ensurePreProvisionedQueue(queueURL, queueARN)
	snsState.subsByTopic[topicARN] = []fakeSubscription{
		{subARN: topicARN + ":sub", queueARN: queueARN, rawMessageDelivery: true},
	}

	cfg := defaultConfig()
	WithQueueURL(queueURL)(&cfg)
	WithTopicARN(topicARN)(&cfg)

	c, err := newClientWithBackends(snsState, sqsState, cfg)
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}

	sqsState.failReceiveOnce.Store(true)
	ch, err := c.Subscribe(ctx, "retry-topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := c.Publish(ctx, "retry-topic", []byte("after-retry")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case got := <-ch:
		if string(got) != "after-retry" {
			t.Errorf("got %q, want after-retry", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message after transient ReceiveMessage error")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	assertNoGoroutineLeaks(t)
}

// --- Layer 2: Real AWS SDK v2 wire-protocol integration test via httptest.Server ---

type wireServer struct {
	mu           sync.Mutex
	topics       map[string]string
	queues       map[string]*wireQueue
	subs         map[string]wireSub
	subSeq       int
	msgSeq       int
	receiptSeq   int
	unsubscribed []string
	deletedQs    []string
}

type wireQueue struct {
	url      string
	arn      string
	policy   string
	messages chan wireSQSMessage
}

type wireSub struct {
	subARN             string
	topicARN           string
	queueARN           string
	rawMessageDelivery bool
}

type wireSQSMessage struct {
	MessageID         string                        `json:"MessageId"`
	ReceiptHandle     string                        `json:"ReceiptHandle"`
	Body              string                        `json:"Body"`
	MD5OfBody         string                        `json:"MD5OfBody"`
	MessageAttributes map[string]wireSQSMessageAttr `json:"MessageAttributes,omitempty"`
}

type wireSQSMessageAttr struct {
	DataType    string `json:"DataType"`
	StringValue string `json:"StringValue,omitempty"`
}

func newWireServer() *wireServer {
	return &wireServer{
		topics: make(map[string]string),
		queues: make(map[string]*wireQueue),
		subs:   make(map[string]wireSub),
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (w *wireServer) handleSNS(rw http.ResponseWriter, req *http.Request) {
	rawBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	vals, err := url.ParseQuery(string(rawBody))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	action := vals.Get("Action")
	rw.Header().Set("Content-Type", "text/xml")
	switch action {
	case "CreateTopic":
		name := vals.Get("Name")
		w.mu.Lock()
		topicARN, ok := w.topics[name]
		if !ok {
			topicARN = "arn:aws:sns:us-east-1:123456789012:" + name
			w.topics[name] = topicARN
		}
		w.mu.Unlock()
		_, _ = fmt.Fprintf(rw,
			`<CreateTopicResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><CreateTopicResult><TopicArn>%s</TopicArn></CreateTopicResult><ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></CreateTopicResponse>`,
			topicARN)

	case "Subscribe":
		topicARN := vals.Get("TopicArn")
		endpoint := vals.Get("Endpoint")
		rawDelivery := false
		for i := 1; ; i++ {
			k := vals.Get(fmt.Sprintf("Attributes.entry.%d.key", i))
			if k == "" {
				break
			}
			if k == "RawMessageDelivery" && vals.Get(fmt.Sprintf("Attributes.entry.%d.value", i)) == "true" {
				rawDelivery = true
			}
		}
		w.mu.Lock()
		w.subSeq++
		subARN := fmt.Sprintf("%s:wire-sub-%d", topicARN, w.subSeq)
		w.subs[subARN] = wireSub{
			subARN:             subARN,
			topicARN:           topicARN,
			queueARN:           endpoint,
			rawMessageDelivery: rawDelivery,
		}
		w.mu.Unlock()
		_, _ = fmt.Fprintf(rw,
			`<SubscribeResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><SubscribeResult><SubscriptionArn>%s</SubscriptionArn></SubscribeResult><ResponseMetadata><RequestId>req-2</RequestId></ResponseMetadata></SubscribeResponse>`,
			subARN)

	case "Publish":
		topicARN := vals.Get("TopicArn")
		message := vals.Get("Message")
		attrs := make(map[string]wireSQSMessageAttr)
		for i := 1; ; i++ {
			name := vals.Get(fmt.Sprintf("MessageAttributes.entry.%d.Name", i))
			if name == "" {
				break
			}
			attrs[name] = wireSQSMessageAttr{
				DataType:    vals.Get(fmt.Sprintf("MessageAttributes.entry.%d.Value.DataType", i)),
				StringValue: vals.Get(fmt.Sprintf("MessageAttributes.entry.%d.Value.StringValue", i)),
			}
		}

		w.mu.Lock()
		w.msgSeq++
		msgID := fmt.Sprintf("wire-msg-%d", w.msgSeq)
		var targets []wireSub
		for _, sub := range w.subs {
			if sub.topicARN == topicARN {
				targets = append(targets, sub)
			}
		}
		queuesByARN := make(map[string]*wireQueue, len(w.queues))
		for _, q := range w.queues {
			queuesByARN[q.arn] = q
		}
		w.mu.Unlock()

		for _, sub := range targets {
			q := queuesByARN[sub.queueARN]
			if q == nil {
				continue
			}
			w.mu.Lock()
			w.receiptSeq++
			receipt := fmt.Sprintf("wire-receipt-%d", w.receiptSeq)
			w.mu.Unlock()

			if sub.rawMessageDelivery {
				q.messages <- wireSQSMessage{
					MessageID:         msgID,
					ReceiptHandle:     receipt,
					Body:              message,
					MD5OfBody:         md5Hex(message),
					MessageAttributes: attrs,
				}
			} else {
				envAttrs := make(map[string]snsEnvelopeAttr, len(attrs))
				for k, v := range attrs {
					envAttrs[k] = snsEnvelopeAttr{Type: v.DataType, Value: v.StringValue}
				}
				env := snsNotificationEnvelope{
					Type:              "Notification",
					MessageID:         msgID,
					TopicARN:          topicARN,
					Message:           aws.String(message),
					MessageAttributes: envAttrs,
				}
				envBytes, _ := json.Marshal(env)
				envStr := string(envBytes)
				q.messages <- wireSQSMessage{
					MessageID:     msgID,
					ReceiptHandle: receipt,
					Body:          envStr,
					MD5OfBody:     md5Hex(envStr),
				}
			}
		}
		_, _ = fmt.Fprintf(rw,
			`<PublishResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><PublishResult><MessageId>%s</MessageId></PublishResult><ResponseMetadata><RequestId>req-3</RequestId></ResponseMetadata></PublishResponse>`,
			msgID)

	case "Unsubscribe":
		subARN := vals.Get("SubscriptionArn")
		w.mu.Lock()
		w.unsubscribed = append(w.unsubscribed, subARN)
		delete(w.subs, subARN)
		w.mu.Unlock()
		_, _ = io.WriteString(rw,
			`<UnsubscribeResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/"><ResponseMetadata><RequestId>req-4</RequestId></ResponseMetadata></UnsubscribeResponse>`)

	default:
		http.Error(rw, "unsupported SNS action: "+action, http.StatusBadRequest)
	}
}

func (w *wireServer) handleSQS(baseURL string, rw http.ResponseWriter, req *http.Request) {
	rawBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	target := req.Header.Get("X-Amz-Target")
	rw.Header().Set("Content-Type", "application/x-amz-json-1.0")

	switch target {
	case "AmazonSQS.CreateQueue":
		var in struct {
			QueueName string `json:"QueueName"`
		}
		if err := json.Unmarshal(rawBody, &in); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		qURL := baseURL + "/queues/" + in.QueueName
		qARN := "arn:aws:sqs:us-east-1:123456789012:" + in.QueueName
		w.mu.Lock()
		w.queues[qURL] = &wireQueue{
			url:      qURL,
			arn:      qARN,
			messages: make(chan wireSQSMessage, 32),
		}
		w.mu.Unlock()
		resp, _ := json.Marshal(map[string]string{"QueueUrl": qURL})
		_, _ = rw.Write(resp)

	case "AmazonSQS.GetQueueAttributes":
		var in struct {
			QueueURL string `json:"QueueUrl"`
		}
		if err := json.Unmarshal(rawBody, &in); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		w.mu.Lock()
		q := w.queues[in.QueueURL]
		w.mu.Unlock()
		if q == nil {
			rw.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(rw, `{"__type":"QueueDoesNotExist","message":"missing"}`)
			return
		}
		resp, _ := json.Marshal(map[string]any{
			"Attributes": map[string]string{"QueueArn": q.arn},
		})
		_, _ = rw.Write(resp)

	case "AmazonSQS.SetQueueAttributes":
		var in struct {
			QueueURL   string            `json:"QueueUrl"`
			Attributes map[string]string `json:"Attributes"`
		}
		if err := json.Unmarshal(rawBody, &in); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		w.mu.Lock()
		if q := w.queues[in.QueueURL]; q != nil {
			q.policy = in.Attributes["Policy"]
		}
		w.mu.Unlock()
		_, _ = io.WriteString(rw, `{}`)

	case "AmazonSQS.ReceiveMessage":
		var in struct {
			QueueURL string `json:"QueueUrl"`
		}
		if err := json.Unmarshal(rawBody, &in); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		w.mu.Lock()
		q := w.queues[in.QueueURL]
		w.mu.Unlock()
		if q == nil {
			rw.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(rw, `{"__type":"QueueDoesNotExist","message":"missing"}`)
			return
		}
		select {
		case msg := <-q.messages:
			resp, _ := json.Marshal(map[string]any{"Messages": []wireSQSMessage{msg}})
			_, _ = rw.Write(resp)
		case <-req.Context().Done():
			return
		case <-time.After(25 * time.Millisecond):
			_, _ = io.WriteString(rw, `{"Messages":[]}`)
		}

	case "AmazonSQS.DeleteMessage":
		_, _ = io.WriteString(rw, `{}`)

	case "AmazonSQS.DeleteQueue":
		var in struct {
			QueueURL string `json:"QueueUrl"`
		}
		if err := json.Unmarshal(rawBody, &in); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		w.mu.Lock()
		w.deletedQs = append(w.deletedQs, in.QueueURL)
		delete(w.queues, in.QueueURL)
		w.mu.Unlock()
		_, _ = io.WriteString(rw, `{}`)

	default:
		http.Error(rw, "unsupported SQS target: "+target, http.StatusBadRequest)
	}
}

func TestClient_WireProtocolHTTPTest(t *testing.T) {
	srvState := newWireServer()
	var snsSrv, sqsSrv *httptest.Server
	snsSrv = httptest.NewServer(http.HandlerFunc(srvState.handleSNS))
	defer snsSrv.Close()
	sqsSrv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		srvState.handleSQS(sqsSrv.URL, rw, req)
	}))
	defer sqsSrv.Close()

	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}

	t.Run("auto_managed_wire_roundtrip", func(t *testing.T) {
		c, err := NewClient(
			awsCfg,
			WithSNSOptions(func(o *sns.Options) { o.BaseEndpoint = aws.String(snsSrv.URL) }),
			WithSQSOptions(func(o *sqs.Options) { o.BaseEndpoint = aws.String(sqsSrv.URL) }),
			WithWaitTimeSeconds(1),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		ctx := t.Context()
		ch, err := c.Subscribe(ctx, "wire-events")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		utf8Msg := []byte(`{"o":"node-a","k":["item:1"]}`)
		if err := c.Publish(ctx, "wire-events", utf8Msg); err != nil {
			t.Fatalf("Publish(utf8) error = %v", err)
		}
		select {
		case got := <-ch:
			if !bytes.Equal(got, utf8Msg) {
				t.Errorf("got %q, want %q", got, utf8Msg)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for UTF-8 wire message")
		}

		binMsg := []byte{0xff, 0xfe, 0x00, 0x01, 'B'}
		if err := c.Publish(ctx, "wire-events", binMsg); err != nil {
			t.Fatalf("Publish(binary) error = %v", err)
		}
		select {
		case got := <-ch:
			if !bytes.Equal(got, binMsg) {
				t.Errorf("got %x, want %x", got, binMsg)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for binary wire message")
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		srvState.mu.Lock()
		unsubCount := len(srvState.unsubscribed)
		delCount := len(srvState.deletedQs)
		srvState.mu.Unlock()
		if unsubCount != 1 || delCount != 1 {
			t.Errorf("expected wire cleanup (unsub=%d, delQ=%d), want (1, 1)", unsubCount, delCount)
		}
	})

	t.Run("pre_provisioned_envelope_wire_roundtrip", func(t *testing.T) {
		preTopicARN := "arn:aws:sns:us-east-1:123456789012:pre-wire-topic"
		preQueueURL := sqsSrv.URL + "/queues/pre-wire-q"
		preQueueARN := "arn:aws:sqs:us-east-1:123456789012:pre-wire-q"
		srvState.mu.Lock()
		srvState.queues[preQueueURL] = &wireQueue{
			url:      preQueueURL,
			arn:      preQueueARN,
			messages: make(chan wireSQSMessage, 16),
		}
		srvState.subs[preTopicARN+":pre"] = wireSub{
			subARN:             preTopicARN + ":pre",
			topicARN:           preTopicARN,
			queueARN:           preQueueARN,
			rawMessageDelivery: false,
		}
		srvState.mu.Unlock()

		c, err := NewClient(
			awsCfg,
			WithSNSOptions(func(o *sns.Options) { o.BaseEndpoint = aws.String(snsSrv.URL) }),
			WithSQSOptions(func(o *sqs.Options) { o.BaseEndpoint = aws.String(sqsSrv.URL) }),
			WithQueueURL(preQueueURL),
			WithTopicARN(preTopicARN),
			WithWaitTimeSeconds(1),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		ctx := t.Context()
		ch, err := c.Subscribe(ctx, "pre-wire-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		want := []byte(`{"o":"node-b","k":["k9"]}`)
		if err := c.Publish(ctx, "pre-wire-topic", want); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		select {
		case got := <-ch:
			if !bytes.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for envelope wire message")
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	assertNoGoroutineLeaks(t)
}

func TestClient_ConcurrentResubscribeExpiredCtxNoLeak(t *testing.T) {
	srvState := newWireServer()
	var onSubscribe func()
	var onUnsubscribe func()
	var hookMu sync.Mutex
	var unsubErrors atomic.Int32
	var delQErrors atomic.Int32

	var snsSrv, sqsSrv *httptest.Server
	snsSrv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rawBody, _ := io.ReadAll(req.Body)
		vals, _ := url.ParseQuery(string(rawBody))
		switch vals.Get("Action") {
		case "Subscribe":
			hookMu.Lock()
			cb := onSubscribe
			hookMu.Unlock()
			if cb != nil {
				cb()
			}
		case "Unsubscribe":
			hookMu.Lock()
			cb := onUnsubscribe
			hookMu.Unlock()
			if cb != nil {
				cb()
			}
			if req.Context().Err() != nil {
				unsubErrors.Add(1)
				http.Error(rw, "context canceled", http.StatusRequestTimeout)
				return
			}
		}
		req.Body = io.NopCloser(strings.NewReader(string(rawBody)))
		srvState.handleSNS(rw, req)
	}))
	defer snsSrv.Close()

	sqsSrv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Amz-Target") == "AmazonSQS.DeleteQueue" && req.Context().Err() != nil {
			delQErrors.Add(1)
			http.Error(rw, "context canceled", http.StatusRequestTimeout)
			return
		}
		srvState.handleSQS(sqsSrv.URL, rw, req)
	}))
	defer sqsSrv.Close()

	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	c, err := NewClient(
		awsCfg,
		WithSNSOptions(func(o *sns.Options) { o.BaseEndpoint = aws.String(snsSrv.URL) }),
		WithSQSOptions(func(o *sqs.Options) { o.BaseEndpoint = aws.String(sqsSrv.URL) }),
		WithWaitTimeSeconds(1),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// Case 1: existing != nil replacement cleanup (aws.go:218) where caller ctx
	// cancels right as Unsubscribe runs. Cleanup must still finish via cleanupCtx.
	if _, err := c.Subscribe(t.Context(), "hot-topic"); err != nil {
		_ = c.Close()
		t.Fatalf("initial Subscribe() error = %v", err)
	}
	ctx1, cancel1 := context.WithCancel(t.Context())
	hookMu.Lock()
	onUnsubscribe = func() {
		cancel1()
		time.Sleep(15 * time.Millisecond)
	}
	hookMu.Unlock()
	_, _ = c.Subscribe(ctx1, "hot-topic")

	// Case 2: concurrent replacement cleanup (aws.go:337, prev != nil) where
	// two Subscribes pass line 213 simultaneously and ctx2 cancels right as
	// replacement stopSubscription runs Unsubscribe.
	sub1Entered := make(chan struct{})
	secondSubDone := make(chan struct{})
	var subCount atomic.Int32
	ctx2, cancel2 := context.WithCancel(t.Context())

	hookMu.Lock()
	onSubscribe = func() {
		idx := subCount.Add(1)
		if idx == 1 {
			close(sub1Entered)
			// Block goroutine 1 until goroutine 2 has finished Subscribe and
			// registered c.subscriptions["race-topic"], so goroutine 1 hits prev != nil.
			<-secondSubDone
		}
	}
	onUnsubscribe = func() {
		cancel2()
		time.Sleep(15 * time.Millisecond)
	}
	hookMu.Unlock()

	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := c.Subscribe(ctx2, "race-topic")
		if err != nil {
			t.Errorf("concurrent Subscribe(ctx2) error = %v", err)
		}
	})
	<-sub1Entered
	if _, err := c.Subscribe(t.Context(), "race-topic"); err != nil {
		close(secondSubDone)
		wg.Wait()
		_ = c.Close()
		t.Fatalf("concurrent Subscribe(bg) error = %v", err)
	}
	close(secondSubDone)
	wg.Wait()

	hookMu.Lock()
	onSubscribe = nil
	onUnsubscribe = nil
	hookMu.Unlock()
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	srvState.mu.Lock()
	remainingQueues := len(srvState.queues)
	remainingSubs := len(srvState.subs)
	srvState.mu.Unlock()
	if remainingQueues != 0 || remainingSubs != 0 || unsubErrors.Load() != 0 || delQErrors.Load() != 0 {
		t.Fatalf("leaked AWS resources on replacement Subscribe: remainingQueues=%d remainingSubs=%d unsubErrors=%d delQErrors=%d",
			remainingQueues, remainingSubs, unsubErrors.Load(), delQErrors.Load())
	}

	assertNoGoroutineLeaks(t)
}

type batchSQSClient struct {
	*fakeSQSState
	batchOnce       atomic.Bool
	batchMsgs       []sqstypes.Message
	failDeleteFirst atomic.Bool
}

func (b *batchSQSClient) ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if b.batchOnce.CompareAndSwap(true, false) {
		return &sqs.ReceiveMessageOutput{Messages: b.batchMsgs}, nil
	}
	return b.fakeSQSState.ReceiveMessage(ctx, params, optFns...)
}

func (b *batchSQSClient) DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	if b.failDeleteFirst.CompareAndSwap(true, false) {
		return nil, errors.New("transient delete failure")
	}
	return b.fakeSQSState.DeleteMessage(ctx, params, optFns...)
}

func TestClient_SlowConsumerMultiMessageBatchAndDeleteFailure(t *testing.T) {
	ctx := t.Context()
	baseSQS := newFakeSQSState()
	snsState := newFakeSNSState(baseSQS)
	sqsWrap := &batchSQSClient{
		fakeSQSState: baseSQS,
		batchMsgs: []sqstypes.Message{
			{MessageId: aws.String("m0"), ReceiptHandle: aws.String("r0"), Body: aws.String("msg-0")},
			{MessageId: aws.String("m1"), ReceiptHandle: aws.String("r1"), Body: aws.String("msg-1")},
			{MessageId: aws.String("m2"), ReceiptHandle: aws.String("r2"), Body: aws.String("msg-2")},
			{MessageId: aws.String("m3"), ReceiptHandle: aws.String("r3"), Body: aws.String("msg-3")},
		},
	}
	sqsWrap.batchOnce.Store(true)
	sqsWrap.failDeleteFirst.Store(true)

	cfg := defaultConfig()
	WithChannelBuffer(1)(&cfg)
	WithMaxNumberOfMessages(4)(&cfg)

	c, err := newClientWithBackends(snsState, sqsWrap, cfg)
	if err != nil {
		t.Fatalf("newClientWithBackends() error = %v", err)
	}

	ch, err := c.Subscribe(ctx, "batch-topic")
	if err != nil {
		_ = c.Close()
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Read first two messages: first DeleteMessage fails transiently, loop must
	// continue and deliver msg-1 and saturate buffer with msg-2 while blocking on msg-3.
	for i := range 2 {
		select {
		case got := <-ch:
			want := fmt.Sprintf("msg-%d", i)
			if string(got) != want {
				_ = c.Close()
				t.Fatalf("got %q, want %q", string(got), want)
			}
		case <-time.After(2 * time.Second):
			_ = c.Close()
			t.Fatalf("timed out waiting for msg-%d", i)
		}
	}

	bufDeadline := time.Now().Add(2 * time.Second)
	for len(ch) == 0 && time.Now().Before(bufDeadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Unsubscribe must unblock case <-ctx.Done() cleanly while blocked on ch <- payload.
	unsubDone := make(chan error, 1)
	go func() {
		unsubDone <- c.Unsubscribe(ctx, "batch-topic")
	}()
	select {
	case err := <-unsubDone:
		if err != nil {
			t.Fatalf("Unsubscribe() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unsubscribe() deadlocked on slow consumer mid-batch")
	}

	for range ch {
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	assertNoGoroutineLeaks(t)
}

type barrierSNSClient struct {
	*fakeSNSState
	subEntered    chan struct{}
	subRelease    chan struct{}
	subEnteredOne sync.Once
	emptyCreate   bool
}

func (b *barrierSNSClient) CreateTopic(ctx context.Context, params *sns.CreateTopicInput, optFns ...func(*sns.Options)) (*sns.CreateTopicOutput, error) {
	if b.emptyCreate {
		return &sns.CreateTopicOutput{TopicArn: aws.String("")}, nil
	}
	return b.fakeSNSState.CreateTopic(ctx, params, optFns...)
}

func (b *barrierSNSClient) Subscribe(ctx context.Context, params *sns.SubscribeInput, optFns ...func(*sns.Options)) (*sns.SubscribeOutput, error) {
	if b.subEntered != nil {
		b.subEnteredOne.Do(func() { close(b.subEntered) })
		<-b.subRelease
	}
	return b.fakeSNSState.Subscribe(ctx, params, optFns...)
}

func TestClient_ConcurrentLifecycleAndGoroutineLeaks(t *testing.T) {
	ctx := t.Context()

	// 1. Concurrent Subscribe/Publish/Unsubscribe/Close under -race + goroutineleak.
	t.Run("ConcurrentStress", func(t *testing.T) {
		sqsState := newFakeSQSState()
		snsState := newFakeSNSState(sqsState)
		c, err := newClientWithBackends(snsState, sqsState, defaultConfig())
		if err != nil {
			t.Fatalf("newClientWithBackends() error = %v", err)
		}

		topics := []string{"t-0", "t-1", "t-2"}
		var wg sync.WaitGroup
		var drainers sync.WaitGroup
		for g := range 6 {
			wg.Go(func() {
				for i := range 8 {
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
						_ = c.Publish(ctx, topic, []byte("payload"))
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

		sqsState.mu.Lock()
		remQueues := len(sqsState.queuesByURL)
		sqsState.mu.Unlock()
		snsState.mu.Lock()
		var remSubs int
		for _, subs := range snsState.subsByTopic {
			remSubs += len(subs)
		}
		snsState.mu.Unlock()
		if remQueues != 0 || remSubs != 0 {
			t.Errorf("leaked resources after concurrent Close: queues=%d subs=%d", remQueues, remSubs)
		}
		assertNoGoroutineLeaks(t)
	})

	// 2. Close racing with in-flight Subscribe post-setup triggers rollback.
	t.Run("CloseDuringSubscribePostSetup", func(t *testing.T) {
		sqsState := newFakeSQSState()
		baseSNS := newFakeSNSState(sqsState)
		barrierSNS := &barrierSNSClient{
			fakeSNSState: baseSNS,
			subEntered:   make(chan struct{}),
			subRelease:   make(chan struct{}),
		}
		c, err := newClientWithBackends(barrierSNS, sqsState, defaultConfig())
		if err != nil {
			t.Fatalf("newClientWithBackends() error = %v", err)
		}

		subErrCh := make(chan error, 1)
		go func() {
			_, subErr := c.Subscribe(ctx, "race-close-topic")
			subErrCh <- subErr
		}()

		<-barrierSNS.subEntered
		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		close(barrierSNS.subRelease)

		select {
		case subErr := <-subErrCh:
			if !errors.Is(subErr, ErrClosed) {
				t.Fatalf("Subscribe() error = %v, want ErrClosed", subErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for Subscribe after Close")
		}

		sqsState.mu.Lock()
		remQueues := len(sqsState.queuesByURL)
		delQueues := len(sqsState.deletedQueues)
		sqsState.mu.Unlock()
		baseSNS.mu.Lock()
		unsubCalls := baseSNS.unsubscribeCalls
		baseSNS.mu.Unlock()
		if remQueues != 0 || delQueues != 1 || unsubCalls != 1 {
			t.Errorf("expected post-setup rollback cleanup: remQueues=%d delQueues=%d unsubCalls=%d", remQueues, delQueues, unsubCalls)
		}
		assertNoGoroutineLeaks(t)
	})

	// 3. CreateTopic error, empty TopicArn, and sns.Publish error wrapping.
	t.Run("CreateTopicAndPublishErrors", func(t *testing.T) {
		sqsState := newFakeSQSState()
		baseSNS := newFakeSNSState(sqsState)
		baseSNS.createErr = errors.New("create topic boom")
		cErr, err := newClientWithBackends(baseSNS, sqsState, defaultConfig())
		if err != nil {
			t.Fatalf("newClientWithBackends() error = %v", err)
		}
		if err := cErr.Publish(ctx, "bad-create", []byte("x")); err == nil || !strings.Contains(err.Error(), "create topic boom") {
			t.Errorf("Publish(CreateTopic error) = %v, want wrapped create topic boom", err)
		}
		_ = cErr.Close()

		emptySNS := &barrierSNSClient{
			fakeSNSState: newFakeSNSState(sqsState),
			emptyCreate:  true,
		}
		cEmpty, err := newClientWithBackends(emptySNS, sqsState, defaultConfig())
		if err != nil {
			t.Fatalf("newClientWithBackends(empty) error = %v", err)
		}
		if err := cEmpty.Publish(ctx, "empty-arn", []byte("x")); err == nil || !strings.Contains(err.Error(), "empty TopicArn") {
			t.Errorf("Publish(empty TopicArn) = %v, want empty TopicArn error", err)
		}
		_ = cEmpty.Close()

		pubErrSNS := newFakeSNSState(sqsState)
		pubErrSNS.publishErr = errors.New("sns publish boom")
		cPubErr, err := newClientWithBackends(pubErrSNS, sqsState, defaultConfig())
		if err != nil {
			t.Fatalf("newClientWithBackends(pubErr) error = %v", err)
		}
		if err := cPubErr.Publish(ctx, "pub-fail", []byte("x")); err == nil || !strings.Contains(err.Error(), "sns publish boom") {
			t.Errorf("Publish(sns.Publish error) = %v, want wrapped sns publish boom", err)
		}
		_ = cPubErr.Close()
		assertNoGoroutineLeaks(t)
	})
}

func assertNoGoroutineLeaks(t *testing.T) {
	t.Helper()
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/aws") {
			t.Errorf("detected leaked goroutine in pubsub/aws:\n%s", buf.String())
		}
	}
}

// Compile-time structural check that Client satisfies memorystore.PubSubClient
// and that *sns.Client / *sqs.Client satisfy internal backend interfaces.
type pubSubContract interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}

var (
	_ pubSubContract = (*Client)(nil)
	_ snsClient      = (*sns.Client)(nil)
	_ sqsClient      = (*sqs.Client)(nil)
)
