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
	"uuid"
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

	// evictionSampleSize is the number of entries inspected per shard when
	// selecting a victim for capacity eviction (volatile-TTL sampling).
	evictionSampleSize = 8
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

// evictedEntry buffers an eviction notification so OnEvict runs outside shard locks.
type evictedEntry struct {
	key    string
	value  []byte
	reason EvictReason
}

// StoreMetrics holds statistics about the cache usage.
type StoreMetrics struct {
	Items     int   // Current number of items in the cache
	Bytes     int64 // Current stored key + value payload bytes
	Hits      int64 // Total number of cache hits
	Misses    int64 // Total number of cache misses
	Evictions int64 // Total number of items evicted (expired or capacity)
}

// shard holds one partition of the cache along with shard-local counters.
// Keeping counters per-shard eliminates cross-core cache-line contention on
// hot Get/Set paths while fitting into a single 64-byte cache line on 64-bit
// platforms (24B RWMutex + 8B map + 24B atomic counters + 8B bytes = 64B).
type shard struct {
	mu        sync.RWMutex
	store     map[string]item
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
	bytes     int64 // Guarded by mu; total len(key)+len(value) in this shard
}

// MemoryStore implements a thread-safe, sharded in-memory cache with automatic
// cleanup of expired items, optional bounded capacity, stampede protection,
// Go 1.27 generic JSON helpers, and a pluggable Publish/Subscribe system with
// optional distributed cache invalidation.
type MemoryStore struct {
	// lifecycleMu protects the lifecycle state (cancelFunc)
	lifecycleMu sync.RWMutex

	hashSeed          maphash.Seed     // Per-instance seed for hardware-accelerated shard hashing
	baseTime          time.Time        // Monotonic reference time for pointer-free item expiration
	shards            [numShards]shard // Inline 64-byte cache-line aligned sharded storage
	ps                PubSubClient     // Pluggable Pub/Sub client
	pubSubTimeout     time.Duration    // Default timeout for non-context Pub/Sub helpers
	instanceID        string           // Unique store instance ID for invalidation echo suppression
	invalidationTopic string           // Optional Pub/Sub topic for distributed cache invalidation
	maxItems          int64            // Soft global max items (<=0 unbounded)
	maxBytes          int64            // Soft global max payload bytes (<=0 unbounded)
	bounded           bool             // True when maxItems > 0 || maxBytes > 0
	totalItems        atomic.Int64     // Tracked only when bounded == true
	totalBytes        atomic.Int64     // Tracked only when bounded == true
	onEvict           func(string, []byte, EvictReason)
	sf                singleflightGroup  // Zero-dependency singleflight for GetOrLoad / GetOrLoadJSON
	cleanupInterval   time.Duration      // Background expiration sweep interval
	logger            *slog.Logger       // Structured logger (defaults to discard)
	ctx               context.Context    // Context for controlling background workers
	cancelFunc        context.CancelFunc // Function to stop background workers
	wg                sync.WaitGroup     // WaitGroup for background goroutine synchronization
}

// NewMemoryStore creates and initializes a new MemoryStore instance using
// default settings (InMemoryPubSub, 1-minute cleanup interval, silent logger).
// Use NewMemoryStoreWithConfig to configure custom PubSubClient, distributed
// invalidation, capacity limits, eviction callbacks, or logging.
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

	maxItems := int64(max(config.MaxItems, 0))
	maxBytes := max(config.MaxBytes, 0)

	ms := &MemoryStore{
		hashSeed:          maphash.MakeSeed(),
		baseTime:          time.Now(),
		ps:                ps,
		pubSubTimeout:     config.PubSubTimeout,
		instanceID:        uuid.New().String(),
		invalidationTopic: config.InvalidationTopic,
		maxItems:          maxItems,
		maxBytes:          maxBytes,
		bounded:           maxItems > 0 || maxBytes > 0,
		onEvict:           config.OnEvict,
		cleanupInterval:   cleanupInterval,
		logger:            logger,
		ctx:               ctx,
		cancelFunc:        cancel,
	}

	for i := range ms.shards {
		ms.shards[i].store = make(map[string]item)
	}

	ms.startCleanupWorker()
	ms.startInvalidationBackplane()
	return ms
}

// InstanceID returns the unique RFC 9562 UUID assigned to this MemoryStore
// instance (used for echo suppression in distributed cache invalidation).
func (m *MemoryStore) InstanceID() string {
	return m.instanceID
}

// nowNano returns monotonic nanoseconds elapsed since the store was created.
func (m *MemoryStore) nowNano() int64 {
	return int64(time.Since(m.baseTime))
}

// calcExpiresAt computes the monotonic expiration timestamp from now and duration.
func calcExpiresAt(now int64, duration time.Duration) int64 {
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

func payloadSize(key string, value []byte) int64 {
	return int64(len(key) + len(value))
}

func (m *MemoryStore) notifyEvicted(entries []evictedEntry) {
	if m.onEvict == nil || len(entries) == 0 {
		return
	}
	for _, e := range entries {
		m.onEvict(e.key, e.value, e.reason)
	}
}

// Stop gracefully shuts down the MemoryStore by stopping background workers,
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

	// Wait for cleanup and invalidation goroutines to finish.
	m.wg.Wait()

	// Clear the store to free up memory.
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		s.store = nil
		s.bytes = 0
		s.mu.Unlock()
	}
	if m.bounded {
		m.totalItems.Store(0)
		m.totalBytes.Store(0)
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
	var evictedBuf []evictedEntry
	for i := range m.shards {
		s := &m.shards[i]
		var evicted int64
		var freedBytes int64
		s.mu.Lock()
		for key, it := range s.store {
			if now >= it.expiresAt {
				size := payloadSize(key, it.value)
				delete(s.store, key)
				s.bytes -= size
				freedBytes += size
				evicted++
				if m.onEvict != nil {
					evictedBuf = append(evictedBuf, evictedEntry{key: key, value: it.value, reason: EvictExpired})
				}
			}
		}
		s.mu.Unlock()
		if evicted > 0 {
			s.evictions.Add(evicted)
			if m.bounded {
				m.totalItems.Add(-evicted)
				m.totalBytes.Add(-freedBytes)
			}
		}
	}
	m.notifyEvicted(evictedBuf)
}

// overBounds reports whether the store currently exceeds MaxItems or MaxBytes.
func (m *MemoryStore) overBounds() bool {
	if m.maxItems > 0 && m.totalItems.Load() > m.maxItems {
		return true
	}
	if m.maxBytes > 0 && m.totalBytes.Load() > m.maxBytes {
		return true
	}
	return false
}

// evictOneLocked removes one victim from s (preferring expired items, then the
// earliest-expiring item among evictionSampleSize random entries) while skipping
// protectedKey if other candidates exist. Caller must hold s.mu.Lock().
func (m *MemoryStore) evictOneLocked(s *shard, now int64, protectedKey string) (evictedEntry, bool) {
	if len(s.store) == 0 {
		return evictedEntry{}, false
	}

	var victimKey string
	var victimItem item
	var victimReason EvictReason
	found := false
	sampled := 0

	for k, it := range s.store {
		if protectedKey != "" && k == protectedKey {
			continue
		}
		if now >= it.expiresAt {
			victimKey = k
			victimItem = it
			victimReason = EvictExpired
			found = true
			break
		}
		if !found || it.expiresAt < victimItem.expiresAt {
			victimKey = k
			victimItem = it
			victimReason = EvictCapacity
			found = true
		}
		sampled++
		if sampled >= evictionSampleSize {
			break
		}
	}

	if !found {
		return evictedEntry{}, false
	}

	size := payloadSize(victimKey, victimItem.value)
	delete(s.store, victimKey)
	s.bytes -= size
	s.evictions.Add(1)
	m.totalItems.Add(-1)
	m.totalBytes.Add(-size)
	return evictedEntry{key: victimKey, value: victimItem.value, reason: victimReason}, true
}

// enforceBounds evicts entries starting from startShard until totalItems and
// totalBytes are within configured limits (or no non-protected entries remain).
func (m *MemoryStore) enforceBounds(startShard uint8, now int64, protectedKey string) {
	if !m.overBounds() {
		return
	}

	var notified []evictedEntry
	for pass := 0; pass < 2 && m.overBounds(); pass++ {
		madeProgress := false
		for offset := range numShards {
			if !m.overBounds() {
				break
			}
			idx := uint8((int(startShard) + offset) & (numShards - 1))
			s := &m.shards[idx]
			s.mu.Lock()
			for m.overBounds() {
				skip := ""
				if pass == 0 && idx == startShard {
					skip = protectedKey
				}
				entry, ok := m.evictOneLocked(s, now, skip)
				if !ok {
					break
				}
				madeProgress = true
				if m.onEvict != nil {
					notified = append(notified, entry)
				}
				// If only protectedKey remained on pass 0, move to next shard.
				if pass == 0 && idx == startShard && len(s.store) <= 1 {
					break
				}
			}
			s.mu.Unlock()
		}
		if !madeProgress {
			break
		}
	}

	m.notifyEvicted(notified)
}

// Set stores a raw byte slice in the cache with the specified key and duration.
// Items with duration <= 0 expire immediately.
func (m *MemoryStore) Set(key string, value []byte, duration time.Duration) error {
	now := m.nowNano()
	exp := calcExpiresAt(now, duration)

	idx := m.shardIndex(key)
	s := &m.shards[idx]

	if !m.bounded {
		s.mu.Lock()
		s.store[key] = item{
			value:     value,
			expiresAt: exp,
		}
		s.mu.Unlock()
	} else {
		newSize := payloadSize(key, value)
		s.mu.Lock()
		var deltaItems, deltaBytes int64
		if old, existed := s.store[key]; existed {
			deltaBytes = int64(len(value)) - int64(len(old.value))
		} else {
			deltaItems = 1
			deltaBytes = newSize
		}
		s.store[key] = item{
			value:     value,
			expiresAt: exp,
		}
		s.bytes += deltaBytes
		s.mu.Unlock()

		if deltaItems != 0 {
			m.totalItems.Add(deltaItems)
		}
		if deltaBytes != 0 {
			m.totalBytes.Add(deltaBytes)
		}
		if m.overBounds() {
			m.enforceBounds(idx, now, key)
		}
	}

	if m.invalidationTopic != "" {
		m.broadcastInvalidation(key)
	}

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

// deleteLocal removes key from local storage without broadcasting invalidation.
func (m *MemoryStore) deleteLocal(key string) {
	s := m.getShard(key)
	s.mu.Lock()
	old, existed := s.store[key]
	if existed {
		size := payloadSize(key, old.value)
		delete(s.store, key)
		s.bytes -= size
		if m.bounded {
			m.totalItems.Add(-1)
			m.totalBytes.Add(-size)
		}
	}
	s.mu.Unlock()

	if existed && m.onEvict != nil {
		m.onEvict(key, old.value, EvictDeleted)
	}
}

// Delete removes an item from the cache.
// If the key doesn't exist, the operation is a no-op locally (but still
// broadcasts an invalidation if Config.InvalidationTopic is configured).
func (m *MemoryStore) Delete(key string) {
	m.deleteLocal(key)
	if m.invalidationTopic != "" {
		m.broadcastInvalidation(key)
	}
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

	now := m.nowNano()
	expiresAt := calcExpiresAt(now, duration)

	var totalDeltaItems, totalDeltaBytes int64

	if n <= smallBatchThreshold {
		var lastIdx uint8
		for key, value := range items {
			idx := m.shardIndex(key)
			lastIdx = idx
			s := &m.shards[idx]
			if !m.bounded {
				s.mu.Lock()
				s.store[key] = item{value: value, expiresAt: expiresAt}
				s.mu.Unlock()
			} else {
				s.mu.Lock()
				var dItems, dBytes int64
				if old, existed := s.store[key]; existed {
					dBytes = int64(len(value)) - int64(len(old.value))
				} else {
					dItems = 1
					dBytes = payloadSize(key, value)
				}
				s.store[key] = item{value: value, expiresAt: expiresAt}
				s.bytes += dBytes
				s.mu.Unlock()
				totalDeltaItems += dItems
				totalDeltaBytes += dBytes
			}
		}
		if m.bounded {
			if totalDeltaItems != 0 {
				m.totalItems.Add(totalDeltaItems)
			}
			if totalDeltaBytes != 0 {
				m.totalBytes.Add(totalDeltaBytes)
			}
			if m.overBounds() {
				m.enforceBounds(lastIdx, now, "")
			}
		}
		if m.invalidationTopic != "" {
			keys := make([]string, 0, n)
			for k := range items {
				keys = append(keys, k)
			}
			m.broadcastInvalidation(keys...)
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
		if !m.bounded {
			s.mu.Lock()
			for e := heads[shardIdx]; e != -1; e = entries[e].next {
				s.store[entries[e].key] = item{
					value:     entries[e].value,
					expiresAt: expiresAt,
				}
			}
			s.mu.Unlock()
		} else {
			var dItems, dBytes int64
			s.mu.Lock()
			for e := heads[shardIdx]; e != -1; e = entries[e].next {
				k := entries[e].key
				v := entries[e].value
				if old, existed := s.store[k]; existed {
					dBytes += int64(len(v)) - int64(len(old.value))
				} else {
					dItems++
					dBytes += payloadSize(k, v)
				}
				s.store[k] = item{
					value:     v,
					expiresAt: expiresAt,
				}
			}
			s.bytes += dBytes
			s.mu.Unlock()
			totalDeltaItems += dItems
			totalDeltaBytes += dBytes
		}
	}

	if m.bounded {
		if totalDeltaItems != 0 {
			m.totalItems.Add(totalDeltaItems)
		}
		if totalDeltaBytes != 0 {
			m.totalBytes.Add(totalDeltaBytes)
		}
		if m.overBounds() {
			m.enforceBounds(touched[0], now, "")
		}
	}

	if m.invalidationTopic != "" {
		keys := make([]string, 0, len(entries))
		for _, e := range entries {
			keys = append(keys, e.key)
		}
		m.broadcastInvalidation(keys...)
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
		if m.bounded {
			metrics.Bytes += s.bytes
		} else {
			for k, it := range s.store {
				metrics.Bytes += payloadSize(k, it.value)
			}
		}
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
