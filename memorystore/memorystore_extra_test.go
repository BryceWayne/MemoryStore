package memorystore

import (
	"os"
	"testing"
	"time"
)

func TestMemoryStore_PubSub_Stopped(t *testing.T) {
	ms := NewMemoryStore()
	_ = ms.Stop()

	if _, err := ms.Subscribe("test"); err != ErrStoreStopped {
		t.Errorf("Subscribe() should return ErrStoreStopped, got %v", err)
	}

	if err := ms.Publish("test", []byte("msg")); err != ErrStoreStopped {
		t.Errorf("Publish() should return ErrStoreStopped, got %v", err)
	}

	if err := ms.Unsubscribe("test"); err != ErrStoreStopped {
		t.Errorf("Unsubscribe() should return ErrStoreStopped, got %v", err)
	}
}

func TestMemoryStore_InitPubSub_GCP(t *testing.T) {
	// Save original env
	orig := os.Getenv("GOOGLE_CLOUD_PROJECT")
	defer os.Setenv("GOOGLE_CLOUD_PROJECT", orig)

	// Set env to trigger GCP path
	os.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")

	// This should fail to init GCP (no creds) and fall back to memory
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	// Check if it's running (fallback worked)
	if ms.ps == nil {
		t.Fatal("PubSub client should be initialized (fallback to memory)")
	}

	// Verify it is indeed InMemoryPubSub by checking type or behavior
	// internal field ms.ps is private, but we can check behavior
	// or use reflection/unsafe if really needed, but generally if it works it works.
	// We can check if SubscriberCount works, as it only works for InMemory

	// Create a subscription
	_, err := ms.Subscribe("test")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	// Check count
	if count := ms.SubscriberCount("test"); count != 1 {
		t.Errorf("Expected subscriber count 1, got %d. This implies fallback to InMemoryPubSub failed or behavior changed.", count)
	}
}

func TestMemoryStore_SetJSON_Error(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	// Channel is not JSON marshalsable
	ch := make(chan int)
	err := ms.SetJSON("key", ch, time.Minute)
	if err == nil {
		t.Error("SetJSON should fail for unmarshalable type")
	}
}
