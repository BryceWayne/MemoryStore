package memorystore

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"hash/maphash"
	"log/slog"
	"math"
	"math/rand/v2"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"uuid"
)

// NoExpiration indicates that a cache entry should never expire based on TTL.
// Entries with NoExpiration remain eligible for capacity eviction (EvictCapacity)
// when MaxItems or MaxBytes is configured.
const NoExpiration time.Duration = -1

const (
	// numShards is the maximum number of storage shards. Must be a power of 2 and <= 256.
	numShards = 256

	// minItemsPerShard, minBytesPerShard, and singleShardMaxItems govern adaptive
	// active shard sizing for bounded stores so S3-FIFO / W-TinyLFU retain
	// coherent multi-item per-shard state at small capacities.
	minItemsPerShard    = 64
	minBytesPerShard    = 8192
	singleShardMaxItems = 256

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

	// defaultInvalidationQueueSize is the default bounded async invalidation
	// queue depth when InvalidationTopic is configured.
	defaultInvalidationQueueSize = 4096

	// defaultWriteThroughMaxBytes is the default maximum encoded payload size
	// for InvalidationModeWriteThrough broadcasts before falling back to
	// InvalidationModeInvalidateOnly.
	defaultWriteThroughMaxBytes = 16 * 1024

	// defaultInvalidationPubSubTimeout is applied when InvalidationTopic is
	// configured and PubSubTimeout <= 0.
	defaultInvalidationPubSubTimeout = 5 * time.Second

	// defaultSWRRefreshTimeout bounds detached-context background refreshes
	// triggered by Stale-While-Revalidate hits.
	defaultSWRRefreshTimeout = 30 * time.Second
)

// emptyHeads is a pre-initialized sentinel array of -1 values used by batch
// operations to group keys by shard without heap-allocating per-shard maps.
var emptyHeads [numShards]int32

func init() {
	for i := range emptyHeads {
		emptyHeads[i] = -1
	}
}

type setOptions struct {
	jitter        float64
	hasJitter     bool
	sliding       bool
	noExpiration  bool
	staleGrace    time.Duration
	hasStaleGrace bool
	ttlCapNanos   int64
	err           error
}

// SetOption configures per-entry behavior for MemoryStore.SetWithOptions.
type SetOption func(*setOptions)

// WithTTLJitter applies randomized +/- fraction jitter to the entry's positive
// TTL (validated in [0.0, 0.5]), overriding any store-wide DefaultTTLJitter.
func WithTTLJitter(fraction float64) SetOption {
	return func(o *setOptions) {
		if math.IsNaN(fraction) || fraction < 0 || fraction > 0.5 {
			if o.err == nil {
				o.err = fmt.Errorf("invalid ttl jitter %v: must be in [0.0, 0.5]", fraction)
			}
			return
		}
		o.jitter = fraction
		o.hasJitter = true
	}
}

// WithSlidingExpiration refreshes the entry's TTL on each cache hit.
func WithSlidingExpiration() SetOption {
	return func(o *setOptions) {
		o.sliding = true
	}
}

// WithNoExpiration marks the entry as never expiring due to TTL, regardless of
// the duration passed to SetWithOptions.
func WithNoExpiration() SetOption {
	return func(o *setOptions) {
		o.noExpiration = true
	}
}

// WithStaleWindow configures the Stale-While-Revalidate (SWR) grace window
// after the entry's soft TTL expires. During [softExpiresAt, softExpiresAt+grace),
// Get treats the entry as a miss while GetOrLoad / GetOrLoadJSON serve the stale
// value immediately and coalesce a detached-context background refresh.
// Negative durations fail SetWithOptions with a validation error.
func WithStaleWindow(grace time.Duration) SetOption {
	return func(o *setOptions) {
		if grace < 0 {
			if o.err == nil {
				o.err = fmt.Errorf("invalid stale window %v: must be >= 0", grace)
			}
			return
		}
		o.staleGrace = grace
		o.hasStaleGrace = true
	}
}

// evictedEntry buffers an eviction notification so OnEvict and MetricsRecorder
// run outside shard locks.
type evictedEntry struct {
	key    string
	value  []byte
	reason EvictReason
}

// StoreMetrics holds statistics about the cache usage.
type StoreMetrics struct {
	Items             int   // Current number of items in the cache
	Bytes             int64 // Current stored key + value payload bytes
	Hits              int64 // Total number of cache hits
	Misses            int64 // Total number of cache misses
	Evictions         int64 // Total number of items evicted (expired or capacity)
	InvalidationDrops int64 // Total number of async invalidation events dropped due to full queue
}

// shard holds one partition of the cache across two 64-byte cache lines (128B
// on 64-bit platforms) so lock-free readers accessing table never contend with
// counter updates or shard.mu writes on cache line 1.
type shard struct {
	// Cache line 0 (64B): read-only between table resizes
	table atomic.Pointer[bucketTable]
	_     [56]byte

	// Cache line 1 (64B): counters + writer lock + accounting
	hits            atomic.Int64
	misses          atomic.Int64
	evictions       atomic.Int64
	mu              sync.Mutex
	bytes           atomic.Int64  // Written under mu; read lock-free in GetMetrics
	count           atomic.Int64  // Written under mu; read lock-free in GetMetrics
	resizeThreshold int64         // Guarded by mu
	evict           *shardEvictor // Non-nil when bounded; pointer immutable after New
}

// MemoryStore implements a thread-safe, sharded in-memory cache with automatic
// cleanup of expired items, optional bounded capacity, stampede protection,
// Go 1.27 generic JSON helpers, real-time observability hooks, and a pluggable
// Publish/Subscribe system with optional distributed cache invalidation.
type MemoryStore struct {
	// Read-hot fields (immutable after New except stopped).
	stopped        atomic.Bool
	preciseClock   bool
	copyOnWrite    bool
	copyOnRead     bool
	trackFreq      bool
	bounded        bool
	evictionPolicy EvictionPolicy
	shardMask      uint8
	hashSeed       maphash.Seed
	baseTime       time.Time
	recorder       MetricsRecorder

	// Isolated 64-byte cache line for the 1ms coarse monotonic clock ticker.
	_         [56]byte
	coarseNow atomic.Int64
	_         [56]byte

	// Inline 128-byte cache-line-aligned sharded storage.
	shards [numShards]shard

	// Bounds, lifecycle, pubsub, and worker coordination.
	lifecycleMu          sync.RWMutex
	maxItems             int64 // Soft global max items (<=0 unbounded)
	maxBytes             int64 // Soft global max payload bytes (<=0 unbounded)
	shardTargetItems     int64
	shardTargetBytes     int64
	totalItems           atomic.Int64
	totalBytes           atomic.Int64
	evictCursor          atomic.Uint32
	defaultTTLJitter     float64
	defaultStaleGrace    time.Duration
	jitterMu             sync.Mutex
	jitterRng            *rand.Rand
	onEvict              func(string, []byte, EvictReason)
	ps                   PubSubClient
	pubSubTimeout        time.Duration
	instanceID           string
	invalidationTopic    string
	invalidationMode     InvalidationMode
	writeThroughMaxBytes int
	asyncInvalidation    bool
	invalidationQueue    chan invalidationTask
	invalidationDrops    atomic.Int64
	lastDropLogNano      atomic.Int64
	dropRecorder         InvalidationDropRecorder
	invalidationErr      atomic.Pointer[error]
	sf                   singleflightGroup
	cleanupInterval      time.Duration
	logger               *slog.Logger
	ctx                  context.Context
	cancelFunc           context.CancelFunc
	wg                   sync.WaitGroup
}

func validateConfig(cfg *Config) error {
	if cfg.MaxItems < 0 {
		return fmt.Errorf("invalid MaxItems %d: must be >= 0", cfg.MaxItems)
	}
	if cfg.MaxBytes < 0 {
		return fmt.Errorf("invalid MaxBytes %d: must be >= 0", cfg.MaxBytes)
	}
	if cfg.CleanupInterval < 0 {
		return fmt.Errorf("invalid CleanupInterval %v: must be >= 0", cfg.CleanupInterval)
	}
	if cfg.PubSubTimeout < 0 {
		return fmt.Errorf("invalid PubSubTimeout %v: must be >= 0", cfg.PubSubTimeout)
	}
	if math.IsNaN(cfg.DefaultTTLJitter) || cfg.DefaultTTLJitter < 0 || cfg.DefaultTTLJitter > 0.5 {
		return fmt.Errorf("invalid DefaultTTLJitter %v: must be in [0.0, 0.5]", cfg.DefaultTTLJitter)
	}
	if cfg.DefaultStaleWhileRevalidate < 0 {
		return fmt.Errorf("invalid DefaultStaleWhileRevalidate %v: must be >= 0", cfg.DefaultStaleWhileRevalidate)
	}
	if cfg.InvalidationTopic != "" && cfg.PubSub == nil {
		return errors.New("InvalidationTopic requires a non-nil PubSubClient")
	}
	switch cfg.InvalidationMode {
	case InvalidationModeInvalidateOnly, InvalidationModeWriteThrough:
	default:
		return fmt.Errorf("unknown invalidation mode %d", cfg.InvalidationMode)
	}
	if cfg.WriteThroughMaxBytes < 0 {
		return fmt.Errorf("invalid WriteThroughMaxBytes %d: must be >= 0", cfg.WriteThroughMaxBytes)
	}
	if cfg.asyncInvalidationSet && cfg.AsyncInvalidationQueueSize <= 0 {
		return fmt.Errorf("invalid AsyncInvalidationQueueSize %d: must be > 0", cfg.AsyncInvalidationQueueSize)
	}
	if !cfg.asyncInvalidationSet && cfg.AsyncInvalidationQueueSize < 0 {
		return fmt.Errorf("invalid AsyncInvalidationQueueSize %d: must be >= 0", cfg.AsyncInvalidationQueueSize)
	}

	bounded := cfg.MaxItems > 0 || cfg.MaxBytes > 0
	if !cfg.evictionPolicySet {
		if bounded {
			cfg.EvictionPolicy = EvictionS3FIFO
		} else {
			cfg.EvictionPolicy = EvictionVolatileTTL
		}
	}

	switch cfg.EvictionPolicy {
	case EvictionVolatileTTL:
	case EvictionS3FIFO, EvictionTinyLFU:
		if !bounded {
			return fmt.Errorf("eviction policy %s requires MaxItems > 0 or MaxBytes > 0", cfg.EvictionPolicy)
		}
	default:
		return fmt.Errorf("unknown eviction policy %d", cfg.EvictionPolicy)
	}
	return nil
}

func normalizeMetricsRecorder(r MetricsRecorder) MetricsRecorder {
	if r == nil {
		return nil
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		if v.IsNil() {
			return nil
		}
	}
	return r
}

// New creates and initializes a new MemoryStore using functional options.
// Invalid bounds, negative intervals, out-of-range jitter, or unsupported
// options return a descriptive error rather than silently clamping values.
func New(opts ...Option) (*MemoryStore, error) {
	var cfg Config
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	cleanupInterval := cfg.CleanupInterval
	if cleanupInterval == 0 {
		cleanupInterval = defaultCleanupInterval
	}

	ps := cfg.PubSub
	if ps == nil {
		ps = NewInMemoryPubSub()
		logger.Debug("initialized default in-memory pubsub")
	}

	maxItems := int64(cfg.MaxItems)
	maxBytes := cfg.MaxBytes
	bounded := maxItems > 0 || maxBytes > 0

	pubSubTimeout := cfg.PubSubTimeout
	if cfg.InvalidationTopic != "" && pubSubTimeout <= 0 {
		pubSubTimeout = defaultInvalidationPubSubTimeout
	}

	writeThroughMaxBytes := defaultWriteThroughMaxBytes
	if cfg.writeThroughMaxBytesSet {
		writeThroughMaxBytes = cfg.WriteThroughMaxBytes
	}

	asyncInvalidation := cfg.InvalidationTopic != "" && !cfg.syncInvalidation
	var invalidationQueue chan invalidationTask
	if asyncInvalidation {
		qSize := defaultInvalidationQueueSize
		if cfg.asyncInvalidationSet {
			qSize = cfg.AsyncInvalidationQueueSize
		}
		invalidationQueue = make(chan invalidationTask, qSize)
	}

	recorder := normalizeMetricsRecorder(cfg.MetricsRecorder)
	var dropRecorder InvalidationDropRecorder
	if dr, ok := recorder.(InvalidationDropRecorder); ok {
		dropRecorder = dr
	}

	var jitterRng *rand.Rand
	if cfg.hasTTLJitterSeed {
		jitterRng = rand.New(rand.NewPCG(cfg.ttlJitterSeed, cfg.ttlJitterSeed^0x9e3779b97f4a7c15))
	}

	active := calcActiveShards(maxItems, maxBytes)

	ms := &MemoryStore{
		preciseClock:         cfg.PreciseClock,
		copyOnWrite:          cfg.CopyOnWrite,
		copyOnRead:           cfg.CopyOnRead,
		trackFreq:            bounded && cfg.EvictionPolicy != EvictionVolatileTTL,
		bounded:              bounded,
		evictionPolicy:       cfg.EvictionPolicy,
		shardMask:            uint8(active - 1),
		hashSeed:             maphash.MakeSeed(),
		baseTime:             time.Now(),
		recorder:             recorder,
		maxItems:             maxItems,
		maxBytes:             maxBytes,
		shardTargetItems:     maxItems / int64(active),
		shardTargetBytes:     maxBytes / int64(active),
		defaultTTLJitter:     cfg.DefaultTTLJitter,
		defaultStaleGrace:    cfg.DefaultStaleWhileRevalidate,
		jitterRng:            jitterRng,
		onEvict:              cfg.OnEvict,
		ps:                   ps,
		pubSubTimeout:        pubSubTimeout,
		instanceID:           uuid.New().String(),
		invalidationTopic:    cfg.InvalidationTopic,
		invalidationMode:     cfg.InvalidationMode,
		writeThroughMaxBytes: writeThroughMaxBytes,
		asyncInvalidation:    asyncInvalidation,
		invalidationQueue:    invalidationQueue,
		dropRecorder:         dropRecorder,
		cleanupInterval:      cleanupInterval,
		logger:               logger,
		ctx:                  ctx,
		cancelFunc:           cancel,
	}
	ms.lastDropLogNano.Store(-1)

	const initialThreshold = (initialBuckets * bucketSlots * 3) / 4
	for i := range ms.activeShards() {
		ms.shards[i].table.Store(newBucketTable(initialBuckets))
		ms.shards[i].resizeThreshold = initialThreshold
		if bounded {
			ms.shards[i].evict = newShardEvictor(cfg.EvictionPolicy, ms.shardTargetItems, ms.shardTargetBytes)
		}
	}

	if !cfg.PreciseClock {
		ms.startCoarseClock()
	}
	ms.startCleanupWorker()
	ms.startInvalidationSender()
	ms.startInvalidationBackplane()
	return ms, nil
}

// InstanceID returns the unique RFC 9562 UUID assigned to this MemoryStore
// instance (used for echo suppression in distributed cache invalidation).
func (m *MemoryStore) InstanceID() string {
	return m.instanceID
}

// nowExact returns exact monotonic nanoseconds elapsed since the store was created.
func (m *MemoryStore) nowExact() int64 {
	return int64(time.Since(m.baseTime))
}

// nowCoarse returns the 1ms coarse monotonic clock (or exact clock when WithPreciseClock is set).
func (m *MemoryStore) nowCoarse() int64 {
	if m.preciseClock {
		return m.nowExact()
	}
	return m.coarseNow.Load()
}

// nowNano returns monotonic nanoseconds elapsed since the store was created.
func (m *MemoryStore) nowNano() int64 {
	return m.nowExact()
}

func (m *MemoryStore) startCoarseClock() {
	m.wg.Go(func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.coarseNow.Store(int64(time.Since(m.baseTime)))
			case <-m.ctx.Done():
				return
			}
		}
	})
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

func calcActiveShards(maxItems, maxBytes int64) int {
	if maxItems <= 0 && maxBytes <= 0 {
		return numShards
	}
	limit := int64(numShards)
	if maxItems > 0 {
		if maxItems < singleShardMaxItems {
			return 1
		}
		limit = min(limit, max(maxItems/minItemsPerShard, 1))
	}
	if maxBytes > 0 {
		limit = min(limit, max(maxBytes/minBytesPerShard, 1))
	}
	shards := 1
	for shards*2 <= int(limit) {
		shards *= 2
	}
	return shards
}

func (m *MemoryStore) activeShards() int {
	return int(m.shardMask) + 1
}

// shardHash computes the 64-bit hash and shard index for key.
func (m *MemoryStore) shardHash(key string) (uint64, uint8) {
	h := maphash.String(m.hashSeed, key)
	return h, uint8(h) & m.shardMask
}

// shardIndex returns the shard index responsible for the given key.
func (m *MemoryStore) shardIndex(key string) uint8 {
	return uint8(maphash.String(m.hashSeed, key)) & m.shardMask
}

// lookupEntry returns the raw *entry for key (used by white-box tests).
func (m *MemoryStore) lookupEntry(key string) *entry {
	h, idx := m.shardHash(key)
	t := m.shards[idx].table.Load()
	if t == nil {
		return nil
	}
	return t.lookup(h, key)
}

func payloadSize(key string, value []byte) int64 {
	return int64(len(key) + len(value))
}

func (m *MemoryStore) nextJitterFloat64() float64 {
	if m.jitterRng == nil {
		return rand.Float64()
	}
	m.jitterMu.Lock()
	v := m.jitterRng.Float64()
	m.jitterMu.Unlock()
	return v
}

func (m *MemoryStore) applyJitter(ttl time.Duration, fraction float64) time.Duration {
	if fraction <= 0 || ttl <= 0 {
		return ttl
	}
	factor := 1.0 + (m.nextJitterFloat64()*2.0-1.0)*fraction
	scaled := float64(ttl) * factor
	if scaled >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	minTTL := max(time.Duration(float64(ttl)*(1.0-fraction)), 1)
	maxTTL := time.Duration(float64(ttl) * (1.0 + fraction))
	if float64(ttl)*(1.0+fraction) >= float64(math.MaxInt64) {
		maxTTL = math.MaxInt64
	}
	d := time.Duration(scaled)
	if d < minTTL {
		return minTTL
	}
	if d > maxTTL {
		return maxTTL
	}
	return d
}

func (m *MemoryStore) buildEntry(key string, value []byte, hash uint64, duration time.Duration, opts setOptions) (*entry, int64) {
	if opts.noExpiration || duration == NoExpiration {
		e := &entry{
			key:   key,
			value: value,
			flags: entryFlagNoExpiration,
			hash:  hash,
		}
		e.expiresAt.Store(math.MaxInt64)
		return e, m.nowCoarse()
	}

	jitter := m.defaultTTLJitter
	if opts.hasJitter {
		jitter = opts.jitter
	}
	effectiveTTL := m.applyJitter(duration, jitter)

	var flags uint8
	var now int64
	if m.preciseClock || duration < preciseTTLThreshold || effectiveTTL < preciseTTLThreshold {
		flags |= entryFlagPreciseTTL
		now = m.nowExact()
	} else {
		now = m.nowCoarse()
	}

	var ttlNanos int64
	if opts.sliding {
		flags |= entryFlagSliding
		ttlNanos = int64(effectiveTTL)
		if opts.ttlCapNanos > 0 {
			ttlNanos = opts.ttlCapNanos
		}
	}

	staleGrace := int64(m.defaultStaleGrace)
	if opts.hasStaleGrace {
		staleGrace = int64(opts.staleGrace)
	}

	exp := calcExpiresAt(now, effectiveTTL)
	e := &entry{
		key:        key,
		value:      value,
		ttlNanos:   ttlNanos,
		flags:      flags,
		hash:       hash,
		staleGrace: staleGrace,
	}
	e.expiresAt.Store(exp)
	return e, now
}

func (m *MemoryStore) notifyEvicted(entries []evictedEntry) {
	if (m.onEvict == nil && m.recorder == nil) || len(entries) == 0 {
		return
	}
	for _, e := range entries {
		if m.onEvict != nil {
			m.onEvict(e.key, e.value, e.reason)
		}
		if m.recorder != nil {
			m.recorder.RecordEvict(e.reason, payloadSize(e.key, e.value))
		}
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

	m.stopped.Store(true)
	m.cancelFunc()
	m.cancelFunc = nil

	var closeErr error
	if m.ps != nil {
		if err := m.ps.Close(); err != nil {
			closeErr = err
			m.logger.Error("error closing pubsub client", "error", err)
		}
	}

	// Wait for coarse clock, cleanup, and invalidation goroutines to finish.
	m.wg.Wait()

	// Clear tables and eviction queues while keeping s.evict non-nil so any
	// concurrent in-flight reader never races on the s.evict pointer.
	for i := range m.activeShards() {
		s := &m.shards[i]
		s.mu.Lock()
		s.table.Store(nil)
		if s.evict != nil {
			s.evict.clearLocked()
		}
		s.count.Store(0)
		s.bytes.Store(0)
		s.resizeThreshold = 0
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
	return m.stopped.Load()
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
	now := m.nowExact()
	var evictedBuf []evictedEntry
	needNotify := m.onEvict != nil || m.recorder != nil

	for i := range m.activeShards() {
		s := &m.shards[i]
		var evicted int64
		var freedBytes int64
		s.mu.Lock()
		tbl := s.table.Load()
		if tbl == nil || m.stopped.Load() {
			s.mu.Unlock()
			return
		}
		for bIdx := range tbl.buckets {
			b := &tbl.buckets[bIdx]
			ctrl := b.ctrl.Load()
			if ctrl&slotCtrlMask == 0 {
				continue
			}
			for j := range bucketSlots {
				if (ctrl>>(j*8))&0xFF == 0 {
					continue
				}
				e := b.slots[j].Load()
				if e == nil || e.isNoExpiration() || now < e.hardExpiresAt() {
					continue
				}
				b.slots[j].Store(nil)
				ctrl &^= uint64(0xFF) << (j * 8)
				b.ctrl.Store(ctrl)

				sz := payloadSize(e.key, e.value)
				if s.evict != nil {
					s.evict.onDeleteLocked(e, sz)
				}
				freedBytes += sz
				evicted++
				if needNotify {
					evictedBuf = append(evictedBuf, evictedEntry{
						key:    e.key,
						value:  e.value,
						reason: EvictExpired,
					})
				}
			}
		}
		if evicted > 0 {
			s.count.Store(s.count.Load() - evicted)
			s.bytes.Store(s.bytes.Load() - freedBytes)
			s.evictions.Add(evicted)
			if m.bounded {
				m.totalItems.Add(-evicted)
				m.totalBytes.Add(-freedBytes)
			}
		}
		s.mu.Unlock()
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

func (m *MemoryStore) shardOverTarget(s *shard) bool {
	if s.count.Load() <= 1 {
		return false
	}
	if m.maxItems > 0 && m.totalItems.Load() > m.maxItems && s.count.Load() > max(m.shardTargetItems, 1) {
		return true
	}
	if m.maxBytes > 0 && m.totalBytes.Load() > m.maxBytes && s.bytes.Load() > max(m.shardTargetBytes, 1) {
		return true
	}
	return false
}

func (m *MemoryStore) shardOverEnforceTarget(s *shard) bool {
	if m.maxItems > 0 && m.totalItems.Load() > m.maxItems && s.count.Load() > m.shardTargetItems {
		return true
	}
	if m.maxBytes > 0 && m.totalBytes.Load() > m.maxBytes && s.bytes.Load() > m.shardTargetBytes {
		return true
	}
	return false
}

// enforceBounds evicts entries starting from startShard until totalItems and
// totalBytes are within configured limits (or no non-protected entries remain).
func (m *MemoryStore) enforceBounds(startShard uint8, now int64, protectedKey string) {
	m.enforceBoundsWithBatch(startShard, now, protectedKey, nil)
}

func (m *MemoryStore) enforceBoundsWithBatch(startShard uint8, now int64, protectedKey string, protectedBatch map[string][]byte) {
	if m.stopped.Load() || !m.overBounds() {
		return
	}

	var protectedShard uint8
	hasProtected := protectedKey != ""
	if hasProtected {
		protectedShard = m.shardIndex(protectedKey)
	}
	hasBatch := len(protectedBatch) > 0

	var notified []evictedEntry
	needNotify := m.onEvict != nil || m.recorder != nil
	active := m.activeShards()

	for pass := 0; pass < 2 && m.overBounds(); pass++ {
		madeProgress := false
		for offset := range active {
			if m.stopped.Load() || !m.overBounds() {
				break
			}
			idx := uint8((int(startShard) + offset) & int(m.shardMask))
			s := &m.shards[idx]
			if s.count.Load() == 0 {
				continue
			}
			if pass == 0 && !m.shardOverEnforceTarget(s) {
				continue
			}
			s.mu.Lock()
			if s.table.Load() == nil || m.stopped.Load() {
				s.mu.Unlock()
				m.notifyEvicted(notified)
				return
			}
			for m.overBounds() {
				if pass == 0 && !m.shardOverEnforceTarget(s) {
					break
				}
				skip := ""
				if hasProtected && idx == protectedShard {
					skip = protectedKey
				}
				evEntry, ok := m.evictOneProtectedLocked(s, now, skip, protectedBatch)
				if !ok {
					break
				}
				madeProgress = true
				if needNotify {
					notified = append(notified, evEntry)
				}
				if pass == 0 && hasProtected && !hasBatch && idx == protectedShard && s.count.Load() <= 1 {
					break
				}
			}
			s.mu.Unlock()
		}
		if pass > 0 && !madeProgress {
			break
		}
	}

	// Final fallback if concurrent writes or an oversized batch left bounds exceeded
	// after exhausting all non-protected entries across all shards.
	if m.overBounds() && (hasProtected || hasBatch) && !m.stopped.Load() {
		if !hasBatch {
			s := &m.shards[protectedShard]
			s.mu.Lock()
			if s.table.Load() != nil && !m.stopped.Load() {
				for m.overBounds() {
					evEntry, ok := m.evictOneProtectedLocked(s, now, "", nil)
					if !ok {
						break
					}
					if needNotify {
						notified = append(notified, evEntry)
					}
				}
			}
			s.mu.Unlock()
		} else {
			for offset := range active {
				if m.stopped.Load() || !m.overBounds() {
					break
				}
				idx := uint8((int(startShard) + offset) & int(m.shardMask))
				s := &m.shards[idx]
				if s.count.Load() == 0 {
					continue
				}
				s.mu.Lock()
				if s.table.Load() == nil || m.stopped.Load() {
					s.mu.Unlock()
					m.notifyEvicted(notified)
					return
				}
				for m.overBounds() {
					evEntry, ok := m.evictOneProtectedLocked(s, now, "", nil)
					if !ok {
						break
					}
					if needNotify {
						notified = append(notified, evEntry)
					}
				}
				s.mu.Unlock()
			}
		}
	}

	m.notifyEvicted(notified)
}

func (m *MemoryStore) tryLazyExpire(s *shard, e *entry, now int64) {
	if !s.mu.TryLock() {
		return
	}
	tbl := s.table.Load()
	var evicted []evictedEntry
	if tbl != nil && !m.stopped.Load() && !e.isNoExpiration() && now >= e.hardExpiresAt() {
		if tbl.deletePtrLocked(e) {
			sz := payloadSize(e.key, e.value)
			if s.evict != nil {
				s.evict.onDeleteLocked(e, sz)
			}
			s.count.Store(s.count.Load() - 1)
			s.bytes.Store(s.bytes.Load() - sz)
			s.evictions.Add(1)
			if m.bounded {
				m.totalItems.Add(-1)
				m.totalBytes.Add(-sz)
			}
			if m.onEvict != nil || m.recorder != nil {
				evicted = append(evicted, evictedEntry{
					key:    e.key,
					value:  e.value,
					reason: EvictExpired,
				})
			}
		}
	}
	s.mu.Unlock()
	m.notifyEvicted(evicted)
}

func (m *MemoryStore) deleteExpiredOnSet(key string) error {
	h, idx := m.shardHash(key)
	s := &m.shards[idx]

	s.mu.Lock()
	tbl := s.table.Load()
	if tbl == nil || m.stopped.Load() {
		s.mu.Unlock()
		return ErrStoreStopped
	}
	var evicted []evictedEntry
	if oldE, ok := tbl.deleteLocked(h, key); ok {
		sz := payloadSize(key, oldE.value)
		if s.evict != nil {
			s.evict.onDeleteLocked(oldE, sz)
		}
		s.count.Store(s.count.Load() - 1)
		s.bytes.Store(s.bytes.Load() - sz)
		s.evictions.Add(1)
		if m.bounded {
			m.totalItems.Add(-1)
			m.totalBytes.Add(-sz)
		}
		if m.onEvict != nil || m.recorder != nil {
			evicted = append(evicted, evictedEntry{
				key:    key,
				value:  oldE.value,
				reason: EvictExpired,
			})
		}
	}
	s.mu.Unlock()

	m.notifyEvicted(evicted)
	if m.invalidationTopic != "" {
		m.broadcastInvalidation(key)
	}
	return nil
}

// Set stores a raw byte slice in the cache with the specified key and duration.
// Use NoExpiration (-1) for non-expiring items; items with duration <= 0
// (other than NoExpiration) expire immediately (deleting any existing key).
//
// Unless WithCopyOnWrite(true) is enabled, Set stores value without copying;
// callers must not mutate the slice after passing it to Set.
func (m *MemoryStore) Set(key string, value []byte, duration time.Duration) error {
	return m.setInternal(key, value, duration, setOptions{}, m.copyOnWrite, true)
}

// SetWithOptions stores a raw byte slice in the cache with per-entry options
// such as WithTTLJitter, WithSlidingExpiration, WithNoExpiration, or
// WithStaleWindow.
func (m *MemoryStore) SetWithOptions(key string, value []byte, ttl time.Duration, opts ...SetOption) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}
	var cfg setOptions
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&cfg)
	}
	if cfg.err != nil {
		return cfg.err
	}
	return m.setInternal(key, value, ttl, cfg, m.copyOnWrite, true)
}

func (m *MemoryStore) setLocal(key string, value []byte, duration time.Duration, opts setOptions) error {
	return m.setInternal(key, value, duration, opts, false, false)
}

func (s *shard) rejectOversizedLocked(m *MemoryStore, tbl *bucketTable, h uint64, key string, val []byte, needNotify bool, evictedBuf *[]evictedEntry) {
	if oldE, ok := tbl.deleteLocked(h, key); ok {
		oldSize := payloadSize(key, oldE.value)
		if s.evict != nil {
			s.evict.onDeleteLocked(oldE, oldSize)
		}
		s.count.Store(s.count.Load() - 1)
		s.bytes.Store(s.bytes.Load() - oldSize)
		if m.bounded {
			m.totalItems.Add(-1)
			m.totalBytes.Add(-oldSize)
		}
	}
	s.evictions.Add(1)
	if needNotify {
		*evictedBuf = append(*evictedBuf, evictedEntry{
			key:    key,
			value:  val,
			reason: EvictCapacity,
		})
	}
}

func (m *MemoryStore) setInternal(key string, value []byte, duration time.Duration, opts setOptions, copyWrite, broadcast bool) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}
	if !opts.noExpiration && duration != NoExpiration && duration <= 0 {
		if broadcast {
			return m.deleteExpiredOnSet(key)
		}
		return m.deleteLocal(key)
	}
	if copyWrite {
		value = bytes.Clone(value)
	}

	h, idx := m.shardHash(key)
	s := &m.shards[idx]
	newSize := payloadSize(key, value)

	var evictedBuf []evictedEntry
	needNotify := m.onEvict != nil || m.recorder != nil

	if m.maxBytes > 0 && newSize > m.maxBytes {
		s.mu.Lock()
		tbl := s.table.Load()
		if tbl == nil || m.stopped.Load() {
			s.mu.Unlock()
			return ErrStoreStopped
		}
		s.rejectOversizedLocked(m, tbl, h, key, value, needNotify, &evictedBuf)
		s.mu.Unlock()
		m.notifyEvicted(evictedBuf)
		if broadcast && m.invalidationTopic != "" {
			m.broadcastInvalidation(key)
		}
		return nil
	}

	newE, now := m.buildEntry(key, value, h, duration, opts)

	s.mu.Lock()
	if s.table.Load() == nil || m.stopped.Load() {
		s.mu.Unlock()
		return ErrStoreStopped
	}

	oldE, existed := s.upsertLocked(m, h, newE, now, &evictedBuf)
	if existed {
		oldSize := payloadSize(key, oldE.value)
		dBytes := newSize - oldSize
		s.bytes.Store(s.bytes.Load() + dBytes)
		if s.evict != nil {
			s.evict.onOverwriteLocked(oldE, newE, oldSize, newSize)
		}
		if m.bounded && dBytes != 0 {
			m.totalBytes.Add(dBytes)
		}
	} else {
		s.count.Store(s.count.Load() + 1)
		s.bytes.Store(s.bytes.Load() + newSize)
		if s.evict != nil {
			s.evict.onInsertLocked(newE, newSize)
		}
		if m.bounded {
			m.totalItems.Add(1)
			m.totalBytes.Add(newSize)
		}
	}

	if m.bounded {
		for m.overBounds() && m.shardOverTarget(s) {
			evEntry, ok := m.evictOneLocked(s, now, key)
			if !ok {
				break
			}
			if needNotify {
				evictedBuf = append(evictedBuf, evEntry)
			}
		}
	}
	s.mu.Unlock()

	m.notifyEvicted(evictedBuf)

	if m.bounded && !m.stopped.Load() && m.overBounds() {
		cursor := uint8(m.evictCursor.Add(1) & uint32(m.shardMask))
		m.enforceBounds(cursor, now, key)
	}

	if broadcast && m.invalidationTopic != "" {
		if m.invalidationMode == InvalidationModeWriteThrough && m.writeThroughMaxBytes > 0 && len(value) <= m.writeThroughMaxBytes {
			m.broadcastSetWriteThrough(newE)
		} else {
			m.broadcastInvalidation(key)
		}
	}

	return nil
}

// SetJSON stores a JSON-serializable value in the cache using encoding/json/v2.
// Returns an error if JSON marshaling fails or ErrStoreStopped if the store is stopped.
func (m *MemoryStore) SetJSON(key string, value any, duration time.Duration) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return m.setInternal(key, data, duration, setOptions{}, false, true)
}

// SetJSONWithOptions stores a JSON-serializable value in the cache using encoding/json/v2
// with the provided marshal/encode options (such as json.Deterministic or encoding/json.DefaultOptionsV1).
func (m *MemoryStore) SetJSONWithOptions(key string, value any, duration time.Duration, opts ...json.Options) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}
	data, err := json.Marshal(value, opts...)
	if err != nil {
		return err
	}
	return m.setInternal(key, data, duration, setOptions{}, false, true)
}

func (m *MemoryStore) entryNow(e *entry) int64 {
	if m.preciseClock || e.isPreciseTTL() {
		return m.nowExact()
	}
	return m.coarseNow.Load()
}

func refreshSlidingEntry(e *entry, exp, now int64) {
	threshold := e.ttlNanos - (e.ttlNanos >> 2)
	if remaining := exp - now; remaining > 0 && remaining < threshold {
		newExp := calcExpiresAt(now, time.Duration(e.ttlNanos))
		if newExp > exp {
			e.expiresAt.CompareAndSwap(exp, newExp)
		}
	}
}

// Get retrieves a value from the cache.
// Returns the value and true if the key exists and has not expired; otherwise returns (nil, false).
// If the store has been stopped, Get returns (nil, false).
//
// Unless WithCopyOnRead(true) is enabled, the returned byte slice aliases
// internal cache storage directly; callers must not mutate it (use bytes.Clone
// if modification is required).
func (m *MemoryStore) Get(key string) ([]byte, bool) {
	if m.stopped.Load() {
		return nil, false
	}

	h, idx := m.shardHash(key)
	s := &m.shards[idx]
	tbl := s.table.Load()
	if tbl == nil {
		return nil, false
	}

	e := tbl.lookup(h, key)
	if e == nil {
		m.recordMiss(s)
		return nil, false
	}

	if !e.isNoExpiration() {
		now := m.entryNow(e)
		exp := e.expiresAt.Load()
		if now >= exp {
			if e.staleGrace == 0 || now >= calcHardExpiry(exp, e.staleGrace) {
				m.tryLazyExpire(s, e, now)
			}
			m.recordMiss(s)
			return nil, false
		}
		if e.isSliding() {
			refreshSlidingEntry(e, exp, now)
		}
	}

	if m.trackFreq {
		m.recordReadAccess(s, e)
	}

	s.hits.Add(1)
	if m.recorder != nil {
		m.recorder.RecordHit()
	}
	if m.copyOnRead {
		return bytes.Clone(e.value), true
	}
	return e.value, true
}

func (m *MemoryStore) recordMiss(s *shard) {
	if !m.stopped.Load() {
		s.misses.Add(1)
		if m.recorder != nil {
			m.recorder.RecordMiss()
		}
	}
}

func (m *MemoryStore) getRaw(key string) ([]byte, bool) {
	return m.get(key, false, false)
}

func (m *MemoryStore) get(key string, copyOnRead bool, recordMetrics bool) ([]byte, bool) {
	if m.stopped.Load() {
		return nil, false
	}

	h, idx := m.shardHash(key)
	s := &m.shards[idx]
	tbl := s.table.Load()
	if tbl == nil {
		return nil, false
	}

	e := tbl.lookup(h, key)
	if e == nil {
		if recordMetrics {
			m.recordMiss(s)
		}
		return nil, false
	}

	if !e.isNoExpiration() {
		now := m.entryNow(e)
		exp := e.expiresAt.Load()
		if now >= exp {
			if e.staleGrace == 0 || now >= calcHardExpiry(exp, e.staleGrace) {
				m.tryLazyExpire(s, e, now)
			}
			if recordMetrics {
				m.recordMiss(s)
			}
			return nil, false
		}
		if e.isSliding() {
			refreshSlidingEntry(e, exp, now)
		}
	}

	if m.trackFreq {
		m.recordReadAccess(s, e)
	}

	if recordMetrics {
		s.hits.Add(1)
		if m.recorder != nil {
			m.recorder.RecordHit()
		}
	}
	if copyOnRead {
		return bytes.Clone(e.value), true
	}
	return e.value, true
}

type hitStatus uint8

const (
	hitMiss hitStatus = iota
	hitFresh
	hitStale
)

func (m *MemoryStore) getWithStale(key string, copyOnRead bool, hasGraceOverride bool, graceOverride time.Duration) ([]byte, int64, hitStatus) {
	if m.stopped.Load() {
		return nil, 0, hitMiss
	}

	h, idx := m.shardHash(key)
	s := &m.shards[idx]
	tbl := s.table.Load()
	if tbl == nil {
		return nil, 0, hitMiss
	}

	e := tbl.lookup(h, key)
	if e == nil {
		m.recordMiss(s)
		return nil, 0, hitMiss
	}

	if !e.isNoExpiration() {
		now := m.entryNow(e)
		exp := e.expiresAt.Load()
		if now >= exp {
			effectiveGrace := e.staleGrace
			if hasGraceOverride {
				effectiveGrace = int64(graceOverride)
			}
			if effectiveGrace > 0 && now < calcHardExpiry(exp, effectiveGrace) {
				if m.trackFreq {
					m.recordReadAccess(s, e)
				}
				s.hits.Add(1)
				if m.recorder != nil {
					m.recorder.RecordHit()
				}
				if copyOnRead {
					return bytes.Clone(e.value), effectiveGrace, hitStale
				}
				return e.value, effectiveGrace, hitStale
			}
			if e.staleGrace == 0 || now >= calcHardExpiry(exp, e.staleGrace) {
				m.tryLazyExpire(s, e, now)
			}
			m.recordMiss(s)
			return nil, 0, hitMiss
		}
		if e.isSliding() {
			refreshSlidingEntry(e, exp, now)
		}
	}

	if m.trackFreq {
		m.recordReadAccess(s, e)
	}

	s.hits.Add(1)
	if m.recorder != nil {
		m.recorder.RecordHit()
	}
	if copyOnRead {
		return bytes.Clone(e.value), e.staleGrace, hitFresh
	}
	return e.value, e.staleGrace, hitFresh
}

// GetJSON retrieves and deserializes a JSON value from the cache into dest
// using encoding/json/v2.
// Returns (false, nil) if the key does not exist or has expired.
func (m *MemoryStore) GetJSON(key string, dest any) (bool, error) {
	data, exists := m.get(key, false, true)
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
	data, exists := m.get(key, false, true)
	if !exists {
		return false, nil
	}

	if err := json.Unmarshal(data, dest, opts...); err != nil {
		return true, err
	}

	return true, nil
}

// deleteLocal removes key from local storage without broadcasting invalidation.
func (m *MemoryStore) deleteLocal(key string) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}

	h, idx := m.shardHash(key)
	s := &m.shards[idx]
	s.mu.Lock()
	tbl := s.table.Load()
	if tbl == nil || m.stopped.Load() {
		s.mu.Unlock()
		return ErrStoreStopped
	}
	oldE, existed := tbl.deleteLocked(h, key)
	var deletedSize int64
	if existed {
		deletedSize = payloadSize(key, oldE.value)
		if s.evict != nil {
			s.evict.onDeleteLocked(oldE, deletedSize)
		}
		s.count.Store(s.count.Load() - 1)
		s.bytes.Store(s.bytes.Load() - deletedSize)
		if m.bounded {
			m.totalItems.Add(-1)
			m.totalBytes.Add(-deletedSize)
		}
	}
	s.mu.Unlock()

	if existed {
		if m.onEvict != nil {
			m.onEvict(key, oldE.value, EvictDeleted)
		}
		if m.recorder != nil {
			m.recorder.RecordEvict(EvictDeleted, deletedSize)
		}
	}
	return nil
}

// Delete removes an item from the cache.
// If the key doesn't exist, the operation is a no-op locally (but still
// broadcasts an invalidation if Config.InvalidationTopic is configured).
// Returns ErrStoreStopped if the store has been stopped.
func (m *MemoryStore) Delete(key string) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}
	if err := m.deleteLocal(key); err != nil {
		return err
	}
	if m.invalidationTopic != "" {
		m.broadcastInvalidation(key)
	}
	return nil
}

type multiSetEntry struct {
	key   string
	value []byte
	hash  uint64
	next  int32
}

// SetMulti stores multiple key-value pairs in the cache with the same expiration duration.
// Keys are grouped by shard to minimize lock acquisitions and heap allocations.
//
// Unless WithCopyOnWrite(true) is enabled, SetMulti stores value slices without
// copying; callers must not mutate slices after passing them to SetMulti.
func (m *MemoryStore) SetMulti(items map[string][]byte, duration time.Duration) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}

	n := len(items)
	if n == 0 {
		return nil
	}

	if duration != NoExpiration && duration <= 0 {
		return m.deleteExpiredBatch(items)
	}

	hasJitter := m.defaultTTLJitter > 0 && duration > 0 && duration != NoExpiration
	var fixedExp int64
	var fixedFlags uint8
	var fixedStaleGrace int64
	var now int64
	if duration == NoExpiration {
		fixedExp = math.MaxInt64
		fixedFlags = entryFlagNoExpiration
		now = m.nowCoarse()
	} else if !hasJitter {
		if m.preciseClock || duration < preciseTTLThreshold {
			fixedFlags = entryFlagPreciseTTL
			now = m.nowExact()
		} else {
			now = m.nowCoarse()
		}
		fixedExp = calcExpiresAt(now, duration)
		fixedStaleGrace = int64(m.defaultStaleGrace)
	} else {
		now = m.nowCoarse()
	}

	var evictedBuf []evictedEntry
	needNotify := m.onEvict != nil || m.recorder != nil

	needWriteThrough := m.invalidationTopic != "" && m.invalidationMode == InvalidationModeWriteThrough && m.writeThroughMaxBytes > 0

	if n <= smallBatchThreshold {
		var wtItems []writeThroughItem
		var invKeys []string
		if needWriteThrough {
			wtItems = make([]writeThroughItem, 0, n)
		}
		for key, value := range items {
			if m.copyOnWrite {
				value = bytes.Clone(value)
			}
			h, idx := m.shardHash(key)
			s := &m.shards[idx]
			newSize := payloadSize(key, value)
			if m.maxBytes > 0 && newSize > m.maxBytes {
				s.mu.Lock()
				tbl := s.table.Load()
				if tbl == nil || m.stopped.Load() {
					s.mu.Unlock()
					return ErrStoreStopped
				}
				s.rejectOversizedLocked(m, tbl, h, key, value, needNotify, &evictedBuf)
				s.mu.Unlock()
				if needWriteThrough {
					invKeys = append(invKeys, key)
				}
				continue
			}

			var newE *entry
			if hasJitter {
				newE, now = m.buildEntry(key, value, h, duration, setOptions{})
			} else {
				newE = &entry{
					key:        key,
					value:      value,
					flags:      fixedFlags,
					hash:       h,
					staleGrace: fixedStaleGrace,
				}
				newE.expiresAt.Store(fixedExp)
			}
			if needWriteThrough {
				if len(value) <= m.writeThroughMaxBytes {
					wtItems = append(wtItems, writeThroughItemFromEntry(newE))
				} else {
					invKeys = append(invKeys, key)
				}
			}

			s.mu.Lock()
			if s.table.Load() == nil || m.stopped.Load() {
				s.mu.Unlock()
				return ErrStoreStopped
			}
			oldE, existed := s.upsertLocked(m, h, newE, now, &evictedBuf)
			if existed {
				oldSize := payloadSize(key, oldE.value)
				dBytes := newSize - oldSize
				s.bytes.Store(s.bytes.Load() + dBytes)
				if s.evict != nil {
					s.evict.onOverwriteLocked(oldE, newE, oldSize, newSize)
				}
				if m.bounded && dBytes != 0 {
					m.totalBytes.Add(dBytes)
				}
			} else {
				s.count.Store(s.count.Load() + 1)
				s.bytes.Store(s.bytes.Load() + newSize)
				if s.evict != nil {
					s.evict.onInsertLocked(newE, newSize)
				}
				if m.bounded {
					m.totalItems.Add(1)
					m.totalBytes.Add(newSize)
				}
			}
			if m.bounded {
				for m.overBounds() && m.shardOverTarget(s) {
					evEntry, ok := m.evictOneProtectedLocked(s, now, "", items)
					if !ok {
						break
					}
					if needNotify {
						evictedBuf = append(evictedBuf, evEntry)
					}
				}
			}
			s.mu.Unlock()
		}
		m.notifyEvicted(evictedBuf)
		if m.bounded && !m.stopped.Load() && m.overBounds() {
			cursor := uint8(m.evictCursor.Add(1) & uint32(m.shardMask))
			m.enforceBoundsWithBatch(cursor, now, "", items)
		}
		if m.invalidationTopic != "" {
			if needWriteThrough {
				m.broadcastSetMultiWriteThrough(wtItems, invKeys)
			} else {
				keys := make([]string, 0, n)
				for k := range items {
					keys = append(keys, k)
				}
				m.broadcastInvalidation(keys...)
			}
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
		if m.copyOnWrite {
			value = bytes.Clone(value)
		}
		h, idx := m.shardHash(key)
		if heads[idx] == -1 {
			touched[touchedCount] = idx
			touchedCount++
		}
		entries = append(entries, multiSetEntry{
			key:   key,
			value: value,
			hash:  h,
			next:  heads[idx],
		})
		heads[idx] = int32(len(entries) - 1)
	}

	var wtItems []writeThroughItem
	var invKeys []string
	if needWriteThrough {
		wtItems = make([]writeThroughItem, 0, n)
	}

	for _, shardIdx := range touched[:touchedCount] {
		s := &m.shards[shardIdx]
		s.mu.Lock()
		if s.table.Load() == nil || m.stopped.Load() {
			s.mu.Unlock()
			return ErrStoreStopped
		}
		for idx := heads[shardIdx]; idx != -1; idx = entries[idx].next {
			k := entries[idx].key
			v := entries[idx].value
			h := entries[idx].hash
			newSize := payloadSize(k, v)
			if m.maxBytes > 0 && newSize > m.maxBytes {
				s.rejectOversizedLocked(m, s.table.Load(), h, k, v, needNotify, &evictedBuf)
				if needWriteThrough {
					invKeys = append(invKeys, k)
				}
				continue
			}

			var newE *entry
			if hasJitter {
				newE, now = m.buildEntry(k, v, h, duration, setOptions{})
			} else {
				newE = &entry{
					key:        k,
					value:      v,
					flags:      fixedFlags,
					hash:       h,
					staleGrace: fixedStaleGrace,
				}
				newE.expiresAt.Store(fixedExp)
			}
			if needWriteThrough {
				if len(v) <= m.writeThroughMaxBytes {
					wtItems = append(wtItems, writeThroughItemFromEntry(newE))
				} else {
					invKeys = append(invKeys, k)
				}
			}
			oldE, existed := s.upsertLocked(m, h, newE, now, &evictedBuf)
			if existed {
				oldSize := payloadSize(k, oldE.value)
				dBytes := newSize - oldSize
				s.bytes.Store(s.bytes.Load() + dBytes)
				if s.evict != nil {
					s.evict.onOverwriteLocked(oldE, newE, oldSize, newSize)
				}
				if m.bounded && dBytes != 0 {
					m.totalBytes.Add(dBytes)
				}
			} else {
				s.count.Store(s.count.Load() + 1)
				s.bytes.Store(s.bytes.Load() + newSize)
				if s.evict != nil {
					s.evict.onInsertLocked(newE, newSize)
				}
				if m.bounded {
					m.totalItems.Add(1)
					m.totalBytes.Add(newSize)
				}
			}
		}
		if m.bounded {
			for m.overBounds() && m.shardOverTarget(s) {
				evEntry, ok := m.evictOneProtectedLocked(s, now, "", items)
				if !ok {
					break
				}
				if needNotify {
					evictedBuf = append(evictedBuf, evEntry)
				}
			}
		}
		s.mu.Unlock()
	}

	m.notifyEvicted(evictedBuf)

	if m.bounded && !m.stopped.Load() && m.overBounds() {
		cursor := uint8(m.evictCursor.Add(1) & uint32(m.shardMask))
		m.enforceBoundsWithBatch(cursor, now, "", items)
	}

	if m.invalidationTopic != "" {
		if needWriteThrough {
			m.broadcastSetMultiWriteThrough(wtItems, invKeys)
		} else {
			keys := make([]string, 0, len(entries))
			for _, e := range entries {
				keys = append(keys, e.key)
			}
			m.broadcastInvalidation(keys...)
		}
	}

	return nil
}

func (m *MemoryStore) deleteExpiredBatch(items map[string][]byte) error {
	var evictedBuf []evictedEntry
	needNotify := m.onEvict != nil || m.recorder != nil
	var keys []string
	if m.invalidationTopic != "" {
		keys = make([]string, 0, len(items))
	}

	for key := range items {
		if m.invalidationTopic != "" {
			keys = append(keys, key)
		}
		h, idx := m.shardHash(key)
		s := &m.shards[idx]
		s.mu.Lock()
		tbl := s.table.Load()
		if tbl == nil || m.stopped.Load() {
			s.mu.Unlock()
			return ErrStoreStopped
		}
		if oldE, ok := tbl.deleteLocked(h, key); ok {
			sz := payloadSize(key, oldE.value)
			if s.evict != nil {
				s.evict.onDeleteLocked(oldE, sz)
			}
			s.count.Store(s.count.Load() - 1)
			s.bytes.Store(s.bytes.Load() - sz)
			s.evictions.Add(1)
			if m.bounded {
				m.totalItems.Add(-1)
				m.totalBytes.Add(-sz)
			}
			if needNotify {
				evictedBuf = append(evictedBuf, evictedEntry{
					key:    key,
					value:  oldE.value,
					reason: EvictExpired,
				})
			}
		}
		s.mu.Unlock()
	}

	m.notifyEvicted(evictedBuf)
	if m.invalidationTopic != "" && len(keys) > 0 {
		m.broadcastInvalidation(keys...)
	}
	return nil
}

// GetMulti retrieves multiple values from the cache.
// Keys that don't exist or have expired are omitted from the returned map.
//
// Unless WithCopyOnRead(true) is enabled, returned byte slices alias internal
// cache storage directly; callers must not mutate them (use bytes.Clone if
// modification is required).
func (m *MemoryStore) GetMulti(keys []string) map[string][]byte {
	if m.stopped.Load() || len(keys) == 0 {
		return map[string][]byte{}
	}

	n := len(keys)
	result := make(map[string][]byte, n)

	if n <= smallBatchThreshold {
		for _, key := range keys {
			h, idx := m.shardHash(key)
			s := &m.shards[idx]
			tbl := s.table.Load()
			if tbl == nil || m.stopped.Load() {
				return map[string][]byte{}
			}
			e := tbl.lookup(h, key)
			if e == nil {
				m.recordMiss(s)
				continue
			}
			if !e.isNoExpiration() {
				now := m.entryNow(e)
				exp := e.expiresAt.Load()
				if now >= exp {
					if e.staleGrace == 0 || now >= calcHardExpiry(exp, e.staleGrace) {
						m.tryLazyExpire(s, e, now)
					}
					m.recordMiss(s)
					continue
				}
				if e.isSliding() {
					refreshSlidingEntry(e, exp, now)
				}
			}
			if m.trackFreq {
				m.recordReadAccess(s, e)
			}
			val := e.value
			if m.copyOnRead {
				val = bytes.Clone(val)
			}
			result[key] = val
			s.hits.Add(1)
			if m.recorder != nil {
				m.recorder.RecordHit()
			}
		}
		return result
	}

	heads := emptyHeads
	var touched [numShards]uint8
	touchedCount := 0

	var stackNext [batchStackLimit]int32
	var stackHashes [batchStackLimit]uint64
	var next []int32
	var hashes []uint64
	if n <= batchStackLimit {
		next = stackNext[:n]
		hashes = stackHashes[:n]
	} else {
		next = make([]int32, n)
		hashes = make([]uint64, n)
	}

	for i, key := range keys {
		h, idx := m.shardHash(key)
		hashes[i] = h
		if heads[idx] == -1 {
			touched[touchedCount] = idx
			touchedCount++
		}
		next[i] = heads[idx]
		heads[idx] = int32(i)
	}

	coarseNow := m.nowCoarse()
	exactNow := int64(-1)

	for _, shardIdx := range touched[:touchedCount] {
		s := &m.shards[shardIdx]
		tbl := s.table.Load()
		if tbl == nil || m.stopped.Load() {
			return map[string][]byte{}
		}
		var hits, misses int64
		for i := heads[shardIdx]; i != -1; i = next[i] {
			key := keys[i]
			e := tbl.lookup(hashes[i], key)
			if e == nil {
				misses++
				continue
			}
			if !e.isNoExpiration() {
				var now int64
				if m.preciseClock || e.isPreciseTTL() {
					if exactNow < 0 {
						exactNow = m.nowExact()
					}
					now = exactNow
				} else {
					now = coarseNow
				}
				exp := e.expiresAt.Load()
				if now >= exp {
					if e.staleGrace == 0 || now >= calcHardExpiry(exp, e.staleGrace) {
						m.tryLazyExpire(s, e, now)
					}
					misses++
					continue
				}
				if e.isSliding() {
					refreshSlidingEntry(e, exp, now)
				}
			}
			if m.trackFreq {
				m.recordReadAccess(s, e)
			}
			val := e.value
			if m.copyOnRead {
				val = bytes.Clone(val)
			}
			result[key] = val
			hits++
		}
		if hits > 0 {
			s.hits.Add(hits)
			if m.recorder != nil {
				for range hits {
					m.recorder.RecordHit()
				}
			}
		}
		if misses > 0 && !m.stopped.Load() {
			s.misses.Add(misses)
			if m.recorder != nil {
				for range misses {
					m.recorder.RecordMiss()
				}
			}
		}
	}

	return result
}

// Health verifies that the MemoryStore is running, its invalidation backplane
// (if configured) is healthy, and the underlying PubSubClient passes Ping (if it
// implements Pinger).
func (m *MemoryStore) Health(ctx context.Context) error {
	if m.IsStopped() {
		return ErrStoreStopped
	}
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.invalidationTopic != "" {
		if ep := m.invalidationErr.Load(); ep != nil {
			return *ep
		}
	}
	if p, ok := m.ps.(Pinger); ok {
		if err := p.Ping(ctx); err != nil {
			return err
		}
	}
	if m.IsStopped() {
		return ErrStoreStopped
	}
	return nil
}

// GetMetrics returns a point-in-time snapshot of cache statistics in lock-free O(1) time.
func (m *MemoryStore) GetMetrics() StoreMetrics {
	var metrics StoreMetrics
	for i := range m.activeShards() {
		s := &m.shards[i]
		metrics.Items += int(s.count.Load())
		metrics.Bytes += s.bytes.Load()
		metrics.Hits += s.hits.Load()
		metrics.Misses += s.misses.Load()
		metrics.Evictions += s.evictions.Load()
	}
	metrics.InvalidationDrops = m.invalidationDrops.Load()

	return metrics
}

func (m *MemoryStore) defaultPubSubContext() (context.Context, context.CancelFunc) {
	if m.pubSubTimeout > 0 {
		return context.WithTimeout(context.Background(), m.pubSubTimeout)
	}
	return context.Background(), func() {}
}

func (m *MemoryStore) backplanePubSubContext() (context.Context, context.CancelFunc) {
	if m.pubSubTimeout > 0 {
		return context.WithTimeout(m.ctx, m.pubSubTimeout)
	}
	return context.WithCancel(m.ctx)
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
