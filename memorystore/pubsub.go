package memorystore

import (
	"errors"
	"time"
)

// PubSubClient defines the interface for Publish/Subscribe operations
// to make the underlying implementation agnostic (In-Memory, GCP, etc).
type PubSubClient interface {
	// Subscribe subscribes to a topic and returns a channel for messages.
	// For GCP PubSub, 'topic' maps to a Topic, and a temporary subscription is created.
	// For In-Memory, 'topic' is the pattern/channel name.
	Subscribe(topic string) (<-chan []byte, error)

	// Publish sends a message to a topic.
	Publish(topic string, message []byte) error

	// Unsubscribe stops receiving messages for a topic and cleans up resources.
	Unsubscribe(topic string) error

	// Close shuts down the client and cleans up resources.
	Close() error
}

// Config holds configuration for the MemoryStore and its components.
type Config struct {
	// GCPProjectID is the Google Cloud Project ID.
	// If set, MemoryStore will attempt to use GCP PubSub.
	GCPProjectID string

	// PubSubTimeout is the timeout for PubSub operations.
	PubSubTimeout time.Duration
}

// Common errors for PubSub operations
var (
	ErrInvalidTopic   = errors.New("invalid topic")
	ErrStoreStopped   = errors.New("store has been stopped")
	ErrNotImplemented = errors.New("feature not implemented by provider")
)
