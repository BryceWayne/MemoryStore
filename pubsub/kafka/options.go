package kafka

import (
	"crypto/tls"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

const defaultChannelBuffer = 100

type config struct {
	channelBuffer    int
	consumerGroup    string
	tlsConfig        *tls.Config
	saslMechanisms   []sasl.Mechanism
	autoCreateTopics bool
	kafkaOptions     []kgo.Opt
}

func defaultConfig() config {
	return config{
		channelBuffer:    defaultChannelBuffer,
		autoCreateTopics: true,
	}
}

// Option configures a Client created by NewClient.
type Option func(*config)

// WithChannelBuffer sets the buffer size of the channel returned by Subscribe.
// Non-positive values are ignored, preserving the default of 100.
func WithChannelBuffer(size int) Option {
	return func(c *config) {
		if size > 0 {
			c.channelBuffer = size
		}
	}
}

// WithConsumerGroup configures a shared Kafka consumer group for Subscribe.
// When non-empty, messages are load-balanced across members of the group
// instead of broadcast to every subscriber. Leave empty (the default) for
// broadcast mode, where each instance receives every newly published message.
//
// Note: in consumer group mode, the adapter sets kgo.OnPartitionsAssigned
// internally to synchronize initial group readiness.
func WithConsumerGroup(group string) Option {
	return func(c *config) {
		c.consumerGroup = group
	}
}

// WithTLSConfig configures TLS for all broker connections via
// kgo.DialTLSConfig.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(c *config) {
		if cfg != nil {
			c.tlsConfig = cfg
		}
	}
}

// WithSASL appends one or more SASL authentication mechanisms via kgo.SASL.
// Nil mechanisms are ignored.
func WithSASL(mechanisms ...sasl.Mechanism) Option {
	return func(c *config) {
		for _, m := range mechanisms {
			if m != nil {
				c.saslMechanisms = append(c.saslMechanisms, m)
			}
		}
	}
}

// WithSCRAMSHA256 appends a SASL SCRAM-SHA-256 mechanism using the supplied
// username and password.
func WithSCRAMSHA256(user, pass string) Option {
	return func(c *config) {
		c.saslMechanisms = append(c.saslMechanisms, scram.Auth{
			User: user,
			Pass: pass,
		}.AsSha256Mechanism())
	}
}

// WithSCRAMSHA512 appends a SASL SCRAM-SHA-512 mechanism using the supplied
// username and password.
func WithSCRAMSHA512(user, pass string) Option {
	return func(c *config) {
		c.saslMechanisms = append(c.saslMechanisms, scram.Auth{
			User: user,
			Pass: pass,
		}.AsSha512Mechanism())
	}
}

// WithAutoCreateTopics controls whether metadata requests allow broker-side
// automatic topic creation. Defaults to true.
func WithAutoCreateTopics(enabled bool) Option {
	return func(c *config) {
		c.autoCreateTopics = enabled
	}
}

// WithKafkaOptions appends custom franz-go kgo.Opt values to the underlying
// producer and consumer clients. Producer-only options are applied only to the
// producer client, consumer-only options only to consumer clients, and group
// options only when WithConsumerGroup is configured. Nil options are ignored.
func WithKafkaOptions(opts ...kgo.Opt) Option {
	return func(c *config) {
		for _, opt := range opts {
			if opt != nil {
				c.kafkaOptions = append(c.kafkaOptions, opt)
			}
		}
	}
}
