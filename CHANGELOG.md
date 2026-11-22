# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Distributed Cache Invalidation Backplane:** Setting `Config.InvalidationTopic` automatically broadcasts compact `encoding/json/v2` invalidation events on local `Set`, `SetMulti`, and `Delete` calls over any `PubSubClient` (`InMemoryPubSub`, `pubsub/gcp`, `pubsub/azure`, or custom brokers), with per-instance RFC 9562 UUID echo suppression (`MemoryStore.InstanceID()`).
- **Go 1.27 Generic Methods & Stampede Protection:**
  - `MemoryStore.GetJSONAs[T](key, opts...)` for typed `encoding/json/v2` reads.
  - `MemoryStore.GetOrLoad(ctx, key, ttl, loader)` and `MemoryStore.GetOrLoadJSON[T](ctx, key, ttl, loader, opts...)` with zero-dependency singleflight coalescing so concurrent cache misses invoke the loader only once.
- **Bounded Capacity & Eviction Callbacks:**
  - `Config.MaxItems` and `Config.MaxBytes` enforcing soft global limits via volatile-TTL sampled eviction with zero overhead on unbounded caches.
  - `Config.OnEvict` callback (`EvictExpired`, `EvictCapacity`, `EvictDeleted`) invoked safely outside shard locks.
  - `StoreMetrics.Bytes` tracking stored key + value payload bytes inside the existing 64-byte `shard` cache line.
- **Head-to-Head Benchmarks Sub-Module (`benchmarks/`) & Branded Chart (`docs/images/benchmarks.svg`):** Isolated benchmark suite comparing `MemoryStore` against `go-cache`, `bigcache/v3`, `freecache`, `otter/v2`, and `ristretto/v2` (`make bench-compare`).
- Standalone `github.com/BryceWayne/MemoryStore/pubsub/azure` sub-module implementing `memorystore.PubSubClient` using Azure Service Bus (`azservicebus` + `azservicebus/admin`) with Managed Identity (`azcore.TokenCredential`) and connection string auth, ephemeral auto-managed or pre-provisioned RBAC subscription modes, PeekLock / ReceiveAndDelete support, and cached senders.
- Custom branded SVG architecture diagram (`docs/images/architecture.svg`) in `README.md`.

## [0.2.0] - 2026-11-22

### Added
- Pluggable `Config.PubSub` field allowing any `PubSubClient` implementation (Redis, NATS, Kafka, GCP Pub/Sub, mocks) to be injected into `MemoryStore`.
- Context-aware Pub/Sub methods on `MemoryStore`: `SubscribeContext`, `PublishContext`, and `UnsubscribeContext` (with `Config.PubSubTimeout` now enforced by non-context helpers).
- Optional `SubscriberCounter` interface so any `PubSubClient` backend can expose subscriber counts.
- Exported `NewInMemoryPubSub()` constructor.
- Configurable `Config.CleanupInterval` and structured `Config.Logger` (`*slog.Logger`, defaulting to silent `slog.DiscardHandler`).
- Standalone `github.com/BryceWayne/MemoryStore/pubsub/gcp` sub-module backed by `cloud.google.com/go/pubsub/v2` with cached publisher cleanup.
- Runnable demo relocated to `examples/basic/main.go`.

### Changed
- **Zero-dependency core module:** Removed all external dependencies from the root `go.mod` by decoupling Google Cloud Pub/Sub into `pubsub/gcp`.
- **Context-aware `PubSubClient` interface:** `Subscribe`, `Publish`, and `Unsubscribe` now accept `context.Context` as their first parameter.
- **Performance optimizations:**
  - Moved `hits`, `misses`, and `evictions` `atomic.Int64` counters per-shard inside each 64-byte cache-line padded shard, eliminating cross-core cache-line bouncing (**-74.8%** / ~4x faster `GetParallel`: 27.0 ns → **7.88 ns/op**).
  - Added stack-allocated scratch buffers (up to 128 keys) and small-batch fast paths to `SetMulti` and `GetMulti` (`SetMulti` drops to **3.15 µs/op, 0 B/op, 0 allocs/op**).
  - Split `InMemoryPubSub` into O(1) exact-topic lookup and wildcard pattern buckets (`Publish` across 100 exact topics runs in **38.6 ns/op, 0 B/op, 0 allocs/op**).
- Upgraded GitHub Actions workflows to `actions/checkout@v7` and `actions/setup-go@v7` and expanded CI/Makefile/Dependabot to cover both the root module and `pubsub/gcp`.
- Core `memorystore` statement test coverage increased to **99.1%**.

### Removed
- `Config.GCPProjectID` and automatic `GOOGLE_CLOUD_PROJECT` detection from core `memorystore` (replaced by injecting `gcp.NewClient(...)` into `Config.PubSub`).
- Root `main.go` (moved to `examples/basic/main.go`) and `go.sum` (no longer needed with zero root dependencies).

## [0.1.0] - 2026-11-22

Initial tagged release of **MemoryStore**, built for **Go 1.27+**.

### Added
- `MemoryStore.SetJSONWithOptions` and `MemoryStore.GetJSONWithOptions` exposing Go 1.27 `encoding/json/v2` and `encoding/json/jsontext` options (including `encoding/json.DefaultOptionsV1()` compatibility).
- Comprehensive benchmark suite in `memorystore/benchmark_test.go` covering single-key, concurrent parallel, JSON (small & complex), batch (`SetMulti`/`GetMulti`), and Pub/Sub workloads.
- Expanded unit tests in `memorystore/jsonv2_test.go` covering `encoding/json/v2` semantics, options, batch/expiration edge cases, topic matching edge cases, and Go 1.27's `goroutineleak` profiler.
- Runnable Go documentation examples (`memorystore/example_test.go`) and package-level documentation (`memorystore/doc.go`).
- Open-source repository standards and governance files: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `CHANGELOG.md`, `.golangci.yml`, `.gitignore`, `.gitattributes`, `.editorconfig`, `.github/PULL_REQUEST_TEMPLATE.md`, `.github/ISSUE_TEMPLATE/config.yml`, `.github/dependabot.yml`, and `.github/workflows/release.yml`.

### Changed
- **Go 1.27 upgrade:** Updated module requirement to `go 1.27.0` (`toolchain go1.27.4`), taking advantage of Green Tea GC, size-specialized small allocations, stack-allocated slice backing stores, and container-aware `GOMAXPROCS`.
- **JSON engine migration:** Migrated `SetJSON` and `GetJSON` from `github.com/goccy/go-json` to Go 1.27's standard library `encoding/json/v2`.
  - *Behavioral note:* `encoding/json/v2` marshals nil slices as `[]` and nil maps as `{}`, rejects duplicate JSON object member names, and rejects invalid UTF-8 by default. Callers needing legacy v1 behavior can pass `encoding/json.DefaultOptionsV1()` to `SetJSONWithOptions` / `GetJSONWithOptions`.
- **Standard library UUID:** Migrated UUID generation in `main.go` and `memorystore/pubsub_gcp.go` from `github.com/google/uuid` to Go 1.27's standard library `uuid` package.
- **Performance optimizations:**
  - Replaced `hash/fnv` shard hashing with zero-allocation hardware-accelerated `hash/maphash.String` and bitmask indexing.
  - Embedded 256 cache-line padded shards inline in `MemoryStore` to eliminate per-shard heap allocations and reduce false sharing under parallel load.
  - Replaced per-item `time.Time` expiration fields with pointer-free monotonic `int64` nanosecond offsets, shrinking `item` from 48 B to 32 B and reducing GC pointer scanning.
  - Refactored `SetMulti` and `GetMulti` to use flat intrusive-index shard bucketing and batched `atomic.Int64` metric updates (-81% time / -99% allocs on `SetMulti`, -73% time / -96% allocs on `GetMulti`).
  - Replaced `strings.Split` in `InMemoryPubSub` topic matching with zero-allocation `strings.Cut` (-33% time, 0 B/op, 0 allocs/op).
- Adopted Go 1.27 idioms including `atomic.Int64`, `sync.WaitGroup.Go`, and `omitzero` struct tags.
- Updated GitHub Actions CI workflow to `actions/checkout@v4`, `actions/setup-go@v5` (`go-version: '1.27'`), and `golangci-lint v2.15.0`.
- Updated `README.md` with API reference, Go 1.27 benchmark results, architecture diagrams, and contributor links.

### Removed
- Direct external dependencies on `github.com/goccy/go-json` and `github.com/google/uuid`.

### Fixed
- Deterministic immediate expiration for items set with `duration <= 0`, and overflow saturation for extreme TTL durations.
- Unchecked `Close()` error returns in `memorystore/pubsub_memory_test.go`.
- Corrected `.github/ISSUE_TEMPLATE/feature_request.md` (previously contained PR template content) and updated `.github/ISSUE_TEMPLATE/bug_report.md` for Go 1.27+.

### Earlier Development Highlights (Pre-1.0)
- Sharded in-memory key-value storage with background expiration cleanup.
- JSON serialization and deserialization (`SetJSON`, `GetJSON`).
- Batch operations (`SetMulti`, `GetMulti`) and thread-safe `StoreMetrics`.
- Pluggable `PubSubClient` interface with In-Memory wildcard broker and Google Cloud Pub/Sub backend.
- Race-tested unit test suite, Makefile automation, and GitHub Actions CI.

[Unreleased]: https://github.com/BryceWayne/MemoryStore/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/BryceWayne/MemoryStore/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/BryceWayne/MemoryStore/releases/tag/v0.1.0
