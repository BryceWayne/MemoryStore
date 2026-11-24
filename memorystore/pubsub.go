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

// Pinger is an optional interface that a PubSubClient may implement to
// support readiness and connectivity health checks via MemoryStore.Health.
type Pinger interface {
	Ping(ctx context.Context) error
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

// EvictionPolicy selects the cache eviction algorithm used when MaxItems or
// MaxBytes is configured.
type EvictionPolicy uint8

const (
	// EvictionVolatileTTL evicts expired entries first and otherwise samples
	// entries for the earliest expiration timestamp.
	EvictionVolatileTTL EvictionPolicy = iota
	// EvictionS3FIFO selects the S3-FIFO eviction algorithm (requires bounded capacity).
	EvictionS3FIFO
	// EvictionTinyLFU selects the W-TinyLFU eviction algorithm (requires bounded capacity).
	EvictionTinyLFU
)

// String returns a human-readable name for the EvictionPolicy.
func (p EvictionPolicy) String() string {
	switch p {
	case EvictionVolatileTTL:
		return "volatile-ttl"
	case EvictionS3FIFO:
		return "s3-fifo"
	case EvictionTinyLFU:
		return "w-tinylfu"
	default:
		return "unknown"
	}
}

// MetricsRecorder receives real-time cache and invalidation events.
// Implementations MUST be safe for concurrent calls from multiple goroutines
// and MUST be non-blocking (never perform network I/O inline on hot paths).
type MetricsRecorder interface {
	RecordHit()
	RecordMiss()
	RecordEvict(reason EvictReason, bytes int64)
	RecordLoad(duration time.Duration, shared bool, err error)
	RecordInvalidation(sent bool, keyCount int)
}

// InvalidationDropRecorder is an optional interface that a MetricsRecorder
// may implement to receive notifications when async invalidation events are
// dropped due to a full invalidation queue.
type InvalidationDropRecorder interface {
	RecordInvalidationDrop(keyCount int)
}

// InvalidationMode selects how distributed cache invalidations are broadcast
// over Config.InvalidationTopic.
type InvalidationMode uint8

const (
	// InvalidationModeInvalidateOnly broadcasts only changed keys so peer
	// nodes delete their local copies (default).
	InvalidationModeInvalidateOnly InvalidationMode = iota
	// InvalidationModeWriteThrough broadcasts key, payload, and remaining TTL
	// for Set/SetWithOptions/SetMulti entries within WriteThroughMaxBytes so
	// peer nodes can warm their local caches without an upstream miss.
	InvalidationModeWriteThrough
)

// String returns a human-readable name for the InvalidationMode.
func (m InvalidationMode) String() string {
	switch m {
	case InvalidationModeInvalidateOnly:
		return "invalidate-only"
	case InvalidationModeWriteThrough:
		return "write-through"
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
	// When 0, no default timeout is applied. Must be >= 0.
	PubSubTimeout time.Duration

	// InvalidationTopic, when non-empty, enables the distributed cache
	// invalidation backplane over PubSub. Local Set, SetWithOptions, SetMulti,
	// and Delete operations broadcast invalidation events tagged with InstanceID
	// so peer MemoryStore nodes evict stale entries without self-echo loops.
	// Requires a non-nil PubSubClient.
	InvalidationTopic string

	// MaxItems sets a soft upper bound on the total number of cached entries.
	// When 0, item count is unbounded. Must be >= 0.
	MaxItems int

	// MaxBytes sets a soft upper bound on total stored key+value payload bytes.
	// When 0, memory usage is unbounded. Must be >= 0.
	MaxBytes int64

	// OnEvict is an optional callback invoked outside shard locks whenever an
	// item is removed due to expiration, capacity eviction, or deletion.
	OnEvict func(key string, value []byte, reason EvictReason)

	// CleanupInterval controls how often the background worker removes expired
	// cache entries. Defaults to 1 minute when 0. Must be >= 0.
	CleanupInterval time.Duration

	// Logger is an optional structured logger for lifecycle events.
	// When nil, logging is silenced using slog.DiscardHandler.
	Logger *slog.Logger

	// CopyOnWrite, when true, clones caller byte slices on Set, SetWithOptions,
	// and SetMulti so subsequent caller mutations cannot corrupt cache storage.
	CopyOnWrite bool

	// CopyOnRead, when true, clones returned byte slices on Get, GetMulti, and
	// GetOrLoad so caller mutations cannot corrupt cache storage.
	CopyOnRead bool

	// MetricsRecorder receives real-time cache and invalidation metrics events.
	// When nil, metric recording is disabled with zero allocations.
	MetricsRecorder MetricsRecorder

	// EvictionPolicy selects the capacity eviction policy.
	// Bounded stores (MaxItems > 0 || MaxBytes > 0) default to EvictionS3FIFO;
	// unbounded stores default to EvictionVolatileTTL.
	EvictionPolicy    EvictionPolicy
	evictionPolicySet bool

	// PreciseClock disables the 1ms coarse monotonic clock ticker and forces
	// exact monotonic clock reads on every Get/Set operation.
	PreciseClock bool

	// DefaultTTLJitter sets a store-wide +/- jitter fraction in [0.0, 0.5]
	// applied to positive TTLs unless overridden by WithTTLJitter.
	DefaultTTLJitter float64

	// DefaultStaleWhileRevalidate sets a store-wide Stale-While-Revalidate
	// grace window applied to expiring entries unless overridden by
	// WithStaleWindow or WithStaleWhileRevalidate. Must be >= 0.
	DefaultStaleWhileRevalidate time.Duration

	// InvalidationMode selects InvalidationModeInvalidateOnly (default) or
	// InvalidationModeWriteThrough when InvalidationTopic is configured.
	InvalidationMode InvalidationMode

	// WriteThroughMaxBytes sets the maximum encoded invalidation message size
	// in bytes for InvalidationModeWriteThrough before falling back to
	// InvalidationModeInvalidateOnly. Defaults to 16 KiB when unset.
	WriteThroughMaxBytes    int
	writeThroughMaxBytesSet bool

	// AsyncInvalidationQueueSize configures the bounded async invalidation
	// queue depth when InvalidationTopic is set. Defaults to 4096 when unset
	// unless WithSyncInvalidation() is enabled.
	AsyncInvalidationQueueSize int
	asyncInvalidationSet       bool
	syncInvalidation           bool

	ttlJitterSeed    uint64
	hasTTLJitterSeed bool
}

// Option configures a MemoryStore created via New.
type Option func(*Config) error

// WithMaxItems sets a soft upper bound on the total number of cached entries.
func WithMaxItems(n int) Option {
	return func(c *Config) error {
		c.MaxItems = n
		return nil
	}
}

// WithMaxBytes sets a soft upper bound on total stored key+value payload bytes.
func WithMaxBytes(bytes int64) Option {
	return func(c *Config) error {
		c.MaxBytes = bytes
		return nil
	}
}

// WithCleanupInterval controls how often the background worker removes expired entries.
func WithCleanupInterval(d time.Duration) Option {
	return func(c *Config) error {
		c.CleanupInterval = d
		return nil
	}
}

// WithPubSub configures a custom PubSubClient backend.
func WithPubSub(ps PubSubClient) Option {
	return func(c *Config) error {
		c.PubSub = ps
		return nil
	}
}

// WithPubSubTimeout sets the default timeout for non-context Pub/Sub helpers.
func WithPubSubTimeout(d time.Duration) Option {
	return func(c *Config) error {
		c.PubSubTimeout = d
		return nil
	}
}

// WithInvalidationTopic enables distributed cache invalidation over the given
// Pub/Sub topic. Requires WithPubSub to be set.
func WithInvalidationTopic(topic string) Option {
	return func(c *Config) error {
		c.InvalidationTopic = topic
		return nil
	}
}

// WithOnEvict registers an eviction callback invoked outside shard locks.
func WithOnEvict(fn func(key string, value []byte, reason EvictReason)) Option {
	return func(c *Config) error {
		c.OnEvict = fn
		return nil
	}
}

// WithLogger configures the structured logger used for lifecycle events.
func WithLogger(l *slog.Logger) Option {
	return func(c *Config) error {
		c.Logger = l
		return nil
	}
}

// WithCopyOnWrite enables or disables defensive copying of input []byte slices
// on Set, SetWithOptions, and SetMulti.
func WithCopyOnWrite(enabled bool) Option {
	return func(c *Config) error {
		c.CopyOnWrite = enabled
		return nil
	}
}

// WithCopyOnRead enables or disables defensive copying of returned []byte slices
// on Get, GetMulti, and GetOrLoad.
func WithCopyOnRead(enabled bool) Option {
	return func(c *Config) error {
		c.CopyOnRead = enabled
		return nil
	}
}

// WithMetricsRecorder configures a real-time MetricsRecorder hook.
// Passing nil (or a typed nil pointer) disables recording with zero allocations.
func WithMetricsRecorder(r MetricsRecorder) Option {
	return func(c *Config) error {
		c.MetricsRecorder = r
		return nil
	}
}

// WithEvictionPolicy selects the capacity eviction policy.
func WithEvictionPolicy(p EvictionPolicy) Option {
	return func(c *Config) error {
		c.EvictionPolicy = p
		c.evictionPolicySet = true
		return nil
	}
}

// WithPreciseClock opts out of the default 1ms coarse monotonic clock and reads
// the exact monotonic clock on every Get and Set operation.
func WithPreciseClock() Option {
	return func(c *Config) error {
		c.PreciseClock = true
		return nil
	}
}

// WithDefaultTTLJitter sets a store-wide +/- TTL jitter fraction in [0.0, 0.5].
func WithDefaultTTLJitter(fraction float64) Option {
	return func(c *Config) error {
		c.DefaultTTLJitter = fraction
		return nil
	}
}

// WithTTLJitterSeed configures a deterministic PRNG seed for TTL jitter
// calculations (intended for reproducible unit tests). When unset, jitter uses
// the lock-free runtime PRNG.
func WithTTLJitterSeed(seed uint64) Option {
	return func(c *Config) error {
		c.ttlJitterSeed = seed
		c.hasTTLJitterSeed = true
		return nil
	}
}

// WithDefaultStaleWhileRevalidate sets a store-wide Stale-While-Revalidate
// grace window applied to expiring entries unless overridden per-entry via
// WithStaleWindow or per-load via WithStaleWhileRevalidate.
func WithDefaultStaleWhileRevalidate(grace time.Duration) Option {
	return func(c *Config) error {
		c.DefaultStaleWhileRevalidate = grace
		return nil
	}
}

// WithInvalidationMode selects the distributed invalidation broadcast mode
// (InvalidationModeInvalidateOnly or InvalidationModeWriteThrough).
func WithInvalidationMode(mode InvalidationMode) Option {
	return func(c *Config) error {
		c.InvalidationMode = mode
		return nil
	}
}

// WithWriteThroughMaxBytes sets the maximum encoded payload size in bytes for
// InvalidationModeWriteThrough broadcasts before falling back to
// InvalidationModeInvalidateOnly. Defaults to 16 KiB when unset; 0 forces
// immediate fallback to InvalidateOnly.
func WithWriteThroughMaxBytes(limit int) Option {
	return func(c *Config) error {
		c.WriteThroughMaxBytes = limit
		c.writeThroughMaxBytesSet = true
		return nil
	}
}

// WithAsyncInvalidation configures the bounded queue depth for non-blocking
// asynchronous invalidation dispatch. Must be > 0.
func WithAsyncInvalidation(queueSize int) Option {
	return func(c *Config) error {
		c.AsyncInvalidationQueueSize = queueSize
		c.asyncInvalidationSet = true
		c.syncInvalidation = false
		return nil
	}
}

// WithSyncInvalidation forces invalidation broadcasts on Set/SetMulti/Delete
// to publish synchronously rather than using the async invalidation queue.
func WithSyncInvalidation() Option {
	return func(c *Config) error {
		c.syncInvalidation = true
		c.asyncInvalidationSet = false
		c.AsyncInvalidationQueueSize = 0
		return nil
	}
}

// Common errors returned by MemoryStore and PubSubClient operations.
var (
	ErrInvalidTopic               = errors.New("invalid topic")
	ErrStoreStopped               = errors.New("store has been stopped")
	ErrNotImplemented             = errors.New("feature not implemented by provider")
	ErrCorruptSnapshot            = errors.New("corrupt snapshot")
	ErrUnsupportedSnapshotVersion = errors.New("unsupported snapshot version")
)
