package nats

import (
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	defaultChannelBuffer  = 100
	defaultCleanupTimeout = 10 * time.Second
	defaultDrainTimeout   = 2 * time.Second
	defaultJSInactiveTTL  = 5 * time.Minute
)

type config struct {
	channelBuffer    int
	queueGroup       string
	closeConn        *bool
	natsOptions      []nats.Option
	drainTimeout     time.Duration
	jetStreamEnabled bool
	streamName       string
	durableEnabled   bool
	durablePrefix    string
	jsAutoCreate     bool
	jsStorageSet     bool
	jsStorage        jetstream.StorageType
}

func defaultConfig() config {
	return config{
		channelBuffer: defaultChannelBuffer,
		drainTimeout:  defaultDrainTimeout,
		jsAutoCreate:  true,
	}
}

func (c config) resolveCloseConn(defaultClose bool) bool {
	if c.closeConn != nil {
		return *c.closeConn
	}
	return defaultClose
}

// Option configures a Client created by NewClient or NewClientFromConn.
type Option func(*config)

// WithChannelBuffer sets the buffer size of the channel returned by Subscribe.
// Non-positive sizes are ignored and keep the default of 100.
func WithChannelBuffer(size int) Option {
	return func(c *config) {
		if size > 0 {
			c.channelBuffer = size
		}
	}
}

// WithQueueGroup configures a NATS queue group name for Subscribe so only one
// member of the group receives each message.
func WithQueueGroup(group string) Option {
	return func(c *config) {
		c.queueGroup = group
	}
}

// WithCloseConn controls whether Client.Close drains and closes the underlying
// *nats.Conn. NewClient defaults this to true; NewClientFromConn defaults this
// to false.
func WithCloseConn(closeConn bool) Option {
	return func(c *config) {
		c.closeConn = &closeConn
	}
}

// WithNATSOptions appends nats.Option values forwarded to nats.Connect by
// NewClient. Nil options are ignored. This option has no effect on
// NewClientFromConn.
func WithNATSOptions(opts ...nats.Option) Option {
	return func(c *config) {
		for _, opt := range opts {
			if opt != nil {
				c.natsOptions = append(c.natsOptions, opt)
			}
		}
	}
}

// WithDrainTimeout sets the maximum duration waited for subscription or
// connection draining during Unsubscribe and Close before forcing teardown.
// Non-positive durations are ignored and keep the default of 2 seconds.
func WithDrainTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.drainTimeout = d
		}
	}
}

// WithJetStream enables NATS JetStream mode backed by the given stream name.
func WithJetStream(stream string) Option {
	return func(c *config) {
		c.jetStreamEnabled = true
		c.streamName = stream
	}
}

// WithJetStreamDurable enables durable pull consumers with the given prefix
// when WithJetStream is configured. Durable consumers survive Unsubscribe and
// Close so unacknowledged messages are delivered upon re-subscription.
func WithJetStreamDurable(prefix string) Option {
	return func(c *config) {
		c.durableEnabled = true
		c.durablePrefix = prefix
	}
}

// WithJetStreamAutoCreate controls whether Publish and Subscribe automatically
// create or update the JetStream stream when missing or missing the target
// subject. Defaults to true.
//
// When enabled, Client creates the stream on demand or widens its Subjects
// list (pruning or merging overlapping wildcard subjects) and retries
// concurrent expansion races. Because NATS JetStream stream updates replace
// Subjects without server-side compare-and-swap (CAS), multi-client or
// multi-pod deployments using dynamic subjects should pre-provision a covering
// wildcard stream (for example, "app.>") and set WithJetStreamAutoCreate(false).
//
// When disabled (false), a pre-provisioned stream is required (missing streams
// return jetstream.ErrStreamNotFound): Publish requires the stream's configured
// subjects to cover the published topic, and Subscribe requires stream
// existence without creating or modifying the stream.
func WithJetStreamAutoCreate(enabled bool) Option {
	return func(c *config) {
		c.jsAutoCreate = enabled
	}
}

// WithJetStreamStorage sets the storage backend (for example,
// jetstream.MemoryStorage or jetstream.FileStorage) when auto-creating a
// JetStream stream.
func WithJetStreamStorage(storage jetstream.StorageType) Option {
	return func(c *config) {
		c.jsStorageSet = true
		c.jsStorage = storage
	}
}
