// Package redis provides a Redis and Valkey Pub/Sub adapter backed by
// github.com/redis/go-redis/v9 that satisfies memorystore.PubSubClient.
//
// Because memorystore.PubSubClient uses only standard library types, this
// package is maintained as an independent Go module so core MemoryStore users
// do not incur Redis client dependencies.
//
// Both constructors operate against redis.UniversalClient, supporting
// standalone Redis/Valkey instances, Redis Sentinel deployments, and Redis
// Cluster topologies seamlessly.
//
// # Client Ownership & Options
//
// Use NewClient with *redis.UniversalOptions to construct and manage a new
// redis.UniversalClient whose lifecycle is owned by the adapter (Close closes
// the underlying Redis client by default). Use NewClientFromClient to wrap an
// existing caller-managed redis.UniversalClient; in that mode Close leaves the
// injected client open by default so it can be shared with other components.
//
// Both defaults can be overridden with WithCloseClient, the buffer size of
// channels returned by Subscribe can be configured with WithChannelBuffer
// (default 100), and structured background/cleanup logging can be enabled with
// WithLogger.
//
// # Wildcard Pattern Routing
//
// Exact topics are subscribed via Redis SUBSCRIBE and unsubscribed via
// UNSUBSCRIBE. Topics containing Redis glob metacharacters ('*', '?', or '[')
// are automatically routed via Redis PSUBSCRIBE and PUNSUBSCRIBE.
//
// Note the semantic difference between Redis pattern matching and
// memorystore.InMemoryPubSub: Redis PSUBSCRIBE applies glob matching across ':'
// delimiters (for example, "user:*" matches both "user:123" and
// "user:123:profile", and "?" / "[abc]" act as single-character and
// character-class globs), whereas InMemoryPubSub restricts '*' to matching a
// single ':'-delimited segment.
//
// # Test Dependencies
//
// This module's go.mod requires github.com/alicebob/miniredis/v2 solely for
// offline unit tests (*_test.go). Under Go 1.27's pruned module graph,
// miniredis/v2 is excluded from production builds (go list -deps .) and from
// downstream consumers' go.mod require lists.
package redis
