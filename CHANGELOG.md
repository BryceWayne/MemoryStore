# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [2.0.0] - 2026-11-23

Major **v2.0.0** release (`github.com/BryceWayne/MemoryStore/v2`) delivering a pure-Go, zero-`unsafe`, lock-free Swiss-table read path (**2.6 ns/op** `GetParallel`, **4.6 ns/op** `MixedParallel`), pluggable S3-FIFO / W-TinyLFU eviction policies, functional options with strict validation and post-`Stop()` lifecycle safety, Stale-While-Revalidate (SWR) & async write-through invalidation, streaming CRC32C binary & `jsontext` snapshot persistence, standalone OpenTelemetry & Prometheus observability sub-modules, and cross-adapter Pub/Sub hardening.

### Breaking Changes
- **Go `/v2` Module Path (#41):** Root module path updated to `github.com/BryceWayne/MemoryStore/v2` (retaining **zero external dependencies** and no root `go.sum`). Update imports to `github.com/BryceWayne/MemoryStore/v2/memorystore`.
- **Functional Options Constructor (#41):** Replaced `NewMemoryStore()` and `NewMemoryStoreWithConfig(Config)` with `New(opts ...Option) (*MemoryStore, error)` enforcing explicit option validation (rejecting negative bounds, out-of-range jitter, `InvalidationTopic` without `PubSub`, and bounded-only eviction policies without capacity limits).
- **Typed Lifecycle Errors (#41):** Mutating, load, and snapshot operations (`Set`, `SetWithOptions`, `SetMulti`, `SetJSON*`, `Delete`, `GetOrLoad*`, `Save`, `Load`) now return `ErrStoreStopped` after `Stop()` instead of panicking on nil shard maps; `Get` remains context-free and returns `(nil, false)`.
- **Bounded Default Eviction & Async Invalidation (#42, #44):** Bounded stores (`MaxItems > 0` or `MaxBytes > 0`) now default to `EvictionS3FIFO` (use `WithEvictionPolicy(EvictionVolatileTTL)` to retain v1 random TTL sampling); `InvalidationTopic` now dispatches asynchronously via a bounded non-blocking queue with a default `5s` `PubSubTimeout` (use `WithSyncInvalidation()` for synchronous test visibility).

### Added
- **Functional Options, `NoExpiration`, Per-Item `Set` Options & Copy Isolation (#41):** Added `New(opts...)`, `NoExpiration = -1`, `SetWithOptions` (`WithTTLJitter`, `WithSlidingExpiration`, `WithNoExpiration`, `WithStaleWindow`), `WithCopyOnWrite`, and `WithCopyOnRead`.
- **Pluggable High-Hit-Ratio Eviction Policies (#42):** Added `EvictionS3FIFO`, `EvictionTinyLFU` (4-bit Count-Min Sketch + Doorkeeper + Window/SLRU), and `EvictionVolatileTTL` via `WithEvictionPolicy(...)` with lock-free/batched `0 B/op` read-path tracking (**+28–29%** Zipfian and **+47–48%** scan hit-ratio gains vs `EvictionVolatileTTL`).
- **Sub-5ns Pure-Go Lock-Free Read Path & Coarse Monotonic Clock (#43):** Replaced `sync.RWMutex` + Go `map` reads with a pure-Go, zero-`unsafe`, `-race`-clean 64-byte Swiss-style open-addressed bucket table (`atomic.Pointer[entry]` + packed `h2` `atomic.Uint64` tags), 128-byte cache-line padded shards, 1ms coarse monotonic clock (`WithPreciseClock()` opt-out + automatic `< 10ms` precise fallback), and `< 3/4` sliding-TTL refresh coalescing (`2.44–2.62 ns/op` `GetParallel` at `0 B/op, 0 allocs/op`; `4.31–4.59 ns/op` `MixedParallel`).
- **Stale-While-Revalidate (SWR), TTL Jitter & Resilient Write-Through Invalidation (#44):** Added SWR (`softExpiresAt`/`hardExpiresAt`, fresh-only `Get()` vs stale-serving `GetOrLoad*` / `GetOrLoadJSONWithOptions[T]` with coalesced detached-context background refresh), deterministic TTL jitter (`[0.0, 0.5]`), `InvalidationModeWriteThrough` with `WithWriteThroughMaxBytes` fallback, non-blocking async batched invalidation, and exponential-backoff backplane reconnect.
- **Observability Hooks & Standalone `metrics/otel` / `metrics/prometheus` Sub-Modules (#45):** Added zero-dependency `MetricsRecorder` and `InvalidationDropRecorder` (`0 B/op` when nil), `InMemoryPubSub.DroppedCount()`, `MemoryStore.Health(ctx)` + optional `PubSubClient` `Pinger`, `WithLogger(*slog.Logger)` across all 6 `pubsub/*` adapters, and standalone `github.com/BryceWayne/MemoryStore/metrics/otel` (`v1.0.0`) & `github.com/BryceWayne/MemoryStore/metrics/prometheus` (`v1.0.0`) sub-modules.
- **Streaming Snapshot Persistence & Warm Restart (#46):** Added zero-dependency streaming `Save(io.Writer, ...SnapshotOption)` and `Load(io.Reader, ...SnapshotOption)` supporting chunked `SnapshotBinary` (`MSNAP\x02` + per-frame `CRC32C` + EOF sentinel) and streaming `SnapshotJSON` (`encoding/json/jsontext`), caller `compress/gzip` compatibility, relative TTL & `NoExpiration` preservation, `Overwrite`/`KeepExisting` conflict policies, incremental `MaxItems`/`MaxBytes` enforcement, and 100K-entry O(1) memory streaming benchmarks.
- **Fuzzing, Opaque-Box E2E Suite & Engineering Roadmap:** Added `FuzzMatchesTopic`, `FuzzInvalidationUnmarshal`, `FuzzSnapshotLoad`, `FuzzAWSExtractPayload`, standalone `e2e/` test suite (124 tests wired into `Makefile`, `go.work`, and CI), `ROADMAP.md`, and cross-adapter Broker Capability & Wildcard Matrix in `README.md`.

### Fixed
- **Core Lifecycle, Singleflight & Accounting:** Eliminated TOCTOU post-`Stop()` nil-map panics across `Set`/`SetMulti`/`GetOrLoad*`, fixed `singleflightGroup.do` panic propagation (preventing `(nil, nil)` waiter leaks), removed redundant JSON unmarshal on `GetOrLoadJSON[T]` leader path, made unbounded `GetMetrics()` unconditionally **O(1)**, and fixed `Set(k, v, ttl <= 0)` so expired writes delete without inserting dead entries or evicting live items.
- **Pub/Sub Adapter Hardening (`pubsub/*/v1.2.0`):**
  - `pubsub/gcp`: Stopped/cleaned up prior subscriptions on duplicate `Subscribe`, sanitized subscription IDs (<= 255 chars), checked `ctx.Err()` at entry, and added `WithChannelBuffer` / `WithLogger`.
  - `pubsub/azure`: Fixed `realAdminClient.EnsureTopic` to auto-create topics on HTTP `404 NotFound`, added `WithLogger`, and raised statement coverage from **64.7%** → **97.1%**.
  - `pubsub/redis`: Added `100ms → 5s` exponential backoff in `receiveLoop` and `WithLogger`.
  - `pubsub/nats`, `pubsub/aws`, `pubsub/kafka`: Added `WithLogger(*slog.Logger)` and raised coverage above **91%** across every adapter.
- **Repository & CI Hygiene (#34):** Fixed `.github/dependabot.yml` labels and added `/benchmarks`, `/metrics/otel`, `/metrics/prometheus`, and `/e2e`; updated `.github/ISSUE_TEMPLATE/config.yml` `pkg.go.dev` link to `/v2/memorystore`.

## [1.1.0] - 2026-11-23

Adds four standalone Go 1.27+ Pub/Sub adapters (`pubsub/redis`, `pubsub/nats`, `pubsub/aws`, and `pubsub/kafka`) with zero dependency on the root `memorystore` package, expands CI/Makefile/Dependabot/README coverage across all six `pubsub/*` modules, and raises `pubsub/gcp` offline test coverage to 98%+.

### Added
- **Standalone Redis / Valkey Pub/Sub adapter (`pubsub/redis`, #37):** `github.com/BryceWayne/MemoryStore/pubsub/redis` backed by `github.com/redis/go-redis/v9` supporting `redis.UniversalClient` (standalone, Sentinel, Cluster), `NewClient` and `NewClientFromClient`, exact `SUBSCRIBE` and wildcard `*` `PSUBSCRIBE` routing, `WithChannelBuffer`, `WithCloseClient`, and offline `miniredis/v2` + Go 1.27 `goroutineleak` tests (95.4% coverage).
- **Standalone NATS Pub/Sub adapter (`pubsub/nats`, #38):** `github.com/BryceWayne/MemoryStore/pubsub/nats` backed by `github.com/nats-io/nats.go` supporting `NewClient` and `NewClientFromConn`, NATS `*`/`>` subject wildcards, `WithQueueGroup`, `WithCloseConn`, `WithNATSOptions`, `WithDrainTimeout`, optional JetStream durable delivery mode, and embedded `nats-server/v2` + `goroutineleak` tests (89.4% coverage).
- **Standalone AWS SNS + SQS Pub/Sub adapter (`pubsub/aws`, #39):** `github.com/BryceWayne/MemoryStore/pubsub/aws` backed by the AWS SDK for Go v2 (`service/sns` + `service/sqs`) supporting default ephemeral auto-managed SQS queues (with automatic policy setup and cleanup on `Unsubscribe`/`Close`), pre-provisioned least-privilege IAM mode (`WithQueueURL`, `WithAutoManage(false)`), and offline fake/`httptest.Server` + `goroutineleak` tests (93.8% coverage).
- **Standalone Apache Kafka / Redpanda adapter (`pubsub/kafka`, #40):** Pure-Go (`CGO_ENABLED=0`, zero cgo) `github.com/BryceWayne/MemoryStore/pubsub/kafka` backed by `github.com/twmb/franz-go/pkg/kgo` supporting default `AtEnd()` broadcast mode, optional shared consumer groups (`WithConsumerGroup`), pass-through TLS and SASL/SCRAM auth options, clean flush/shutdown, and offline `kfake` + `goroutineleak` tests (86.6% coverage).
- **Repository, CI & Docs Integration:** Added `pubsub/redis`, `pubsub/nats`, `pubsub/aws`, and `pubsub/kafka` to `Makefile`, `.github/workflows/go.yml`, `.github/workflows/release.yml`, `.github/dependabot.yml`, and `README.md` (including `CGO_ENABLED=0` pure-Go Kafka verification).

### Changed
- **Expanded `pubsub/gcp` test suite:** Added offline `cloud.google.com/go/pubsub/v2/pstest` unit tests covering full Subscribe/Publish/Unsubscribe/Close lifecycles, gRPC error injection, slow-consumer `Nack`-on-cancel, concurrent `Publish`/`Close`, and `goroutineleak` verification, increasing statement coverage from **32.7%** to **98.0%+**.

## [1.0.0] - 2026-11-22

First stable **v1.0.0** release of **MemoryStore** (core module + standalone `pubsub/gcp` and `pubsub/azure` adapters), built for **Go 1.27+**.

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

[Unreleased]: https://github.com/BryceWayne/MemoryStore/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/BryceWayne/MemoryStore/compare/v1.1.0...v2.0.0
[1.1.0]: https://github.com/BryceWayne/MemoryStore/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/BryceWayne/MemoryStore/compare/v0.2.0...v1.0.0
[0.2.0]: https://github.com/BryceWayne/MemoryStore/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/BryceWayne/MemoryStore/releases/tag/v0.1.0
