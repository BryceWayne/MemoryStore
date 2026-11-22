// Package memorystore provides a high-performance, thread-safe, sharded in-memory
// key-value cache with automatic TTL expiration, standard-library encoding/json/v2
// serialization, batch operations, metrics, and a pluggable Publish/Subscribe layer.
//
// # Storage and Concurrency
//
// MemoryStore partitions keys across 256 inline, cache-line padded shards routed
// via zero-allocation hash/maphash hashing. Each shard is protected by its own
// sync.RWMutex to minimize lock contention and false sharing under concurrent load.
//
// # JSON Serialization
//
// SetJSON and GetJSON serialize and deserialize structured values using Go 1.27's
// encoding/json/v2 package. Callers that need custom encoding/json/v2 or jsontext
// options—including encoding/json.DefaultOptionsV1 for legacy v1 semantics—can use
// SetJSONWithOptions and GetJSONWithOptions.
//
// # Publish / Subscribe
//
// By default, MemoryStore initializes an In-Memory Pub/Sub broker supporting exact
// topics and ':'-delimited wildcard patterns (such as "user:*"). Providing a
// Google Cloud Project ID via Config.GCPProjectID or the GOOGLE_CLOUD_PROJECT
// environment variable enables Google Cloud Pub/Sub with automatic fallback to the
// In-Memory broker on initialization failure.
//
// # Lifecycle
//
// Always call Stop when finished with a MemoryStore instance to halt background
// expiration cleanup, close active Pub/Sub subscriptions, and release stored data.
package memorystore
