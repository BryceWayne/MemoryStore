package redis

import "log/slog"

const defaultChannelBuffer = 100

type config struct {
	channelBuffer int
	closeClient   bool
	logger        *slog.Logger
}

func defaultConfig(closeClient bool) config {
	return config{
		channelBuffer: defaultChannelBuffer,
		closeClient:   closeClient,
		logger:        discardLogger,
	}
}

// Option configures a Client created by NewClient or NewClientFromClient.
type Option func(*config)

// WithChannelBuffer sets the buffer size of the channel returned by Subscribe.
// Non-positive values are ignored, preserving the default buffer size of 100.
func WithChannelBuffer(size int) Option {
	return func(c *config) {
		if size > 0 {
			c.channelBuffer = size
		}
	}
}

// WithCloseClient controls whether Client.Close also closes the underlying
// redis.UniversalClient. NewClient defaults this to true; NewClientFromClient
// defaults this to false.
func WithCloseClient(closeClient bool) Option {
	return func(c *config) {
		c.closeClient = closeClient
	}
}

// WithLogger sets a structured logger for background receive, retry backoff,
// and subscription cleanup errors. Nil loggers are ignored, preserving the
// default discard logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}
