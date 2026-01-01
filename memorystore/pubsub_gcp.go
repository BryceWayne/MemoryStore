package memorystore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/google/uuid"
	"google.golang.org/api/option"
)

// GCPPubSub implements PubSubClient using Google Cloud PubSub
type GCPPubSub struct {
	client        *pubsub.Client
	mu            sync.RWMutex
	subscriptions map[string]*gcpSubscription // Map topic/pattern -> subscription info
	projectID     string
}

type gcpSubscription struct {
	subID  string
	sub    *pubsub.Subscription
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewGCPPubSub creates a new GCP PubSub client
func NewGCPPubSub(ctx context.Context, projectID string, opts ...option.ClientOption) (*GCPPubSub, error) {
	client, err := pubsub.NewClient(ctx, projectID, opts...)
	if err != nil {
		return nil, err
	}

	return &GCPPubSub{
		client:        client,
		subscriptions: make(map[string]*gcpSubscription),
		projectID:     projectID,
	}, nil
}

// Subscribe subscribes to a topic.
// Note: In GCP PubSub, we must create a Subscription to the Topic.
// Since this is a temporary/session-based subscription, we create a unique subscription ID
// and delete it when Unsubscribe is called or the client is closed.
func (g *GCPPubSub) Subscribe(topicName string) (<-chan []byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 1. Ensure Topic exists
	topic := g.client.Topic(topicName)
	exists, err := topic.Exists(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to check topic existence: %w", err)
	}
	if !exists {
		// Try to create topic if it doesn't exist?
		// Usually good for dev, might fail in strict prod. Let's try.
		topic, err = g.client.CreateTopic(context.Background(), topicName)
		if err != nil {
			return nil, fmt.Errorf("failed to create topic: %w", err)
		}
	}

	// 2. Create a unique subscription
	// We use a unique ID so each client instance gets its own copy of messages (Pub/Sub fan-out).
	subID := fmt.Sprintf("memorystore-sub-%s-%s", topicName, uuid.New().String())
	sub, err := g.client.CreateSubscription(context.Background(), subID, pubsub.SubscriptionConfig{
		Topic:            topic,
		ExpirationPolicy: time.Duration(24 * time.Hour), // Auto-delete if unused
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create subscription: %w", err)
	}

	// 3. Start receiving messages
	ch := make(chan []byte, 100)
	ctx, cancel := context.WithCancel(context.Background())

	gcpSub := &gcpSubscription{
		subID:  subID,
		sub:    sub,
		cancel: cancel,
	}

	gcpSub.wg.Add(1)
	go func() {
		defer gcpSub.wg.Done()
		defer close(ch)

		err := sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
			// Forward message to channel
			select {
			case ch <- msg.Data:
				msg.Ack()
			case <-ctx.Done():
				msg.Nack()
			}
		})
		if err != nil && err != context.Canceled {
			// Log error?
			// fmt.Printf("Receive error: %v\n", err)
		}
	}()

	g.subscriptions[topicName] = gcpSub
	return ch, nil
}

// Publish publishes a message to the topic
func (g *GCPPubSub) Publish(topicName string, message []byte) error {
	// Ensure topic exists (lazy creation)
	// For performance, we might assume it exists or cache existence, but let's be safe.
	// Actually, checking every time is slow.
	// But `client.Topic` is lightweight. `Publish` handles non-existent topic by failing.
	// Let's rely on standard library behavior.

	// However, if we want to auto-create topics like Redis, we should check.
	// Let's check existence for now, or just try to publish.
	topic := g.client.Topic(topicName)

	// We can't easily check existence without an API call.
	// Let's assume the user ensures topics exist, OR we handle the error.
	// But for a "canonical example", it's nice if it Just Works.
	// Let's check existence once per topic per client instance?
	// For now, let's just Publish. If it fails, so be it.

	res := topic.Publish(context.Background(), &pubsub.Message{
		Data: message,
	})

	_, err := res.Get(context.Background())
	return err
}

// Unsubscribe stops the subscription and deletes it
func (g *GCPPubSub) Unsubscribe(topicName string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	sub, ok := g.subscriptions[topicName]
	if !ok {
		return nil
	}

	// Stop receiving
	sub.cancel()
	sub.wg.Wait()

	// Delete subscription from GCP to clean up
	if err := sub.sub.Delete(context.Background()); err != nil {
		// Log error but continue
	}

	delete(g.subscriptions, topicName)
	return nil
}

// Close closes the client and cleans up all subscriptions
func (g *GCPPubSub) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, sub := range g.subscriptions {
		sub.cancel()
		sub.wg.Wait()
		// Best effort delete
		sub.sub.Delete(context.Background())
	}
	g.subscriptions = nil

	return g.client.Close()
}
