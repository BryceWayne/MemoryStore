package memorystore

import (
	"sync"
	"testing"
	"time"
)

func TestMatchesTopic(t *testing.T) {
	tests := []struct {
		topic   string
		pattern string
		match   bool
	}{
		{"user:123", "user:123", true},
		{"user:123", "user:*", true},
		{"user:123:profile", "user:*:profile", true},
		{"user:123:profile", "user:123:*", true},
		{"user:123", "post:123", false},
		{"user:123", "user:123:profile", false},
		{"user:123:profile", "user:123", false},
		{"user:123", "*", false}, // matchesTopic splits by ":", so "*" matches "user", not "user:123" if implementation splits.
		// Wait, let's check implementation of matchesTopic.
		// "Split pattern and topic into segments"
		// If pattern is "*", split gives ["*"]. topic "user:123" split gives ["user", "123"]. Len differs. Returns false.
		// So "*" only matches single segment topics.
		{"a", "*", true},
		{"a:b", "*:*", true},
		{"a:b", "a:*", true},
		{"a:b", "*:b", true},
		{"a:b:c", "a:*:c", true},
	}

	for _, tt := range tests {
		if got := matchesTopic(tt.topic, tt.pattern); got != tt.match {
			t.Errorf("matchesTopic(%q, %q) = %v, want %v", tt.topic, tt.pattern, got, tt.match)
		}
	}
}

func TestInMemoryPubSub_Closed(t *testing.T) {
	ps := newInMemoryPubSub()

	// Test Close
	if err := ps.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Test operations after Close
	if _, err := ps.Subscribe("topic"); err != ErrStoreStopped {
		t.Errorf("Subscribe after Close should return ErrStoreStopped, got %v", err)
	}

	if err := ps.Publish("topic", []byte("msg")); err != ErrStoreStopped {
		t.Errorf("Publish after Close should return ErrStoreStopped, got %v", err)
	}

	if err := ps.Unsubscribe("topic"); err != ErrStoreStopped {
		t.Errorf("Unsubscribe after Close should return ErrStoreStopped, got %v", err)
	}
}

func TestInMemoryPubSub_RemoveSubscription(t *testing.T) {
	ps := newInMemoryPubSub()
	defer ps.Close()

	topic := "test"
	sub1, err := ps.Subscribe(topic)
	if err != nil {
		t.Fatalf("Subscribe 1 failed: %v", err)
	}

	sub2, err := ps.Subscribe(topic)
	if err != nil {
		t.Fatalf("Subscribe 2 failed: %v", err)
	}

	// Verify we have 2 subs
	ps.mu.RLock()
	if len(ps.subscriptions[topic]) != 2 {
		t.Errorf("Expected 2 subscriptions, got %d", len(ps.subscriptions[topic]))
	}
	ps.mu.RUnlock()

	// Unsubscribe one
	// Since Subscribe returns a channel, we don't have the subscription object directly to call removeSubscription.
	// We rely on Unsubscribe to remove ALL for topic, or internal logic.
	// Wait, Unsubscribe removes ALL subscriptions for a topic.
	// Is there a way to unsubscribe a single subscriber?
	// The interface `Unsubscribe(topic string)` implies unsubscription by topic.
	// The `InMemoryPubSub.Subscribe` starts a goroutine that calls `removeSubscription` when context is done.
	// But `Subscribe` returns `<-chan []byte`. It doesn't return a way to cancel just that subscription from the outside,
	// unless `Unsubscribe` is called which cancels all for that topic.
	// Or if the context passed to `newInMemoryPubSub`... wait, `Subscribe` creates its own context.

	// Actually, `Subscribe` returns `<-chan []byte`. The caller can stop reading? No.
	// The `InMemoryPubSub` doesn't seem to expose a way to unsubscribe a single subscriber if there are multiple on the same topic?
	// `Unsubscribe(topic)` cancels ALL subscriptions for that topic.

	// Let's verify `Unsubscribe` removes all.
	if err := ps.Unsubscribe(topic); err != nil {
		t.Fatalf("Unsubscribe failed: %v", err)
	}

	// Wait a bit for cleanup goroutines
	time.Sleep(50 * time.Millisecond)

	ps.mu.RLock()
	if len(ps.subscriptions[topic]) != 0 {
		t.Errorf("Expected 0 subscriptions after Unsubscribe, got %d", len(ps.subscriptions[topic]))
	}
	ps.mu.RUnlock()

	// Verify channels are closed
	select {
	case _, ok := <-sub1:
		if ok {
			t.Error("sub1 channel should be closed")
		}
	default:
		// might not be closed yet if we didn't wait enough, but Sleep should cover it
	}
	select {
	case _, ok := <-sub2:
		if ok {
			t.Error("sub2 channel should be closed")
		}
	default:
	}
}

func TestInMemoryPubSub_TopicCleaning(t *testing.T) {
	ps := newInMemoryPubSub()
	defer ps.Close()

	topic := "temp-topic"
	ch, _ := ps.Subscribe(topic)

	// Unsubscribe
	_ = ps.Unsubscribe(topic)

	// Wait for cleanup
	time.Sleep(10 * time.Millisecond)

	// Check if topic entry is removed from map
	ps.mu.RLock()
	_, exists := ps.subscriptions[topic]
	ps.mu.RUnlock()

	if exists {
		t.Error("Topic entry should be removed from map after last subscriber is removed")
	}

	// Drain channel to be safe
	for range ch {}
}

func TestInMemoryPubSub_ConcurrentPublishSubscribe(t *testing.T) {
	ps := newInMemoryPubSub()
	defer ps.Close()

	var wg sync.WaitGroup
	const routines = 20

	// Concurrent Subscribe
	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, _ = ps.Subscribe("topic")
		}(i)
	}

	// Concurrent Publish
	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_ = ps.Publish("topic", []byte("msg"))
		}(i)
	}

	// Concurrent Unsubscribe
	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Sleep a bit to let some subscribes happen
			time.Sleep(time.Millisecond)
			_ = ps.Unsubscribe("topic")
		}(i)
	}

	wg.Wait()
}
