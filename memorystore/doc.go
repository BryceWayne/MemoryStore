// Package memorystore provides a zero-dependency, thread-safe, sharded in-memory
// key-value cache with automatic TTL expiration, optional bounded capacity,
// stampede protection with Stale-While-Revalidate (SWR), deterministic TTL
// jitter, Go 1.27 generic JSON helpers (encoding/json/v2), low-allocation batch
// operations, real-time observability hooks, health checks, resilient async /
// write-through distributed cache invalidation, and zero-dependency streaming
// snapshot persistence (Save / Load).
//
// # Construction, Options, and Lifecycle Safety
//
// Construct a store via New(opts ...Option), which performs explicit
// validation of all bounds and options without silent clamping. Use
// NoExpiration (-1) or WithNoExpiration() for non-expiring entries; durations
// <= 0 (other than NoExpiration) expire immediately and delete any existing key.
// Per-entry behavior such as TTL jitter (WithTTLJitter), sliding expiration
// (WithSlidingExpiration), and SWR grace windows (WithStaleWindow) can be
// configured via SetWithOptions.
//
// Always call Stop when finished with a MemoryStore instance to halt background
// expiration cleanup, coarse clock updates, SWR refreshes, and invalidation
// workers, close the PubSubClient, and release stored data. After Stop,
// mutating, load, and snapshot operations (Set, SetWithOptions, SetMulti,
// SetJSON, SetJSONWithOptions, Delete, GetOrLoad, GetOrLoadWithOptions,
// GetOrLoadJSON, GetOrLoadJSONWithOptions, Save, Load) return ErrStoreStopped,
// while context-free Get returns (nil, false).
//
// # Storage, Concurrency, Coarse Clock, and Bounded Capacity
//
// MemoryStore partitions keys across 256 inline, 128-byte (two-cache-line)
// shards routed via zero-allocation hash/maphash hashing. Readers traverse a
// pure-Go, zero-unsafe Swiss-style open-addressed bucketTable using
// atomic.Pointer[entry] slots and packed h2 control words completely lock-free
// (0 B/op, 0 allocs/op), while writers serialize per shard via sync.Mutex.
//
// By default, MemoryStore uses a 1ms coarse monotonic clock ticker (falling back
// automatically to exact monotonic reads on both Set and Get for TTLs < 10ms);
// pass WithPreciseClock() to read the exact monotonic clock on every operation.
//
// Optional WithMaxItems and WithMaxBytes enforce soft global bounds using
// WithEvictionPolicy — defaulting to EvictionS3FIFO when bounded (with
// EvictionTinyLFU and EvictionVolatileTTL also selectable) and
// EvictionVolatileTTL when unbounded — without impacting unbounded hot paths.
// WithOnEvict receives expiration, capacity, and deletion notifications outside
// shard locks.
//
// # Byte-Slice Aliasing and Copy Safety
//
// By default, Set, SetWithOptions, SetMulti, Get, GetMulti, and GetOrLoad
// operate on []byte values without defensive copying for maximum throughput.
// MemoryStore stores the provided slice header directly and returns slices that
// alias internal cache storage:
//   - Callers MUST NOT mutate a []byte slice after passing it to Set,
//     SetWithOptions, or SetMulti.
//   - Callers MUST NOT mutate a []byte slice returned from Get, GetMulti, or
//     GetOrLoad.
//   - Mutating an aliased slice introduces data races with concurrent readers and
//     corrupts cached payloads. When reusing pooled buffers (for example from a
//     sync.Pool), enable WithCopyOnWrite(true) / WithCopyOnRead(true) or clone
//     with bytes.Clone.
//
// # Architectural Decision: Byte-Slice Core vs. Generic Store[K comparable, V any]
//
// MemoryStore v2 deliberately preserves string keys and []byte values in its
// 256-shard core engine while providing typed generic JSON helpers
// (GetJSONAs[T], GetOrLoadJSON[T], and GetOrLoadJSONWithOptions[T]), rather
// than parameterizing the core storage engine over arbitrary [K comparable, V any]:
//   - Zero-allocation key hashing: string keys hash directly via hardware-accelerated
//     hash/maphash.String without reflection, fmt formatting, or interface boxing.
//   - GC pointer-scanning isolation: keeping the shard entry layout compact
//     avoids GC mark-phase traversal over arbitrary user-defined struct pointers
//     on large caches.
//   - Canonical wire protocols: string keys and []byte values map directly onto
//     cross-node Pub/Sub invalidation payloads and streaming snapshot formats
//     without requiring caller-supplied codecs for every type instantiation.
//
// # JSON Serialization, Stampede Protection, and Stale-While-Revalidate (SWR)
//
// SetJSON, GetJSON, SetJSONWithOptions, and GetJSONWithOptions use Go 1.27's
// standard library encoding/json/v2 package. Go 1.27 generic methods
// GetJSONAs[T], GetOrLoadJSON[T], and GetOrLoadJSONWithOptions[T] provide
// type-safe reads and singleflight-protected cache population alongside
// raw-byte GetOrLoad and GetOrLoadWithOptions.
//
// When a stale grace window is configured via WithDefaultStaleWhileRevalidate,
// WithStaleWindow, or WithStaleWhileRevalidate, Get and GetMulti strictly treat
// entries past softExpiresAt as misses, while GetOrLoad* serve stale hits
// immediately during [softExpiresAt, hardExpiresAt) and coalesce a single
// detached-context background refresh (preserving the stale entry on failure).
//
// # Observability, Health, and Resilient Pub/Sub Invalidation
//
// MemoryStore delegates Pub/Sub operations to any PubSubClient passed via
// WithPubSub (defaulting to InMemoryPubSub with O(1) exact-topic lookup,
// zero-allocation wildcard matching, and DroppedCount tracking). Setting
// WithInvalidationTopic enables automatic cross-node cache invalidation with
// per-instance UUID echo suppression, exponential-backoff reconnects, non-blocking
// bounded async batch coalescing (default queue 4096 via WithAsyncInvalidation,
// or synchronous via WithSyncInvalidation), a 5s default PubSubTimeout when unset,
// and optional InvalidationModeWriteThrough (WithWriteThroughMaxBytes, defaulting
// to 16 KiB with automatic fallback to InvalidationModeInvalidateOnly).
//
// Real-time event hooks can be attached via WithMetricsRecorder(MetricsRecorder)
// with zero allocations when disabled (and optional InvalidationDropRecorder for
// async queue drop notifications alongside StoreMetrics.InvalidationDrops).
// Health(ctx) verifies store lifecycle status, invalidation backplane health,
// and optional PubSubClient Pinger readiness.
//
// # Streaming Snapshots and Warm Restart
//
// Save(w, opts...) and Load(r, opts...) stream cache contents without acquiring
// global locks or buffering the full dataset in memory. Two zero-dependency
// formats are supported via WithSnapshotFormat:
//   - SnapshotBinary (default): framed "MSNAP\x02" binary stream with bounded
//     frames (up to 256 entries / 64 KiB target), per-frame CRC32C checksums,
//     key/value size guards, and an EOF sentinel verifying entry count and stream
//     digest before committing entries to shards.
//   - SnapshotJSON: token-by-token newline-delimited JSON streaming via Go 1.27's
//     encoding/json/jsontext with base64 values and premature-EOF detection.
//
// Callers may wrap io.Writer / io.Reader with compress/gzip for compressed
// snapshots. Save skips expired entries and preserves relative remaining TTLs,
// NoExpiration (-1), sliding TTLs, and SWR grace windows. Load enforces
// MaxItems/MaxBytes incrementally per entry, supports SnapshotOverwrite (default)
// and SnapshotKeepExisting via WithSnapshotConflictPolicy, and performs local
// hydration without broadcasting distributed invalidations.
package memorystore
