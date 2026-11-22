package memorystore

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PubSubClient defines the provider-agnostic contract for Publish/Subscribe
// backends (In-Memory, Google Cloud Pub/Sub, Azure Service Bus, Redis, NATS,
// Kafka, etc.).
//
// Because PubSubClient relies solely on standard library types, external
// broker adapters can satisfy this interface implicitly without importing
// package memorystore.
type PubSubClient interface {
	// Subscribe subscribes to a topic (or provider-supported pattern) and
	// returns a channel that receives published message payloads.
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)

	// Publish sends a message payload to a topic.
	Publish(ctx context.Context, topic string, message []byte) error

	// Unsubscribe stops receiving messages for a topic and cleans up resources.
	Unsubscribe(ctx context.Context, topic string) error

	// Close shuts down the client and releases all associated resources.
	Close() error
}

// SubscriberCounter is an optional interface that a PubSubClient may implement
// to expose the number of active subscribers for a given topic or pattern.
type SubscriberCounter interface {
	SubscriberCount(pattern string) int
}

// EvictReason describes why an entry was removed from MemoryStore.
type EvictReason uint8

const (
	// EvictExpired indicates the entry expired past its TTL.
	EvictExpired EvictReason = iota + 1
	// EvictCapacity indicates the entry was evicted to stay within MaxItems or MaxBytes.
	EvictCapacity
	// EvictDeleted indicates the entry was removed via Delete or remote invalidation.
	EvictDeleted
)

// String returns a human-readable name for the EvictReason.
func (r EvictReason) String() string {
	switch r {
	case EvictExpired:
		return "expired"
	case EvictCapacity:
		return "capacity"
	case EvictDeleted:
		return "deleted"
	default:
		return "unknown"
	}
}

// Config holds configuration for MemoryStore and its components.
type Config struct {
	// PubSub specifies a custom PubSubClient backend (for example,
	// github.com/BryceWayne/MemoryStore/pubsub/gcp,
	// github.com/BryceWayne/MemoryStore/pubsub/azure, Redis, NATS, or a mock).
	// When nil, MemoryStore uses an InMemoryPubSub instance.
	PubSub PubSubClient

	// PubSubTimeout is the default timeout applied to Subscribe, Publish,
	// and Unsubscribe calls that do not supply an explicit context.
	// When <= 0, no default timeout is applied.
	PubSubTimeout time.Duration

	// InvalidationTopic, when non-empty, enables the distributed cache
	// invalidation backplane over PubSub. Local Set, SetMulti, and Delete
	// operations broadcast invalidation events tagged with InstanceID so peer
	// MemoryStore nodes evict stale entries without self-echo loops.
	InvalidationTopic string

	// MaxItems sets a soft upper bound on the total number of cached entries.
	// When <= 0, item count is unbounded.
	MaxItems int

	// MaxBytes sets a soft upper bound on total stored key+value payload bytes.
	// When <= 0, memory usage is unbounded.
	MaxBytes int64

	// OnEvict is an optional callback invoked outside shard locks whenever an
	// item is removed due to expiration, capacity eviction, or deletion.
	OnEvict func(key string, value []byte, reason EvictReason)

	// CleanupInterval controls how often the background worker removes expired
	// cache entries. Defaults to 1 minute when <= 0.
	CleanupInterval time.Duration

	// Logger is an optional structured logger for lifecycle events.
	// When nil, logging is silenced using slog.DiscardHandler.
	Logger *slog.Logger
}

// Common errors returned by MemoryStore and PubSubClient operations.
var (
	ErrInvalidTopic   = errors.New("invalid topic")
	ErrStoreStopped   = errors.New("store has been stopped")
	ErrNotImplemented = errors.New("feature not implemented by provider")
)
