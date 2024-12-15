// Package memorystore provides a simple in-memory cache implementation with automatic cleanup
// of expired items. It supports both raw byte storage and JSON serialization/deserialization
// of structured data.
package memorystore

import (
	"context"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// item represents a single cache entry with its value and expiration time.
type item struct {
	value     []byte    // Raw data stored as a byte slice
	expiresAt time.Time // Time at which this item should be considered expired
}

// MemoryStore implements an in-memory cache with automatic cleanup of expired items.
// It is safe for concurrent use by multiple goroutines.
type MemoryStore struct {
	mu         sync.RWMutex       // Protects access to the store map
	store      map[string]item    // Internal storage for cache items
	ctx        context.Context    // Context for controlling the cleanup worker
	cancelFunc context.CancelFunc // Function to stop the cleanup worker
	wg         sync.WaitGroup     // WaitGroup for cleanup goroutine synchronization
}

// NewMemoryStore creates and initializes a new MemoryStore instance.
// It starts a background worker that periodically cleans up expired items.
// The returned MemoryStore is ready for use.
func NewMemoryStore() *MemoryStore {
	ctx, cancel := context.WithCancel(context.Background())
	ms := &MemoryStore{
		store:      make(map[string]item),
		ctx:        ctx,
		cancelFunc: cancel,
	}
	ms.startCleanupWorker()
	return ms
}

// Stop gracefully shuts down the MemoryStore by stopping the cleanup goroutine
// and releasing associated resources. After calling Stop, the store cannot be used.
// Multiple calls to Stop will not cause a panic and return nil.
//
// Example:
//
//	store := NewMemoryStore()
//	defer store.Stop()
func (m *MemoryStore) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancelFunc == nil {
		return nil
	}

	m.cancelFunc()
	m.cancelFunc = nil

	// Wait for cleanup goroutine to finish
	m.wg.Wait()

	// Clear the store to free up memory
	m.store = nil

	return nil
}

// IsStopped returns true if the MemoryStore has been stopped and can no longer be used.
// This method is safe for concurrent use.
//
// Example:
//
//	if store.IsStopped() {
//	    log.Println("Store is no longer available")
//	    return
//	}
func (m *MemoryStore) IsStopped() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cancelFunc == nil
}

// startCleanupWorker initiates a background goroutine that periodically
// removes expired items from the cache. The cleanup interval is set to 1 minute.
func (m *MemoryStore) startCleanupWorker() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.cleanupExpiredItems()
			case <-m.ctx.Done():
				return
			}
		}
	}()
}

// cleanupExpiredItems removes all expired items from the cache.
// This method acquires a write lock on the store while performing the cleanup.
func (m *MemoryStore) cleanupExpiredItems() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, item := range m.store {
		if time.Now().After(item.expiresAt) {
			delete(m.store, key)
		}
	}
}

// Set stores a raw byte slice in the cache with the specified key and duration.
// The item will automatically expire after the specified duration.
// If an error occurs, it will be returned to the caller.
func (m *MemoryStore) Set(key string, value []byte, duration time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.store[key] = item{
		value:     value,
		expiresAt: time.Now().Add(duration),
	}

	return nil
}

// SetJSON stores a JSON-serializable value in the cache.
// The value is serialized to JSON before storage.
// Returns an error if JSON marshaling fails.
//
// Example:
//
//	type User struct {
//	    Name string
//	    Age  int
//	}
//	user := User{Name: "John", Age: 30}
//	err := cache.SetJSON("user:123", user, 1*time.Hour)
func (m *MemoryStore) SetJSON(key string, value interface{}, duration time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return m.Set(key, data, duration)
}

// Get retrieves a value from the cache.
// Returns the value and a boolean indicating whether the key was found.
// If the item has expired, returns (nil, false).
func (m *MemoryStore) Get(key string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	it, exists := m.store[key]
	if !exists || time.Now().After(it.expiresAt) {
		return nil, false
	}

	return it.value, true
}

// GetJSON retrieves and deserializes a JSON value from the cache into the provided interface.
// Returns a boolean indicating if the key was found and any error that occurred during deserialization.
//
// Example:
//
//	var user User
//	exists, err := cache.GetJSON("user:123", &user)
//	if err != nil {
//	    // Handle error
//	} else if exists {
//	    fmt.Printf("Found user: %+v\n", user)
//	}
func (m *MemoryStore) GetJSON(key string, dest interface{}) (bool, error) {
	data, exists := m.Get(key)
	if !exists {
		return false, nil
	}

	err := json.Unmarshal(data, dest)
	if err != nil {
		return true, err
	}

	return true, nil
}

// Delete removes an item from the cache.
// If the key doesn't exist, the operation is a no-op.
func (m *MemoryStore) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, key)
}
