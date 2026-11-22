package memorystore

import (
	"context"
	"errors"
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
	ctx := t.Context()
	ps := NewInMemoryPubSub()

	if err := ps.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := ps.Close(); err != nil {
		t.Errorf("Second Close should return nil, got %v", err)
	}

	if _, err := ps.Subscribe(ctx, "topic"); !errors.Is(err, ErrStoreStopped) {
		t.Errorf("Subscribe after Close should return ErrStoreStopped, got %v", err)
	}

	if err := ps.Publish(ctx, "topic", []byte("msg")); !errors.Is(err, ErrStoreStopped) {
		t.Errorf("Publish after Close should return ErrStoreStopped, got %v", err)
	}

	if err := ps.Unsubscribe(ctx, "topic"); !errors.Is(err, ErrStoreStopped) {
		t.Errorf("Unsubscribe after Close should return ErrStoreStopped, got %v", err)
	}
}

func TestInMemoryPubSub_RemoveSubscription(t *testing.T) {
	ctx := t.Context()
	ps := NewInMemoryPubSub()
	defer func() {
		_ = ps.Close()
	}()

	topic := "test"
	sub1, err := ps.Subscribe(ctx, topic)
	if err != nil {
		t.Fatalf("Subscribe 1 failed: %v", err)
	}

	sub2, err := ps.Subscribe(ctx, topic)
	if err != nil {
		t.Fatalf("Subscribe 2 failed: %v", err)
	}

	if got := ps.SubscriberCount(topic); got != 2 {
		t.Errorf("Expected 2 subscriptions, got %d", got)
	}

	if err := ps.Unsubscribe(ctx, topic); err != nil {
		t.Fatalf("Unsubscribe failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if got := ps.SubscriberCount(topic); got != 0 {
		t.Errorf("Expected 0 subscriptions after Unsubscribe, got %d", got)
	}

	select {
	case _, ok := <-sub1:
		if ok {
			t.Error("sub1 channel should be closed")
		}
	default:
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
	ctx := t.Context()
	ps := NewInMemoryPubSub()
	topic := "topic"
	if _, err := ps.Subscribe(ctx, topic); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	if err := ps.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	dummySub := &subscription{topic: topic}
	ps.removeSubscription(topic, dummySub)
}

func TestInMemoryPubSub_RemoveSubscription_TopicNotFound(t *testing.T) {
	ps := NewInMemoryPubSub()
	defer func() {
		_ = ps.Close()
	}()

	dummySub := &subscription{topic: "non-existent"}
	ps.removeSubscription("non-existent", dummySub)
}

func TestInMemoryPubSub_TopicCleaning(t *testing.T) {
	ctx := t.Context()
	ps := NewInMemoryPubSub()
	defer func() {
		_ = ps.Close()
	}()

	topic := "temp-topic"
	ch, _ := ps.Subscribe(ctx, topic)

	_ = ps.Unsubscribe(ctx, topic)
	time.Sleep(20 * time.Millisecond)

	ps.mu.RLock()
	_, exists := ps.exactSubs[topic]
	ps.mu.RUnlock()

	if exists {
		t.Error("Topic entry should be removed from map after last subscriber is removed")
	}

	for range ch {
	}
}

func TestInMemoryPubSub_ConcurrentPublishSubscribe(t *testing.T) {
	ctx := context.Background()
	ps := NewInMemoryPubSub()
	defer func() {
		_ = ps.Close()
	}()

	var wg sync.WaitGroup
	const routines = 20

	for range routines {
		wg.Go(func() {
			_, _ = ps.Subscribe(ctx, "topic")
			_, _ = ps.Subscribe(ctx, "topic:*")
		})
	}

	for range routines {
		wg.Go(func() {
			_ = ps.Publish(ctx, "topic", []byte("msg"))
			_ = ps.Publish(ctx, "topic:1", []byte("msg"))
		})
	}

	for range routines {
		wg.Go(func() {
			time.Sleep(time.Millisecond)
			_ = ps.Unsubscribe(ctx, "topic")
			_ = ps.Unsubscribe(ctx, "topic:*")
		})
	}

	wg.Wait()
}
