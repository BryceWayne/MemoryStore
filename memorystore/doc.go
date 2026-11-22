// Package memorystore provides a zero-dependency, thread-safe, sharded in-memory
// key-value cache with automatic TTL expiration, optional bounded capacity,
// stampede protection, Go 1.27 generic JSON helpers (encoding/json/v2),
// low-allocation batch operations, per-shard metrics, and a pluggable
// Publish/Subscribe layer with optional distributed cache invalidation.
//
// # Storage, Concurrency, and Bounded Capacity
//
// MemoryStore partitions keys across 256 inline, 64-byte cache-line aligned
// shards routed via zero-allocation hash/maphash hashing. Each shard owns its
// own sync.RWMutex, atomic hit/miss/eviction counters, and byte tracker.
// Optional Config.MaxItems and Config.MaxBytes enforce soft global bounds using
// volatile-TTL sampling without impacting unbounded hot paths, and Config.OnEvict
// receives expiration, capacity, and deletion notifications outside shard locks.
//
// # JSON Serialization, Generic Methods, and Stampede Protection
//
// SetJSON, GetJSON, SetJSONWithOptions, and GetJSONWithOptions use Go 1.27's
// standard library encoding/json/v2 package. Go 1.27 generic methods GetJSONAs[T]
// and GetOrLoadJSON[T] provide type-safe reads and singleflight-protected cache
// population alongside raw-byte GetOrLoad.
//
// # Pluggable Pub/Sub & Distributed Cache Invalidation
//
// MemoryStore delegates Pub/Sub operations to any PubSubClient passed via
// Config.PubSub (defaulting to InMemoryPubSub with O(1) exact-topic lookup and
// zero-allocation wildcard matching). Setting Config.InvalidationTopic enables
// automatic cross-node cache invalidation over PubSub with per-instance UUID
// echo suppression so multi-pod deployments stay coherent without Redis.
//
// # Lifecycle
//
// Always call Stop when finished with a MemoryStore instance to halt background
// expiration cleanup and invalidation listeners, close the PubSubClient, and
// release stored data.
package memorystore
