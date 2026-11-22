package memorystore

import (
	"context"
	json "encoding/json/v2"
	"hash/maphash"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// numShards is the number of storage shards. Must be a power of 2 and <= 256.
	numShards = 256

	// defaultCleanupInterval is the default period for background expiration sweeps.
	defaultCleanupInterval = time.Minute

	// batchStackLimit is the maximum batch size that uses stack-allocated scratch
	// storage in SetMulti / GetMulti before spilling to a single heap slice.
	batchStackLimit = 128

	// smallBatchThreshold is the batch size at or below which direct per-key
	// shard operations avoid initializing the 256-entry shard head table.
	smallBatchThreshold = 4
)

// emptyHeads is a pre-initialized sentinel array of -1 values used by batch
// operations to group keys by shard without heap-allocating per-shard maps.
var emptyHeads [numShards]int32

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

// shard holds one partition of the cache along with shard-local atomic counters.
// Keeping counters per-shard eliminates cross-core cache-line contention on
// hot Get/Set paths while fitting cleanly into a single 64-byte cache line on
// 64-bit platforms (24B RWMutex + 8B map + 24B counters + 8B pad = 64B).
type shard struct {
	mu        sync.RWMutex
	store     map[string]item
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
	_         [8]byte
}

// MemoryStore implements a thread-safe, sharded in-memory cache with automatic
// cleanup of expired items and a pluggable Publish/Subscribe system.
type MemoryStore struct {
	// lifecycleMu protects the lifecycle state (cancelFunc)
	lifecycleMu sync.RWMutex

	hashSeed        maphash.Seed       // Per-instance seed for hardware-accelerated shard hashing
	baseTime        time.Time          // Monotonic reference time for pointer-free item expiration
	shards          [numShards]shard   // Inline cache-line padded sharded storage
	ps              PubSubClient       // Pluggable Pub/Sub client
	pubSubTimeout   time.Duration      // Default timeout for non-context Pub/Sub helpers
	cleanupInterval time.Duration      // Background expiration sweep interval
	logger          *slog.Logger       // Structured logger (defaults to discard)
	ctx             context.Context    // Context for controlling the cleanup worker
	cancelFunc      context.CancelFunc // Function to stop the cleanup worker
	wg              sync.WaitGroup     // WaitGroup for cleanup goroutine synchronization
}

// NewMemoryStore creates and initializes a new MemoryStore instance using
// default settings (InMemoryPubSub, 1-minute cleanup interval, silent logger).
// Use NewMemoryStoreWithConfig to inject a custom PubSubClient, logger,
// cleanup interval, or Pub/Sub timeout.
func NewMemoryStore() *MemoryStore {
	return NewMemoryStoreWithConfig(Config{})
}

// NewMemoryStoreWithConfig creates a new MemoryStore with the provided configuration.
func NewMemoryStoreWithConfig(config Config) *MemoryStore {
	ctx, cancel := context.WithCancel(context.Background())

	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	cleanupInterval := config.CleanupInterval
	if cleanupInterval <= 0 {
		cleanupInterval = defaultCleanupInterval
	}

	ps := config.PubSub
	if ps == nil {
		ps = NewInMemoryPubSub()
		logger.Debug("initialized default in-memory pubsub")
	}

	ms := &MemoryStore{
		hashSeed:        maphash.MakeSeed(),
		baseTime:        time.Now(),
		ps:              ps,
		pubSubTimeout:   config.PubSubTimeout,
		cleanupInterval: cleanupInterval,
		logger:          logger,
		ctx:             ctx,
		cancelFunc:      cancel,
	}

	for i := range ms.shards {
		ms.shards[i].store = make(map[string]item)
	}

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

// Stop gracefully shuts down the MemoryStore by stopping the cleanup goroutine,
// closing the configured PubSubClient, and releasing stored data.
// After calling Stop, the store cannot be used. Multiple calls to Stop are safe
// and return nil.
func (m *MemoryStore) Stop() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if m.cancelFunc == nil {
		return nil
	}

	m.cancelFunc()
	m.cancelFunc = nil

	var closeErr error
	if m.ps != nil {
		if err := m.ps.Close(); err != nil {
			closeErr = err
			m.logger.Error("error closing pubsub client", "error", err)
		}
	}

	// Wait for cleanup goroutine to finish.
	m.wg.Wait()

	// Clear the store to free up memory.
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		s.store = nil
		s.mu.Unlock()
	}

	return closeErr
}

// IsStopped returns true if the MemoryStore has been stopped and can no longer be used.
// This method is safe for concurrent use.
func (m *MemoryStore) IsStopped() bool {
	m.lifecycleMu.RLock()
	defer m.lifecycleMu.RUnlock()
	return m.cancelFunc == nil
}

// startCleanupWorker initiates a background goroutine that periodically
// removes expired items from the cache.
func (m *MemoryStore) startCleanupWorker() {
	m.wg.Go(func() {
		ticker := time.NewTicker(m.cleanupInterval)
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
// It iterates over shards one by one to avoid global locking.
func (m *MemoryStore) cleanupExpiredItems() {
	now := m.nowNano()
	for i := range m.shards {
		s := &m.shards[i]
		var evicted int64
		s.mu.Lock()
		for key, it := range s.store {
			if now >= it.expiresAt {
				delete(s.store, key)
				evicted++
			}
		}
		s.mu.Unlock()
		if evicted > 0 {
			s.evictions.Add(evicted)
		}
	}
}

// Set stores a raw byte slice in the cache with the specified key and duration.
// Items with duration <= 0 expire immediately.
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
// Returns an error if JSON marshaling fails.
func (m *MemoryStore) SetJSON(key string, value any, duration time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return m.Set(key, data, duration)
}

// SetJSONWithOptions stores a JSON-serializable value in the cache using encoding/json/v2
// with the provided marshal/encode options (such as json.Deterministic or encoding/json.DefaultOptionsV1).
func (m *MemoryStore) SetJSONWithOptions(key string, value any, duration time.Duration, opts ...json.Options) error {
	data, err := json.Marshal(value, opts...)
	if err != nil {
		return err
	}
	return m.Set(key, data, duration)
}

// Get retrieves a value from the cache.
// Returns the value and true if the key exists and has not expired; otherwise returns (nil, false).
func (m *MemoryStore) Get(key string) ([]byte, bool) {
	s := m.getShard(key)
	s.mu.RLock()
	it, exists := s.store[key]
	s.mu.RUnlock()

	if !exists || m.nowNano() >= it.expiresAt {
		s.misses.Add(1)
		return nil, false
	}

	s.hits.Add(1)
	return it.value, true
}

// GetJSON retrieves and deserializes a JSON value from the cache into dest
// using encoding/json/v2.
// Returns (false, nil) if the key does not exist or has expired.
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

// GetJSONWithOptions retrieves and deserializes a JSON value from the cache into dest
// using encoding/json/v2 with the given unmarshal/decode options.
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
	next  int32
}

// SetMulti stores multiple key-value pairs in the cache with the same expiration duration.
// Keys are grouped by shard to minimize lock acquisitions and heap allocations.
func (m *MemoryStore) SetMulti(items map[string][]byte, duration time.Duration) error {
	n := len(items)
	if n == 0 {
		return nil
	}

	expiresAt := m.expireAt(duration)
	if n <= smallBatchThreshold {
		for key, value := range items {
			s := m.getShard(key)
			s.mu.Lock()
			s.store[key] = item{value: value, expiresAt: expiresAt}
			s.mu.Unlock()
		}
		return nil
	}

	heads := emptyHeads
	var touched [numShards]uint8
	touchedCount := 0

	var stackEntries [batchStackLimit]multiSetEntry
	var entries []multiSetEntry
	if n <= batchStackLimit {
		entries = stackEntries[:0]
	} else {
		entries = make([]multiSetEntry, 0, n)
	}

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
		heads[idx] = int32(len(entries) - 1)
	}

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
// Keys that don't exist or have expired are omitted from the returned map.
func (m *MemoryStore) GetMulti(keys []string) map[string][]byte {
	n := len(keys)
	result := make(map[string][]byte, n)
	if n == 0 {
		return result
	}

	now := m.nowNano()
	if n <= smallBatchThreshold {
		for _, key := range keys {
			s := m.getShard(key)
			s.mu.RLock()
			it, exists := s.store[key]
			s.mu.RUnlock()
			if exists && now < it.expiresAt {
				result[key] = it.value
				s.hits.Add(1)
			} else {
				s.misses.Add(1)
			}
		}
		return result
	}

	heads := emptyHeads
	var touched [numShards]uint8
	touchedCount := 0

	var stackNext [batchStackLimit]int32
	var next []int32
	if n <= batchStackLimit {
		next = stackNext[:n]
	} else {
		next = make([]int32, n)
	}

	for i, key := range keys {
		idx := m.shardIndex(key)
		if heads[idx] == -1 {
			touched[touchedCount] = idx
			touchedCount++
		}
		next[i] = heads[idx]
		heads[idx] = int32(i)
	}

	for _, shardIdx := range touched[:touchedCount] {
		s := &m.shards[shardIdx]
		var hits, misses int64
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
		if hits > 0 {
			s.hits.Add(hits)
		}
		if misses > 0 {
			s.misses.Add(misses)
		}
	}

	return result
}

// GetMetrics returns a point-in-time snapshot of cache statistics.
func (m *MemoryStore) GetMetrics() StoreMetrics {
	var metrics StoreMetrics
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		metrics.Items += len(s.store)
		s.mu.RUnlock()
		metrics.Hits += s.hits.Load()
		metrics.Misses += s.misses.Load()
		metrics.Evictions += s.evictions.Load()
	}

	return metrics
}

func (m *MemoryStore) defaultPubSubContext() (context.Context, context.CancelFunc) {
	if m.pubSubTimeout > 0 {
		return context.WithTimeout(context.Background(), m.pubSubTimeout)
	}
	return context.Background(), func() {}
}

// Subscribe subscribes to a topic using the store's default Pub/Sub context
// (applying Config.PubSubTimeout if configured).
func (m *MemoryStore) Subscribe(topic string) (<-chan []byte, error) {
	ctx, cancel := m.defaultPubSubContext()
	defer cancel()
	return m.SubscribeContext(ctx, topic)
}

// SubscribeContext subscribes to a topic with the provided context.
func (m *MemoryStore) SubscribeContext(ctx context.Context, topic string) (<-chan []byte, error) {
	if m.IsStopped() {
		return nil, ErrStoreStopped
	}
	return m.ps.Subscribe(ctx, topic)
}

// Publish publishes a message to a topic using the store's default Pub/Sub context
// (applying Config.PubSubTimeout if configured).
func (m *MemoryStore) Publish(topic string, message []byte) error {
	ctx, cancel := m.defaultPubSubContext()
	defer cancel()
	return m.PublishContext(ctx, topic, message)
}

// PublishContext publishes a message to a topic with the provided context.
func (m *MemoryStore) PublishContext(ctx context.Context, topic string, message []byte) error {
	if m.IsStopped() {
		return ErrStoreStopped
	}
	return m.ps.Publish(ctx, topic, message)
}

// Unsubscribe unsubscribes from a topic using the store's default Pub/Sub context
// (applying Config.PubSubTimeout if configured).
func (m *MemoryStore) Unsubscribe(topic string) error {
	ctx, cancel := m.defaultPubSubContext()
	defer cancel()
	return m.UnsubscribeContext(ctx, topic)
}

// UnsubscribeContext unsubscribes from a topic with the provided context.
func (m *MemoryStore) UnsubscribeContext(ctx context.Context, topic string) error {
	if m.IsStopped() {
		return ErrStoreStopped
	}
	return m.ps.Unsubscribe(ctx, topic)
}

// SubscriberCount returns the number of subscribers for a topic or pattern
// if the configured PubSubClient implements SubscriberCounter; otherwise returns 0.
func (m *MemoryStore) SubscriberCount(pattern string) int {
	if counter, ok := m.ps.(SubscriberCounter); ok {
		return counter.SubscriberCount(pattern)
	}
	return 0
}
