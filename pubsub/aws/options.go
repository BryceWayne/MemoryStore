package aws

import (
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const (
	defaultChannelBuffer       = 100
	defaultWaitTimeSeconds     = int32(20)
	defaultMaxNumberOfMessages = int32(10)
	maxWaitTimeSeconds         = int32(20)
	maxNumberOfMessages        = int32(10)
)

type config struct {
	queueURL            string
	queueURLFunc        func(topic string) string
	topicARN            string
	autoManage          bool
	autoManageSet       bool
	waitTimeSeconds     int32
	maxNumberOfMessages int32
	channelBuffer       int
	snsOptions          []func(*sns.Options)
	sqsOptions          []func(*sqs.Options)
	logger              *slog.Logger
}

func defaultConfig() config {
	return config{
		autoManage:          true,
		waitTimeSeconds:     defaultWaitTimeSeconds,
		maxNumberOfMessages: defaultMaxNumberOfMessages,
		channelBuffer:       defaultChannelBuffer,
	}
}

func (c config) shouldAutoManage() bool {
	if c.autoManageSet {
		return c.autoManage
	}
	// When a caller provides a pre-provisioned SQS queue URL or resolver func,
	// default to non-managing mode so least-privilege IAM roles work out of the box.
	return c.queueURL == "" && c.queueURLFunc == nil
}

// Option configures a Client created by NewClient.
type Option func(*config)

// WithChannelBuffer sets the buffer size of the channel returned by Subscribe.
// Non-positive values are ignored. Defaults to 100.
func WithChannelBuffer(size int) Option {
	return func(c *config) {
		if size > 0 {
			c.channelBuffer = size
		}
	}
}

// WithAutoManage controls whether the Client dynamically creates and deletes
// SNS topics, ephemeral SQS queues, queue policies, and SNS-to-SQS
// subscriptions. Defaults to true unless WithQueueURL is configured.
func WithAutoManage(enabled bool) Option {
	return func(c *config) {
		c.autoManage = enabled
		c.autoManageSet = true
	}
}

// WithQueueURL configures a pre-provisioned SQS queue URL to poll on Subscribe.
// Unless overridden by WithAutoManage(true), setting a non-empty queue URL
// automatically disables dynamic resource management so least-privilege IAM
// roles work without SNS/SQS management permissions.
func WithQueueURL(url string) Option {
	return func(c *config) {
		c.queueURL = url
	}
}

// WithQueueURLFunc configures a custom function that resolves the SQS queue URL
// for a given topic. When set, it takes precedence over WithQueueURL and
// automatically disables dynamic resource management unless overridden by
// WithAutoManage(true).
func WithQueueURLFunc(fn func(topic string) string) Option {
	return func(c *config) {
		c.queueURLFunc = fn
	}
}

// WithLogger sets an optional structured logger for background receive, delete,
// base64 payload decode, slow-consumer, and cleanup warnings. Nil loggers
// disable logging.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		c.logger = l
	}
}

// WithTopicARN configures the SNS topic ARN used for Publish and Subscribe when
// callers pass logical topic names rather than full ARNs.
func WithTopicARN(topicARN string) Option {
	return func(c *config) {
		c.topicARN = topicARN
	}
}

// WithWaitTimeSeconds sets the SQS ReceiveMessage long-poll wait duration in
// seconds (0..20). Out-of-range values are ignored. Defaults to 20.
func WithWaitTimeSeconds(seconds int32) Option {
	return func(c *config) {
		if seconds >= 0 && seconds <= maxWaitTimeSeconds {
			c.waitTimeSeconds = seconds
		}
	}
}

// WithMaxNumberOfMessages sets the maximum number of SQS messages requested per
// ReceiveMessage call (1..10). Out-of-range values are ignored. Defaults to 10.
func WithMaxNumberOfMessages(n int32) Option {
	return func(c *config) {
		if n >= 1 && n <= maxNumberOfMessages {
			c.maxNumberOfMessages = n
		}
	}
}

// WithSNSOptions appends custom option functions passed to sns.NewFromConfig.
func WithSNSOptions(opts ...func(*sns.Options)) Option {
	return func(c *config) {
		for _, opt := range opts {
			if opt != nil {
				c.snsOptions = append(c.snsOptions, opt)
			}
		}
	}
}

// WithSQSOptions appends custom option functions passed to sqs.NewFromConfig.
func WithSQSOptions(opts ...func(*sqs.Options)) Option {
	return func(c *config) {
		for _, opt := range opts {
			if opt != nil {
				c.sqsOptions = append(c.sqsOptions, opt)
			}
		}
	}
}
