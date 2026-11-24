# MemoryStore

[![CI](https://github.com/BryceWayne/MemoryStore/actions/workflows/go.yml/badge.svg)](https://github.com/BryceWayne/MemoryStore/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/BryceWayne/MemoryStore/v2/memorystore.svg)](https://pkg.go.dev/github.com/BryceWayne/MemoryStore/v2/memorystore)
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
  - [Go 1.27 Generic Methods, Stampede Protection & Stale-While-Revalidate](#go-127-generic-methods-stampede-protection--stale-while-revalidate)
  - [Per-Entry Options & Expiration (`SetWithOptions`, `NoExpiration`)](#per-entry-options--expiration-setwithoptions-noexpiration)
  - [Distributed Cache Invalidation (Multi-Pod Coherency)](#distributed-cache-invalidation-multi-pod-coherency)
  - [Bounded Capacity & Eviction Callbacks](#bounded-capacity--eviction-callbacks)
  - [Streaming Snapshots & Warm Restart (`Save` / `Load`)](#streaming-snapshots--warm-restart-save--load)
  - [Batch Operations](#batch-operations)
  - [Byte-Slice Aliasing & Memory Safety](#byte-slice-aliasing--memory-safety)
  - [Metrics, Health & Observability](#metrics-health--observability)
    - [OpenTelemetry (`metrics/otel`)](#opentelemetry-metricsotel)
    - [Prometheus (`metrics/prometheus`)](#prometheus-metricsprometheus)
    - [Health Checks (`Health` & `Pinger`)](#health-checks-health--pinger)
  - [Publish / Subscribe (Pluggable Backends)](#publish--subscribe-pluggable-backends)
    - [Broker Capability & Wildcard Matrix](#broker-capability--wildcard-matrix)
  - [Configuration, Logging & Lifecycle](#configuration-logging--lifecycle)
  - [Migrating from `v1.x` to `v2.0`](#migrating-from-v1x-to-v20)
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

- **Zero External Dependencies (Core):** The root `github.com/BryceWayne/MemoryStore/v2` module uses **only the Go 1.27 standard library**. Optional broker adapters (`pubsub/gcp`, `pubsub/azure`, `pubsub/redis`, `pubsub/nats`, `pubsub/aws`, `pubsub/kafka`) and observability adapters (`metrics/otel`, `metrics/prometheus`) live in standalone sub-modules.
- **High-Throughput Lock-Free Sharded Concurrency:** 256 inline, 128-byte (two-cache-line) shards with pure-Go, zero-`unsafe` Swiss-style `atomic.Pointer[entry]` open-addressed buckets, a 1ms coarse monotonic clock (`WithPreciseClock()` opt-out + `<10ms` exact fallback), per-shard `atomic.Int64` metrics, O(1) `Bytes` tracking, and zero-allocation `hash/maphash` routing (**2.60 ns/op** 12-core parallel reads, **4.74 ns/op** mixed parallel, `#1` single-threaded reads & writes).
- **Functional Options & Strict Validation:** Idiomatic `memorystore.New(opts ...Option) (*MemoryStore, error)` constructor with explicit validation (no silent clamping), `NoExpiration` (`-1`), `SetWithOptions` (`WithTTLJitter`, `WithSlidingExpiration`, `WithNoExpiration`, `WithStaleWindow`), deterministic `WithTTLJitterSeed`, opt-in `WithCopyOnWrite`/`WithCopyOnRead`, and explicit `ErrStoreStopped` lifecycle checks.
- **Resilient Distributed Cache Invalidation Backplane:** Opt in via `memorystore.WithInvalidationTopic(...)` to keep local RAM caches coherent across pods over GCP Pub/Sub, Azure Service Bus, Redis/Valkey, NATS, AWS SNS + SQS, Apache Kafka / Redpanda, or any `PubSubClient` — with per-instance UUID echo suppression, non-blocking bounded async batch coalescing (`WithAsyncInvalidation` / `WithSyncInvalidation`), optional `InvalidationModeWriteThrough` (`WithWriteThroughMaxBytes` fallback), exponential-backoff reconnect (`100ms` → `30s`), and `5s` default `PubSubTimeout`.
- **Go 1.27 Generic Methods, Stampede Protection & SWR:** Typed `GetJSONAs[T]` and singleflight-protected `GetOrLoad` / `GetOrLoadWithOptions` / `GetOrLoadJSON[T]` / `GetOrLoadJSONWithOptions[T]` coalesce concurrent cache misses (with panic propagation) and support Stale-While-Revalidate (`WithStaleWhileRevalidate`, `WithDefaultStaleWhileRevalidate`, `WithStaleWindow`) with detached-context background healing.
- **Bounded Capacity & High-Hit-Ratio Eviction:** Optional `WithMaxItems` and `WithMaxBytes` enforce soft global bounds using `WithEvictionPolicy` — defaulting to `EvictionS3FIFO` when bounded (with `EvictionTinyLFU` / `w-tinylfu` and `EvictionVolatileTTL` also selectable) and `0 B/op` lock-free read tracking, plus lock-free `WithOnEvict` callbacks.
- **Standard Library JSON v2:** Built on Go 1.27's `encoding/json/v2` with strict RFC 8259 defaults and optional `json.Options` customization (`SetJSONWithOptions` / `GetJSONWithOptions`).
- **Zero-Allocation Batch Writes:** `SetMulti` and `GetMulti` group keys per shard with stack-allocated scratch buffers (for batches up to 128 keys).
- **Streaming Snapshots & Warm Restart (`Save` / `Load`):** Zero-dependency `O(1)`-buffer streaming persistence supporting `SnapshotBinary` (framed `"MSNAP\x02"` with per-frame `CRC32C` + EOF sentinel) and `SnapshotJSON` (`encoding/json/jsontext` NDJSON), `compress/gzip` compatibility, relative remaining TTL & `NoExpiration` preservation, `SnapshotOverwrite`/`SnapshotKeepExisting`, and incremental `MaxItems`/`MaxBytes` enforcement on `Load`.
- **Pluggable Observability & Health:** Zero-dependency `MetricsRecorder` hook (`WithMetricsRecorder`, zero-allocation nil guard) + optional `InvalidationDropRecorder` & `StoreMetrics.InvalidationDrops`, standalone `metrics/otel` and `metrics/prometheus` sub-modules, `Health(ctx)` + `Pinger` readiness checks, and `InMemoryPubSub.DroppedCount()`.
- **Pluggable, Context-Aware Pub/Sub:** Provider-agnostic `PubSubClient` interface (using only stdlib types) supporting In-Memory (O(1) exact routing + zero-alloc wildcard matching), Google Cloud Pub/Sub v2 (`pubsub/gcp`), Azure Service Bus (`pubsub/azure`), Redis / Valkey (`pubsub/redis`), NATS / JetStream (`pubsub/nats`), AWS SNS + SQS (`pubsub/aws`), Apache Kafka / Redpanda (`pubsub/kafka`), or custom brokers — all with optional `*slog.Logger` (`WithLogger`).
- **Library-Friendly Logging:** Silent by default (`slog.DiscardHandler`); opt in via `memorystore.WithLogger`.
- **Verified Quality:** Tested under `-race` and Go 1.27's `goroutineleak` profiler.

---

## Requirements

- **Go 1.27** or later

---

## Installation

Install the zero-dependency core module:

```bash
go get github.com/BryceWayne/MemoryStore/v2/memorystore
```

*(Optional)* Install standalone observability sub-modules (`v1.0.0`):

```bash
# OpenTelemetry metrics recorder & observable gauges
go get github.com/BryceWayne/MemoryStore/metrics/otel

# Prometheus Collector & MetricsRecorder
go get github.com/BryceWayne/MemoryStore/metrics/prometheus
```

*(Optional)* Install a standalone Pub/Sub adapter:

```bash
# Google Cloud Pub/Sub (v2)
go get github.com/BryceWayne/MemoryStore/pubsub/gcp

# Azure Service Bus
go get github.com/BryceWayne/MemoryStore/pubsub/azure

# Redis / Valkey
go get github.com/BryceWayne/MemoryStore/pubsub/redis

# NATS / JetStream
go get github.com/BryceWayne/MemoryStore/pubsub/nats

# AWS SNS + SQS
go get github.com/BryceWayne/MemoryStore/pubsub/aws

# Apache Kafka / Redpanda (pure Go, zero cgo)
go get github.com/BryceWayne/MemoryStore/pubsub/kafka
```

---

## Quick Start

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

func main() {
	store, err := memorystore.New()
	if err != nil {
		log.Fatal(err)
	}
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

store, err := memorystore.New()
if err != nil {
	log.Fatal(err)
}
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

### Go 1.27 Generic Methods, Stampede Protection & Stale-While-Revalidate

Use Go 1.27 generic methods for ergonomic typed reads and singleflight-protected cache population (coalescing concurrent misses for the same key into a single upstream load), plus **Stale-While-Revalidate (SWR)** so expired entries within a grace window are served immediately while a single detached-context background refresh heals the cache:

```go
store, err := memorystore.New(
	memorystore.WithDefaultStaleWhileRevalidate(5 * time.Minute),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()

// Typed JSON read (Go 1.27 generic method; Get/GetJSONAs strictly treat soft-expired keys as misses)
user, ok, err := store.GetJSONAs[User]("user:123")

// Singleflight-protected typed JSON load (coalesces concurrent misses)
user, err = store.GetOrLoadJSON[User](ctx, "user:123", time.Hour, func(ctx context.Context) (User, error) {
	return db.FetchUser(ctx, "123")
})

// Per-call SWR override on raw or typed loads: serves stale immediately during [TTL, TTL+10m)
// and refreshes asynchronously in the background (preserving stale value if loader fails)
raw, err := store.GetOrLoadWithOptions(
	ctx,
	"report:daily",
	10*time.Minute,
	func(bgCtx context.Context) ([]byte, error) {
		return generateReport(bgCtx)
	},
	memorystore.WithStaleWhileRevalidate(10*time.Minute),
)

user, err = store.GetOrLoadJSONWithOptions[User](
	ctx,
	"user:456",
	time.Hour,
	func(bgCtx context.Context) (User, error) {
		return db.FetchUser(bgCtx, "456")
	},
	[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(5 * time.Minute)},
)
```

### Per-Entry Options & Expiration (`SetWithOptions`, `NoExpiration`)

Use `memorystore.NoExpiration` (`-1`) or `memorystore.WithNoExpiration()` for entries that should never expire by TTL (they still participate in capacity eviction when `MaxItems` or `MaxBytes` is configured). A `ttl <= 0` (when not `NoExpiration`) expires immediately.

Use `SetWithOptions` for per-entry sliding expiration, TTL jitter, or non-expiring entries:

```go
// Never expire by TTL
if err := store.Set("config:global", data, memorystore.NoExpiration); err != nil {
	log.Fatal(err)
}

// Refresh TTL on read hits, spread expiration by +/- 10%, and allow 5m SWR grace
if err := store.SetWithOptions(
	"session:abc",
	sessionBytes,
	30*time.Minute,
	memorystore.WithSlidingExpiration(),
	memorystore.WithTTLJitter(0.10),
	memorystore.WithStaleWindow(5*time.Minute),
); err != nil {
	log.Fatal(err)
}

// Equivalent non-expiring option override
if err := store.SetWithOptions("pinned:key", val, 0, memorystore.WithNoExpiration()); err != nil {
	log.Fatal(err)
}
```

You can also configure a store-wide default TTL jitter via `memorystore.WithDefaultTTLJitter(0.10)` (validated in `[0.0, 0.5]`; per-entry `WithTTLJitter` overrides the store default) or `memorystore.WithTTLJitterSeed(seed)` for deterministic tests.

### Distributed Cache Invalidation (Multi-Pod Coherency)

Keep local RAM reads (~2.6 ns) coherent across multiple application pods over any `PubSubClient` (`pubsub/gcp`, `pubsub/azure`, `pubsub/redis`, `pubsub/nats`, `pubsub/aws`, `pubsub/kafka`, etc.) without a central cache server:

```go
store, err := memorystore.New(
	memorystore.WithPubSub(pubsubClient),
	memorystore.WithInvalidationTopic("cache-invalidations"),
	memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough), // Optional: warm peers on small writes
	memorystore.WithWriteThroughMaxBytes(16*1024),                             // Fallback to InvalidateOnly if larger
	memorystore.WithAsyncInvalidation(4096),                                   // Non-blocking coalesced queue (default when topic set)
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()

// Local Set, SetWithOptions, SetMulti, and Delete enqueue non-blocking invalidation events
// tagged with store.InstanceID() so peer pods evict (or warm in WriteThrough mode) without
// self-echo loops or stalling local writes if the broker slows down.
```

- **Resilient Backplane:** Subscribes synchronously at startup and automatically reconnects on transient disconnects using exponential backoff (`100ms` → `30s` with jitter), defaulting `PubSubTimeout` to `5s` when unset.
- **Non-Blocking & Observable Drops:** If the bounded async invalidation queue fills during a broker outage, local `Set`/`Delete` stay sub-microsecond while incrementing `StoreMetrics.InvalidationDrops`, notifying optional `InvalidationDropRecorder` implementations, and emitting rate-limited warnings. Pass `WithSyncInvalidation()` when synchronous publish on write is desired.

### Bounded Capacity & Eviction Callbacks

Guard against unbounded memory growth with soft global `MaxItems` / `MaxBytes` limits (zero overhead when unset) and an `OnEvict` hook invoked outside shard locks:

```go
store, err := memorystore.New(
	memorystore.WithMaxItems(100_000),
	memorystore.WithMaxBytes(256*1024*1024), // 256 MiB key+value payload budget
	memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO), // Default for bounded stores
	memorystore.WithOnEvict(func(key string, value []byte, reason memorystore.EvictReason) {
		// reason is EvictExpired, EvictCapacity, or EvictDeleted
	}),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

> **Note:** Bounded stores (`MaxItems > 0 || MaxBytes > 0`) default to `EvictionS3FIFO`; `EvictionTinyLFU` (`w-tinylfu`) and `EvictionVolatileTTL` (`volatile-ttl`) are also selectable via `WithEvictionPolicy`. Unbounded stores default to `EvictionVolatileTTL` (and reject `EvictionS3FIFO` / `EvictionTinyLFU`).

### Streaming Snapshots & Warm Restart (`Save` / `Load`)

Persist and warm-restart caches in `O(1)` streaming buffer memory without global locks using `Save` and `Load`:

```go
// Save binary CRC32C-framed snapshot wrapped in gzip
f, err := os.Create("cache.msnap.gz")
if err != nil {
	log.Fatal(err)
}
gw := gzip.NewWriter(f)
if err := store.Save(gw); err != nil { // SnapshotBinary is the default
	log.Fatal(err)
}
_ = gw.Close()
_ = f.Close()

// Warm-restart into a new (or live) store without broadcasting invalidations
rf, err := os.Open("cache.msnap.gz")
if err != nil {
	log.Fatal(err)
}
defer rf.Close()
gr, err := gzip.NewReader(rf)
if err != nil {
	log.Fatal(err)
}
defer gr.Close()

if err := newStore.Load(
	gr,
	memorystore.WithSnapshotFormat(memorystore.SnapshotBinary),
	memorystore.WithSnapshotConflictPolicy(memorystore.SnapshotKeepExisting),
); err != nil {
	log.Fatal(err)
}
```

- **`SnapshotBinary` (default):** Streams `"MSNAP\x02"` frames (up to 256 entries / 64 KiB target per frame) with per-frame `CRC32C` checksums, key/value size guards, and an EOF sentinel verifying entry count and stream digest before committing entries to shards.
- **`SnapshotJSON`:** Streams newline-delimited JSON records token-by-token via Go 1.27's `encoding/json/jsontext` with base64 values and premature-EOF detection.
- **TTL & Bounds Semantics:** `Save` skips already-expired entries and encodes relative remaining TTLs alongside `NoExpiration` (`-1`), sliding TTLs, and SWR grace windows. `Load` enforces `MaxItems`/`MaxBytes` incrementally per entry, hydrates eviction metadata, and does **not** broadcast distributed invalidations.

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

### Byte-Slice Aliasing & Memory Safety

To achieve zero-allocation (`0 B/op`) cache reads and writes, `Set`, `SetWithOptions`, `SetMulti`, `Get`, `GetMulti`, and `GetOrLoad` store and return `[]byte` slices **by reference (zero-copy aliasing)** by default rather than defensively copying slice backing arrays:

- **Writes (`Set`, `SetWithOptions`, `SetMulti`, and `GetOrLoad` loaders):** Do **not** mutate or recycle a `[]byte` slice after passing it to `Set`/`SetMulti` or returning it from a `GetOrLoad` loader. If your application reuses pooled buffers (for example, via `sync.Pool`), clone before storing (or enable `memorystore.WithCopyOnWrite(true)`):

  ```go
  buf := bufPool.Get().(*[]byte)
  defer bufPool.Put(buf)

  // Clone before storing so returning buf to the pool cannot corrupt cached data.
  if err := store.Set("payload", bytes.Clone(*buf), time.Minute); err != nil {
      log.Fatal(err)
  }
  ```

- **Reads (`Get`, `GetMulti`, `GetOrLoad`):** Treat returned `[]byte` slices as **immutable**. Modifying elements of a returned slice in place mutates the cached entry directly and introduces data races with concurrent readers. Copy with `bytes.Clone(val)` whenever you need to mutate or retain a writable buffer (or enable `memorystore.WithCopyOnRead(true)`).
- **Opt-in Defensive Copying:** When convenience outweighs zero-copy throughput, configure `memorystore.WithCopyOnWrite(true)` and/or `memorystore.WithCopyOnRead(true)` at construction time.
- **Typed JSON helpers (`SetJSON*`, `GetJSON*`, `GetOrLoadJSON`):** Serialize into fresh byte slices and unmarshal into caller-provided Go values, so returned structs are safe to modify.

### Metrics, Health & Observability

Inspect live, O(1) per-shard aggregated statistics on both bounded and unbounded stores at any time:

```go
metrics := store.GetMetrics()
log.Printf(
	"Items=%d Bytes=%d Hits=%d Misses=%d Evictions=%d",
	metrics.Items, metrics.Bytes, metrics.Hits, metrics.Misses, metrics.Evictions,
)
```

For real-time event emission without adding dependencies to the core module, implement `memorystore.MetricsRecorder` (`RecordHit`, `RecordMiss`, `RecordEvict`, `RecordLoad`, `RecordInvalidation`) and pass `memorystore.WithMetricsRecorder(recorder)` (when `nil`, the nil guard incurs zero allocations).

When using the built-in `InMemoryPubSub` directly, `ps.DroppedCount()` reports the number of messages dropped due to full subscriber channel buffers.

#### OpenTelemetry (`metrics/otel`)

The standalone `github.com/BryceWayne/MemoryStore/metrics/otel` sub-module (`v1.0.0`) implements `memorystore.MetricsRecorder` and `memorystore.InvalidationDropRecorder`, registering observable gauges (`memorystore.items`, `memorystore.bytes`) plus counters/histograms (`memorystore.hits`, `memorystore.misses`, `memorystore.evictions`, `memorystore.evicted_bytes`, `memorystore.loads`, `memorystore.load_duration_ms`, `memorystore.invalidations`, `memorystore.invalidation_drops`):

```go
import (
	"log"

	"github.com/BryceWayne/MemoryStore/metrics/otel"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

rec, err := otel.NewRecorder(otel.WithMeterProvider(meterProvider))
if err != nil {
	log.Fatal(err)
}
defer rec.Close()

store, err := memorystore.New(memorystore.WithMetricsRecorder(rec))
if err != nil {
	log.Fatal(err)
}
defer store.Stop()

if err := rec.RegisterStore(store); err != nil {
	log.Fatal(err)
}
```

#### Prometheus (`metrics/prometheus`)

The standalone `github.com/BryceWayne/MemoryStore/metrics/prometheus` sub-module (`v1.0.0`) provides a `Collector` implementing `prometheus.Collector`, `memorystore.MetricsRecorder`, and `memorystore.InvalidationDropRecorder` (`memorystore_hits_total`, `memorystore_misses_total`, `memorystore_evictions_total`, `memorystore_evicted_bytes_total`, `memorystore_loads_total`, `memorystore_load_duration_seconds`, `memorystore_invalidations_total`, `memorystore_invalidation_drops_total`, `memorystore_items`, `memorystore_bytes`):

```go
import (
	"log"

	prommetrics "github.com/BryceWayne/MemoryStore/metrics/prometheus"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"github.com/prometheus/client_golang/prometheus"
)

col, err := prommetrics.NewCollector(prommetrics.WithRegisterer(prometheus.DefaultRegisterer))
if err != nil {
	log.Fatal(err)
}
defer col.Close() // Also calls Unregister()

store, err := memorystore.New(memorystore.WithMetricsRecorder(col))
if err != nil {
	log.Fatal(err)
}
defer store.Stop()

col.SetStore(store)
```

#### Health Checks (`Health` & `Pinger`)

Check store readiness, invalidation backplane health, and optional `Pinger` (`Ping(ctx context.Context) error`) connectivity on the configured `PubSubClient`:

```go
if err := store.Health(ctx); err != nil {
	log.Printf("cache unhealthy: %v", err)
}
```

### Publish / Subscribe (Pluggable Backends)

#### Built-In In-Memory Broker (Default)

Provides **O(1)** lookup for exact topics and zero-allocation `:`-delimited wildcard matching (`user:*`, `orders:*:created`):

```go
store, err := memorystore.New()
if err != nil {
	log.Fatal(err)
}
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

Supports both `gcp.NewClient` and `gcp.NewClientWithOptions` (`WithClientOptions`, `WithChannelBuffer`, `WithSubscriptionID`, `WithSubscriptionIDFunc`, `WithAutoManage`, and `WithLogger`):

```go
import (
	"context"
	"log"
	"log/slog"
	"time"

	"github.com/BryceWayne/MemoryStore/pubsub/gcp"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

ctx := context.Background()
gcpClient, err := gcp.NewClientWithOptions(
	ctx,
	"my-gcp-project-id",
	gcp.WithChannelBuffer(256),
	gcp.WithLogger(slog.Default()),
)
if err != nil {
	log.Fatal(err)
}

store, err := memorystore.New(
	memorystore.WithPubSub(gcpClient),
	memorystore.WithPubSubTimeout(5*time.Second),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

#### Azure Service Bus (`pubsub/azure` sub-module)

Requires an Azure Service Bus **Standard** or **Premium** namespace (Topics & Subscriptions). Supports both Managed Identity / Entra ID (`azcore.TokenCredential`) and SAS connection strings, ephemeral auto-managed or pre-provisioned least-privilege subscriptions (`WithSubscriptionName`, `WithSubscriptionNameFunc`), `WithChannelBuffer`, and `WithLogger`:

```go
import (
	"log"
	"log/slog"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/BryceWayne/MemoryStore/pubsub/azure"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

cred, err := azidentity.NewDefaultAzureCredential(nil)
if err != nil {
	log.Fatal(err)
}

// Ephemeral auto-managed subscription (default, AutoDeleteOnIdle = 24h)
azureClient, err := azure.NewClient(
	"my-ns.servicebus.windows.net",
	cred,
	azure.WithLogger(slog.Default()),
)
if err != nil {
	log.Fatal(err)
}

// Or for least-privilege RBAC (Data Sender/Receiver without admin permissions):
// azureClient, err := azure.NewClient(
//     "my-ns.servicebus.windows.net", cred,
//     azure.WithSubscriptionName("my-preprovisioned-sub"),
// )

store, err := memorystore.New(
	memorystore.WithPubSub(azureClient),
	memorystore.WithPubSubTimeout(5*time.Second),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

#### Redis / Valkey (`pubsub/redis` sub-module)

Backed by `go-redis/v9`'s `UniversalClient` for standalone Redis/Valkey, Redis Sentinel, and Redis Cluster topologies. Automatically routes exact topics via `SUBSCRIBE` and wildcard globs (`*`, `?`, `[`) via `PSUBSCRIBE`, and supports `WithChannelBuffer` and `WithLogger`:

```go
import (
	"log"
	"log/slog"
	"time"

	"github.com/BryceWayne/MemoryStore/pubsub/redis"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	goredis "github.com/redis/go-redis/v9"
)

redisClient, err := redis.NewClient(
	&goredis.UniversalOptions{
		Addrs: []string{"127.0.0.1:6379"},
	},
	redis.WithLogger(slog.Default()),
)
if err != nil {
	log.Fatal(err)
}

// Or wrap an existing caller-managed redis.UniversalClient:
// redisClient, err := redis.NewClientFromClient(existingClient)

store, err := memorystore.New(
	memorystore.WithPubSub(redisClient),
	memorystore.WithPubSubTimeout(5*time.Second),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

#### NATS / JetStream (`pubsub/nats` sub-module)

Supports Core NATS (`*` single-token and `>` multi-token dot-delimited wildcards), optional queue groups (`WithQueueGroup`), optional JetStream durability (`WithJetStream`), `WithLogger`, and `natsClient.DroppedCount()` (tracking Core NATS drops on full buffers):

```go
import (
	"log"
	"log/slog"
	"time"

	"github.com/BryceWayne/MemoryStore/pubsub/nats"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

natsClient, err := nats.NewClient(
	"nats://127.0.0.1:4222",
	nats.WithLogger(slog.Default()),
)
if err != nil {
	log.Fatal(err)
}

// Or enable JetStream, queue groups, or wrap an existing *nats.Conn:
// natsClient, err := nats.NewClient("nats://127.0.0.1:4222", nats.WithJetStream("cache-events"))
// natsClient, err := nats.NewClient("nats://127.0.0.1:4222", nats.WithQueueGroup("cache-workers"))
// natsClient, err := nats.NewClientFromConn(existingConn) // import natsgo "github.com/nats-io/nats.go"

store, err := memorystore.New(
	memorystore.WithPubSub(natsClient),
	memorystore.WithPubSubTimeout(5*time.Second),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

#### AWS SNS + SQS (`pubsub/aws` sub-module)

Fans out messages via AWS SNS topics to SQS long-polling (`ReceiveMessage`). Supports ephemeral auto-managed SQS queues (default), pre-provisioned least-privilege IAM resources (`WithQueueURL`, `WithQueueURLFunc`, `WithTopicARN`), `WithChannelBuffer`, and `WithLogger`:

```go
import (
	"context"
	"log"
	"log/slog"
	"time"

	"github.com/BryceWayne/MemoryStore/pubsub/aws"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"github.com/aws/aws-sdk-go-v2/config"
)

ctx := context.Background()
cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
if err != nil {
	log.Fatal(err)
}

// Ephemeral auto-managed SQS queue per subscription (default)
awsClient, err := aws.NewClient(cfg, aws.WithLogger(slog.Default()))
if err != nil {
	log.Fatal(err)
}

// Or for least-privilege IAM (only sns:Publish, sqs:ReceiveMessage, sqs:DeleteMessage):
// awsClient, err := aws.NewClient(
//     cfg,
//     aws.WithTopicARN("arn:aws:sns:us-east-1:123456789012:cache-invalidation"),
//     aws.WithQueueURLFunc(func(topic string) string {
//         return "https://sqs.us-east-1.amazonaws.com/123456789012/pod-1-" + topic
//     }),
// )

store, err := memorystore.New(
	memorystore.WithPubSub(awsClient),
	memorystore.WithPubSubTimeout(5*time.Second),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

#### Apache Kafka / Redpanda (`pubsub/kafka` sub-module)

Pure-Go (`CGO_ENABLED=0`) Kafka/Redpanda adapter backed by `franz-go` (`kgo`). Defaults to broadcast mode (isolated consumers starting at the latest log-end offset `AtEnd` so every pod receives new events) with optional shared consumer groups (`WithConsumerGroup`), TLS (`WithTLSConfig`), SASL/SCRAM (`WithSCRAMSHA256`, `WithSCRAMSHA512`, `WithSASL`), `WithChannelBuffer`, and `WithLogger`:

```go
import (
	"log"
	"log/slog"
	"time"

	"github.com/BryceWayne/MemoryStore/pubsub/kafka"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

kafkaClient, err := kafka.NewClient(
	[]string{"127.0.0.1:9092"},
	kafka.WithLogger(slog.Default()),
)
if err != nil {
	log.Fatal(err)
}

// Or configure TLS, SCRAM authentication, or a shared consumer group:
// kafkaClient, err := kafka.NewClient(
//     []string{"broker-1:9093"},
//     kafka.WithTLSConfig(tlsCfg),
//     kafka.WithSCRAMSHA256("user", "password"),
//     kafka.WithConsumerGroup("worker-group"),
// )

store, err := memorystore.New(
	memorystore.WithPubSub(kafkaClient),
	memorystore.WithPubSubTimeout(5*time.Second),
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

#### Broker Capability & Wildcard Matrix

All `pubsub/*` adapters implement `memorystore.PubSubClient` via structural typing, while wildcard syntax, backpressure, delivery guarantees, and `Subscribe` lifecycle behavior reflect each underlying broker:

| Broker / Adapter | Exact Match | Wildcard Syntax & Delimiter | Multi-Segment Wildcard | Slow-Consumer Behavior | Delivery Model | Duplicate `Subscribe` Behavior | `SubscriberCounter` | Configurable Channel Buffer |
| :--- | :---: | :--- | :---: | :--- | :--- | :--- | :---: | :---: |
| **`InMemoryPubSub`** (built-in) | Yes (`O(1)`) | `*` single-segment, `:`-delimited | No (`*` matches 1 segment) | Non-blocking drop when buffer full | Broadcast (all matching subs) | Appends additional subscriber channel | **Yes** | No (fixed `100`) |
| **`pubsub/gcp`** | Yes (literal) | None (literal topics only) | No | Blocks on `ch <- msg.Data` before `Ack` (`Nack` on ctx cancel) | Broadcast (ephemeral sub) or shared (`WithSubscriptionID`) | Replaces prior sub cleanly (stops & deletes old sub, closes old `ch`) | No | Yes (`WithChannelBuffer` via `NewClientWithOptions`, default `100`) |
| **`pubsub/azure`** | Yes (literal) | None (literal topics only) | No | Blocks on `ch <- msg.Body` before `CompleteMessage` (`AbandonMessage` on cancel in `PeekLock`) | Broadcast (ephemeral sub) or shared (`WithSubscriptionName`) | Replaces prior sub cleanly (stops & deletes ephemeral old sub, closes old `ch`) | No | Yes (`WithChannelBuffer`, default `100`) |
| **`pubsub/redis`** | Yes (`SUBSCRIBE`) | Redis glob (`*`, `?`, `[...]`) via `PSUBSCRIBE` (any delimiter) | Yes (`*` matches across `:`) | Blocks on `ch <- payload` until consumed or stopped | Broadcast (all matching clients) | Replaces prior sub cleanly (stops old sub, closes old `ch`) | No | Yes (`WithChannelBuffer`, default `100`) |
| **`pubsub/nats`** | Yes | `*` (1 token) & `>` (1+ trailing tokens), `.`-delimited | Yes (`>` trailing tokens) | Core NATS drops on full buffer; JetStream blocks before `DoubleAck` (`Nak` on cancel) | Broadcast (default) or queue group (`WithQueueGroup`) | Replaces prior sub cleanly (drains/unsubscribes old sub, closes old `ch`) | No | Yes (`WithChannelBuffer`, default `100`) |
| **`pubsub/aws`** | Yes (literal / ARN) | None (literal topics / ARNs only) | No | Blocks on `ch <- payload` before SQS `DeleteMessage` | Broadcast (ephemeral SQS queue) or shared (`WithQueueURL`) | Replaces prior sub cleanly (cleans up ephemeral old sub, closes old `ch`) | No | Yes (`WithChannelBuffer`, default `100`) |
| **`pubsub/kafka`** | Yes (literal) | None (literal topics only) | No | Blocks on `ch <- rec.Value` before offset mark (group mode) | Broadcast (`AtEnd` default) or consumer group (`WithConsumerGroup`) | Replaces prior sub cleanly (stops old consumer, closes old `ch`) | No | Yes (`WithChannelBuffer`, default `100`) |

### Configuration, Logging & Lifecycle

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
store, err := memorystore.New(
	memorystore.WithPubSub(customBroker),              // Defaults to InMemoryPubSub
	memorystore.WithPubSubTimeout(5*time.Second),      // Default timeout for non-context Pub/Sub helpers
	memorystore.WithCleanupInterval(30*time.Second),   // Background expiration sweep interval (default 1m)
	memorystore.WithLogger(logger),                    // Defaults to slog.DiscardHandler (silent)
)
if err != nil {
	log.Fatal(err)
}
defer store.Stop()
```

- `memorystore.New(opts...)` validates all options strictly without silent clamping (e.g., negative bounds, invalid `DefaultTTLJitter` outside `[0.0, 0.5]`, or `WithInvalidationTopic` without `WithPubSub` return descriptive errors).
- Items with `duration <= 0` (when not `memorystore.NoExpiration`) expire immediately. Use `memorystore.NoExpiration` (`-1`) or `memorystore.WithNoExpiration()` for entries without a TTL.
- Always call `store.Stop()` when shutting down to stop background workers, close the `PubSubClient`, and release memory. Multiple calls to `Stop()` are safe and idempotent.
- **Post-`Stop()` behavior:** Mutating, loader, and snapshot methods (`Set`, `SetWithOptions`, `SetMulti`, `SetJSON`, `SetJSONWithOptions`, `Delete`, `GetOrLoad`, `GetOrLoadWithOptions`, `GetOrLoadJSON`, `GetOrLoadJSONWithOptions`, `Save`, `Load`) return `memorystore.ErrStoreStopped` after `Stop()`. Context-free `Get` returns `(nil, false)`.

### Migrating from `v1.x` to `v2.0`

1. Update imports from the `v1` package path to `github.com/BryceWayne/MemoryStore/v2/memorystore`.
2. Replace `v1` struct-based constructors with `store, err := memorystore.New(opts...)` and handle validation errors returned by `New`.
3. Update `Delete` callers to handle `Delete(key string) error` and check `errors.Is(err, memorystore.ErrStoreStopped)` on mutating, loader, and snapshot methods during graceful shutdown.
4. Use `memorystore.NoExpiration` (`-1`) or `memorystore.WithNoExpiration()` for non-expiring entries (`0` still expires immediately).
5. Note v2.0 architectural defaults: bounded stores (`MaxItems > 0 || MaxBytes > 0`) default to `EvictionS3FIFO` (pass `WithEvictionPolicy(EvictionVolatileTTL)` to keep v1 random TTL sampling); reads use a 1ms coarse monotonic clock by default (pass `WithPreciseClock()` to opt out); and `WithInvalidationTopic` dispatches asynchronously by default with a `5s` default `PubSubTimeout` (pass `WithSyncInvalidation()` for synchronous writes).

---

## API Overview

Full API documentation is available on [pkg.go.dev](https://pkg.go.dev/github.com/BryceWayne/MemoryStore/v2/memorystore).

| Method / Type | Description |
| :--- | :--- |
| `New(opts ...Option) (*MemoryStore, error)` | Creates a validated store using functional options (`WithMaxItems`, `WithMaxBytes`, `WithCleanupInterval`, `WithPubSub`, `WithPubSubTimeout`, `WithInvalidationTopic`, `WithInvalidationMode`, `WithWriteThroughMaxBytes`, `WithAsyncInvalidation`, `WithSyncInvalidation`, `WithOnEvict`, `WithLogger`, `WithCopyOnWrite`, `WithCopyOnRead`, `WithMetricsRecorder`, `WithEvictionPolicy`, `WithPreciseClock`, `WithDefaultTTLJitter`, `WithTTLJitterSeed`, `WithDefaultStaleWhileRevalidate`) |
| `NoExpiration` | Sentinel TTL (`-1`) marking entries as never expiring due to TTL |
| `Set(key string, value []byte, duration time.Duration) error` | Stores raw bytes with a TTL (or `NoExpiration`); returns `ErrStoreStopped` after `Stop()` |
| `SetWithOptions(key, value, duration, opts...) error` | Stores raw bytes with `WithTTLJitter`, `WithSlidingExpiration`, `WithNoExpiration`, and/or `WithStaleWindow` |
| `Get(key string) ([]byte, bool)` | Context-free retrieval returning raw bytes if present and before soft expiration (`(nil, false)` after `Stop()`) |
| `Delete(key string) error` | Removes a key; returns `ErrStoreStopped` after `Stop()` |
| `SetJSON` / `SetJSONWithOptions` | Marshals via `encoding/json/v2` (optionally with `json.Options`) and stores with a TTL |
| `GetJSON` / `GetJSONWithOptions` / `GetJSONAs[T]` | Retrieves and unmarshals via `encoding/json/v2` (including generic `GetJSONAs[T]`) |
| `GetOrLoad` / `GetOrLoadWithOptions` | Raw-byte singleflight cache population with Stale-While-Revalidate (`WithStaleWhileRevalidate`) and panic propagation |
| `GetOrLoadJSON[T]` / `GetOrLoadJSONWithOptions[T]` | Typed generic singleflight cache population with SWR (`LoadOption`) and `json.Options` |
| `Save` / `Load` | Streaming snapshot persistence (`SnapshotBinary` CRC32C-framed or `SnapshotJSON` `jsontext`) with `WithSnapshotFormat` and `WithSnapshotConflictPolicy` (`SnapshotOverwrite` / `SnapshotKeepExisting`) |
| `SetMulti` / `GetMulti` | Batch-sets / batch-gets multiple keys grouped by shard |
| `GetMetrics() StoreMetrics` | O(1) snapshot returning `Items`, `Bytes`, `Hits`, `Misses`, `Evictions`, and `InvalidationDrops` |
| `Health(ctx context.Context) error` | Checks stopped state, invalidation backplane health, and `Pinger.Ping(ctx)` |
| `InstanceID() string` | Returns the store's unique per-instance identifier used for invalidation echo suppression |
| `MetricsRecorder` / `InvalidationDropRecorder` / `Pinger` | Zero-dependency observability and health-probe interfaces |
| `EvictionPolicy` | `EvictionS3FIFO` (default when bounded), `EvictionTinyLFU` (bounded-only), and `EvictionVolatileTTL` (default when unbounded) |
| `InvalidationMode` | `InvalidationModeInvalidateOnly` (default) and `InvalidationModeWriteThrough` |
| `SnapshotFormat` / `SnapshotConflictPolicy` | `SnapshotBinary` / `SnapshotJSON` and `SnapshotOverwrite` / `SnapshotKeepExisting` |
| `ErrStoreStopped` / `ErrInvalidTopic` / `ErrNotImplemented` / `ErrCorruptSnapshot` / `ErrUnsupportedSnapshotVersion` | Typed sentinel errors |
| `Subscribe` / `SubscribeContext` | Subscribes to a topic or pattern |
| `Publish` / `PublishContext` | Publishes a message to matching subscribers |
| `Unsubscribe` / `UnsubscribeContext` | Cancels subscriptions for a topic |
| `SubscriberCount(pattern string) int` | Returns active subscriber count when backend implements `SubscriberCounter` |
| `NewInMemoryPubSub() *InMemoryPubSub` | Instantiates the built-in in-memory Pub/Sub broker (`DroppedCount() int64`, `Ping(ctx)`) |
| `Stop() error` / `IsStopped() bool` | Gracefully stops the store and checks lifecycle state |

---

## Repository Layout

```text
.
├── memorystore/         # Core zero-dependency cache, generic helpers & PubSubClient (/v2)
├── pubsub/gcp/          # Standalone Go module for Google Cloud Pub/Sub (v2)
├── pubsub/azure/        # Standalone Go module for Azure Service Bus
├── pubsub/redis/        # Standalone Go module for Redis / Valkey Pub/Sub
├── pubsub/nats/         # Standalone Go module for NATS / JetStream
├── pubsub/aws/          # Standalone Go module for AWS SNS + SQS
├── pubsub/kafka/        # Standalone pure-Go module for Apache Kafka / Redpanda
├── metrics/otel/        # Standalone OpenTelemetry MetricsRecorder & gauges sub-module
├── metrics/prometheus/  # Standalone Prometheus Collector & MetricsRecorder sub-module
├── benchmarks/          # Isolated head-to-head comparison benchmarks vs peer caches
├── e2e/                 # 4-tier opaque-box integration & end-to-end test suite
├── docs/images/         # Architecture & benchmark SVG diagrams
├── examples/basic/      # Runnable usage example
├── go.work              # Go workspace wiring local /v2 across sub-modules
├── Makefile             # Multi-module build, test, lint, and benchmark targets
└── .github/             # CI, release workflow, Dependabot, and issue/PR templates
```

---

## Architecture

![MemoryStore Sharded Cache and Pluggable Pub/Sub Architecture](./docs/images/architecture.svg)

MemoryStore v2.0.0 is built around a zero-`unsafe`, `-race`-clean sharded storage engine:

- **256 128-Byte Shards & Lock-Free Swiss-Style Buckets:** Each shard spans two 64-byte cache lines (`table` on line 0; counters, accounting, and writer `sync.Mutex` on line 1). Each 64-byte bucket packs 7 `h2` tags + overflow bit in `atomic.Uint64` alongside 7 `atomic.Pointer[entry]` slots, letting `Get` / `GetParallel` probe without locks (`0 B/op`).
- **96-Byte Entry Layout & 1ms Coarse Monotonic Clock:** Hot read fields (`key`, `value`, `expiresAt`, `ttlNanos`, `freq`, `flags`) fit on cache line 0; SWR (`staleGrace`), `hash`, and intrusive eviction list pointers sit on cache line 1. A background 1ms ticker drives `coarseNow` (with automatic exact-clock fallback for TTLs `< 10ms` and `WithPreciseClock()` opt-out), and sliding TTLs coalesce updates below 3/4 remaining TTL.
- **Pluggable Eviction & Lock-Free Read Tracking:** Bounded stores default to `EvictionS3FIFO` (2-bit atomic `freq` + ghost ring) with `EvictionTinyLFU` (4-row Count-Min Sketch + lossy 64-slot `atomic.Pointer` read ring + SLRU) and `EvictionVolatileTTL` also available.
- **Resilient Invalidation & Streaming Persistence:** Distributed invalidations flow through a bounded async batch-coalescing worker with exponential-backoff reconnect (`100ms` → `30s`) and `WriteThrough` support; `Save`/`Load` stream CRC32C-framed `"MSNAP\x02"` binary or `jsontext` NDJSON with `O(1)` buffer memory.

---

## Performance & Benchmarks

![MemoryStore Head-to-Head Go Cache Benchmarks](./docs/images/benchmarks.svg)

### Head-to-Head Comparison (`benchmarks/`, `go1.27.0`, Intel Core i9-12900K, 12 threads, `benchstat -count=5 -benchtime=400ms`)

| Library | `Set` | `Get` | `GetParallel` | `MixedParallel` (87.5% R / 12.5% W) | `Get` Allocs |
| :--- | ---: | ---: | ---: | ---: | :---: |
| **MemoryStore v2** | **51.40 ns** (96 B / 1 alloc) | **13.31 ns** (0 B) | **2.596 ns** (0 B) | **4.743 ns** (12 B / 0 allocs) | **0 B / 0 allocs** |
| `otter/v2` | 114.4 ns (24 B / 1 alloc) | 40.34 ns (0 B) | 4.405 ns (0 B) | 8.556 ns (3 B / 0 allocs) | 0 B / 0 allocs |
| `freecache` | 77.45 ns (0 B) | 96.78 ns (64 B / 1 alloc) | 22.79 ns (64 B / 1 alloc) | 25.74 ns (56 B / 0 allocs) | 64 B / 1 alloc |
| `go-cache` | 73.66 ns (24 B / 1 alloc) | 48.13 ns (0 B) | 37.57 ns (0 B) | 58.26 ns (3 B / 0 allocs) | 0 B / 0 allocs |
| `bigcache/v3` | 235.0 ns (92 B / 0 allocs) | 111.5 ns (88 B / 2 allocs) | 13.18 ns (88 B / 2 allocs) | 52.50 ns (106 B / 1 alloc) | 88 B / 2 allocs |
| `ristretto/v2` | 233.9 ns (96 B / 1 alloc) | 71.56 ns (2 B / 0 allocs) | 11.85 ns (0 B) | 70.96 ns (17 B / 0 allocs) | 0–2 B / 0 allocs |

#### Bounded Throughput & Eviction Hit-Ratio Comparison (`benchmarks/`, `benchstat -count=5`)

| Library / Policy | Bounded `GetParallel` | Zipfian Hit Ratio (`s=0.99`) | Scan Hit Ratio |
| :--- | ---: | ---: | ---: |
| **MemoryStore (`EvictionS3FIFO`)** | **2.702 ns** (0 B / 0 allocs) | **58.47%** (+24.5% vs VolatileTTL) | **91.72%** |
| **MemoryStore (`EvictionTinyLFU`)** | **9.458 ns** (0 B / 0 allocs) | **60.45%** (+28.7% vs VolatileTTL) | **92.82%** |
| **MemoryStore (`EvictionVolatileTTL`)** | — | 46.98% | 44.26% |
| `otter/v2` | 4.356 ns (0 B) | 60.01% | 80.62% |
| `ristretto/v2` | 12.68 ns (0 B) | 59.49% | 99.62% |

Reproduce head-to-head benchmarks via `make bench-compare` (see [benchmarks/README.md](benchmarks/README.md)).

### Internal Benchmarks (`benchstat`, `go1.27.0`, Intel Core i9-12900K, `count=5`)

| Benchmark | Time / op | Bytes / op | Allocs / op |
| :--- | ---: | ---: | ---: |
| `BenchmarkMemoryStore_Set` | **51.62 ns** | 96 B | 1 |
| `BenchmarkMemoryStore_Get` | **11.58 ns** | **0 B** | **0** |
| `BenchmarkMemoryStore_GetParallel` | **2.503 ns** | **0 B** | **0** |
| `BenchmarkMemoryStore_GetParallel_NilRecorder` | **2.508 ns** | **0 B** | **0** |
| `BenchmarkMemoryStore_BoundedGetParallel_S3FIFO` | **2.606 ns** | **0 B** | **0** |
| `BenchmarkMemoryStore_BoundedGetParallel_TinyLFU` | **9.207 ns** | **0 B** | **0** |
| `BenchmarkMemoryStore_SetGetParallel` | **7.437 ns** | 12 B | 0 |
| `BenchmarkMemoryStore_SetMulti` (100 keys) | 5.851 µs | 9,600 B | 100 |
| `BenchmarkMemoryStore_GetMulti` (100 keys) | **2.863 µs** | 5,464 B | 4 |
| `BenchmarkMemoryStore_SetJSON_Small` | 232.5 ns | 144 B | 2 |
| `BenchmarkMemoryStore_GetJSON_Small` | 228.0 ns | **0 B** | **0** |
| `BenchmarkMemoryStore_SetJSON_Complex` | 1.623 µs | 608 B | 9 |
| `BenchmarkMemoryStore_GetJSON_Complex` | 2.168 µs | 976 B | 17 |
| `BenchmarkMemoryStore_PubSub` (wildcard) | 74.05 ns | **0 B** | **0** |
| `BenchmarkMemoryStore_PubSub_ExactManyTopics` (100 topics) | **29.39 ns** | **0 B** | **0** |
| `BenchmarkAsyncInvalidation_StalledPubSub` | **97.31 ns** | 112 B | 2 |
| `BenchmarkMemoryStore_SnapshotSave_100K/Binary` (100K × 256B) | **4.067 ms** | **72.05 KiB** | **4** |
| `BenchmarkMemoryStore_SnapshotSave_100K/JSON` (100K × 256B) | 41.25 ms | **7.04 KiB** | 20 |
| `BenchmarkMemoryStore_SnapshotLoad_100K/Binary` (100K × 256B) | **64.95 ms** | 91.13 MiB | 329,900 |
| `BenchmarkMemoryStore_SnapshotLoad_100K/JSON` (100K × 256B) | 139.9 ms | 129.1 MiB | 1,030,000 |

Reproduce locally:

```bash
make bench
# Or with benchstat-friendly repetition:
go test -run='^$' -bench=. -benchmem -count=5 ./memorystore
```

---

## Development & Testing

Use the included [Makefile](Makefile):

```bash
make all           # Run clean, fmt, vet, lint, test, and build across all modules
make test          # Run tests with -race and coverage across all modules
make fuzz-smoke    # Run testing.F fuzz smoke targets (5s per target)
make bench         # Run internal memorystore benchmarks with memory stats
make bench-compare # Run head-to-head comparison benchmarks in benchmarks/
make lint          # Run golangci-lint (v2) across all modules
make vet           # Run go vet across all modules
make fmt           # Format code with go fmt
make coverage      # Generate HTML coverage report
make run-example   # Run examples/basic
make help          # List all targets
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
