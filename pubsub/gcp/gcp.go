package gcp

import (
	"context"
	"errors"
	"fmt"
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
)

// ErrInvalidTopic is returned when an empty topic name is passed.
var ErrInvalidTopic = errors.New("invalid topic")

// ErrClosed is returned when operations are attempted on a closed Client.
var ErrClosed = errors.New("gcp pubsub client is closed")

// Client implements memorystore.PubSubClient using Google Cloud Pub/Sub (v2).
type Client struct {
	client        *pubsub.Client
	projectID     string
	mu            sync.RWMutex
	subscriptions map[string]*gcpSubscription
	publishers    map[string]*pubsub.Publisher
	closed        bool
}

type gcpSubscription struct {
	subName string
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewClient creates a new Google Cloud Pub/Sub client for the given project ID.
func NewClient(ctx context.Context, projectID string, opts ...option.ClientOption) (*Client, error) {
	client, err := pubsub.NewClient(ctx, projectID, opts...)
	if err != nil {
		return nil, err
	}

	return &Client{
		client:        client,
		projectID:     projectID,
		subscriptions: make(map[string]*gcpSubscription),
		publishers:    make(map[string]*pubsub.Publisher),
	}, nil
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

// Subscribe ensures the topic exists, creates a temporary session subscription
// with a 24-hour expiration policy, and streams received message payloads to the
// returned channel.
func (c *Client) Subscribe(ctx context.Context, topicName string) (<-chan []byte, error) {
	if topicName == "" {
		return nil, ErrInvalidTopic
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, ErrClosed
	}

	topicNamePath := c.topicPath(topicName)
	_, err := c.client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{
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

	subID := fmt.Sprintf("memorystore-sub-%s-%s", topicName, uuid.New().String())
	subNamePath := c.subscriptionPath(subID)
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

	ch := make(chan []byte, defaultChannelBuffer)
	recvCtx, cancel := context.WithCancel(context.Background())
	sub := c.client.Subscriber(subNamePath)

	entry := &gcpSubscription{
		subName: subNamePath,
		cancel:  cancel,
	}

	entry.wg.Go(func() {
		defer close(ch)
		err := sub.Receive(recvCtx, func(ctx context.Context, msg *pubsub.Message) {
			select {
			case ch <- msg.Data:
				msg.Ack()
			case <-ctx.Done():
				msg.Nack()
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			_ = err
		}
	})

	c.subscriptions[topicName] = entry
	return ch, nil
}

// Publish publishes a message payload to the specified topic and waits for
// confirmation from Google Cloud Pub/Sub.
func (c *Client) Publish(ctx context.Context, topicName string, message []byte) error {
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

	entry.cancel()
	entry.wg.Wait()

	if err := c.client.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{
		Subscription: entry.subName,
	}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("failed to delete subscription: %w", err)
	}
	return nil
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
		entry.cancel()
		entry.wg.Wait()
		_ = c.client.SubscriptionAdminClient.DeleteSubscription(cleanupCtx, &pubsubpb.DeleteSubscriptionRequest{
			Subscription: entry.subName,
		})
	}

	return c.client.Close()
}
