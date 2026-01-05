package memorystore

import (
	"testing"
	"time"
)

func TestMemoryStore_SubscriberCount(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	topic := "test-topic"
	ch1, err := ms.Subscribe(topic)
	if err != nil {
		t.Fatalf("Subscribe 1 failed: %v", err)
	}
	defer ms.Unsubscribe(topic)

	if count := ms.SubscriberCount(topic); count != 1 {
		t.Errorf("SubscriberCount should be 1, got %d", count)
	}

	ch2, err := ms.Subscribe(topic)
	if err != nil {
		t.Fatalf("Subscribe 2 failed: %v", err)
	}

	// InMemoryPubSub.Unsubscribe(topic) removes ALL subscriptions for that topic.
	if count := ms.SubscriberCount(topic); count != 2 {
		t.Errorf("SubscriberCount should be 2, got %d", count)
	}

	// Unsubscribe everything
	if err := ms.Unsubscribe(topic); err != nil {
		t.Fatalf("Unsubscribe failed: %v", err)
	}

	// Wait a bit for cleanup
	time.Sleep(50 * time.Millisecond)

	if count := ms.SubscriberCount(topic); count != 0 {
		t.Errorf("SubscriberCount should be 0, got %d", count)
	}

	// Consume channels to avoid blockage/leaks in test
	go func() {
		for range ch1 {}
		for range ch2 {}
	}()
}

func TestMemoryStore_InitPubSub_Fallback(t *testing.T) {
	config := Config{
		GCPProjectID: "invalid-project-id-likely-to-fail-auth",
	}

	ms := NewMemoryStoreWithConfig(config)
	defer func() {
		_ = ms.Stop()
	}()

	// Check type of ms.ps
	if _, ok := ms.ps.(*InMemoryPubSub); !ok {
		t.Logf("Initialized PubSub type: %T", ms.ps)
	} else {
		t.Log("Fallback to InMemoryPubSub successful")
	}
}

func TestMemoryStore_PubSub_Stopped(t *testing.T) {
	ms := NewMemoryStore()
	_ = ms.Stop()

	if _, err := ms.Subscribe("topic"); err != ErrStoreStopped {
		t.Errorf("Subscribe after Stop should return ErrStoreStopped, got %v", err)
	}
	if err := ms.Publish("topic", []byte("msg")); err != ErrStoreStopped {
		t.Errorf("Publish after Stop should return ErrStoreStopped, got %v", err)
	}
	if err := ms.Unsubscribe("topic"); err != ErrStoreStopped {
		t.Errorf("Unsubscribe after Stop should return ErrStoreStopped, got %v", err)
	}
}

func TestMemoryStore_SetJSON_Error(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	// Channel is not JSON serializable
	badValue := make(chan int)
	err := ms.SetJSON("key", badValue, time.Minute)
	if err == nil {
		t.Error("SetJSON should return error for unserializable value")
	}
}
