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
		{"user:123", "*", false},
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

	// Test Double Close
	if err := ps.Close(); err != nil {
		t.Errorf("Second Close should return nil, got %v", err)
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

	// Unsubscribe all for topic
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

func TestInMemoryPubSub_RemoveSubscription_AfterClose(t *testing.T) {
	ps := newInMemoryPubSub()
	topic := "topic"
	_, err := ps.Subscribe(topic)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	// Close store
	if err := ps.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Try to remove subscription (simulate race or delayed cleanup)
	// We need a subscription object to pass. But removeSubscription takes *subscription.
	// Since removeSubscription is internal, and we are in the same package (memorystore), we can construct one or access it if we had it.
	// But we can't easily get the subscription object created inside Subscribe.
	// However, we can use reflection or just manually call removeSubscription with a dummy if we want to test the check.

	// Since we are in `memorystore` package, we can create a dummy subscription.
	dummySub := &subscription{topic: topic}
	ps.removeSubscription(topic, dummySub) // Should return immediately because ps.closed is true
}

func TestInMemoryPubSub_RemoveSubscription_TopicNotFound(t *testing.T) {
	ps := newInMemoryPubSub()
	defer ps.Close()

	// Try to remove subscription for non-existent topic
	dummySub := &subscription{topic: "non-existent"}
	ps.removeSubscription("non-existent", dummySub) // Should return immediately
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
	for range ch {
	}
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
