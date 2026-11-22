// Package gcp provides a Google Cloud Pub/Sub adapter backed by
// cloud.google.com/go/pubsub/v2 that satisfies memorystore.PubSubClient.
//
// Because memorystore.PubSubClient uses only standard library types, this
// package is maintained as an independent Go module so core MemoryStore users
// do not incur Google Cloud, gRPC, or OpenTelemetry dependencies.
//
// Example:
//
//	ps, err := gcp.NewClient(ctx, "my-gcp-project")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	store := memorystore.NewMemoryStoreWithConfig(memorystore.Config{
//	    PubSub: ps,
//	})
//	defer store.Stop()
package gcp
