// memorystore/pubsub.go
package memorystore

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Constants for PubSub configuration
const (
	defaultChannelBuffer = 100 // Default buffer size for subscriber channels
)

// Common errors for PubSub operations
var (
	ErrInvalidPattern = errors.New("invalid subscription pattern")
	ErrStoreStopped   = errors.New("store has been stopped")
)

// subscription represents an individual subscriber
type subscription struct {
	pattern string          // The pattern this subscription matches
	ch      chan []byte     // Channel for sending messages to the subscriber
	ctx     context.Context // Context for managing subscription lifetime
	cancel  func()          // Function to cancel the subscription context
}

// pubSubManager handles all publish/subscribe operations
type pubSubManager struct {
	mu            sync.RWMutex
	subscriptions map[string][]*subscription // Pattern -> subscriptions mapping
	wg            sync.WaitGroup             // For graceful shutdown
}

// newPubSubManager creates and initializes a new pubSubManager
func newPubSubManager() *pubSubManager {
	return &pubSubManager{
		subscriptions: make(map[string][]*subscription),
	}
}

// Subscribe creates a new subscription for the given pattern
func (m *MemoryStore) Subscribe(pattern string) (<-chan []byte, error) {
	if m.IsStopped() {
		return nil, ErrStoreStopped
	}

	if pattern == "" {
		return nil, ErrInvalidPattern
	}

	// Create subscription context
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan []byte, defaultChannelBuffer)

	sub := &subscription{
		pattern: pattern,
		ch:      ch,
		ctx:     ctx,
		cancel:  cancel,
	}

	// Add subscription to manager
	m.ps.mu.Lock()
	m.ps.subscriptions[pattern] = append(m.ps.subscriptions[pattern], sub)
	m.ps.mu.Unlock()

	// Add to wait group for graceful shutdown
	m.ps.wg.Add(1)

	// Cleanup goroutine
	go func() {
		defer m.ps.wg.Done()
		<-ctx.Done()
		m.removeSubscription(pattern, sub)
		close(ch)
	}()

	return ch, nil
}

// Publish sends a message to all subscribers matching the given channel
func (m *MemoryStore) Publish(channel string, message []byte) error {
	if m.IsStopped() {
		return ErrStoreStopped
	}

	m.ps.mu.RLock()
	defer m.ps.mu.RUnlock()

	// Find all matching subscriptions and publish to them
	for pattern, subs := range m.ps.subscriptions {
		if matchesPattern(channel, pattern) {
			for _, sub := range subs {
				select {
				case <-sub.ctx.Done():
					continue // Skip closed subscriptions
				default:
					select {
					case sub.ch <- message:
					default:
						// Channel is full, skip this subscriber
						// Could add logging or metrics here
					}
				}
			}
		}
	}

	return nil
}

// Unsubscribe cancels all subscriptions for the given pattern
func (m *MemoryStore) Unsubscribe(pattern string) error {
	if m.IsStopped() {
		return ErrStoreStopped
	}

	m.ps.mu.Lock()
	defer m.ps.mu.Unlock()

	subs, exists := m.ps.subscriptions[pattern]
	if !exists {
		return nil
	}

	// Cancel all subscriptions for this pattern
	for _, sub := range subs {
		sub.cancel()
	}

	delete(m.ps.subscriptions, pattern)
	return nil
}

// SubscriberCount returns the number of subscribers for a given pattern
func (m *MemoryStore) SubscriberCount(pattern string) int {
	m.ps.mu.RLock()
	defer m.ps.mu.RUnlock()

	count := 0
	for p, subs := range m.ps.subscriptions {
		if p == pattern {
			count += len(subs)
		}
	}
	return count
}

// removeSubscription removes a specific subscription
func (m *MemoryStore) removeSubscription(pattern string, sub *subscription) {
	m.ps.mu.Lock()
	defer m.ps.mu.Unlock()

	subs := m.ps.subscriptions[pattern]
	for i, s := range subs {
		if s == sub {
			// Remove subscription from slice
			subs = append(subs[:i], subs[i+1:]...)
			break
		}
	}

	if len(subs) == 0 {
		delete(m.ps.subscriptions, pattern)
	} else {
		m.ps.subscriptions[pattern] = subs
	}
}

// matchesPattern checks if a channel matches a subscription pattern
func matchesPattern(channel, pattern string) bool {
	// Split pattern and channel into segments
	patternParts := strings.Split(pattern, ":")
	channelParts := strings.Split(channel, ":")

	if len(patternParts) != len(channelParts) {
		return false
	}

	// Check each segment
	for i, patternPart := range patternParts {
		if patternPart != "*" && patternPart != channelParts[i] {
			return false
		}
	}

	return true
}

// initPubSub initializes the PubSub system for the MemoryStore
func (m *MemoryStore) initPubSub() {
	m.ps = newPubSubManager()
}

// cleanupPubSub performs cleanup of the PubSub system during store shutdown
func (m *MemoryStore) cleanupPubSub() {
	m.ps.mu.Lock()
	// Cancel all subscriptions
	for pattern, subs := range m.ps.subscriptions {
		for _, sub := range subs {
			sub.cancel()
		}
		delete(m.ps.subscriptions, pattern)
	}
	m.ps.mu.Unlock()

	// Wait for all subscription goroutines to finish
	m.ps.wg.Wait()
}
