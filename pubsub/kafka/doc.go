// Package kafka provides a pure-Go Apache Kafka and Redpanda adapter backed by
// github.com/twmb/franz-go/pkg/kgo that satisfies memorystore.PubSubClient.
//
// Because memorystore.PubSubClient uses only standard library types, this
// package is maintained as an independent Go module with zero cgo dependencies
// (CGO_ENABLED=0) so core MemoryStore users do not incur Kafka client
// dependencies.
//
// # Subscription Lifecycle Modes
//
// By default, Subscribe operates in broadcast mode: each subscription creates an
// isolated consumer starting at the latest log-end offset (AtEnd) across all
// partitions of the topic, without joining a consumer group. Every MemoryStore
// instance therefore receives all newly published cache invalidation events
// without replaying historical records or polluting broker consumer-group
// state.
//
// For work-queue fan-out where messages should be load-balanced across
// instances, configure a shared group ID with WithConsumerGroup. In consumer
// group mode, Unsubscribe and Close commit marked (delivered) offsets and leave
// the group cleanly within a bounded shutdown timeout.
//
// # Authentication & Security
//
// Configure TLS with WithTLSConfig and SASL authentication with
// WithSCRAMSHA256, WithSCRAMSHA512, or WithSASL for arbitrary sasl.Mechanism
// implementations. Additional franz-go options can be supplied via
// WithKafkaOptions; producer-only, consumer-only, and group-only options are
// automatically routed to the appropriate underlying kgo clients.
//
// # Topic Provisioning & Observability
//
// Automatic topic creation in metadata requests is enabled by default
// (matching brokers configured with auto.create.topics.enable=true). For
// clusters with pre-provisioned topics or strict ACLs, pass
// WithAutoCreateTopics(false). Use WithLogger to capture structured warnings
// for background fetch, offset commit, consumer group leave, and producer flush
// errors.
//
// # Test Dependencies
//
// This module's go.mod requires github.com/twmb/franz-go/pkg/kfake solely for
// offline unit tests (*_test.go). Under Go 1.27's pruned module graph, kfake
// is excluded from production builds (go list -deps .) and from downstream
// consumers' go.mod require lists.
package kafka
