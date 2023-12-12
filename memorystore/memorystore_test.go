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
    expiration := 5 * time.Second

    ms.Set(key, value, expiration)

    // Test immediate retrieval
    retrievedValue, exists := ms.Get(key)
    if !exists || retrievedValue != value {
        t.Errorf("expected %v, got %v", value, retrievedValue)
    }

    // Test retrieval after expiration
    time.Sleep(expiration + time.Second)
    _, exists = ms.Get(key)
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
    ms.Set(key, value, 5*time.Minute)

    ms.Delete(key)
    _, exists := ms.Get(key)
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

    ms.Set(key, value, shortExpiration)

    time.Sleep(shortExpiration + 2*time.Second) // wait for item to expire and for cleanup worker to run

    _, exists := ms.Get(key)
    if exists {
        t.Error("expected expired value to be cleaned up by the worker")
    }
}
