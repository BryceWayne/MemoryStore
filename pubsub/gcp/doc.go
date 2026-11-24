// Package gcp provides a Google Cloud Pub/Sub adapter backed by
// cloud.google.com/go/pubsub/v2 that satisfies memorystore.PubSubClient.
//
// Because memorystore.PubSubClient uses only standard library types, this
// package is maintained as an independent Go module so core MemoryStore users
// do not incur Google Cloud, gRPC, or OpenTelemetry dependencies.
//
// # Options
//
// NewClient preserves the original signature taking Google API client options.
// NewClientWithOptions accepts functional Option values including:
//   - WithClientOptions: forward Google API client options (e.g., credentials, gRPC conn)
//   - WithChannelBuffer: configure the per-subscription delivery channel buffer size
//   - WithLogger: set a *slog.Logger for background receive and cleanup diagnostics
//   - WithSubscriptionID / WithSubscriptionIDFunc / WithAutoManage: attach to
//     pre-provisioned subscriptions without requiring topic/subscription admin IAM roles
//
// # Example
//
//	import (
//	    "github.com/BryceWayne/MemoryStore/v2/memorystore"
//	    "github.com/BryceWayne/MemoryStore/pubsub/gcp"
//	)
//
//	ps, err := gcp.NewClientWithOptions(ctx, "my-gcp-project",
//	    gcp.WithChannelBuffer(256),
//	    gcp.WithLogger(slog.Default()),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	store, err := memorystore.New(memorystore.WithPubSub(ps))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer store.Stop()
package gcp
