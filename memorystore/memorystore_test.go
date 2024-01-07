package memorystore

import (
    "testing"
    "time"

    "github.com/goccy/go-json"
)

// TestMemoryStore_SetGet tests the Set and Get methods of MemoryStore
func TestMemoryStore_SetGet(t *testing.T) {
    ms := NewMemoryStore()
    defer ms.Stop()

    key := "key1"
    value := "value1"
    expiration := 1 * time.Second

    // Serialize the value to a byte slice
    valueBytes, err := json.Marshal(value)
    if err != nil {
        t.Fatalf("Failed to marshal value: %v", err)
    }

    // Call Set method without expecting a return value
    err = ms.Set(key, valueBytes, expiration)
    if err != nil {
        t.Fatalf("Set failed: %v", err)
    }

    // Test immediate retrieval
    retrievedBytes, exists := ms.Get(key)
    if !exists {
        t.Error("expected value to exist")
    }

    var retrievedValue string
    if err := json.Unmarshal(retrievedBytes, &retrievedValue); err != nil {
        t.Fatalf("Failed to unmarshal retrieved value: %v", err)
    }

    if !exists || retrievedValue != value {
        t.Errorf("expected %v, got %v", value, retrievedValue)
    }

    // Test retrieval after expiration
    time.Sleep(expiration + time.Second)
    _, exists = ms.Get(key) // Ignoring error for simplicity
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

    // Serialize the value to a byte slice
    valueBytes, err := json.Marshal(value)
    if err != nil {
        t.Fatalf("Failed to marshal value: %v", err)
    }

    // Call Set method without expecting a return value
    err = ms.Set(key, valueBytes, 5*time.Minute)
    if err != nil {
        t.Fatalf("Set failed: %v", err)
    }

    ms.Delete(key)
    _, exists := ms.Get(key) // Ignoring error for simplicity
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

    // Serialize the value to a byte slice
    valueBytes, err := json.Marshal(value)
    if err != nil {
        t.Fatalf("Failed to marshal value: %v", err)
    }

    // Call Set method without expecting a return value
    ms.Set(key, valueBytes, shortExpiration)

    time.Sleep(shortExpiration + 1*time.Second) // wait for item to expire and for cleanup worker to run

    _, exists := ms.Get(key) // Ignoring error for simplicity
    if exists {
        t.Error("expected expired value to be cleaned up by the worker")
    }
}
