package memorystore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
)

// defaultChannelBuffer is the buffer capacity for subscriber channels.
const defaultChannelBuffer = 100

// subscription represents an individual subscriber.
type subscription struct {
	topic  string
	ch     chan []byte
	ctx    context.Context
	cancel context.CancelFunc
}

// InMemoryPubSub is a thread-safe, in-memory implementation of PubSubClient
// and SubscriberCounter supporting exact topics and ':'-delimited wildcard
// patterns (for example, "user:*" or "orders:*:created").
type InMemoryPubSub struct {
	mu           sync.RWMutex
	exactSubs    map[string][]*subscription // Exact-match topics (O(1) publish lookup)
	wildcardSubs map[string][]*subscription // Wildcard patterns containing '*'
	wg           sync.WaitGroup             // Tracks subscription cleanup goroutines
	closed       bool
	dropped      atomic.Int64 // Count of messages dropped due to full subscriber channels
}

// NewInMemoryPubSub creates and initializes a new InMemoryPubSub broker.
func NewInMemoryPubSub() *InMemoryPubSub {
	return &InMemoryPubSub{
		exactSubs:    make(map[string][]*subscription),
		wildcardSubs: make(map[string][]*subscription),
	}
}

func isWildcardPattern(topic string) bool {
	return strings.ContainsRune(topic, '*')
}

func (ps *InMemoryPubSub) bucketFor(topic string) map[string][]*subscription {
	if isWildcardPattern(topic) {
		return ps.wildcardSubs
	}
	return ps.exactSubs
}

// Subscribe creates a new subscription for the given topic or wildcard pattern.
func (ps *InMemoryPubSub) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.closed {
		return nil, ErrStoreStopped
	}
	if topic == "" {
		return nil, ErrInvalidTopic
	}

	subCtx, cancel := context.WithCancel(context.Background())
	ch := make(chan []byte, defaultChannelBuffer)
	sub := &subscription{
		topic:  topic,
		ch:     ch,
		ctx:    subCtx,
		cancel: cancel,
	}

	bucket := ps.bucketFor(topic)
	bucket[topic] = append(bucket[topic], sub)

	ps.wg.Go(func() {
		<-subCtx.Done()
		ps.removeSubscription(topic, sub)
		close(ch)
	})

	return ch, nil
}

// Publish delivers a message to all subscribers whose exact topic or wildcard
// pattern matches topic. Slow subscribers whose channel buffer is full are skipped.
func (ps *InMemoryPubSub) Publish(ctx context.Context, topic string, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	ps.mu.RLock()
	defer ps.mu.RUnlock()

	if ps.closed {
		return ErrStoreStopped
	}

	// Fast O(1) lookup for exact-topic subscribers.
	if subs, ok := ps.exactSubs[topic]; ok {
		ps.deliverToSubscribers(subs, message)
	}

	// Evaluate wildcard patterns only when wildcard subscribers exist.
	for pattern, subs := range ps.wildcardSubs {
		if matchesTopic(topic, pattern) {
			ps.deliverToSubscribers(subs, message)
		}
	}

	return nil
}

func (ps *InMemoryPubSub) deliverToSubscribers(subs []*subscription, message []byte) {
	for _, sub := range subs {
		select {
		case <-sub.ctx.Done():
			continue
		default:
			select {
			case sub.ch <- message:
			default:
				// Channel is full, skip this subscriber and record the drop.
				ps.dropped.Add(1)
			}
		}
	}
}

// DroppedCount returns the total number of message deliveries skipped because
// a subscriber's buffered channel was full.
func (ps *InMemoryPubSub) DroppedCount() int64 {
	return ps.dropped.Load()
}

// Ping implements Pinger, reporting ErrStoreStopped if the broker is closed.
func (ps *InMemoryPubSub) Ping(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ps.mu.RLock()
	closed := ps.closed
	ps.mu.RUnlock()
	if closed {
		return ErrStoreStopped
	}
	return nil
}

// Unsubscribe cancels all active subscriptions for the given topic or pattern.
func (ps *InMemoryPubSub) Unsubscribe(ctx context.Context, topic string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.closed {
		return ErrStoreStopped
	}

	subs, exists := ps.bucketFor(topic)[topic]
	if !exists {
		return nil
	}

	for _, sub := range subs {
		sub.cancel()
	}

	return nil
}

// SubscriberCount returns the number of active subscribers registered under
// the exact given topic or wildcard pattern.
func (ps *InMemoryPubSub) SubscriberCount(pattern string) int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.bucketFor(pattern)[pattern])
}

// Close shuts down the broker, cancels all subscriptions, and waits for
// cleanup goroutines to exit. Multiple calls are safe and idempotent.
func (ps *InMemoryPubSub) Close() error {
	ps.mu.Lock()
	if ps.closed {
		ps.mu.Unlock()
		return nil
	}
	ps.closed = true

	for topic, subs := range ps.exactSubs {
		for _, sub := range subs {
			sub.cancel()
		}
		delete(ps.exactSubs, topic)
	}
	for pattern, subs := range ps.wildcardSubs {
		for _, sub := range subs {
			sub.cancel()
		}
		delete(ps.wildcardSubs, pattern)
	}
	ps.mu.Unlock()

	ps.wg.Wait()
	return nil
}

// removeSubscription removes a specific subscription from its bucket.
func (ps *InMemoryPubSub) removeSubscription(topic string, sub *subscription) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.closed {
		return
	}

	bucket := ps.bucketFor(topic)
	subs, ok := bucket[topic]
	if !ok {
		return
	}

	for i, s := range subs {
		if s == sub {
			subs = append(subs[:i], subs[i+1:]...)
			break
		}
	}

	if len(subs) == 0 {
		delete(bucket, topic)
	} else {
		bucket[topic] = subs
	}
}

// matchesTopic checks if a topic matches a ':'-delimited subscription pattern
// without allocating slices.
func matchesTopic(topic, pattern string) bool {
	if topic == pattern {
		return true
	}

	for {
		patternPart, patternRest, patternHasMore := strings.Cut(pattern, ":")
		topicPart, topicRest, topicHasMore := strings.Cut(topic, ":")

		if patternPart != "*" && patternPart != topicPart {
			return false
		}
		if patternHasMore != topicHasMore {
			return false
		}
		if !patternHasMore {
			return true
		}

		pattern = patternRest
		topic = topicRest
	}
}
