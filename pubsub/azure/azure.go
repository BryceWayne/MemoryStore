package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"uuid"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
)

const (
	defaultCleanupTimeout = 10 * time.Second
	receiveRetryDelay     = 250 * time.Millisecond
)

// Common errors returned by the Azure Service Bus Pub/Sub client.
var (
	ErrInvalidTopic            = errors.New("invalid topic")
	ErrInvalidNamespace        = errors.New("fully qualified namespace is required")
	ErrInvalidCredential       = errors.New("token credential is required")
	ErrInvalidConnectionString = errors.New("connection string is required")
	ErrInvalidSubscriptionName = errors.New("subscription name must be 1..50 characters")
	ErrClosed                  = errors.New("azure service bus client is closed")
)

// Internal minimal interfaces around the Azure SDK to enable thorough unit
// testing without requiring a live Azure Service Bus namespace.
type sbDataClient interface {
	NewSender(topic string) (sbSender, error)
	NewReceiverForSubscription(topic, subscription string, opts *azservicebus.ReceiverOptions) (sbReceiver, error)
	Close(ctx context.Context) error
}

type sbAdminClient interface {
	EnsureTopic(ctx context.Context, topic string) error
	CreateSubscription(ctx context.Context, topic, subscription, autoDeleteOnIdle string) error
	DeleteSubscription(ctx context.Context, topic, subscription string) error
}

type sbSender interface {
	SendMessage(ctx context.Context, message *azservicebus.Message) error
	Close(ctx context.Context) error
}

type sbReceiver interface {
	ReceiveMessages(ctx context.Context, maxMessages int) ([]*azservicebus.ReceivedMessage, error)
	CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage) error
	AbandonMessage(ctx context.Context, message *azservicebus.ReceivedMessage) error
	Close(ctx context.Context) error
}

type realDataClient struct{ inner *azservicebus.Client }

func (r *realDataClient) NewSender(topic string) (sbSender, error) {
	s, err := r.inner.NewSender(topic, nil)
	if err != nil {
		return nil, err
	}
	return &realSender{inner: s}, nil
}

func (r *realDataClient) NewReceiverForSubscription(topic, subscription string, opts *azservicebus.ReceiverOptions) (sbReceiver, error) {
	recv, err := r.inner.NewReceiverForSubscription(topic, subscription, opts)
	if err != nil {
		return nil, err
	}
	return &realReceiver{inner: recv}, nil
}

func (r *realDataClient) Close(ctx context.Context) error {
	return r.inner.Close(ctx)
}

type realSender struct{ inner *azservicebus.Sender }

func (s *realSender) SendMessage(ctx context.Context, message *azservicebus.Message) error {
	return s.inner.SendMessage(ctx, message, nil)
}

func (s *realSender) Close(ctx context.Context) error {
	return s.inner.Close(ctx)
}

type realReceiver struct{ inner *azservicebus.Receiver }

func (r *realReceiver) ReceiveMessages(ctx context.Context, maxMessages int) ([]*azservicebus.ReceivedMessage, error) {
	return r.inner.ReceiveMessages(ctx, maxMessages, nil)
}

func (r *realReceiver) CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage) error {
	return r.inner.CompleteMessage(ctx, message, nil)
}

func (r *realReceiver) AbandonMessage(ctx context.Context, message *azservicebus.ReceivedMessage) error {
	return r.inner.AbandonMessage(ctx, message, nil)
}

func (r *realReceiver) Close(ctx context.Context) error {
	return r.inner.Close(ctx)
}

type realAdminClient struct{ inner *admin.Client }

func (a *realAdminClient) EnsureTopic(ctx context.Context, topic string) error {
	resp, err := a.inner.GetTopic(ctx, topic, nil)
	if err != nil {
		return fmt.Errorf("failed to check topic existence: %w", err)
	}
	if resp != nil {
		return nil
	}
	if _, err := a.inner.CreateTopic(ctx, topic, nil); err != nil && !isHTTPStatus(err, http.StatusConflict) {
		return fmt.Errorf("failed to create topic: %w", err)
	}
	return nil
}

func (a *realAdminClient) CreateSubscription(ctx context.Context, topic, subscription, autoDeleteOnIdle string) error {
	var props *admin.SubscriptionProperties
	if autoDeleteOnIdle != "" {
		idle := autoDeleteOnIdle
		props = &admin.SubscriptionProperties{
			AutoDeleteOnIdle: &idle,
		}
	}
	_, err := a.inner.CreateSubscription(ctx, topic, subscription, &admin.CreateSubscriptionOptions{
		Properties: props,
	})
	if err != nil && !isHTTPStatus(err, http.StatusConflict) {
		return fmt.Errorf("failed to create subscription: %w", err)
	}
	return nil
}

func (a *realAdminClient) DeleteSubscription(ctx context.Context, topic, subscription string) error {
	_, err := a.inner.DeleteSubscription(ctx, topic, subscription, nil)
	if err != nil && !isHTTPStatus(err, http.StatusNotFound) {
		return fmt.Errorf("failed to delete subscription: %w", err)
	}
	return nil
}

func isHTTPStatus(err error, code int) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == code
}

// Client implements memorystore.PubSubClient using Azure Service Bus Topics
// and Subscriptions.
type Client struct {
	data          sbDataClient
	admin         sbAdminClient
	cfg           config
	mu            sync.RWMutex
	subscriptions map[string]*azureSubscription
	senders       map[string]sbSender
	closed        bool
}

type azureSubscription struct {
	topic       string
	subName     string
	autoManaged bool
	receiver    sbReceiver
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// NewClient creates a new Azure Service Bus Pub/Sub client authenticated with
// an azcore.TokenCredential (such as azidentity.NewDefaultAzureCredential).
// The fullyQualifiedNamespace should look like "<name>.servicebus.windows.net".
func NewClient(fullyQualifiedNamespace string, credential azcore.TokenCredential, opts ...Option) (*Client, error) {
	if fullyQualifiedNamespace == "" {
		return nil, ErrInvalidNamespace
	}
	if credential == nil {
		return nil, ErrInvalidCredential
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	dataClient, err := azservicebus.NewClient(fullyQualifiedNamespace, credential, cfg.clientOptions)
	if err != nil {
		return nil, err
	}

	var adm sbAdminClient
	if cfg.shouldAutoManage() {
		ac, err := admin.NewClient(fullyQualifiedNamespace, credential, cfg.adminClientOptions)
		if err != nil {
			_ = dataClient.Close(context.Background())
			return nil, err
		}
		adm = &realAdminClient{inner: ac}
	}

	return newClientWithBackends(&realDataClient{inner: dataClient}, adm, cfg), nil
}

// NewClientFromConnectionString creates a new Azure Service Bus Pub/Sub client
// using a Shared Access Signature (SAS) connection string.
func NewClientFromConnectionString(connectionString string, opts ...Option) (*Client, error) {
	if connectionString == "" {
		return nil, ErrInvalidConnectionString
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	dataClient, err := azservicebus.NewClientFromConnectionString(connectionString, cfg.clientOptions)
	if err != nil {
		return nil, err
	}

	var adm sbAdminClient
	if cfg.shouldAutoManage() {
		ac, err := admin.NewClientFromConnectionString(connectionString, cfg.adminClientOptions)
		if err != nil {
			_ = dataClient.Close(context.Background())
			return nil, err
		}
		adm = &realAdminClient{inner: ac}
	}

	return newClientWithBackends(&realDataClient{inner: dataClient}, adm, cfg), nil
}

func newClientWithBackends(data sbDataClient, adm sbAdminClient, cfg config) *Client {
	return &Client{
		data:          data,
		admin:         adm,
		cfg:           cfg,
		subscriptions: make(map[string]*azureSubscription),
		senders:       make(map[string]sbSender),
	}
}

func (c *Client) resolveSubscriptionName(topic string) (string, error) {
	var name string
	switch {
	case c.cfg.subscriptionNameFn != nil:
		name = c.cfg.subscriptionNameFn(topic)
	case c.cfg.subscriptionName != "":
		name = c.cfg.subscriptionName
	default:
		// Compact 39-char unique name guaranteed to fit Azure's 50-char limit.
		name = "ms-" + uuid.New().String()
	}
	if name == "" || len(name) > maxSubscriptionNameLen {
		return "", fmt.Errorf("%w: got %q (%d chars)", ErrInvalidSubscriptionName, name, len(name))
	}
	return name, nil
}

func (c *Client) getOrCreateSender(topic string) (sbSender, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, ErrClosed
	}
	sender, ok := c.senders[topic]
	c.mu.RUnlock()
	if ok {
		return sender, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if sender, ok = c.senders[topic]; ok {
		return sender, nil
	}
	sender, err := c.data.NewSender(topic)
	if err != nil {
		return nil, err
	}
	c.senders[topic] = sender
	return sender, nil
}

// Subscribe subscribes to topicName and returns a channel delivering received
// message bodies. When auto-management is enabled (the default), Subscribe
// ensures the topic exists and creates a temporary subscription with
// AutoDeleteOnIdle.
func (c *Client) Subscribe(ctx context.Context, topicName string) (<-chan []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topicName == "" {
		return nil, ErrInvalidTopic
	}

	subName, err := c.resolveSubscriptionName(topicName)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	existing := c.subscriptions[topicName]
	delete(c.subscriptions, topicName)
	c.mu.Unlock()

	if existing != nil {
		_ = c.stopSubscription(ctx, existing)
	}

	autoManaged := c.cfg.shouldAutoManage() && c.admin != nil
	if autoManaged {
		if err := c.admin.EnsureTopic(ctx, topicName); err != nil {
			return nil, err
		}
		if err := c.admin.CreateSubscription(ctx, topicName, subName, c.cfg.autoDeleteOnIdle); err != nil {
			return nil, err
		}
	}

	receiver, err := c.data.NewReceiverForSubscription(topicName, subName, &azservicebus.ReceiverOptions{
		ReceiveMode: c.cfg.receiveMode,
	})
	if err != nil {
		if autoManaged {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			_ = c.admin.DeleteSubscription(cleanupCtx, topicName, subName)
			cancel()
		}
		return nil, fmt.Errorf("failed to create receiver: %w", err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
		_ = receiver.Close(cleanupCtx)
		if autoManaged {
			_ = c.admin.DeleteSubscription(cleanupCtx, topicName, subName)
		}
		cancel()
		return nil, ErrClosed
	}

	ch := make(chan []byte, c.cfg.channelBuffer)
	recvCtx, cancel := context.WithCancel(context.Background())
	entry := &azureSubscription{
		topic:       topicName,
		subName:     subName,
		autoManaged: autoManaged,
		receiver:    receiver,
		cancel:      cancel,
	}

	peekLock := c.cfg.receiveMode == azservicebus.ReceiveModePeekLock
	maxMsgs := c.cfg.maxMessages

	entry.wg.Go(func() {
		defer close(ch)
		for {
			if recvCtx.Err() != nil {
				return
			}
			msgs, err := receiver.ReceiveMessages(recvCtx, maxMsgs)
			if err != nil {
				if recvCtx.Err() != nil {
					return
				}
				timer := time.NewTimer(receiveRetryDelay)
				select {
				case <-timer.C:
				case <-recvCtx.Done():
					timer.Stop()
					return
				}
				continue
			}

			for _, msg := range msgs {
				select {
				case ch <- msg.Body:
					if peekLock {
						_ = receiver.CompleteMessage(recvCtx, msg)
					}
				case <-recvCtx.Done():
					if peekLock {
						abandonCtx, abandonCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
						_ = receiver.AbandonMessage(abandonCtx, msg)
						abandonCancel()
					}
					return
				}
			}
		}
	})

	c.subscriptions[topicName] = entry
	c.mu.Unlock()

	return ch, nil
}

// Publish sends message to topicName using a cached Service Bus sender.
func (c *Client) Publish(ctx context.Context, topicName string, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if topicName == "" {
		return ErrInvalidTopic
	}

	sender, err := c.getOrCreateSender(topicName)
	if err != nil {
		return err
	}

	return sender.SendMessage(ctx, &azservicebus.Message{
		Body: message,
	})
}

// Unsubscribe stops receiving messages for topicName, closes the receiver, and
// deletes the subscription if it was auto-managed.
func (c *Client) Unsubscribe(ctx context.Context, topicName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	entry, ok := c.subscriptions[topicName]
	if !ok {
		c.mu.Unlock()
		return nil
	}
	delete(c.subscriptions, topicName)
	c.mu.Unlock()

	return c.stopSubscription(ctx, entry)
}

func (c *Client) stopSubscription(ctx context.Context, entry *azureSubscription) error {
	entry.cancel()
	entry.wg.Wait()

	closeCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()
	_ = entry.receiver.Close(closeCtx)

	if entry.autoManaged && c.admin != nil {
		return c.admin.DeleteSubscription(ctx, entry.topic, entry.subName)
	}
	return nil
}

// Close stops all active subscriptions, deletes any auto-managed ephemeral
// subscriptions, closes cached senders, and closes the underlying Azure
// Service Bus client. Multiple calls are safe and idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	subs := c.subscriptions
	senders := c.senders
	c.subscriptions = nil
	c.senders = nil
	c.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()

	for _, entry := range subs {
		_ = c.stopSubscription(cleanupCtx, entry)
	}

	for _, sender := range senders {
		_ = sender.Close(cleanupCtx)
	}

	return c.data.Close(cleanupCtx)
}
