# MemoryStore

[![CI](https://github.com/BryceWayne/MemoryStore/actions/workflows/go.yml/badge.svg)](https://github.com/BryceWayne/MemoryStore/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/BryceWayne/MemoryStore/memorystore.svg)](https://pkg.go.dev/github.com/BryceWayne/MemoryStore/memorystore)
[![Go Report Card](https://goreportcard.com/badge/github.com/BryceWayne/MemoryStore)](https://goreportcard.com/report/github.com/BryceWayne/MemoryStore)
[![Go Version](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](https://go.dev/doc/go1.27)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**MemoryStore** is a **zero-dependency**, high-performance, thread-safe, sharded in-memory key-value cache and pluggable Pub/Sub library for Go. Built for **Go 1.27+**, it combines cache-line padded sharded storage with per-shard atomic metrics, automatic TTL expiration, standard-library **`encoding/json/v2`** serialization, stack-optimized batch operations, and a provider-agnostic Publish/Subscribe interface.

---

## Table of Contents

- [Features](#features)
- [Requirements](#requirements)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Usage](#usage)
  - [JSON Serialization (`encoding/json/v2`)](#json-serialization-encodingjsonv2)
  - [Batch Operations](#batch-operations)
  - [Metrics & Monitoring](#metrics--monitoring)
  - [Publish / Subscribe (Pluggable Backends)](#publish--subscribe-pluggable-backends)
  - [Configuration, Logging & Lifecycle](#configuration-logging--lifecycle)
- [API Overview](#api-overview)
- [Repository Layout](#repository-layout)
- [Architecture](#architecture)
- [Performance & Benchmarks](#performance--benchmarks)
- [Development & Testing](#development--testing)
- [Contributing](#contributing)
- [Security](#security)
- [Changelog](#changelog)
- [License](#license)

---

## Features

- **Zero External Dependencies (Core):** The root `github.com/BryceWayne/MemoryStore` module uses **only the Go 1.27 standard library**. Optional broker adapters (like Google Cloud Pub/Sub) live in standalone sub-modules.
- **High-Throughput Sharded Concurrency:** 256 inline, 64-byte cache-line padded shards with per-shard `sync.RWMutex` locks, per-shard `atomic.Int64` metrics (eliminating cross-core cache-line bouncing), and zero-allocation `hash/maphash` routing.
- **Automatic Expiration:** Pointer-free monotonic TTL timestamps with lazy expiration checks and configurable background cleanup.
- **Standard Library JSON v2:** Built on Go 1.27's `encoding/json/v2` with strict RFC 8259 defaults and optional `json.Options` customization (`SetJSONWithOptions` / `GetJSONWithOptions`).
- **Zero-Allocation Batch Writes:** `SetMulti` and `GetMulti` group keys per shard with stack-allocated scratch buffers (for batches up to 128 keys) to minimize lock acquisitions and eliminate heap churn.
- **Pluggable, Context-Aware Pub/Sub:** Provider-agnostic `PubSubClient` interface (using only stdlib types) so any broker — In-Memory (O(1) exact routing + zero-alloc wildcard matching), Google Cloud Pub/Sub v2 (`pubsub/gcp`), Redis, NATS, Kafka, or custom mocks — plugs straight into `Config.PubSub`.
- **Library-Friendly Logging:** Silent by default (`slog.DiscardHandler`); opt in by passing any `*slog.Logger` via `Config.Logger`.
- **Verified Quality:** 99%+ statement coverage on core `memorystore`, tested with `-race` and Go 1.27's `goroutineleak` profiler.

---

## Requirements

- **Go 1.27** or later

---

## Installation

Install the zero-dependency core module:

```bash
go get github.com/BryceWayne/MemoryStore/memorystore
```

*(Optional)* Install the standalone Google Cloud Pub/Sub v2 adapter:

```bash
go get github.com/BryceWayne/MemoryStore/pubsub/gcp
```

---

## Quick Start

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/BryceWayne/MemoryStore/memorystore"
)

func main() {
	store := memorystore.NewMemoryStore()
	defer func() {
		if err := store.Stop(); err != nil {
			log.Printf("failed to stop store: %v", err)
		}
	}()

	if err := store.Set("greeting", []byte("Hello, World!"), time.Minute); err != nil {
		log.Fatal(err)
	}

	if value, ok := store.Get("greeting"); ok {
		fmt.Println(string(value))
	}
}
```

See [examples/basic/main.go](examples/basic/main.go) for a complete runnable program (`make run-example`).

---

## Usage

### JSON Serialization (`encoding/json/v2`)

MemoryStore uses Go 1.27's standard library `encoding/json/v2` for `SetJSON` and `GetJSON`:

```go
type User struct {
	Name    string    `json:"name"`
	Email   string    `json:"email"`
	Created time.Time `json:"created,omitzero"`
}

store := memorystore.NewMemoryStore()
defer store.Stop()

user := User{Name: "Alice", Email: "alice@example.com", Created: time.Now()}
if err := store.SetJSON("user:123", &user, time.Hour); err != nil {
	log.Fatal(err)
}

var retrieved User
exists, err := store.GetJSON("user:123", &retrieved)
if err != nil {
	log.Fatal(err)
}
if exists {
	log.Printf("User: %+v", retrieved)
}
```

> **Note on `encoding/json/v2` defaults:** `encoding/json/v2` marshals nil slices as `[]` and nil maps as `{}`, rejects duplicate JSON object member names, and rejects invalid UTF-8 by default. Passing a pointer (`&user`) to `SetJSON` avoids copying struct values during marshaling.

#### Custom Options & Legacy `encoding/json` v1 Compatibility

Use `SetJSONWithOptions` and `GetJSONWithOptions` to pass any `encoding/json/v2` or `encoding/json/jsontext` options — including `encoding/json.DefaultOptionsV1()` when legacy v1 semantics are required:

```go
import (
	jsonv1 "encoding/json"
	json "encoding/json/v2"
)

// Deterministic map key ordering on marshal
err := store.SetJSONWithOptions("cfg", cfgMap, time.Hour, json.Deterministic(true))

// Reject unknown JSON fields on unmarshal
exists, err := store.GetJSONWithOptions("cfg", &cfg, json.RejectUnknownMembers(true))

// Opt in to legacy encoding/json v1 semantics
err = store.SetJSONWithOptions("legacy", payload, time.Hour, jsonv1.DefaultOptionsV1())
```

### Batch Operations

Group multiple reads or writes into a single per-shard lock pass:

```go
items := map[string][]byte{
	"key1": []byte("val1"),
	"key2": []byte("val2"),
}
if err := store.SetMulti(items, time.Minute); err != nil {
	log.Fatal(err)
}

results := store.GetMulti([]string{"key1", "key2"})
```

### Metrics & Monitoring

Inspect live, per-shard aggregated statistics at any time:

```go
metrics := store.GetMetrics()
log.Printf(
	"Items=%d Hits=%d Misses=%d Evictions=%d",
	metrics.Items, metrics.Hits, metrics.Misses, metrics.Evictions,
)
```

### Publish / Subscribe (Pluggable Backends)

#### Built-In In-Memory Broker (Default)

Provides **O(1)** lookup for exact topics and zero-allocation `:`-delimited wildcard matching (`user:*`, `orders:*:created`):

```go
store := memorystore.NewMemoryStore()
defer store.Stop()

msgs, err := store.Subscribe("user:*")
if err != nil {
	log.Fatal(err)
}

go func() {
	for msg := range msgs {
		log.Printf("Received: %s", string(msg))
	}
}()

if err := store.Publish("user:123", []byte("logged in")); err != nil {
	log.Fatal(err)
}

_ = store.Unsubscribe("user:*")
```

Context-aware variants (`SubscribeContext`, `PublishContext`, `UnsubscribeContext`) are also available for per-call cancellation and deadlines.

#### Plugging In Any External Broker (`PubSubClient`)

Any type implementing the four standard-library methods below automatically satisfies `memorystore.PubSubClient` without even importing `memorystore`:

```go
type PubSubClient interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}
```

#### Google Cloud Pub/Sub (`pubsub/gcp` sub-module)

```go
import (
	"context"
	"log"
	"time"

	"github.com/BryceWayne/MemoryStore/memorystore"
	"github.com/BryceWayne/MemoryStore/pubsub/gcp"
)

ctx := context.Background()
gcpClient, err := gcp.NewClient(ctx, "my-gcp-project-id")
if err != nil {
	log.Fatal(err)
}

store := memorystore.NewMemoryStoreWithConfig(memorystore.Config{
	PubSub:        gcpClient,
	PubSubTimeout: 5 * time.Second,
})
defer store.Stop()
```

### Configuration, Logging & Lifecycle

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
store := memorystore.NewMemoryStoreWithConfig(memorystore.Config{
	PubSub:          customBroker,        // Defaults to InMemoryPubSub
	PubSubTimeout:   5 * time.Second,     // Default timeout for non-context Pub/Sub helpers
	CleanupInterval: 30 * time.Second,    // Background expiration sweep interval (default 1m)
	Logger:          logger,              // Defaults to slog.DiscardHandler (silent)
})
defer store.Stop()
```

- Items with `duration <= 0` expire immediately.
- Always call `store.Stop()` when shutting down to stop background workers, close the `PubSubClient`, and release memory. Multiple calls to `Stop()` are safe and idempotent.

---

## API Overview

Full API documentation is available on [pkg.go.dev](https://pkg.go.dev/github.com/BryceWayne/MemoryStore/memorystore).

| Method / Type | Description |
| :--- | :--- |
| `NewMemoryStore() *MemoryStore` | Creates a store with default settings (In-Memory Pub/Sub, silent logger) |
| `NewMemoryStoreWithConfig(Config) *MemoryStore` | Creates a store with custom `PubSub`, `PubSubTimeout`, `CleanupInterval`, `Logger` |
| `Set(key string, value []byte, duration time.Duration) error` | Stores raw bytes with a TTL |
| `Get(key string) ([]byte, bool)` | Retrieves raw bytes if present and unexpired |
| `Delete(key string)` | Removes a key |
| `SetJSON(key string, value any, duration time.Duration) error` | Marshals via `encoding/json/v2` and stores with a TTL |
| `SetJSONWithOptions(key, value, duration, opts...) error` | Marshals with custom `encoding/json/v2` options |
| `GetJSON(key string, dest any) (bool, error)` | Retrieves and unmarshals via `encoding/json/v2` |
| `GetJSONWithOptions(key, dest, opts...) (bool, error)` | Retrieves and unmarshals with custom `encoding/json/v2` options |
| `SetMulti(items map[string][]byte, duration time.Duration) error` | Batch-sets multiple keys grouped by shard |
| `GetMulti(keys []string) map[string][]byte` | Batch-gets multiple keys grouped by shard |
| `GetMetrics() StoreMetrics` | Returns `Items`, `Hits`, `Misses`, and `Evictions` |
| `Subscribe` / `SubscribeContext` | Subscribes to a topic or pattern |
| `Publish` / `PublishContext` | Publishes a message to matching subscribers |
| `Unsubscribe` / `UnsubscribeContext` | Cancels subscriptions for a topic |
| `SubscriberCount(pattern string) int` | Returns active subscriber count when backend implements `SubscriberCounter` |
| `NewInMemoryPubSub() *InMemoryPubSub` | Instantiates the built-in in-memory Pub/Sub broker directly |
| `Stop() error` / `IsStopped() bool` | Gracefully stops the store and checks lifecycle state |

---

## Repository Layout

```text
.
├── memorystore/        # Core zero-dependency cache & PubSubClient interface
├── pubsub/gcp/         # Standalone Go module for Google Cloud Pub/Sub (v2)
├── docs/images/        # Architecture diagrams
├── examples/basic/     # Runnable usage example
├── Makefile            # Multi-module build, test, lint, and benchmark targets
└── .github/            # CI, release workflow, Dependabot, and issue/PR templates
```

---

## Architecture

![MemoryStore Sharded Cache and Pluggable Pub/Sub Architecture](./docs/images/architecture.svg)

---

## Performance & Benchmarks

MemoryStore is engineered for low latency, high multi-core scalability, and minimal GC pressure on Go 1.27+:

- **Go 1.27 runtime advantages:** Size-specialized small allocations (<80 B), default Green Tea GC, stack-allocated slice backing stores, and container-aware `GOMAXPROCS`.
- **Zero-allocation shard hashing:** `hash/maphash.String` with power-of-2 bitmasking.
- **Zero cross-core metric contention:** 256 inline 64-byte cache-line padded shards, each owning its own `sync.RWMutex` and `atomic.Int64` hit/miss/eviction counters.
- **Pointer-free expiration:** Monotonic `int64` nanosecond offsets shrink each cache entry from 48 B to 32 B and cut GC pointer scanning.
- **Stack-allocated batch grouping:** Intrusive-index shard bucketing with stack scratch buffers achieves **0 B/op, 0 allocs/op** on `SetMulti` batches up to 128 keys.
- **O(1) exact-topic Pub/Sub + zero-alloc wildcards:** Separate exact and wildcard subscription maps in `InMemoryPubSub`.

Representative `benchstat` results (`go1.27.4`, `linux/amd64`, Intel Core i9-12900K, `n=6`, `-benchtime=500ms`):

| Benchmark | Time / op | Bytes / op | Allocs / op |
| :--- | ---: | ---: | ---: |
| `BenchmarkMemoryStore_Set` | 48.6 ns | 0 B | 0 |
| `BenchmarkMemoryStore_Get` | 40.9 ns | 0 B | 0 |
| `BenchmarkMemoryStore_GetParallel` | **7.88 ns** | 0 B | 0 |
| `BenchmarkMemoryStore_SetGetParallel` | 29.3 ns | 0 B | 0 |
| `BenchmarkMemoryStore_SetMulti` (100 keys) | **3.15 µs** | **0 B** | **0** |
| `BenchmarkMemoryStore_GetMulti` (100 keys) | **3.13 µs** | 5,464 B | 4 |
| `BenchmarkMemoryStore_SetJSON_Small` | 207.8 ns | 48 B | 1 |
| `BenchmarkMemoryStore_GetJSON_Small` | 251.6 ns | **0 B** | **0** |
| `BenchmarkMemoryStore_SetJSON_Complex` | 1.73 µs | 512 B | 8 |
| `BenchmarkMemoryStore_GetJSON_Complex` | 2.21 µs | 976 B | 17 |
| `BenchmarkMemoryStore_PubSub` (wildcard) | 80.7 ns | 0 B | 0 |
| `BenchmarkMemoryStore_PubSub_ExactManyTopics` (100 topics) | **38.6 ns** | **0 B** | **0** |

Reproduce locally:

```bash
make bench
# Or with benchstat-friendly repetition:
go test -run='^$' -bench=. -benchmem -count=6 ./memorystore
```

---

## Development & Testing

Use the included [Makefile](Makefile):

```bash
make all          # Run clean, fmt, vet, lint, test, and build across all modules
make test         # Run tests with -race and coverage across all modules
make bench        # Run benchmarks with memory stats
make lint         # Run golangci-lint (v2) across all modules
make vet          # Run go vet across all modules
make fmt          # Format code with go fmt
make coverage     # Generate HTML coverage report
make run-example  # Run examples/basic
make help         # List all targets
```

---

## Contributing

Contributions are welcome and appreciated! Please read our [Contributing Guide](CONTRIBUTING.md) and [Code of Conduct](CODE_OF_CONDUCT.md) before opening an issue or pull request.

- Report bugs via the [Bug Report template](.github/ISSUE_TEMPLATE/bug_report.md).
- Propose enhancements or new `PubSubClient` adapters via the [Feature Request template](.github/ISSUE_TEMPLATE/feature_request.md).
- Target all pull requests against the `root` branch.

---

## Security

Please report security vulnerabilities responsibly as described in [SECURITY.md](SECURITY.md). Do not open public issues for security bugs.

---

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for a history of changes and release notes.

---

## License

This project is licensed under the MIT License — see [LICENSE](LICENSE) for details.
