// memorystore/pubsub_memory.go
package memorystore

import (
	"context"
	"strings"
	"sync"
)

// Constants for PubSub configuration
const (
	defaultChannelBuffer = 100 // Default buffer size for subscriber channels
)

// subscription represents an individual subscriber
type subscription struct {
	topic  string          // The topic this subscription matches
	ch     chan []byte     // Channel for sending messages to the subscriber
	ctx    context.Context // Context for managing subscription lifetime
	cancel func()          // Function to cancel the subscription context
}

// InMemoryPubSub handles all publish/subscribe operations in memory
type InMemoryPubSub struct {
	mu            sync.RWMutex
	subscriptions map[string][]*subscription // Topic -> subscriptions mapping
	wg            sync.WaitGroup             // For graceful shutdown
	closed        bool
}

// newInMemoryPubSub creates and initializes a new InMemoryPubSub
func newInMemoryPubSub() *InMemoryPubSub {
	return &InMemoryPubSub{
		subscriptions: make(map[string][]*subscription),
	}
}

// Subscribe creates a new subscription for the given topic
func (ps *InMemoryPubSub) Subscribe(topic string) (<-chan []byte, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.closed {
		return nil, ErrStoreStopped
	}

	if topic == "" {
		return nil, ErrInvalidTopic
	}

	// Create subscription context
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan []byte, defaultChannelBuffer)

	sub := &subscription{
		topic:  topic,
		ch:     ch,
		ctx:    ctx,
		cancel: cancel,
	}

	// Add subscription to manager
	ps.subscriptions[topic] = append(ps.subscriptions[topic], sub)

	// Add to wait group for graceful shutdown
	ps.wg.Add(1)

	// Cleanup goroutine
	go func() {
		defer ps.wg.Done()
		<-ctx.Done()
		ps.removeSubscription(topic, sub)
		close(ch)
	}()

	return ch, nil
}

// Publish sends a message to all subscribers matching the given topic
func (ps *InMemoryPubSub) Publish(topic string, message []byte) error {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	if ps.closed {
		return ErrStoreStopped
	}

	// Find all matching subscriptions and publish to them
	// We support simple pattern matching here for backward compatibility
	for subTopic, subs := range ps.subscriptions {
		if matchesTopic(topic, subTopic) {
			for _, sub := range subs {
				select {
				case <-sub.ctx.Done():
					continue // Skip closed subscriptions
				default:
					select {
					case sub.ch <- message:
					default:
						// Channel is full, skip this subscriber
					}
				}
			}
		}
	}

	return nil
}

// Unsubscribe cancels all subscriptions for the given topic
func (ps *InMemoryPubSub) Unsubscribe(topic string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.closed {
		return ErrStoreStopped
	}

	subs, exists := ps.subscriptions[topic]
	if !exists {
		return nil
	}

	// Cancel all subscriptions for this topic
	for _, sub := range subs {
		sub.cancel()
	}

	// We don't delete the topic from the map here.
	// The cleanup goroutines (triggered by cancel()) will call removeSubscription,
	// which will remove the subscriptions from the slice and delete the topic
	// when the last subscription is removed.
	return nil
}

// Close shuts down the PubSub manager
func (ps *InMemoryPubSub) Close() error {
	ps.mu.Lock()
	if ps.closed {
		ps.mu.Unlock()
		return nil
	}
	ps.closed = true

	// Cancel all subscriptions
	for topic, subs := range ps.subscriptions {
		for _, sub := range subs {
			sub.cancel()
		}
		delete(ps.subscriptions, topic)
	}
	ps.mu.Unlock()

	// Wait for all subscription goroutines to finish
	ps.wg.Wait()
	return nil
}

// removeSubscription removes a specific subscription
func (ps *InMemoryPubSub) removeSubscription(topic string, sub *subscription) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.closed {
		return
	}

	subs, ok := ps.subscriptions[topic]
	if !ok {
		return
	}

	for i, s := range subs {
		if s == sub {
			// Remove subscription from slice
			subs = append(subs[:i], subs[i+1:]...)
			break
		}
	}

	if len(subs) == 0 {
		delete(ps.subscriptions, topic)
	} else {
		ps.subscriptions[topic] = subs
	}
}

// matchesTopic checks if a topic matches a subscription pattern
func matchesTopic(topic, pattern string) bool {
	// If exactly equal, return true
	if topic == pattern {
		return true
	}

	// Split pattern and topic into segments
	patternParts := strings.Split(pattern, ":")
	topicParts := strings.Split(topic, ":")

	if len(patternParts) != len(topicParts) {
		return false
	}

	// Check each segment
	for i, patternPart := range patternParts {
		if patternPart != "*" && patternPart != topicParts[i] {
			return false
		}
	}

	return true
}
