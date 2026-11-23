// Package nats provides a NATS Pub/Sub adapter backed by
// github.com/nats-io/nats.go that satisfies memorystore.PubSubClient.
//
// Because memorystore.PubSubClient uses only standard library types, this
// package is maintained as an independent Go module so core MemoryStore users
// do not incur NATS client dependencies.
//
// # Connection & Ownership
//
// Use NewClient to dial a NATS server URL (or comma-separated cluster URLs)
// with optional WithNATSOptions passthrough; Clients created with NewClient own
// the underlying *nats.Conn and drain/close it on Close by default. Use
// NewClientFromConn to wrap an existing *nats.Conn (caller-owned by default;
// Close leaves the connection open unless overridden with WithCloseConn(true)).
//
// # Wildcards & Queue Groups
//
// Core NATS subjects natively support single-token (*) and multi-token (>)
// wildcards (for example, "cache.*" or "cache.>"). Note that NATS subjects are
// dot-delimited ('.') whereas memorystore.InMemoryPubSub uses colon-delimited
// (':') single-segment '*' matching.
//
// Configure WithQueueGroup to join a NATS queue group so only one subscriber
// instance in the group receives each published message, rather than
// broadcasting to every subscriber.
//
// # Optional JetStream Mode
//
// By default, Client uses Core NATS at-most-once publish/subscribe. Enable
// WithJetStream(stream) to publish and consume via NATS JetStream on the named
// stream (auto-creating or updating the stream's subjects on demand, pruning or
// merging overlapping wildcard subjects as needed, unless disabled with
// WithJetStreamAutoCreate(false)). In JetStream mode, Subscribe uses an
// ephemeral OrderedConsumer for broadcast delivery, a shared pull consumer when
// WithQueueGroup is set, or a persistent durable pull consumer when
// WithJetStreamDurable(prefix) is set.
//
// Because NATS JetStream stream updates overwrite Subjects without server-side
// compare-and-swap (CAS), Client serializes stream updates within the process
// and verifies/retries concurrent subject expansions. For multi-pod or
// multi-client deployments using dynamic subjects, pre-create a covering
// wildcard stream (for example, "app.>") and configure
// WithJetStreamAutoCreate(false). With auto-create disabled, the stream must
// already exist (otherwise Publish and Subscribe return
// jetstream.ErrStreamNotFound), Publish requires the stream's configured
// subjects to cover the target topic, and Subscribe requires stream existence.
//
// # Options
//
// Client behavior can be tuned with WithChannelBuffer (default 100),
// WithQueueGroup, WithCloseConn, WithNATSOptions, WithDrainTimeout,
// WithJetStream, WithJetStreamDurable, WithJetStreamAutoCreate, and
// WithJetStreamStorage.
//
// Example:
//
//	ps, err := nats.NewClient("nats://127.0.0.1:4222")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	store := memorystore.NewMemoryStoreWithConfig(memorystore.Config{
//	    PubSub: ps,
//	})
//	defer store.Stop()
package nats
