// memorystore/memorystore.go
// Package memorystore provides a simple in-memory cache implementation with automatic cleanup
// of expired items. It supports both raw byte storage and JSON serialization/deserialization
// of structured data using Go's encoding/json/v2 package.
package memorystore

import (
	"context"
	json "encoding/json/v2"
	"hash/maphash"
	"log"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// numShards is the number of storage shards. Must be a power of 2 and <= 256.
const numShards = 256

// emptyHeads is a pre-initialized sentinel array of -1 values used by batch
// operations to group keys by shard without heap-allocating per-shard maps.
var emptyHeads [numShards]int

func init() {
	for i := range emptyHeads {
		emptyHeads[i] = -1
	}
}

// item represents a single cache entry with its value and monotonic expiration timestamp.
type item struct {
	value     []byte // Raw data stored as a byte slice
	expiresAt int64  // Monotonic nanoseconds since MemoryStore.baseTime
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
	_     [32]byte // Pad to 64-byte cache line to avoid false sharing between adjacent shards
}

// MemoryStore implements an in-memory cache with automatic cleanup of expired items.
// It is safe for concurrent use by multiple goroutines.
type MemoryStore struct {
	// lifecycleMu protects the lifecycle state (cancelFunc)
	lifecycleMu sync.RWMutex

	hashSeed   maphash.Seed       // Per-instance seed for hardware-accelerated shard hashing
	baseTime   time.Time          // Monotonic reference time for pointer-free item expiration
	shards     [numShards]shard   // Inline cache-line padded sharded storage
	ps         PubSubClient       // PubSub client for cache events
	ctx        context.Context    // Context for controlling the cleanup worker
	cancelFunc context.CancelFunc // Function to stop the cleanup worker
	wg         sync.WaitGroup     // WaitGroup for cleanup goroutine synchronization

	// Metrics
	hits      atomic.Int64 // Atomic counter for cache hits
	misses    atomic.Int64 // Atomic counter for cache misses
	evictions atomic.Int64 // Atomic counter for evicted items
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
		hashSeed:   maphash.MakeSeed(),
		baseTime:   time.Now(),
		ctx:        ctx,
		cancelFunc: cancel,
	}

	for i := range ms.shards {
		ms.shards[i].store = make(map[string]item)
	}

	ms.initPubSub(config)
	ms.startCleanupWorker()
	return ms
}

// nowNano returns monotonic nanoseconds elapsed since the store was created.
func (m *MemoryStore) nowNano() int64 {
	return int64(time.Since(m.baseTime))
}

// expireAt returns the monotonic expiration timestamp for the given duration.
func (m *MemoryStore) expireAt(duration time.Duration) int64 {
	now := m.nowNano()
	if duration <= 0 {
		return now
	}
	exp := now + int64(duration)
	if exp < now {
		return math.MaxInt64
	}
	return exp
}

// shardIndex returns the shard index responsible for the given key.
func (m *MemoryStore) shardIndex(key string) uint8 {
	return uint8(maphash.String(m.hashSeed, key) & (numShards - 1))
}

// getShard returns the shard responsible for the given key.
func (m *MemoryStore) getShard(key string) *shard {
	return &m.shards[m.shardIndex(key)]
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
	for i := range m.shards {
		s := &m.shards[i]
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
	m.wg.Go(func() {
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
	})
}

// cleanupExpiredItems removes all expired items from the cache.
// It iterates over shards and cleans them one by one to avoid global locking.
func (m *MemoryStore) cleanupExpiredItems() {
	now := m.nowNano()
	var evicted int64
	for i := range m.shards {
		s := &m.shards[i]
		// Lock only the current shard
		s.mu.Lock()
		for key, it := range s.store {
			if now >= it.expiresAt {
				delete(s.store, key)
				evicted++
			}
		}
		s.mu.Unlock()
	}
	if evicted > 0 {
		m.evictions.Add(evicted)
	}
}

// Set stores a raw byte slice in the cache with the specified key and duration.
// The item will automatically expire after the specified duration.
// If an error occurs, it will be returned to the caller.
func (m *MemoryStore) Set(key string, value []byte, duration time.Duration) error {
	exp := m.expireAt(duration)
	s := m.getShard(key)
	s.mu.Lock()
	s.store[key] = item{
		value:     value,
		expiresAt: exp,
	}
	s.mu.Unlock()

	return nil
}

// SetJSON stores a JSON-serializable value in the cache using encoding/json/v2.
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
func (m *MemoryStore) SetJSON(key string, value any, duration time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return m.Set(key, data, duration)
}

// SetJSONWithOptions stores a JSON-serializable value in the cache using encoding/json/v2
// with the provided marshal/encode options (such as json.Deterministic or jsonv1.DefaultOptionsV1).
func (m *MemoryStore) SetJSONWithOptions(key string, value any, duration time.Duration, opts ...json.Options) error {
	data, err := json.Marshal(value, opts...)
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
	it, exists := s.store[key]
	s.mu.RUnlock()

	if !exists || m.nowNano() >= it.expiresAt {
		m.misses.Add(1)
		return nil, false
	}

	m.hits.Add(1)
	return it.value, true
}

// GetJSON retrieves and deserializes a JSON value from the cache into the provided destination
// using encoding/json/v2.
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
func (m *MemoryStore) GetJSON(key string, dest any) (bool, error) {
	data, exists := m.Get(key)
	if !exists {
		return false, nil
	}

	if err := json.Unmarshal(data, dest); err != nil {
		return true, err
	}

	return true, nil
}

// GetJSONWithOptions retrieves and deserializes a JSON value from the cache into the provided
// destination using encoding/json/v2 with the given unmarshal/decode options.
func (m *MemoryStore) GetJSONWithOptions(key string, dest any, opts ...json.Options) (bool, error) {
	data, exists := m.Get(key)
	if !exists {
		return false, nil
	}

	if err := json.Unmarshal(data, dest, opts...); err != nil {
		return true, err
	}

	return true, nil
}

// Delete removes an item from the cache.
// If the key doesn't exist, the operation is a no-op.
func (m *MemoryStore) Delete(key string) {
	s := m.getShard(key)
	s.mu.Lock()
	delete(s.store, key)
	s.mu.Unlock()
}

type multiSetEntry struct {
	key   string
	value []byte
	next  int
}

// SetMulti stores multiple key-value pairs in the cache.
// This is more efficient than calling Set multiple times as it groups keys by shard.
// All items will have the same expiration duration.
func (m *MemoryStore) SetMulti(items map[string][]byte, duration time.Duration) error {
	if len(items) == 0 {
		return nil
	}

	expiresAt := m.expireAt(duration)
	heads := emptyHeads
	var touched [numShards]uint8
	touchedCount := 0
	entries := make([]multiSetEntry, 0, len(items))

	for key, value := range items {
		idx := m.shardIndex(key)
		if heads[idx] == -1 {
			touched[touchedCount] = idx
			touchedCount++
		}
		entries = append(entries, multiSetEntry{
			key:   key,
			value: value,
			next:  heads[idx],
		})
		heads[idx] = len(entries) - 1
	}

	// Apply updates per touched shard
	for _, shardIdx := range touched[:touchedCount] {
		s := &m.shards[shardIdx]
		s.mu.Lock()
		for e := heads[shardIdx]; e != -1; e = entries[e].next {
			s.store[entries[e].key] = item{
				value:     entries[e].value,
				expiresAt: expiresAt,
			}
		}
		s.mu.Unlock()
	}

	return nil
}

// GetMulti retrieves multiple values from the cache.
// It returns a map of found items. Keys that don't exist or are expired are omitted.
func (m *MemoryStore) GetMulti(keys []string) map[string][]byte {
	result := make(map[string][]byte, len(keys))
	if len(keys) == 0 {
		return result
	}

	now := m.nowNano()
	heads := emptyHeads
	var touched [numShards]uint8
	touchedCount := 0
	next := make([]int, len(keys))

	for i, key := range keys {
		idx := m.shardIndex(key)
		if heads[idx] == -1 {
			touched[touchedCount] = idx
			touchedCount++
		}
		next[i] = heads[idx]
		heads[idx] = i
	}

	var hits, misses int64
	for _, shardIdx := range touched[:touchedCount] {
		s := &m.shards[shardIdx]
		s.mu.RLock()
		for i := heads[shardIdx]; i != -1; i = next[i] {
			key := keys[i]
			if it, exists := s.store[key]; exists && now < it.expiresAt {
				result[key] = it.value
				hits++
			} else {
				misses++
			}
		}
		s.mu.RUnlock()
	}

	if hits > 0 {
		m.hits.Add(hits)
	}
	if misses > 0 {
		m.misses.Add(misses)
	}

	return result
}

// GetMetrics returns the current statistics of the MemoryStore.
// It returns a copy of the metrics to ensure thread safety.
func (m *MemoryStore) GetMetrics() StoreMetrics {
	itemCount := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		itemCount += len(s.store)
		s.mu.RUnlock()
	}

	return StoreMetrics{
		Items:     itemCount,
		Hits:      m.hits.Load(),
		Misses:    m.misses.Load(),
		Evictions: m.evictions.Load(),
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
		_ = m.ps.Close()
	}
}
