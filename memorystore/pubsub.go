package memorystore

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PubSubClient defines the provider-agnostic contract for Publish/Subscribe
// backends (In-Memory, Google Cloud Pub/Sub, Redis, NATS, Kafka, etc.).
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

// Config holds configuration for MemoryStore and its components.
type Config struct {
	// PubSub specifies a custom PubSubClient backend (for example,
	// github.com/BryceWayne/MemoryStore/pubsub/gcp, Redis, NATS, or a mock).
	// When nil, MemoryStore uses an InMemoryPubSub instance.
	PubSub PubSubClient

	// PubSubTimeout is the default timeout applied to Subscribe, Publish,
	// and Unsubscribe calls that do not supply an explicit context.
	// When <= 0, no default timeout is applied.
	PubSubTimeout time.Duration

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
