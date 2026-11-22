// Package memorystore provides a zero-dependency, thread-safe, sharded in-memory
// key-value cache with automatic TTL expiration, standard-library encoding/json/v2
// serialization, low-allocation batch operations, per-shard metrics, and a
// pluggable Publish/Subscribe interface.
//
// # Storage and Concurrency
//
// MemoryStore partitions keys across 256 inline, cache-line padded shards routed
// via zero-allocation hash/maphash hashing. Each shard owns its own sync.RWMutex
// and atomic metrics counters to minimize lock contention and cross-core false
// sharing under high concurrency.
//
// # JSON Serialization
//
// SetJSON and GetJSON serialize and deserialize structured values using Go 1.27's
// encoding/json/v2 package. Callers that need custom encoding/json/v2 or jsontext
// options—including encoding/json.DefaultOptionsV1 for legacy v1 semantics—can use
// SetJSONWithOptions and GetJSONWithOptions.
//
// # Pluggable Publish / Subscribe
//
// MemoryStore delegates Pub/Sub operations to any PubSubClient implementation
// passed via Config.PubSub. Because PubSubClient uses only standard library
// types, external adapters (such as github.com/BryceWayne/MemoryStore/pubsub/gcp,
// Redis, NATS, or Kafka) can satisfy it without importing package memorystore.
// When Config.PubSub is nil, MemoryStore uses the built-in InMemoryPubSub broker,
// which provides O(1) exact-topic routing and zero-allocation ':'-delimited
// wildcard matching.
//
// # Lifecycle
//
// Always call Stop when finished with a MemoryStore instance to halt background
// expiration cleanup, close the PubSubClient, and release stored data.
package memorystore
