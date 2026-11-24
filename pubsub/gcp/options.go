package gcp

import (
	"log/slog"

	"google.golang.org/api/option"
)

type config struct {
	clientOptions      []option.ClientOption
	channelBuffer      int
	logger             *slog.Logger
	subscriptionID     string
	subscriptionIDFunc func(topic string) string
	autoManage         *bool
}

func defaultConfig() config {
	return config{
		channelBuffer: defaultChannelBuffer,
	}
}

func (c config) shouldAutoManage() bool {
	if c.autoManage != nil {
		return *c.autoManage
	}
	// When a caller provides a static pre-provisioned subscription ID,
	// default to non-managing mode so least-privilege subscriber IAM roles work.
	return c.subscriptionID == ""
}

// Option configures a Client created by NewClientWithOptions.
type Option func(*config)

// WithClientOptions appends Google API client options passed to the underlying
// cloud.google.com/go/pubsub/v2 client. Nil elements are ignored.
func WithClientOptions(opts ...option.ClientOption) Option {
	return func(c *config) {
		for _, opt := range opts {
			if opt != nil {
				c.clientOptions = append(c.clientOptions, opt)
			}
		}
	}
}

// WithChannelBuffer sets the buffer size of the channel returned by Subscribe.
// Non-positive sizes are ignored, preserving the default buffer size of 100.
func WithChannelBuffer(size int) Option {
	return func(c *config) {
		if size > 0 {
			c.channelBuffer = size
		}
	}
}

// WithLogger sets an optional structured logger for background receive,
// cancellation, and subscription cleanup errors. Nil loggers disable logging.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		c.logger = l
	}
}

// WithSubscriptionID configures a pre-provisioned Google Cloud Pub/Sub
// subscription ID (3 to 255 characters, not a full projects/... resource path)
// to attach to instead of creating ephemeral subscriptions. Unless overridden by
// WithAutoManage(true), setting a static subscription ID disables automatic
// topic/subscription creation and deletion.
func WithSubscriptionID(id string) Option {
	return func(c *config) {
		c.subscriptionID = id
	}
}

// WithSubscriptionIDFunc configures a custom function that derives the Google
// Cloud Pub/Sub subscription ID (3 to 255 characters) for a given topic.
func WithSubscriptionIDFunc(fn func(topic string) string) Option {
	return func(c *config) {
		c.subscriptionIDFunc = fn
	}
}

// WithAutoManage controls whether the Client checks/creates topics and creates
// or deletes ephemeral subscriptions on Subscribe, Unsubscribe, and Close.
func WithAutoManage(enabled bool) Option {
	return func(c *config) {
		c.autoManage = &enabled
	}
}
