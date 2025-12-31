// memorystore/memorystore.go
// Package memorystore provides a simple in-memory cache implementation with automatic cleanup
// of expired items. It supports both raw byte storage and JSON serialization/deserialization
// of structured data.
package memorystore

import (
	"context"
	"hash/fnv"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
)

const numShards = 256

// item represents a single cache entry with its value and expiration time.
type item struct {
	value     []byte    // Raw data stored as a byte slice
	expiresAt time.Time // Time at which this item should be considered expired
}

// StoreMetrics holds statistics about the cache usage.
type StoreMetrics struct {
	Items     int   // Current number of items in the cache
	Hits      int64 // Total number of cache hits
	Misses    int64 // Total number of cache misses
	Evictions int64 // Total number of items evicted (expired)
}

type shard struct {
	mu    sync.RWMutex
	store map[string]item
}

// MemoryStore implements an in-memory cache with automatic cleanup of expired items.
// It is safe for concurrent use by multiple goroutines.
type MemoryStore struct {
	// lifecycleMu protects the lifecycle state (cancelFunc)
	lifecycleMu sync.RWMutex

	shards     []*shard           // Sharded storage
	ps         PubSubClient       // PubSub client for cache events
	ctx        context.Context    // Context for controlling the cleanup worker
	cancelFunc context.CancelFunc // Function to stop the cleanup worker
	wg         sync.WaitGroup     // WaitGroup for cleanup goroutine synchronization

	// Metrics
	hits      int64 // Atomic counter for cache hits
	misses    int64 // Atomic counter for cache misses
	evictions int64 // Atomic counter for evicted items
}

// NewMemoryStore creates and initializes a new MemoryStore instance.
// It checks for GOOGLE_CLOUD_PROJECT environment variable to decide whether to use GCP PubSub.
// Use NewMemoryStoreWithConfig for more control.
func NewMemoryStore() *MemoryStore {
	config := Config{}
	if projectID := os.Getenv("GOOGLE_CLOUD_PROJECT"); projectID != "" {
		config.GCPProjectID = projectID
	}
	return NewMemoryStoreWithConfig(config)
}

// NewMemoryStoreWithConfig creates a new MemoryStore with the provided configuration.
func NewMemoryStoreWithConfig(config Config) *MemoryStore {
	ctx, cancel := context.WithCancel(context.Background())
	ms := &MemoryStore{
		shards:     make([]*shard, numShards),
		ctx:        ctx,
		cancelFunc: cancel,
	}

	for i := 0; i < numShards; i++ {
		ms.shards[i] = &shard{
			store: make(map[string]item),
		}
	}

	ms.initPubSub(config)
	ms.startCleanupWorker()
	return ms
}

// getShard returns the shard responsible for the given key.
func (m *MemoryStore) getShard(key string) *shard {
	h := fnv.New64a()
	h.Write([]byte(key))
	return m.shards[h.Sum64()%numShards]
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
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if m.cancelFunc == nil {
		return nil
	}

	m.cancelFunc()
	m.cancelFunc = nil

	m.cleanupPubSub()

	// Wait for cleanup goroutine to finish
	m.wg.Wait()

	// Clear the store to free up memory
	for _, s := range m.shards {
		s.mu.Lock()
		s.store = nil
		s.mu.Unlock()
	}

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
	m.lifecycleMu.RLock()
	defer m.lifecycleMu.RUnlock()
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
// It iterates over shards and cleans them one by one to avoid global locking.
func (m *MemoryStore) cleanupExpiredItems() {
	now := time.Now()
	for _, s := range m.shards {
		// Lock only the current shard
		s.mu.Lock()
		for key, item := range s.store {
			if now.After(item.expiresAt) {
				delete(s.store, key)
				atomic.AddInt64(&m.evictions, 1)
			}
		}
		s.mu.Unlock()
	}
}

// Set stores a raw byte slice in the cache with the specified key and duration.
// The item will automatically expire after the specified duration.
// If an error occurs, it will be returned to the caller.
func (m *MemoryStore) Set(key string, value []byte, duration time.Duration) error {
	s := m.getShard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	s.store[key] = item{
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
	s := m.getShard(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	it, exists := s.store[key]
	if !exists || time.Now().After(it.expiresAt) {
		atomic.AddInt64(&m.misses, 1)
		return nil, false
	}

	atomic.AddInt64(&m.hits, 1)
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
	s := m.getShard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.store, key)
}

// SetMulti stores multiple key-value pairs in the cache.
// This is more efficient than calling Set multiple times as it groups keys by shard.
// All items will have the same expiration duration.
func (m *MemoryStore) SetMulti(items map[string][]byte, duration time.Duration) error {
	// Group items by shard
	shardItems := make(map[*shard]map[string]item)
	expiresAt := time.Now().Add(duration)

	for key, value := range items {
		s := m.getShard(key)
		if _, ok := shardItems[s]; !ok {
			shardItems[s] = make(map[string]item)
		}
		shardItems[s][key] = item{
			value:     value,
			expiresAt: expiresAt,
		}
	}

	// Apply updates per shard
	for s, items := range shardItems {
		s.mu.Lock()
		for k, v := range items {
			s.store[k] = v
		}
		s.mu.Unlock()
	}
	return nil
}

// GetMulti retrieves multiple values from the cache.
// It returns a map of found items. Keys that don't exist or are expired are omitted.
func (m *MemoryStore) GetMulti(keys []string) map[string][]byte {
	result := make(map[string][]byte)
	now := time.Now()

	// Group keys by shard
	shardKeys := make(map[*shard][]string)
	for _, key := range keys {
		s := m.getShard(key)
		shardKeys[s] = append(shardKeys[s], key)
	}

	// Retrieve from each shard
	for s, keys := range shardKeys {
		s.mu.RLock()
		for _, key := range keys {
			it, exists := s.store[key]
			if exists && !now.After(it.expiresAt) {
				result[key] = it.value
				atomic.AddInt64(&m.hits, 1)
			} else {
				atomic.AddInt64(&m.misses, 1)
			}
		}
		s.mu.RUnlock()
	}

	return result
}

// GetMetrics returns the current statistics of the MemoryStore.
// It returns a copy of the metrics to ensure thread safety.
func (m *MemoryStore) GetMetrics() StoreMetrics {
	itemCount := 0
	for _, s := range m.shards {
		s.mu.RLock()
		itemCount += len(s.store)
		s.mu.RUnlock()
	}

	return StoreMetrics{
		Items:     itemCount,
		Hits:      atomic.LoadInt64(&m.hits),
		Misses:    atomic.LoadInt64(&m.misses),
		Evictions: atomic.LoadInt64(&m.evictions),
	}
}

// Subscribe subscribes to a topic.
func (m *MemoryStore) Subscribe(topic string) (<-chan []byte, error) {
	if m.IsStopped() {
		return nil, ErrStoreStopped
	}
	return m.ps.Subscribe(topic)
}

// Publish publishes a message to a topic.
func (m *MemoryStore) Publish(topic string, message []byte) error {
	if m.IsStopped() {
		return ErrStoreStopped
	}
	return m.ps.Publish(topic, message)
}

// Unsubscribe unsubscribes from a topic.
func (m *MemoryStore) Unsubscribe(topic string) error {
	if m.IsStopped() {
		return ErrStoreStopped
	}
	return m.ps.Unsubscribe(topic)
}

// SubscriberCount returns the number of subscribers for a pattern.
// Note: This is not supported by the common interface and will return 0 or error in future.
// For now, it only works if the underlying implementation is In-Memory.
func (m *MemoryStore) SubscriberCount(pattern string) int {
	// Not part of the interface.
	// If we need this, we should add it to the interface or check type.
	if ps, ok := m.ps.(*InMemoryPubSub); ok {
		ps.mu.RLock()
		defer ps.mu.RUnlock()
		count := 0
		for p, subs := range ps.subscriptions {
			if p == pattern {
				count += len(subs)
			}
		}
		return count
	}
	return 0
}

// initPubSub initializes the PubSub system.
func (m *MemoryStore) initPubSub(config Config) {
	if config.GCPProjectID != "" {
		ps, err := NewGCPPubSub(context.Background(), config.GCPProjectID)
		if err == nil {
			m.ps = ps
			log.Printf("Initialized GCP PubSub with project %s", config.GCPProjectID)
			return
		}
		log.Printf("Failed to initialize GCP PubSub: %v. Falling back to In-Memory.", err)
	}

	m.ps = newInMemoryPubSub()
	log.Println("Initialized In-Memory PubSub")
}

// cleanupPubSub cleans up the PubSub system.
func (m *MemoryStore) cleanupPubSub() {
	if m.ps != nil {
		m.ps.Close()
	}
}
