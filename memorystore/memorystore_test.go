package memorystore

import (
    "testing"
    "time"
)

// TestMemoryStore_SetGet tests the Set and Get methods of MemoryStore
func TestMemoryStore_SetGet(t *testing.T) {
    ms := NewMemoryStore()
    defer ms.Stop()

    key := "key1"
    value := "value1"
    expiration := 1 * time.Second

    if err := ms.Set(key, value, expiration); err != nil {
        t.Fatalf("Set failed: %v", err)
    }

    // Test immediate retrieval
    retrievedValue, exists, err := ms.Get(key)
    if err != nil {
        t.Fatalf("Get failed: %v", err)
    }
    if !exists || retrievedValue != value {
        t.Errorf("expected %v, got %v", value, retrievedValue)
    }

    // Test retrieval after expiration
    time.Sleep(expiration + time.Second)
    _, exists, _ = ms.Get(key) // Ignoring error for simplicity
    if exists {
        t.Error("expected value to be expired and not exist")
    }
}

// TestMemoryStore_Delete tests the Delete method of MemoryStore
func TestMemoryStore_Delete(t *testing.T) {
    ms := NewMemoryStore()
    defer ms.Stop()

    key := "key1"
    value := "value1"

    if err := ms.Set(key, value, 5*time.Minute); err != nil {
        t.Fatalf("Set failed: %v", err)
    }

    ms.Delete(key)
    _, exists, _ := ms.Get(key) // Ignoring error for simplicity
    if exists {
        t.Error("expected value to be deleted")
    }
}

// TestMemoryStore_CleanupWorker tests whether the cleanup worker removes expired items
func TestMemoryStore_CleanupWorker(t *testing.T) {
    ms := NewMemoryStore()
    defer ms.Stop()

    key := "key1"
    value := "value1"
    shortExpiration := 1 * time.Second

    if err := ms.Set(key, value, shortExpiration); err != nil {
        t.Fatalf("Set failed: %v", err)
    }

    time.Sleep(shortExpiration + 1*time.Second) // wait for item to expire and for cleanup worker to run

    _, exists, _ := ms.Get(key) // Ignoring error for simplicity
    if exists {
        t.Error("expected expired value to be cleaned up by the worker")
    }
}
