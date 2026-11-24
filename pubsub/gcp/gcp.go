package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"uuid"

	pubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	defaultChannelBuffer     = 100
	defaultSubscriptionTTL   = 24 * time.Hour
	defaultCleanupRPCsTimout = 10 * time.Second

	subIDPrefix          = "memorystore-sub-"
	minSubscriptionIDLen = 3
	maxSubscriptionIDLen = 255
	uuidStringLen        = 36
	maxSubSlugLen        = maxSubscriptionIDLen - len(subIDPrefix) - 1 - uuidStringLen // 202
)

var discardLogger = slog.New(slog.DiscardHandler)

// ErrInvalidTopic is returned when an empty topic name is passed.
var ErrInvalidTopic = errors.New("invalid topic")

// ErrClosed is returned when operations are attempted on a closed Client.
var ErrClosed = errors.New("gcp pubsub client is closed")

// ErrInvalidSubscriptionID is returned when a caller-supplied subscription ID
// violates Google Cloud Pub/Sub naming rules.
var ErrInvalidSubscriptionID = errors.New("subscription ID must be 3..255 valid GCP characters starting with an ASCII letter and not prefixed with goog")

// ErrMissingSubscriptionID is returned when auto-management is disabled and no
// subscription ID is configured.
var ErrMissingSubscriptionID = errors.New("subscription ID is required when auto-management is disabled")

// Client implements memorystore.PubSubClient using Google Cloud Pub/Sub (v2).
type Client struct {
	client        *pubsub.Client
	projectID     string
	cfg           config
	mu            sync.RWMutex
	subscriptions map[string]*gcpSubscription
	publishers    map[string]*pubsub.Publisher
	wg            sync.WaitGroup
	closed        bool
}

type gcpSubscription struct {
	subName     string
	autoManaged bool
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// NewClient creates a new Google Cloud Pub/Sub client for the given project ID.
func NewClient(ctx context.Context, projectID string, opts ...option.ClientOption) (*Client, error) {
	return NewClientWithOptions(ctx, projectID, WithClientOptions(opts...))
}

// NewClientWithOptions creates a new Google Cloud Pub/Sub client for the given
// project ID configured with functional Option values.
func NewClientWithOptions(ctx context.Context, projectID string, opts ...Option) (*Client, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	client, err := pubsub.NewClient(ctx, projectID, cfg.clientOptions...)
	if err != nil {
		return nil, err
	}

	return &Client{
		client:        client,
		projectID:     projectID,
		cfg:           cfg,
		subscriptions: make(map[string]*gcpSubscription),
		publishers:    make(map[string]*pubsub.Publisher),
	}, nil
}

func (c *Client) log() *slog.Logger {
	if c.cfg.logger != nil {
		return c.cfg.logger
	}
	return discardLogger
}

func (c *Client) channelBuffer() int {
	if c.cfg.channelBuffer > 0 {
		return c.cfg.channelBuffer
	}
	return defaultChannelBuffer
}

func (c *Client) topicPath(topic string) string {
	return fmt.Sprintf("projects/%s/topics/%s", c.projectID, topic)
}

func (c *Client) subscriptionPath(subID string) string {
	return fmt.Sprintf("projects/%s/subscriptions/%s", c.projectID, subID)
}

func (c *Client) getOrCreatePublisher(topic string) (*pubsub.Publisher, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, ErrClosed
	}
	pub, ok := c.publishers[topic]
	c.mu.RUnlock()
	if ok {
		return pub, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if pub, ok = c.publishers[topic]; ok {
		return pub, nil
	}
	pub = c.client.Publisher(c.topicPath(topic))
	c.publishers[topic] = pub
	return pub, nil
}

func isValidGCPSubIDChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_' || r == '.' || r == '~' || r == '+' || r == '%'
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func validateSubscriptionID(id string) error {
	if len(id) < minSubscriptionIDLen || len(id) > maxSubscriptionIDLen {
		return ErrInvalidSubscriptionID
	}
	if !isASCIILetter(id[0]) || strings.HasPrefix(strings.ToLower(id), "goog") {
		return ErrInvalidSubscriptionID
	}
	for _, r := range id {
		if !isValidGCPSubIDChar(r) {
			return ErrInvalidSubscriptionID
		}
	}
	return nil
}

func (c *Client) resolveSubscriptionID(topic string) (string, bool, error) {
	var raw string
	var customProvided bool
	switch {
	case c.cfg.subscriptionIDFunc != nil:
		raw = c.cfg.subscriptionIDFunc(topic)
		customProvided = true
	case c.cfg.subscriptionID != "":
		raw = c.cfg.subscriptionID
		customProvided = true
	default:
		return "", false, nil
	}

	if err := validateSubscriptionID(raw); err != nil {
		return "", false, err
	}
	return raw, customProvided, nil
}

func sanitizeSubscriptionID(topicName string) string {
	var b strings.Builder
	b.Grow(min(len(topicName), maxSubSlugLen))
	for _, r := range topicName {
		if b.Len() >= maxSubSlugLen {
			break
		}
		if isValidGCPSubIDChar(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	id := uuid.New().String()
	if slug == "" {
		return subIDPrefix + id
	}
	subID := subIDPrefix + slug + "-" + id
	if len(subID) > maxSubscriptionIDLen {
		subID = subID[:maxSubscriptionIDLen]
	}
	return subID
}

func (c *Client) stopSubscription(ctx context.Context, entry *gcpSubscription) error {
	entry.cancel()
	entry.wg.Wait()

	if !entry.autoManaged {
		return nil
	}

	if err := c.client.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{
		Subscription: entry.subName,
	}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("failed to delete subscription: %w", err)
	}
	return nil
}

func (c *Client) deleteSubscriptionBestEffort(subName string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupRPCsTimout)
	defer cancel()
	if err := c.client.SubscriptionAdminClient.DeleteSubscription(cleanupCtx, &pubsubpb.DeleteSubscriptionRequest{
		Subscription: subName,
	}); err != nil && status.Code(err) != codes.NotFound {
		c.log().Warn("gcp pubsub failed to delete temporary subscription during rollback", "subscription", subName, "error", err)
	}
}

// Subscribe ensures the topic exists (when auto-managed), attaches to a
// configured or ephemeral 24-hour subscription, and streams received message
// payloads to the returned channel.
func (c *Client) Subscribe(ctx context.Context, topicName string) (<-chan []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topicName == "" {
		return nil, ErrInvalidTopic
	}

	autoManaged := c.cfg.shouldAutoManage()
	subID, customSub, err := c.resolveSubscriptionID(topicName)
	if err != nil {
		return nil, err
	}
	if !autoManaged && !customSub {
		return nil, ErrMissingSubscriptionID
	}
	if !customSub {
		subID = sanitizeSubscriptionID(topicName)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.wg.Add(1)
	defer c.wg.Done()
	existing := c.subscriptions[topicName]
	delete(c.subscriptions, topicName)
	c.mu.Unlock()

	if existing != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupRPCsTimout)
		if stopErr := c.stopSubscription(cleanupCtx, existing); stopErr != nil {
			c.log().Warn("gcp pubsub failed to stop previous subscription on resubscribe", "topic", topicName, "subscription", existing.subName, "error", stopErr)
		}
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	topicNamePath := c.topicPath(topicName)
	subNamePath := c.subscriptionPath(subID)
	if autoManaged {
		_, err = c.client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{
			Topic: topicNamePath,
		})
		if err != nil {
			if status.Code(err) != codes.NotFound {
				return nil, fmt.Errorf("failed to check topic existence: %w", err)
			}
			_, err = c.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{
				Name: topicNamePath,
			})
			if err != nil && status.Code(err) != codes.AlreadyExists {
				return nil, fmt.Errorf("failed to create topic: %w", err)
			}
		}

		_, err = c.client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name:  subNamePath,
			Topic: topicNamePath,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{
				Ttl: durationpb.New(defaultSubscriptionTTL),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create subscription: %w", err)
		}
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if autoManaged {
			c.deleteSubscriptionBestEffort(subNamePath)
		}
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		if autoManaged {
			c.deleteSubscriptionBestEffort(subNamePath)
		}
		return nil, err
	}

	prev := c.subscriptions[topicName]
	ch := make(chan []byte, c.channelBuffer())
	recvCtx, cancel := context.WithCancel(context.Background())
	sub := c.client.Subscriber(subNamePath)

	entry := &gcpSubscription{
		subName:     subNamePath,
		autoManaged: autoManaged,
		cancel:      cancel,
	}

	entry.wg.Go(func() {
		defer close(ch)
		err := sub.Receive(recvCtx, func(ctx context.Context, msg *pubsub.Message) {
			select {
			case ch <- msg.Data:
				msg.Ack()
			case <-ctx.Done():
				c.log().Debug("gcp pubsub nacking message on context cancellation", "topic", topicName, "subscription", subNamePath)
				msg.Nack()
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			c.log().Error("gcp pubsub receive stopped with error", "topic", topicName, "subscription", subNamePath, "error", err)
		}
	})

	c.subscriptions[topicName] = entry
	c.mu.Unlock()

	if prev != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupRPCsTimout)
		if stopErr := c.stopSubscription(cleanupCtx, prev); stopErr != nil {
			c.log().Warn("gcp pubsub failed to stop superseded subscription", "topic", topicName, "subscription", prev.subName, "error", stopErr)
		}
		cancel()
	}

	return ch, nil
}

// Publish publishes a message payload to the specified topic and waits for
// confirmation from Google Cloud Pub/Sub.
func (c *Client) Publish(ctx context.Context, topicName string, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if topicName == "" {
		return ErrInvalidTopic
	}

	pub, err := c.getOrCreatePublisher(topicName)
	if err != nil {
		return err
	}

	res := pub.Publish(ctx, &pubsub.Message{
		Data: message,
	})
	_, err = res.Get(ctx)
	return err
}

// Unsubscribe stops receiving messages for topicName and deletes its temporary
// Google Cloud Pub/Sub subscription.
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
	c.wg.Add(1)
	defer c.wg.Done()
	delete(c.subscriptions, topicName)
	c.mu.Unlock()

	return c.stopSubscription(ctx, entry)
}

// Close stops all cached publishers, cancels and deletes temporary
// subscriptions, and closes the underlying Google Cloud Pub/Sub client.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	subs := c.subscriptions
	pubs := c.publishers
	c.subscriptions = nil
	c.publishers = nil
	c.mu.Unlock()

	for _, pub := range pubs {
		pub.Stop()
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupRPCsTimout)
	defer cancel()

	for _, entry := range subs {
		if err := c.stopSubscription(cleanupCtx, entry); err != nil {
			c.log().Warn("gcp pubsub failed to stop subscription during close", "subscription", entry.subName, "error", err)
		}
	}

	c.wg.Wait()

	return c.client.Close()
}
