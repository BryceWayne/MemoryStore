# MemoryStore Production Engineering Roadmap (`v1.1.x` → `v2.0.0`)

**Repository:** [`BryceWayne/MemoryStore`](https://github.com/BryceWayne/MemoryStore) · **Baseline:** `v1.1.0` (commit `010dde3`, default branch `root`) · **Date:** 2026-11-23

This document is the actionable production engineering roadmap for the MemoryStore project team. It synthesizes a verified codebase audit of `v1.1.0`, all six open `[v2]` GitHub issues (`#41`–`#46`), open Dependabot PR `#34`, and production-readiness gaps across all eight Go modules into concrete specifications, architectural decision recommendations, acceptance criteria, and a sequenced delivery plan.

---

## 1. Executive Summary & Verified Baseline (`v1.1.0`)

### 1.1 Architecture & Zero-Dependency Invariant
- **Toolchain:** Go `1.27.0` (`toolchain go1.27.4`), leveraging stdlib `encoding/json/v2`, `hash/maphash`, `uuid`, `log/slog`, `sync/atomic`, `sync.WaitGroup.Go`, and `runtime/pprof` `goroutineleak` verification.
- **Zero-Dependency Core Invariant:** Root `go.mod` (`github.com/BryceWayne/MemoryStore`) has **zero external dependencies** (no root `go.sum`). All broker SDKs—and future observability adapters—live in isolated sub-modules that do not import the core `memorystore` package.
- **Core Engine (`memorystore/`):** 256 inline, 64-byte cache-line aligned `shard` structs (`sync.RWMutex`, `map[string]item`, per-shard `atomic.Int64` counters `hits`/`misses`/`evictions`, guarded `bytes int64`), 32-byte `item` (`[]byte` + pointer-free monotonic `int64` nanosecond `expiresAt`), volatile-TTL sampled eviction (`evictionSampleSize = 8`), zero-dep `singleflightGroup`, Go 1.27 generic JSON helpers (`GetJSONAs[T]`, `GetOrLoadJSON[T]`), `InMemoryPubSub`, and distributed invalidation over `PubSubClient`.

### 1.2 Module Health & Test Baseline (Verified 2026-11-23)

| Module | Path | Purpose | External Deps | `go test -count=1 -race -cover` | Statement Coverage | `golangci-lint` (`v2.15.0`) |
|---|---|---|---|---|---|---|
| Core | `.` (`memorystore/`, `examples/basic`) | Cache engine, generics, singleflight, in-mem Pub/Sub | **0** | PASS (`1.975s`) | **92.0%** (`examples/basic`: `0.0%`) | 0 issues |
| GCP Adapter | `pubsub/gcp` | Google Cloud Pub/Sub v2 (`pstest` tests) | `cloud.google.com/go/pubsub/v2`, gRPC | PASS (`2.489s`) | **98.0%** | 0 issues |
| Azure Adapter | `pubsub/azure` | Azure Service Bus (`azservicebus` + `admin`) | `azcore`, `azservicebus` | PASS (`1.290s`) | **64.7%** | 0 issues |
| Redis Adapter | `pubsub/redis` | Redis / Valkey UniversalClient (`miniredis/v2` tests) | `go-redis/v9`, `miniredis/v2` | PASS (`1.939s`) | **95.4%** | 0 issues |
| NATS Adapter | `pubsub/nats` | Core NATS + JetStream (`nats-server/v2` tests) | `nats.go`, `nats-server/v2` | PASS (`5.747s`) | **89.4%** | 0 issues |
| AWS Adapter | `pubsub/aws` | AWS SNS + SQS (`httptest` / fake tests) | `aws-sdk-go-v2/{sns,sqs}` | PASS (`1.334s`) | **93.8%** | 0 issues |
| Kafka Adapter | `pubsub/kafka` | Pure-Go Kafka/Redpanda (`CGO_ENABLED=0`, `kfake` tests) | `franz-go`, `kfake`, `kmsg` | PASS (`27.779s`) | **86.6%–87.7%** (`86.6%` latest run) | 0 issues |
| Benchmarks | `benchmarks` | Head-to-head suite vs `otter/v2`, `ristretto/v2`, etc. | Peer caches (`replace => ..`) | Compiles (`go test -c`) | N/A | N/A |

### 1.3 GitHub Issue & PR State (Verified 2026-11-23)
- **Closed `v1.1.0` Adapter Issues:** `#37` (`pubsub/redis`), `#38` (`pubsub/nats`), `#39` (`pubsub/aws`), and `#40` (`pubsub/kafka`) were completed and closed in PR `#47` (`v1.1.0`, commit `010dde3`).
- **Open Pull Requests (1):** PR `#34` (Dependabot bump of `azcore` `1.23.0` → `1.24.0` in `/pubsub/azure`) — currently emits a warning comment because `.github/dependabot.yml` requests labels `dependencies`, `go`, and `ci` which do not exist in the repository's 12 labels (`bug`, `documentation`, `duplicate`, `enhancement`, `good first issue`, `help wanted`, `invalid`, `performance`, `pubsub`, `question`, `v2`, `wontfix`).
- **Open GitHub Issues (6 of 6 — all labeled `v2`, 0 PRs, 0 milestones):**
  - **#41** — `[v2] Functional options constructor, typed KV errors, and per-item Set options` (`enhancement`, `help wanted`, `v2`)
  - **#42** — `[v2] Pluggable high-hit-ratio eviction policies (S3-FIFO / W-TinyLFU)` (`enhancement`, `help wanted`, `v2`, `performance`)
  - **#43** — `[v2] Sub-5ns lock-free read path for GetParallel and MixedParallel` (`enhancement`, `v2`, `performance`)
  - **#44** — `[v2] Stale-While-Revalidate (SWR), TTL jitter, and write-through invalidation modes` (`enhancement`, `help wanted`, `v2`)
  - **#45** — `[v2] Pluggable observability hooks & OpenTelemetry / Prometheus sub-modules` (`enhancement`, `help wanted`, `good first issue`, `v2`)
  - **#46** — `[v2] Streaming snapshot persistence & warm restart (encoding/json/jsontext)` (`enhancement`, `help wanted`, `v2`)

---

## 2. Cross-Cutting Production-Readiness Assessment Matrix (7 Dimensions)

| Dimension | Current Baseline (`v1.1.0`) | Target Production State | Key Blockers & Codebase Evidence | Linked Issues / Work |
|---|---|---|---|---|
| **D1. Lifecycle Safety, API Contracts & Compatibility** | `NewMemoryStoreWithConfig(Config{...})` silently clamps invalid bounds (`memorystore.go:137-138`). `Stop()` sets `s.store = nil` (`242`); `Set`/`SetMulti`/`GetOrLoad` do **not** check `IsStopped()` and panic on nil-map writes (`431, 446, 597, 608, 667, 685`). `Set`/`Get` alias caller `[]byte` slices without copy or docs (`432, 505`). `singleflightGroup.do` leaks `(nil, nil)` to waiters if `fn` panics (`singleflight.go:42-50`). | Idiomatic `v2` module (`github.com/BryceWayne/MemoryStore/v2`) with `New(opts ...Option) (*MemoryStore, error)`, explicit `Config` validation, `ErrStoreStopped` on all post-`Stop()` operations, documented/optional slice copy safety, panic-safe singleflight, and `NoExpiration` sentinel. | Breaking constructor/signature changes require Go `/v2` module path decision; `Get`/`Delete` post-`Stop()` signatures; sliding TTL + `NoExpiration` interactions. | **#41** |
| **D2. Concurrency, Throughput & Scalability** | `Get` acquires `s.mu.RLock()` (`memorystore.go:495`) and calls `time.Since(m.baseTime)` (`173-175`), yielding `8.2 ns/op` (`GetParallel`) and `32.2 ns/op` (`MixedParallel`) vs `otter/v2` `4.6 ns` / `9.0 ns` (`README.md:574-575`). Unbounded `GetMetrics()` performs an **O(N)** full scan of all 256 shard maps under `RLock` (`memorystore.go:803-806`). | Pure-Go lock-free `atomic.Pointer[entry]` shard read path achieving `< 5 ns/op` `GetParallel` (`0 B/op, 0 allocs/op`) and `< 15 ns/op` `MixedParallel` (`<= 1` write alloc, `-race`-clean), 1ms coarse monotonic clock (`WithPreciseClock()` opt-out), **O(1)** `GetMetrics()` on all stores, zero `-race` or `goroutineleak` failures. | Redesigning `shard.store` and `item` affects `#42` read tracking, `#41` sliding TTL, `#44` SWR checks, `#45` nil-observer overhead, and `#46` shard iteration. | **#43** (+ O(1) `GetMetrics` fix) |
| **D3. Memory Management & Eviction Efficiency** | Bounded mode (`MaxItems`/`MaxBytes`) uses random map sampling (`evictionSampleSize = 8`, `memorystore.go:32, 328-373`) picking minimum `expiresAt`. `totalItems`/`totalBytes` update outside shard locks (`453-458`). `Set(k, v, 0)` still inserts & can evict valid items (`422-461`). `Get` lazy-expiration (`499-502`) doesn't delete expired entries. `payloadSize` (`199-201`) ignores map/struct overhead. | Pluggable `EvictionVolatileTTL`, `EvictionS3FIFO`, and `EvictionTinyLFU` with lock-free/batched `0 B/op` read-path tracking, Zipfian & scan-resistant hit-ratio parity/wins vs `otter/v2` & `ristretto/v2`, accurate capacity accounting (no valid evictions on `ttl <= 0`, lazy/periodic expired purge, documented overhead model). | Read-path frequency tracking (ring buffers / 2-bit counters) must remain lock-free and allocation-free inside `#43`'s read path. | **#42** |
| **D4. Multi-Pod Coherency & Resilience** | `Set`/`SetMulti`/`Delete` call `broadcastInvalidation` **synchronously** (`memorystore.go:465, 566, 631, 714`; `invalidation.go:43-63`), turning ~50ns local writes into network RPCs (`PubSubTimeout <= 0` = infinite block, `816-821`). `startInvalidationBackplane` (`invalidation.go:14-41`) subscribes once and exits silently on error or channel close. No SWR, no TTL jitter, no `WriteThrough`. | SWR in `GetOrLoad`/`GetOrLoadJSONWithOptions[T]` (`Stop()`-safe), deterministic configurable TTL jitter, `InvalidationMode` (`InvalidateOnly` vs `WriteThrough` with max payload cap), **bounded async/batched invalidation dispatch**, **backplane reconnect with exponential backoff**, and safe default `PubSubTimeout`. | Sync vs async invalidation delivery guarantees, `WriteThrough` remaining-TTL propagation, detached SWR refresh context, and `#41` jitter precedence. | **#44** |
| **D5. Durability & Warm Restart** | Strictly volatile in-memory storage. Rolling pod restarts start cold and stampede upstream stores. | Streaming `Save(io.Writer) error` and `Load(io.Reader) error` (zero-dep Go 1.27 stdlib), shard-by-shard non-blocking iteration, relative remaining TTL preservation, expired-item filtering, format header + chunked CRC32C frame integrity, incremental `MaxItems`/`MaxBytes` enforcement, optional `compress/gzip` caller wrapping. | Codec choice (`encoding/json/jsontext` vs compact binary), `NoExpiration` wire encoding, `Load` merge vs replace semantics, `InvalidationTopic` interaction on `Load`. | **#46** |
| **D6. Observability, Health & Diagnostics** | Polling-only `GetMetrics()` (`Items`, `Bytes`, `Hits`, `Misses`, `Evictions` at `memorystore.go:59-65`). No eviction-reason counts, load latency, singleflight coalescing rate, invalidation counters, `InMemoryPubSub` drop metrics (`pubsub_memory.go:124-126`), or `Health()`/`Ping()`. External `pubsub/*` adapters lack `*slog.Logger` and swallow background errors (`gcp.go:158-160`, `azure.go:400`, `aws.go:380`). | Zero-dep `MetricsRecorder` / `StatsObserver` (`0 B/op` when nil), standalone `metrics/otel` and `metrics/prometheus` sub-modules in CI/release, `InMemoryPubSub` drop counters, `Health()`/`Ping()` readiness probe, and optional `*slog.Logger` across all 6 `pubsub/*` adapters. | Thread-safety/non-blocking contract for observer callbacks; keeping `RecordHit`/`RecordMiss` zero-cost when nil on `< 5ns` `#43` hot path; multi-module release tags. | **#45** + Adapter Logging |
| **D7. Pub/Sub Adapter Hardening, Security & CI/Release Hygiene** | Latent adapter defects: `pubsub/gcp` leaks prior sub on duplicate `Subscribe` (`gcp.go:163`) & lacks ID sanitization (`126`); `pubsub/azure` `EnsureTopic` fails on 404 instead of creating (`azure.go:110-122`) & has `64.7%` coverage; `pubsub/redis` retries fixed `100ms` forever without resubscribe (`redis.go:333-346`); `azure`/`aws` risk lock/visibility expiry on slow consumers; `redis`/`nats`/`kafka` pull test servers into `go.mod` `require`; `.github/dependabot.yml` uses nonexistent labels & omits `benchmarks/`. | Zero known adapter resource leaks, `>= 90%` offline test coverage across all adapters (including `pubsub/azure`), exponential backoff + reconnect in `redis`, documented wildcard compatibility matrix, fuzz targets (`testing.F`), clean `dependabot.yml`, and isolated test-only server dependencies where feasible. | `pubsub/azure` live 404 verification via mock/SDK response error; `pubsub/kafka` `kgo.Client`-per-topic footprint; cross-adapter breaking vs non-breaking boundaries. | **W7 (Phase 0 + Phase 1)** + PR `#34` |

---

## 3. Detailed Engineering Specifications (`#41`–`#46` & Adapter Hardening)

### 3.1 Issue `#41` — Functional Options Constructor, Typed KV Errors & Per-Item `Set` Options
- **GitHub Reference:** [#41](https://github.com/BryceWayne/MemoryStore/issues/41) · Labels: `enhancement`, `help wanted`, `v2`
- **Codebase Grounding:**
  - Constructors & clamping: `memorystore/memorystore.go:113-164` (`NewMemoryStore`, `NewMemoryStoreWithConfig`, silent `max(config.MaxItems, 0)` / `max(config.MaxBytes, 0)` at `137-138`).
  - `Stop()` & nil-map panics: `Stop()` clears `s.store = nil` at `memorystore.go:242`. `SubscribeContext`/`PublishContext`/`UnsubscribeContext` check `m.IsStopped()` (`833, 849, 865`), whereas `Set` (`431, 446`), `SetMulti` (`597, 608, 667, 685`), and `GetOrLoad`/`GetOrLoadJSON` (`singleflight.go:74`, `generics.go:66` via `m.Set`) panic with `assignment to entry in nil map`. `Get` (`496`), `GetMulti` (`734, 774`), and `deleteLocal` (`543`) read from nil maps and silently behave as misses/no-ops.
  - Expiration calculation: `calcExpiresAt` (`memorystore.go:178-187`) treats `duration <= 0` as immediate expiration (`return now`), leaving no `NoExpiration` sentinel.
  - Slice aliasing & singleflight: `Set` stores raw `value` (`432, 447`) and `Get` returns `it.value` (`505`). `singleflightGroup.do` (`singleflight.go:42-50`) closes `c.done` on panic without propagating panic/error, returning `(nil, nil)` to coalesced waiters. `GetOrLoadJSON` (`generics.go:62-79`) marshals `loaded` then re-unmarshals `raw` on the leader path.
- **Target Design & Proposed Go API (`v2`):**
  ```go
  // Module: github.com/BryceWayne/MemoryStore/v2/memorystore

  const NoExpiration time.Duration = -1 // Distinct sentinel; 0 still means expire immediately (or explicit SetWithNoExpiration).

  type Option func(*Config) error
  func New(opts ...Option) (*MemoryStore, error)

  // Core Option constructors (non-exhaustive):
  func WithMaxItems(n int) Option
  func WithMaxBytes(bytes int64) Option
  func WithCleanupInterval(d time.Duration) Option
  func WithPubSub(ps PubSubClient) Option
  func WithPubSubTimeout(d time.Duration) Option
  func WithInvalidationTopic(topic string) Option
  func WithOnEvict(fn func(key string, value []byte, reason EvictReason)) Option
  func WithLogger(l *slog.Logger) Option
  func WithCopyOnWrite(enabled bool) Option // Optional safe byte-slice isolation
  func WithCopyOnRead(enabled bool) Option

  // Per-entry Set options (zero-alloc value type or bitmask struct):
  type SetOption func(*setOptions)
  func WithTTLJitter(fraction float64) SetOption
  func WithSlidingExpiration() SetOption
  func WithNoExpiration() SetOption
  func (m *MemoryStore) SetWithOptions(key string, value []byte, ttl time.Duration, opts ...SetOption) error
  ```
  - **Lifecycle Guard (TOCTOU-Safe):** Add an atomic `stopped atomic.Bool` fast-path flag (checked in ~0.5 ns without `lifecycleMu.RLock()`) at entry of `Set`, `SetWithOptions`, `SetMulti`, `SetJSON*`, `Delete`, `GetOrLoad`, `GetOrLoadJSON*`, `Save`, and `Load`, **AND** re-check `if s.store == nil || m.stopped.Load()` **inside `s.mu.Lock()`** (at zero extra lock cost since `s.mu.Lock()` is already held; `Stop()` sets `m.stopped.Store(true)` before acquiring shard locks and clearing `s.store = nil`). This prevents the TOCTOU `assignment to entry in nil map` panic when `Stop()` executes between the entry check and `s.mu.Lock()` (both in Phase 0 `v1.1.x` and Phase 1/2 `v2.0.0` shard writers).
  - **Singleflight Panic & Leader Optimization:** Catch panics in `singleflightGroup.do` (`recover()`), store a wrapped panic error (or re-panic consistently on waiters) instead of returning `(nil, nil)` to followers, and avoid the redundant `json.Unmarshal` on the leader path of `GetOrLoadJSON[T]`.
- **Recommended Architectural Decisions (Open Questions):**
  1. **Go `/v2` Module Path:** **Adopt `github.com/BryceWayne/MemoryStore/v2`** in root `go.mod` when shipping `#41`–`#46`. Per Go module SemVer rules, breaking `NewMemoryStoreWithConfig` / `Get` / `Delete` / `Config` changes *must* use a `/v2` module suffix so `v1.x` consumers are never broken by `go get -u`. Provide a thin migration guide; do not support dual v1/v2 APIs in the same major version.
  2. **`Get` and `Delete` Post-`Stop()` Signatures:** Keep `Get(key string) ([]byte, bool)` returning `(nil, false)` after `Stop()` (preserving the zero-alloc 2-return hot-path ergonomics) and add `GetErr(key string) ([]byte, bool, error)` or keep `Get` miss semantics while changing `Delete(key string) error` to return `ErrStoreStopped` when stopped (`nil` otherwise). Document `Get` post-`Stop()` explicitly in GoDoc.
  3. **Context-Free Hot Path:** **Keep `Set` and `Get` context-free** (`~40–50 ns` / `< 5 ns` parallel). Reserve `context.Context` for I/O-bound operations (`GetOrLoad`, `GetOrLoadJSON`, `Subscribe*`, `Publish*`, `Unsubscribe*`, `Health`). Passing `ctx` into `Get`/`Set` adds interface/value-check overhead and encourages misuse (canceling in-memory map writes mid-flight).
  4. **Slice Aliasing Policy:** Keep zero-copy default for maximum throughput (`0 B/op`), explicitly document immutability invariants in GoDoc (`README.md` + `doc.go`), and provide opt-in `WithCopyOnWrite(true)` / `WithCopyOnRead(true)` for callers reusing pooled buffers.
  5. **Sliding TTL + `#43` Lock-Free Reads (Refresh Threshold):** Store `ttlNanos int64` + `flags uint8` (sliding bit, no-expiration bit, precise-TTL bit) in `entry`, and store `expiresAt` as `atomic.Int64`. To prevent MESI cache-line ping-pong on hot sliding-TTL keys under `#43`'s `< 5 ns/op` `GetParallel` target, apply a **refresh threshold / coalescing guard**: only perform `expiresAt.Store(now + ttlNanos)` when remaining TTL `< ttlNanos * 3 / 4` (or elapsed since last refresh `>= refreshMinInterval`, e.g., `1ms` / 25% of TTL).
  6. **TTL Jitter Precedence (`#41` vs `#44`):** Global config (`WithDefaultTTLJitter`) acts as the baseline; per-item `WithTTLJitter(f)` **overrides** (does not compound/multiply) the global jitter. Clamp/validate jitter fraction to `[0.0, 0.5]` (reject `< 0` or `>= 1.0` at `New()` / `SetWithOptions()` to prevent accidental negative/zero TTLs).
  7. **`Store[K comparable, V any]` Typed Generic Wrapper Evaluation (ADR):** Keep `[]byte` + `GetJSONAs[T]`/`GetOrLoadJSON[T]` as the primary `v2` core engine rather than making the 256-shard core generic over `[K, V]`. Arbitrary `K comparable` keys require runtime reflection/formatting or type-specific hashers (allocating on `Get`/`Set` or fracturing `InvalidationTopic` string keys and `Snapshot` serialization), while arbitrary `V` values holding Go pointers re-introduce GC pointer-scanning across shard buckets. Optionally evaluate a thin, zero-overhead `TypedStore[K ~string, V any]` facade in `memorystore` (only ship if benchmarks verify `0 B/op` overhead on `~string` keys).
- **Production-Readiness Acceptance Criteria:**
  - `New()` validates all options (`MaxItems < 0`, `MaxBytes < 0`, invalid jitter, `InvalidationTopic` without PubSub, etc.) and returns descriptive wrapped errors instead of silent clamping.
  - Zero panics when calling `Set`, `SetWithOptions`, `SetMulti`, `SetJSON*`, `Get`, `GetMulti`, `Delete`, `GetOrLoad`, `GetOrLoadJSON` concurrently with or after `Stop()`; mutating methods return `ErrStoreStopped`.
  - `NoExpiration` entries never expire on `cleanupExpiredItems` or `Get`, and `ttl == 0` retains deterministic immediate expiration (`ttl <= 0 && ttl != NoExpiration`).
  - `singleflightGroup.do` panic test verifies coalesced waiters do not receive `(nil, nil)` and subsequent calls recover cleanly.
  - Zero external dependencies in root `go.mod`; `go test -race` + `goroutineleak` pass; `benchstat` shows `<= 5%` variance on `Set`/`Get` vs `v1.1.0`.

---

### 3.2 Issue `#42` — Pluggable High-Hit-Ratio Eviction Policies (`S3-FIFO` / `W-TinyLFU`)
- **GitHub Reference:** [#42](https://github.com/BryceWayne/MemoryStore/issues/42) · Labels: `enhancement`, `help wanted`, `v2`, `performance`
- **Codebase Grounding:**
  - Current eviction: `evictOneLocked` & `enforceBounds` (`memorystore.go:328-418`), `evictionSampleSize = 8` (`32`), `payloadSize` (`199-201`).
  - Soft-bound drift & expired-item bugs: `totalItems`/`totalBytes` are mutated outside `s.mu.Lock()` (`453-458`); `Set(k, v, 0)` inserts an already-expired item and may trigger `enforceBounds` to evict *valid* items before `cleanupExpiredItems` runs (`422-461`); `Get` lazy expiration (`499-502`) leaves expired entries in `s.store` inflating `GetMetrics().Items`/`.Bytes` until the 1-minute sweep.
- **Target Design & Proposed Go API:**
  ```go
  type EvictionPolicy uint8
  const (
      EvictionVolatileTTL EvictionPolicy = iota // Default when unbounded or explicitly chosen
      EvictionS3FIFO                            // Recommended default when bounded (MaxItems > 0 || MaxBytes > 0)
      EvictionTinyLFU                           // W-TinyLFU with 4-bit Count-Min Sketch + Doorkeeper
  )
  func WithEvictionPolicy(p EvictionPolicy) Option
  ```
  - **S3-FIFO Design (Per-Shard or Partitioned Queues):** Small FIFO queue (~10% capacity), Main FIFO queue (~90% capacity), and Ghost queue (metadata keys only, `hash` digests to save RAM). Each entry tracks a 2-bit frequency counter (`freq ∈ [0..3]`) updated atomically or via per-shard lossy read ring buffer (`64` slots, drained on miss/write under shard lock) so `GetParallel` stays lock-free and `0 B/op`.
  - **W-TinyLFU Design:** Admission filter using a compact 4-bit Count-Min Sketch (CMS) + resettable Doorkeeper + Window LRU (1%) + Main SLRU (probation 20% / protected 80%). Batched read-buffer drain updates CMS/SLRU under write lock only when the read buffer fills or on write.
  - **Capacity & Expired-Entry Accounting Fixes:**
    - On `Set(k, v, ttl)` where `ttl <= 0 && ttl != NoExpiration`: delete existing key (if present) and emit `EvictExpired` rather than inserting a dead entry that triggers capacity eviction of live entries.
    - Track per-entry overhead estimate (optional `WithEntryOverhead(bytes)` or documented payload-only `MaxBytes` contract) and trigger async/lazy delete or deferred purge queue for expired entries detected on `Get`.
    - Move `totalItems`/`totalBytes` accounting cleanly or enforce strict per-shard budget quotas (`maxItems / numShards` with cross-shard borrow fallback) so `enforceBounds` does not starve or drift under high-concurrency writes.
- **Recommended Architectural Decisions:**
  - **Default Eviction Policy:** Keep `EvictionVolatileTTL` when unbounded; default to **`EvictionS3FIFO`** when `MaxItems > 0 || MaxBytes > 0` (simpler pointer/slice layout, scan-resistant, lower metadata footprint than W-TinyLFU) unless caller selects `EvictionVolatileTTL` or `EvictionTinyLFU`.
  - **Unbounded + S3-FIFO/TinyLFU:** Return a `Config` validation error from `New()` if `EvictionS3FIFO`/`EvictionTinyLFU` is configured without `MaxItems > 0` or `MaxBytes > 0` (since frequency-bounded eviction requires a capacity target).
  - **Read-Path Tracking Budget:** Use a fixed-size per-shard or striped lossy read ring buffer (`atomic.Uint32` index + entry pointer/slot) or 2-bit atomic frequency field directly in the `item`/slot header (incremented only if `freq < 3` to avoid dirtying the cache line on repeated hits — "read-mostly frequency saturation").
- **Production-Readiness Acceptance Criteria:**
  - Zero external dependencies in core `memorystore`.
  - Zipfian (`s=0.99`) and scan-heavy (one-hit-wonder burst + hot working set) tests in `memorystore/` and `benchmarks/` demonstrating statistically significant hit-ratio improvements over `EvictionVolatileTTL` (target: `>= +15–30%` hit ratio on scan-mixed Zipfian workloads, within `<= 2–3%` of `otter/v2` W-TinyLFU).
  - `benchstat` confirms `0` regression on unbounded `GetParallel`/`Set` (`0 B/op, 0 allocs/op`) and `< 12 ns/op` `GetParallel` on bounded S3-FIFO/TinyLFU.
  - `Set(k, v, 0)` never evicts valid entries; `NoExpiration` items participate fairly in S3-FIFO/TinyLFU capacity eviction (`EvictCapacity`) while skipping TTL eviction (`EvictExpired`).
  - `go test -race` + `goroutineleak` pass under high write contention across all 256 shards.

---

### 3.3 Issue `#43` — Sub-5ns Lock-Free Read Path & Coarse Monotonic Clock
- **GitHub Reference:** [#43](https://github.com/BryceWayne/MemoryStore/issues/43) · Labels: `enhancement`, `v2`, `performance`
- **Codebase Grounding:**
  - Hot read path: `MemoryStore.Get` (`memorystore.go:493-506`) calls `s.mu.RLock()`, looks up Go `map[string]item`, calls `s.mu.RUnlock()`, then calls `m.nowNano()` (`time.Since(m.baseTime)` at `173-175`).
  - Cache-line constraint: `TestShardCacheLineSize` (`memorystore_extra_test.go:210-216`) enforces `unsafe.Sizeof(shard{}) == 64`.
  - O(N) `GetMetrics()` bug: `memorystore.go:803-806` iterates over every entry in every shard map when `!m.bounded` to compute `metrics.Bytes`.
- **Target Design:**
  - **Recommended Architecture: Open-Addressed Swiss-Style Bucket Array with Immutable `atomic.Pointer[entry]` Slots (Pure Go, Zero `unsafe`, `-race`-Clean):**
    - Each shard maintains a flat power-of-2 bucket table (`atomic.Pointer[table]`) where each cache-line-aligned bucket holds `4–7` slots with 1-byte `h2` hash tags (packed in `atomic.Uint64`) and `atomic.Pointer[entry]` pointers to immutable `entry` nodes (`key string`, `value []byte`, `expiresAt atomic.Int64`, `softExpiresAt int64`, `ttlNanos int64`, `flags uint8`, `freq atomic.Uint32` or ring-buffer handle).
    - **Wait-free / Lock-free `Get` (`0 B/op, 0 allocs/op`):** Load `table` pointer (`atomic.Pointer.Load()`), hash key once (`maphash.String` yields 64 bits: low 8 bits = shard index, next bits = bucket index, top 8 bits = `h2` tag), probe bucket checking packed `h2` tags, load `entry` via `slot.Load()`, validate `key == e.key` and `now < e.expiresAt.Load()`. Zero `sync.RWMutex` acquisition on reads; writers serialize per-shard on `s.mu.Lock()`, re-check `m.stopped.Load()`, install a new immutable `entry` via `slot.Store(newEntry)`, and resize via a new `table` swap.
    - **Why `atomic.Pointer[entry]` over a pure-Go seqlock:** A pure-Go seqlock reading non-atomic multi-word `key string` (16B) and `value []byte` (24B) headers concurrently with writer overwrites **fails Go's `-race` (TSan) detector** unless bypassed with `unsafe` or `//go:build !race` splits. Copy-on-write `atomic.Pointer[entry]` is 100% pure Go, zero-`unsafe`, unconditionally `-race`-clean, and preserves `0 B/op, 0 allocs/op` on `Get`/`GetParallel` (with `1` `entry` alloc on `Set` overwrites, matching `otter/v2`'s `1 alloc` on `Set` in `README.md:575`; `0 B/op` on `Set` overwrites is pursued only if an optional `-race`-safe pre-allocated slot pool is implemented without non-atomic header races).
  - **Coarse Monotonic Clock (`WithCoarseClock`):**
    - Single lightweight background ticker goroutine (e.g., `1ms` resolution) writing `m.coarseNow.Store(int64(time.Since(m.baseTime)))` (`atomic.Int64`), tracked by `m.wg.Go(...)` and exited cleanly on `m.ctx.Done()` (`goroutineleak`-clean).
    - **Sub-10ms & Opt-Out Precision:** `WithPreciseClock()` uses `time.Since(m.baseTime)` directly on **both `Set` and `Get`** for sub-millisecond exactness. Under the default `1ms` coarse clock, `Get` also reads `m.coarseNow.Load()` (up to `1ms` lag) *unless* a per-entry `preciseTTL` flag bit (set automatically when `ttl < 10ms`) triggers `nowNano()` on `Get` as well as `Set`, ensuring short-lived entries and sub-10ms TTL tests remain exact without penalizing normal reads.
  - **O(1) `GetMetrics()` Fix:**
    - Track `s.bytes` unconditionally inside `Set`/`SetMulti`/`deleteLocal`/`cleanupExpiredItems` (on the write path only, keeping `Get` untouched) so `GetMetrics()` (`memorystore.go:794-814`) is always O(256 shards) = O(1), never O(N items).
- **Recommended Architectural Decisions:**
  - **Pure Go Safety:** Use standard `sync/atomic` (`atomic.Pointer`, `atomic.Uint64`, `atomic.Int64`)—zero `unsafe` pointer arithmetic on Go `map` internals or `go:linkname` runtime hacks, guaranteeing `-race` compatibility and Go future-version portability.
  - **Coarse Clock Default:** Enable 1ms coarse clock by default in `v2` (with per-entry `preciseTTL` fallback for `ttl < 10ms`), plus `WithPreciseClock()` option for deterministic unit tests and strict sub-millisecond workloads. Update `shard` size assertion (`TestShardCacheLineSize`) for the new cache-line-aligned shard layout (64B or 128B).
- **Production-Readiness Acceptance Criteria:**
  - `benchmarks/` `benchstat` (`-count=5 -benchtime=400ms`) achieves **`< 5.0 ns/op` on `BenchmarkCompare_GetParallel`** (down from `8.2 ns/op`) and **`< 15.0 ns/op` on `BenchmarkCompare_MixedParallel`** (down from `32.2 ns/op`), maintaining strictly **`0 B/op, 0 allocs/op`** on `Get` and `GetParallel` and **`<= 1` write alloc** on `Set` / minimal amortized bytes on `MixedParallel` (matching or beating `otter/v2`'s `1 alloc` `Set` / `3 B` `MixedParallel` profile, or `0 B/op` if a `-race`-safe slot pool is used).
  - Single-threaded `Get` and `Set` improve or stay within parity of `v1.1.0` (`~40.9 ns` / `~48.6 ns`).
  - `GetMetrics()` executes in **O(1)** time (independent of key count `N` on both bounded and unbounded stores).
  - Zero data races under `go test -count=1 -race ./...` (including concurrent `Set`/`Delete`/table resize/`Get`/`Save`/`Stop`) and zero `goroutineleak` findings.
  - Zero external dependencies.

---

### 3.4 Issue `#44` — Stale-While-Revalidate (SWR), TTL Jitter & Write-Through Invalidation
- **GitHub Reference:** [#44](https://github.com/BryceWayne/MemoryStore/issues/44) · Labels: `enhancement`, `help wanted`, `v2`
- **Codebase Grounding:**
  - Singleflight & generics: `memorystore/singleflight.go:23-79` (`GetOrLoad`), `memorystore/generics.go:39-80` (`GetOrLoadJSON[T]`).
  - Synchronous blocking invalidation & fragile backplane: `Set`/`Delete`/`SetMulti` invoke `m.broadcastInvalidation(...)` synchronously on the caller goroutine (`memorystore.go:465, 566, 631, 714`; `invalidation.go:43-63`). `startInvalidationBackplane` (`invalidation.go:14-41`) subscribes once at `NewMemoryStoreWithConfig` and silently exits forever on `Subscribe` failure or closed channel. `defaultPubSubContext` (`memorystore.go:816-821`) has no timeout when `PubSubTimeout <= 0`.
- **Target Design & Proposed Go API:**
  1. **Stale-While-Revalidate (SWR):**
     - Store `hardExpiresAt` (`expiresAt + staleGrace`) alongside `softExpiresAt` (`expiresAt`) in `entry` (or slot metadata). Ensure `cleanupExpiredItems` and `evictOneLocked` treat `now < hardExpiresAt` as still eligible for SWR until `hardExpiresAt` elapses.
     - **`Get()` vs `GetOrLoad*()` Boundary:** `MemoryStore.Get(key)` strictly treats `now >= softExpiresAt` as a **miss** returning `(nil, false)` (keeping plain `Get` fresh-only and consistent for non-loader callers). `GetOrLoad`, `GetOrLoadWithOptions`, `GetOrLoadJSON`, and `GetOrLoadJSONWithOptions` use an internal SWR-aware lookup (`getWithStale(key) -> (val []byte, status hitStatus)` returning `hitFresh | hitStale | hitMiss`) so only loader APIs serve stale hits during `[softExpiresAt, hardExpiresAt)` and trigger refresh.
     - **API (`GetOrLoadJSON[T]` Variadic Signature Fix):** Because `GetOrLoadJSON[T]` (`memorystore/generics.go:44`) already terminates with variadic `opts ...json.Options`, do **not** overload it with `...LoadOption`. Expose dedicated `WithOptions` signatures alongside store/per-`Set` defaults:
       ```go
       type LoadOption func(*loadOptions)
       func WithStaleWhileRevalidate(grace time.Duration) LoadOption
       func (m *MemoryStore) GetOrLoadWithOptions(ctx context.Context, key string, ttl time.Duration, loader func(context.Context) ([]byte, error), opts ...LoadOption) ([]byte, error)
       func (m *MemoryStore) GetOrLoadJSONWithOptions[T any](ctx context.Context, key string, ttl time.Duration, loader func(context.Context) (T, error), loadOpts []LoadOption, jsonOpts ...json.Options) (T, error)
       // Plus store-level WithDefaultStaleWhileRevalidate(grace) / per-Set SetWithStaleWindow(grace) so standard GetOrLoad & GetOrLoadJSON[T] also participate cleanly.
       ```
     - **Refresh Semantics:** When `getWithStale` returns `hitStale` (`softExpiresAt <= now < hardExpiresAt`), return the stale value immediately (`0` wait), and if no singleflight refresh is active for `key`, launch a `m.wg.Go(...)` background refresh using a **detached lifecycle context** derived from `m.ctx` with a bounded refresh timeout (so caller HTTP request cancellation does not abort cache healing, while `Stop()` cancels and awaits all in-flight refreshes cleanly). If background refresh fails, log at `Warn`/`Debug`, emit `RecordLoad(dur, false, err)`, and preserve the stale item until `hardExpiresAt`.
  2. **TTL Jitter:**
     - Config option `WithTTLJitter(fraction float64)` (plus per-item override from `#41`) and internal `jitterRand` / testable `clockAndJitterSource` (e.g., deterministic `math/rand/v2` seeded PRNG option for unit tests). Formula: `ttl * (1 + uniform[-f, +f])`, clamped `>= 1ns` when `ttl > 0`.
  3. **Invalidation Modes & Async Resilient Backplane (Production Hardening):**
     ```go
     type InvalidationMode uint8
     const (
         InvalidationModeInvalidateOnly InvalidationMode = iota // Default: {"o": origin, "k": keys}
         InvalidationModeWriteThrough                           // Broadcast value+remainingTTL when payload <= threshold
     )
     func WithInvalidationMode(mode InvalidationMode) Option
     func WithWriteThroughMaxBytes(limit int) Option // Default e.g. 16 KiB; oversized payloads fall back to InvalidateOnly
     func WithAsyncInvalidation(queueSize int) Option // Non-blocking bounded ring/channel worker + batch coalescing
     ```
     - **Async Non-Blocking Broadcast:** Replace synchronous `m.ps.Publish(...)` on `Set`/`SetMulti`/`Delete` with a non-blocking enqueue into a bounded per-store invalidation channel/batcher drained by a `m.wg.Go` worker (default enabled when `InvalidationTopic != ""`, with optional `WithSyncInvalidation()` for tests requiring immediate visibility). Full queue drops increment metrics (`RecordInvalidationDrop`) and log a rate-limited warning instead of stalling cache writes.
     - **Backplane Reconnect & Default Timeout:** Wrap `startInvalidationBackplane` in an exponential-backoff retry loop (`100ms` → `30s` with jitter, respecting `m.ctx.Done()`) if `Subscribe` fails or `ch` closes unexpectedly, and set a safe default `PubSubTimeout` (e.g., `5 * time.Second`) when `InvalidationTopic != ""` and `PubSubTimeout <= 0`.
     - **`WriteThrough` Protocol:** Extend `invalidationEvent` (`invalidation.go:9-12`) with optional fields (`Mode`, `Entries []writeThroughEntry{Key, Value []byte, TTLNanos int64}` with `omitzero`). If `len(payload) > writeThroughMaxBytes` (including `SetMulti` batches that exceed threshold), split or automatically fall back to `InvalidateOnly` for oversized entries. Echo suppression via `Origin == m.instanceID` remains unchanged.
- **Production-Readiness Acceptance Criteria:**
  - SWR serves stale hit immediately (`< 1 µs` p99) while coalescing concurrent background refreshes to 1 execution via `singleflightGroup`; `Stop()` waits for active refreshes with zero `goroutineleak`s.
  - Deterministic TTL jitter tests prove exact spread within `[ttl*(1-f), ttl*(1+f)]` and validate `f ∈ [0, 0.5]`.
  - `WriteThrough` warms peer pods on small `Set`/`SetMulti` with accurate remaining TTL, falls back to `InvalidateOnly` above `WithWriteThroughMaxBytes`, and preserves `InstanceID` echo suppression.
  - `Set`/`Delete` latency remains sub-microsecond even when an external `PubSubClient` is slow or disconnected (async invalidation queue), and the backplane automatically reconnects after transient broker disconnects.

---

### 3.5 Issue `#45` — Pluggable Observability Hooks & OpenTelemetry / Prometheus Sub-Modules
- **GitHub Reference:** [#45](https://github.com/BryceWayne/MemoryStore/issues/45) · Labels: `enhancement`, `help wanted`, `good first issue`, `v2`
- **Codebase Grounding:**
  - Current metrics: `StoreMetrics` (`memorystore.go:59-65`), `GetMetrics()` (`794-814`), `EvictReason` (`pubsub.go:39-62`).
  - `InMemoryPubSub` silent drops: `deliverToSubscribers` (`pubsub_memory.go:124-126`) drops messages when `sub.ch` is full without tracking drop counts.
- **Target Design & Proposed Go API:**
  - **Core Interface (Zero External Deps):**
    ```go
    // MetricsRecorder receives real-time cache and invalidation events.
    // Implementations MUST be safe for concurrent calls from multiple goroutines
    // and MUST be non-blocking (never perform network I/O inline on hot paths).
    type MetricsRecorder interface {
        RecordHit()
        RecordMiss()
        RecordEvict(reason EvictReason, bytes int64)
        RecordLoad(duration time.Duration, shared bool, err error)
        RecordInvalidation(sent bool, keyCount int)
    }
    func WithMetricsRecorder(r MetricsRecorder) Option
    ```
  - **Zero-Overhead Nil Guard:** On `Get`/`Set`, guard with `if m.recorder != nil { m.recorder.RecordHit() }`. Because `m.recorder` is a single pointer-width interface field read-only after `New()`, CPU branch prediction keeps the nil path at **0 B/op, 0 allocs/op**, and `< 0.3 ns` overhead (verified against `#43`'s `< 5 ns` `GetParallel` budget).
  - **Standalone Adapter Sub-Modules:**
    - `metrics/otel` (`module github.com/BryceWayne/MemoryStore/metrics/otel`, tagged `metrics/otel/v1.0.0`): depends on `go.opentelemetry.io/otel/metric`; registers `memorystore.hits`, `memorystore.misses`, `memorystore.evictions` (tagged by `reason`), `memorystore.evicted_bytes`, `memorystore.loads`, `memorystore.load_duration_ms` (histogram, tagged `shared`, `status`), `memorystore.invalidations` (tagged `direction`), plus optional `ObservableGauge` callback wired to `GetMetrics()`.
    - `metrics/prometheus` (`module github.com/BryceWayne/MemoryStore/metrics/prometheus`, tagged `metrics/prometheus/v1.0.0`): depends on `github.com/prometheus/client_golang/prometheus`; exposes a `Collector` + `MetricsRecorder` implementation with configurable `prometheus.Registerer`, namespace/subsystem, and `Describe`/`Collect` pulling `GetMetrics()`.
    - **Dependency Boundary & Local CI Resolution:** `metrics/otel` and `metrics/prometheus` import `github.com/BryceWayne/MemoryStore/v2/memorystore` (for `EvictReason`, `StoreMetrics`, and `MetricsRecorder`), whereas core `memorystore` never imports `metrics/*` or `pubsub/*`, keeping the root `go.mod` strictly zero-dependency. Mirroring `pubsub/*`, keeping `metrics/{otel,prometheus}` at `v1.0.0` (no `/v2` module suffix) is valid in Go SemVer while depending on `MemoryStore/v2`. Use `go.work` (or CI-managed workspace resolution) so `metrics/*` and `benchmarks/` build and test cleanly against local `/v2` prior to `v2.0.0` tagging.
  - **Additional Core Diagnostics:**
    - `InMemoryPubSub.DroppedCount() int64` (atomic counter incremented at `pubsub_memory.go:125`) + `PubSubMetrics`.
    - `MemoryStore.Health(ctx context.Context) error` / `Ping(ctx context.Context) error`: checks `!m.IsStopped()`, invalidation backplane subscription health (if `InvalidationTopic != ""`), and optional `PubSubClient` `Pinger` interface (`Ping(context.Context) error`).
    - **CI & Build Integration:** Add `metrics/otel` and `metrics/prometheus` to `Makefile` (`SUBMODULES`), `.github/workflows/go.yml`, `.github/workflows/release.yml`, and `.github/dependabot.yml`.
- **Production-Readiness Acceptance Criteria:**
  - Core `go.mod` remains zero-dependency (no `go.sum` at root).
  - `benchstat` proves `WithMetricsRecorder(nil)` adds **0 B/op, 0 allocs/op** and `<= 0.5 ns/op` delta on `BenchmarkCompare_GetParallel`.
  - `metrics/otel` and `metrics/prometheus` pass `go test -count=1 -race -cover ./...` (using `sdk/metric` `ManualReader` and `prometheus/testutil`), `golangci-lint`, and `goroutineleak` checks, with complete GoDoc + `README.md` examples.
  - `InMemoryPubSub` exposes dropped message count; `MemoryStore.Health(ctx)` accurately reports stopped or disconnected-backplane states.

---

### 3.6 Issue `#46` — Streaming Snapshot Persistence & Warm Restart
- **GitHub Reference:** [#46](https://github.com/BryceWayne/MemoryStore/issues/46) · Labels: `enhancement`, `help wanted`, `v2`
- **Codebase Grounding:**
  - `item.expiresAt` (`memorystore.go:48`) is monotonic nanoseconds relative to `m.baseTime` (`89, 142`). Because `baseTime` resets on every process restart, raw `expiresAt` values cannot be persisted directly.
  - `shard` locks (`72`): iterating 256 shards sequentially under per-shard `RLock` (`cleanupExpiredItems` pattern at `285-302`) avoids blocking the entire cache simultaneously.
- **Target Design & Proposed Go API:**
  ```go
  type SnapshotFormat uint8
  const (
      SnapshotBinary SnapshotFormat = iota // Default: length-prefixed framed binary + per-frame CRC32C + EOF sentinel
      SnapshotJSON                         // Streaming Go 1.27 encoding/json/jsontext NDJSON/array
  )
  type SnapshotOption func(*snapshotOptions)
  func WithSnapshotFormat(f SnapshotFormat) SnapshotOption
  func WithSnapshotConflictPolicy(p SnapshotConflictPolicy) SnapshotOption // Overwrite (default) | KeepExisting

  func (m *MemoryStore) Save(w io.Writer, opts ...SnapshotOption) error
  func (m *MemoryStore) Load(r io.Reader, opts ...SnapshotOption) error
  ```
- **Recommended Architectural Decisions:**
  1. **Codec Choice:** Support **chunked/framed compact binary with per-frame CRC32C (`hash/crc32` Castagnoli) as default** for 100K+ entry streaming speed and zero base64 bloat on `[]byte` values, **AND** offer `SnapshotJSON` via Go 1.27 `encoding/json/jsontext.Encoder`/`Decoder` (streaming token-by-token without buffering the entire slice in RAM) for human-inspectable diagnostics. Both use 100% Go stdlib (`0` external deps). Caller wraps `w`/`r` with `compress/gzip` when compression is desired.
  2. **Header, Versioning & Streaming Chunked Integrity (`O(1)` RAM):** Magic header (`"MSNAP\x02"`), format version byte, flags (`hasFrameCRC`), and per-record length limits (`maxSnapshotKeyBytes`, `maxSnapshotValueBytes` caps against corrupt/malicious streams causing OOM panics). On `SnapshotBinary`, write bounded data frames (e.g., up to `64 KiB` / `256` entries per frame), each carrying its payload + per-frame `CRC32C`, followed by an explicit **EOF sentinel frame** (entry count + stream digest). On `Load(r io.Reader)`, decode and verify each frame's `CRC32C` inside a reusable `O(1)` frame buffer **before committing that frame's entries into `m.shards`**, and verify the EOF sentinel frame at stream end—failing loudly with `ErrCorruptSnapshot` / `ErrUnsupportedSnapshotVersion` without buffering the whole snapshot in RAM. Document explicitly that if stream corruption occurs at frame `k` on a non-seekable `io.Reader`, frames `1..k-1` are already verified and committed (callers hydrating a fresh store can discard/clear on error). For `SnapshotJSON`, validate each record token-by-token via `jsontext.Decoder` with bounded field sizes and detect premature EOF.
  3. **`NoExpiration` & Remaining TTL Encoding:** Encode `remainingTTL int64` as signed nanoseconds: `> 0` for remaining TTL, `-1` sentinel for `NoExpiration` (`#41`). On `Save`, skip any item where `now >= it.expiresAt` (unless `NoExpiration`). On `Load`, recompute `expiresAt = calcExpiresAt(now, time.Duration(remainingTTL))`. (If `#44` SWR is enabled, optionally persist `staleRemaining` or reset stale grace cleanly; recommended: only persist unexpired entries with `remainingTTL > 0` or `NoExpiration`.)
  4. **Incremental Bounds, Lifecycle & Invalidation Semantics on `Load`:**
     - `Save`/`Load` return `ErrStoreStopped` if `m.IsStopped()` (and `Load` shard writes re-check `stopped` inside shard locks).
     - `Load` enforces `m.maxItems`/`m.maxBytes` **incrementally per frame/entry as entries arrive** (updating `m.totalItems`/`m.totalBytes`, initializing `#42` eviction policy state, and triggering bounded eviction or stopping intake at capacity), rather than buffering or inserting an unbounded snapshot before running `enforceBounds` post-load—eliminating transient memory spikes above configured limits.
     - `Load` **does NOT broadcast `InvalidationTopic`** events (it is local node warm-start hydration, not a distributed write mutation). Document this explicitly.
     - Default `Load` conflict policy: `KeepExisting` or `Overwrite`? **Recommend `Overwrite` by default** (warm restart usually loads into an empty store, and if called during live operation, `KeepExisting` option prevents overwriting fresher concurrent writes).
- **Production-Readiness Acceptance Criteria:**
  - Zero external dependencies (pure Go 1.27 stdlib).
  - Roundtrip tests verify binary and `jsontext` formats, `compress/gzip` wrapping, remaining-TTL preservation within clock tolerance, skipping of already-expired entries, `NoExpiration` roundtrip, and `ErrStoreStopped` post-`Stop()`.
  - Fault-injection tests reject truncated streams (missing EOF sentinel), bad magic headers, unsupported versions, oversized frame lengths, and corrupted per-frame CRC32C checksums without committing unverified frames or panicking.
  - Bounds tests verify `MaxItems` and `MaxBytes` are enforced **incrementally** (peak RSS/allocated bytes bounded) when loading snapshots larger than configured limits, including with S3-FIFO / W-TinyLFU enabled.
  - Streaming memory benchmark for **100,000+ entries** (`100K` keys × `256B` values) proves `O(1)` streaming buffer allocation per entry (`B/op` proportional to batch/buffer size, not total dataset size) and zero cross-shard global blocking.

---

### 3.7 Workstream W7 — Pub/Sub Adapter Hardening, Security & CI/Release Hygiene
Concrete defects and hardening gaps verified directly in the `v1.1.0` codebase:

| Target | Verified Finding & File/Line | Actionable Fix | Target Phase |
|---|---|---|---|
| **`pubsub/gcp`** | 1. `Subscribe` (`gcp.go:163`) overwrites `c.subscriptions[topicName]` without stopping an existing subscription for the same topic (unlike `azure`, `redis`, `nats`, `aws`, `kafka`), leaking the previous goroutine & GCP subscription.<br>2. `subID` (`gcp.go:126`) formats raw `topicName` (`"memorystore-sub-%s-%s"`) without sanitizing disallowed characters (`:`, `*`) or enforcing GCP's 255-char ID limit.<br>3. `sub.Receive` error ignored (`gcp.go:158-160`); no `WithChannelBuffer` or pre-provisioned subscription option; `Subscribe` does not check `ctx.Err()` first. | Stop/clean up prior `existing` subscription before replacing (mirroring `azure.go:320-326`); add `sanitizeSubscriptionID(topicName)` (mirroring `aws.go:535-568` + 255-char cap); check `ctx.Err()` at entry; add `WithChannelBuffer` & `WithLogger(*slog.Logger)` & optional pre-provisioned mode. | **Phase 0** (leak + ID fix) / **Phase 1** (options/logger) |
| **`pubsub/azure`** | 1. `realAdminClient.EnsureTopic` (`azure.go:110-122`) immediately returns `GetTopic` errors (`if err != nil { return ... }`) instead of checking `isHTTPStatus(err, http.StatusNotFound)` before calling `CreateTopic`—meaning missing topics fail instead of auto-creating.<br>2. Statement coverage is **64.7%** (`real*` wrappers at `58-147` uncovered).<br>3. Blocking `ch <- msg.Body` (`398`) + ignored `CompleteMessage` error (`400`) risk PeekLock expiry on slow consumers. | Fix `EnsureTopic` to treat `http.StatusNotFound` as trigger for `CreateTopic`; expand unit tests (e.g., HTTP transport / mock `azcore` responses + error paths) to reach **>= 90%** coverage; add optional non-blocking drop/dead-letter mode or PeekLock renewal logging via `*slog.Logger`. | **Phase 0** (`EnsureTopic` + coverage) / **Phase 1** (logger) |
| **`pubsub/redis`** | 1. `receiveLoop` (`redis.go:333-346`) loops forever on `100ms` fixed `receiveRetryDelay` without exponential backoff or resubscribe if the `PubSub` connection permanently closes.<br>2. `pubsub/redis/go.mod:8` lists `miniredis/v2` in `require` (same test-server leak in `pubsub/nats/go.mod:8` `nats-server/v2` and `pubsub/kafka/go.mod:9` `kfake`). | Add exponential backoff (`100ms` → `5s`) + `Logger` warning + optional reconnect/resubscribe on dead `redis.PubSub`. Evaluate Go 1.27 `tool` or sub-module/build-tag test isolation for `miniredis/v2`, `nats-server/v2`, and `kfake` in `v2` so downstream `go.mod` graphs stay lean. | **Phase 0** (backoff) / **Phase 1** (`go.mod` hygiene) |
| **`pubsub/aws`** | 1. Blocking `ch <- payload` (`aws.go:378-384`) before `DeleteMessage` risks `VisibilityTimeout` redelivery under slow consumers; `DeleteMessage` error discarded (`380`).<br>2. Pre-provisioned `WithQueueURL` (`options.go:73`) supports only one queue URL per `Client` (multi-topic subscriptions share one queue). | Add `*slog.Logger` to log `DeleteMessage` failures; document single-queue-per-client constraint (or support `WithQueueURLFunc(func(topic string) string)` matching `azure.WithSubscriptionNameFunc`); add slow-consumer visibility warning/timeout option. | **Phase 0/1** |
| **`pubsub/nats`** | Core NATS drops on full buffer (`nats.go:668-672`) whereas JetStream blocks (`701-707`). JetStream `ensureStream` uses process-local 64-stripe lock (`27, 366-500`) because NATS lacks server-side CAS on stream subjects. | Document drop vs block policy clearly in wildcard/delivery matrix; add `*slog.Logger` + dropped-message counter metric. | **Phase 1** |
| **`pubsub/kafka`** | Creates a full `*kgo.Client` per subscribed topic (`kafka.go:225-245`), increasing connection/goroutine overhead for multi-topic caches (`-race` tests take `27.955s`). | Document resource cost per topic and evaluate sharing a single consumer `*kgo.Client` (or `AddConsumeTopics`) for broadcast/direct mode in `v2`. | **Phase 1/2** |
| **Cross-Adapter & CI Hygiene** | 1. Wildcard semantics differ across adapters (`InMemoryPubSub` `:`-delimited `*`, Redis glob `*?[` across `:`, NATS `.`-delimited `*`/`>`, GCP/Azure/AWS/Kafka literal-only); `SubscriberCounter` is `InMemoryPubSub`-only.<br>2. `.github/dependabot.yml:8-73` uses missing labels (`dependencies`, `go`, `ci` — causing PR `#34` warning) and omits `/benchmarks`.<br>3. No fuzz tests (`testing.F`). | 1. Publish a **Broker Capability & Wildcard Matrix** in `README.md`/`doc.go`.<br>2. Fix `.github/dependabot.yml` labels (or create repo labels `dependencies`, `go`, `ci` via repo settings) and add `/benchmarks` to `dependabot.yml`.<br>3. Add `FuzzMatchesTopic`, `FuzzInvalidationUnmarshal`, `FuzzSnapshotLoad`, and `FuzzAWSExtractPayload`. | **Phase 0** (Dependabot + matrix + fuzz) |

---

## 4. Dependency Graph & Phased Delivery Roadmap

### 4.1 Architectural Coupling & Dependency Graph

```text
Phase 0 (v1.1.x Hardening - Non-Breaking)
 ├── Fix Set/SetMulti post-Stop() panic guard (fast-path + lock-internal nil-map check)
 ├── Fix pubsub/gcp re-subscribe leak + ID sanitization
 ├── Fix pubsub/azure EnsureTopic 404 + raise coverage >=90%
 ├── Fix pubsub/redis receiveLoop backoff + dependabot.yml (/benchmarks + labels)
 └── Add fuzz targets (matchesTopic, invalidation, extractPayload) + Wildcard Matrix
          │
          ▼
Phase 1 (v2.0.0 Foundation: API, Lifecycle & Observability)
 ├── #41 Functional Options, New(), /v2 module path, ErrStoreStopped, NoExpiration, Config validation
 └── #45 MetricsRecorder interface (0-alloc nil), InMemoryPubSub drop metrics, Health(), metrics/{otel,prometheus} + CI
          │
          ▼  (Shard & item struct layouts MUST be co-designed once!)
Phase 2 (v2.0.0 Core Storage Engine: Eviction & Lock-Free Hot Path)
 ├── #43 Pure-Go atomic.Pointer[entry] shard read path (<5ns GetParallel, <15ns MixedParallel), 1ms coarse clock, O(1) GetMetrics()
 └── #42 Pluggable Eviction (VolatileTTL, S3-FIFO, W-TinyLFU), 0-alloc read tracking, ttl<=0 & expired accounting
          │
          ▼  (Depends on finalized item timestamps, shard iteration, singleflight, & MetricsRecorder)
Phase 3 (v2.0.0 Resilience, Coherency & Durability)
 ├── #44 SWR (GetOrLoad/GetOrLoadJSONWithOptions), TTL jitter, async batched invalidation, backplane reconnect, WriteThrough
 └── #46 Streaming Save/Load snapshots (framed CRC32C binary + jsontext), relative TTLs, incremental MaxItems/MaxBytes
```

**Why this sequencing is mandatory:**
1. **`#41` + `#45` before `#42`/`#43`:** `#41` establishes the `/v2` module path, `New(opts...)` signature, `ErrStoreStopped` invariants, `NoExpiration`, and sliding-TTL requirements. `#45` defines the `MetricsRecorder` interface so `#42`, `#43`, and `#44` instrument their paths once and verify nil-observer zero-overhead against `#43`'s `< 5 ns` `GetParallel` gate.
2. **`#43` + `#42` co-designed in Phase 2:** Both issues fundamentally reshape `shard` and `item` layouts in `memorystore/memorystore.go`. Implementing `#43` without `#42`'s lock-free read-tracking ring buffers / frequency bits—or `#41`'s sliding-TTL `expiresAt` atomics—would force a second breaking rewrite of the core hash table.
3. **`#44` + `#46` in Phase 3:** `#44` SWR requires `softExpiresAt`/`hardExpiresAt` fields in the finalized Phase 2 `item`/slot layout, `singleflightGroup` panic safety from `#41`, and `RecordLoad`/`RecordInvalidation` hooks from `#45`. `#46` `Save`/`Load` iterates over the finalized Phase 2 shard storage structure, respects `#41`'s `NoExpiration` sentinel and `ErrStoreStopped`, and hydrates Phase 2 eviction policy metadata (`EvictionS3FIFO`/`EvictionTinyLFU`).

---

### 4.2 Phased Execution Plan & Entry/Exit Gates

#### Phase 0 — `v1.1.x` Non-Breaking Production Hardening
- **Scope:**
  - Core: Fix `Set`/`SetMulti` nil-map panic after `Stop()` (add fast-path `stopped.Load()` **and** re-check `if s.store == nil || m.stopped.Load()` inside `s.mu.Lock()` in `Set` (`431, 446`) and `SetMulti` (`597, 608, 667, 685`) to eliminate the TOCTOU race); fix `singleflightGroup` panic propagation; make unbounded `GetMetrics()` O(1) via unconditional `s.bytes` tracking.
  - Adapters: Fix `pubsub/gcp` duplicate-`Subscribe` leak (`gcp.go:163`) & subscription ID sanitization (`126`); fix `pubsub/azure` `EnsureTopic` 404 check (`azure.go:110-122`) & raise coverage from `64.7%` → `>= 90%`; add exponential backoff in `pubsub/redis` `receiveLoop` (`redis.go:333-346`).
  - Hygiene: Fix `.github/dependabot.yml` labels (resolutions for PR `#34`) & add `/benchmarks`; document `[]byte` slice aliasing & cross-adapter wildcard matrix; add initial `testing.F` fuzz targets.
- **Exit Gate:** `make all` (`gofmt`, `go vet`, `golangci-lint`, `-race`, `CGO_ENABLED=0` kafka) green across all 8 modules; `pubsub/azure` coverage `>= 90%`; zero breaking API changes (`v1.1.1` patch release).

#### Phase 1 — `v2.0.0` Foundation: API, Lifecycle & Observability (`#41`, `#45`)
- **Scope:**
  - **Module Path & In-Repo `/v2` Migration:** Bump root `go.mod` to `github.com/BryceWayne/MemoryStore/v2`; update imports from `github.com/BryceWayne/MemoryStore/memorystore` to `github.com/BryceWayne/MemoryStore/v2/memorystore` (and `NewMemoryStore*` calls to `memorystore.New(...)`) in `examples/basic/main.go` (`line 13`), `benchmarks/cache_bench_test.go` (`line 9`), `benchmarks/go.mod` (`replace github.com/BryceWayne/MemoryStore/v2 => ..`), `benchmarks/README.md`, `pubsub/{aws,gcp,nats}/doc.go`, and `README.md`. Add `go.work` (or CI workspace resolution) so `metrics/{otel,prometheus}` and `benchmarks/` build and test cleanly against local `/v2` prior to `v2.0.0` tag publication.
  - **Issue `#41`:** Implement `memorystore.New(opts ...Option) (*MemoryStore, error)`, `Config` validation, `NoExpiration` sentinel, `SetWithOptions` (per-item jitter/sliding/no-exp), `WithCopyOnWrite`/`WithCopyOnRead`, `ErrStoreStopped` (fast-path + lock-internal re-check) across all mutating/load/snapshot APIs, and record the `Store[K comparable, V any]` ADR decision (`[]byte` + `GetJSONAs[T]` primary core; optional `TypedStore[K ~string, V any]` only if zero-alloc).
  - **Issue `#45`:** Add `MetricsRecorder` interface + `WithMetricsRecorder`, `InMemoryPubSub.DroppedCount()`, `MemoryStore.Health(ctx)`, `*slog.Logger` options on all 6 `pubsub/*` adapters, and new standalone `metrics/otel` & `metrics/prometheus` sub-modules (`module .../metrics/{otel,prometheus}`, tagged `v1.0.0`) integrated into `Makefile`, `.github/workflows/go.yml`, `.github/workflows/release.yml`, and `.github/dependabot.yml`. Evaluate isolating test-only broker deps (`miniredis`, `nats-server`, `kfake`) in `pubsub/{redis,nats,kafka}`.
- **Exit Gate:** Root `go.mod` has 0 external deps; `WithMetricsRecorder(nil)` shows `0 B/op, 0 allocs/op` and `<= 0.5 ns/op` delta; `examples/basic`, `benchmarks`, `metrics/otel`, and `metrics/prometheus` compile and pass `-race` + `goroutineleak` tests in CI via `go.work`.

#### Phase 2 — `v2.0.0` Core Storage Engine: Lock-Free Reads & Eviction (`#43`, `#42`)
- **Scope:**
  - **Issue `#43`:** Replace `sync.RWMutex` + Go `map[string]item` read lock with a pure-Go, zero-`unsafe`, `-race`-clean Swiss-style bucket table using `atomic.Pointer[entry]` slots, integrate 1ms coarse monotonic clock ticker (`WithPreciseClock()` opt-out + per-entry `preciseTTL` fallback for `ttl < 10ms` on both `Set` and `Get`), add sliding-TTL refresh threshold (`< 3/4` remaining TTL), and verify O(1) `GetMetrics()`.
  - **Issue `#42`:** Implement `EvictionVolatileTTL`, `EvictionS3FIFO`, and `EvictionTinyLFU` with lock-free/batched read tracking, fix `Set(ttl <= 0)` & lazy-expiration accounting, and add Zipfian/scan-resistance benchmarks in `benchmarks/` against `otter/v2` and `ristretto/v2`.
- **Exit Gate:** `make bench-compare` (`benchstat`, `count=5`) meets **`< 5 ns/op` `GetParallel` (`0 B/op, 0 allocs/op`)** and **`< 15 ns/op` `MixedParallel` (`<= 1` write alloc, matching or beating `otter/v2`)**; bounded Zipfian workloads show `>= +15%` hit ratio vs `EvictionVolatileTTL`; zero `-race` or `goroutineleak` failures.

#### Phase 3 — `v2.0.0` Resilience, Multi-Pod Coherency & Durability (`#44`, `#46`)
- **Scope:**
  - **Issue `#44`:** Add SWR (`softExpiresAt`/`hardExpiresAt`, `Get()` fresh-only miss vs `getWithStale` in `GetOrLoad`/`GetOrLoadWithOptions`/`GetOrLoadJSON`/`GetOrLoadJSONWithOptions[T]`, detached-context singleflight background refresh), deterministic TTL jitter, `InvalidationMode` (`InvalidateOnly` vs `WriteThrough` with `WithWriteThroughMaxBytes`), non-blocking async batched invalidation queue, backplane reconnect with exponential backoff, and safe default `PubSubTimeout`.
  - **Issue `#46`:** Implement streaming `Save(io.Writer)` / `Load(io.Reader)` (chunked/framed binary + per-frame CRC32C + EOF sentinel default, and `encoding/json/jsontext` option, `compress/gzip` caller-compatible), relative TTL + `NoExpiration` serialization, incremental `MaxItems`/`MaxBytes` enforcement per frame/entry during streaming `Load`, and 100K+ entry streaming memory benchmarks.
- **Exit Gate:** SWR, jitter, `WriteThrough`, backplane reconnect, and snapshot roundtrip/corruption/incremental-bounds tests pass under `-race` + `goroutineleak`; async invalidation keeps `Set` at `< 500 ns` even with a stalled/disconnected `PubSubClient`; 100K-entry snapshot streams with O(1) per-entry memory overhead.

---

## 5. Multi-Module CI, Release Tagging & `v2` Migration Checklist

### 5.1 Multi-Module CI & Release Tagging Strategy
- **CI Matrix (`.github/workflows/go.yml` & `Makefile`):**
  - Expand `SUBMODULES` to: `pubsub/gcp pubsub/azure pubsub/redis pubsub/nats pubsub/aws pubsub/kafka metrics/otel metrics/prometheus`.
  - **Pre-Tag Local `/v2` CI Resolution (`go.work`):** Before `github.com/BryceWayne/MemoryStore/v2 v2.0.0` is published on GitHub/GOPROXY, `metrics/otel`, `metrics/prometheus`, and `benchmarks/` cannot resolve `require github.com/BryceWayne/MemoryStore/v2 v2.0.0` over the network. Maintain a root `go.work` (or CI-generated `go work init . ./metrics/otel ./metrics/prometheus ./benchmarks ...` step in `.github/workflows/go.yml` and `Makefile`) during `v2` development so every module builds and tests against local `./memorystore` without committing permanent local `replace` directives in `metrics/*/go.mod`.
  - Add a non-blocking (or weekly) `benchstat` regression gate + fuzz smoke test (`go test -fuzz=. -fuzztime=10s`) to CI.
- **Go Sub-Module Tagging (`v2.0.0`):**
  - Core module (`github.com/BryceWayne/MemoryStore/v2`): tag **`v2.0.0`** first (triggers `.github/workflows/release.yml`).
  - Independent sub-modules that do **not** break their `PubSubClient` compatibility (`pubsub/*`): remain at `pubsub/<name>/v1.x.y` unless their Go API changes (e.g., if `pubsub/*` adds `v2` `WithLogger` or drops test-server deps, tag `pubsub/<name>/v1.2.0` or `pubsub/<name>/v2.0.0` per SemVer).
  - New observability sub-modules (`module github.com/BryceWayne/MemoryStore/metrics/otel` and `module github.com/BryceWayne/MemoryStore/metrics/prometheus`): import `github.com/BryceWayne/MemoryStore/v2/memorystore` for `EvictReason`, `StoreMetrics`, and `MetricsRecorder`, and tag **`metrics/otel/v1.0.0`** and **`metrics/prometheus/v1.0.0`** *after* `v2.0.0` is published (mirroring `pubsub/*`). Per Go module SemVer rules, a module path without `/v2` **must** use `v0`/`v1` tags (`go get` rejects `@v2.0.0` without `/v2` in `go.mod`; tagging `v2.0.0` would require `module .../metrics/{otel,prometheus}/v2`).
  - Update `.github/workflows/release.yml` trigger patterns if sub-module tags (`pubsub/*/v*.*.*`, `metrics/*/v*.*.*`) should also generate release notes.

### 5.2 `v1.1.0` → `v2.0.0` Migration Guide Checklist
- [ ] Update import path from `github.com/BryceWayne/MemoryStore/memorystore` to `github.com/BryceWayne/MemoryStore/v2/memorystore` (including in-repo consumers `examples/basic/main.go:13`, `benchmarks/cache_bench_test.go:9`, `benchmarks/go.mod`, `benchmarks/README.md`, `pubsub/{aws,gcp,nats}/doc.go`, and `README.md`).
- [ ] Replace `memorystore.NewMemoryStore()` / `memorystore.NewMemoryStoreWithConfig(Config{...})` with `store, err := memorystore.New(...)` and handle constructor validation errors.
- [ ] Check `errors.Is(err, memorystore.ErrStoreStopped)` on `Set`, `SetWithOptions`, `SetMulti`, `Delete`, `GetOrLoad*`, `Save`, and `Load` during graceful shutdown.
- [ ] Replace `ttl <= 0` assumptions if intending permanent entries: `0` still expires immediately; pass `memorystore.NoExpiration` (or `WithNoExpiration()`) for non-expiring entries.
- [ ] Review bounded cache behavior: bounded stores default to `EvictionS3FIFO` (set `WithEvictionPolicy(EvictionVolatileTTL)` to retain v1 random TTL sampling) and `InvalidationTopic` dispatches asynchronously by default with a bounded `PubSubTimeout`.
- [ ] Optionally wire `metrics/otel` or `metrics/prometheus` (`v1.0.0`) via `WithMetricsRecorder(...)` and configure `Save`/`Load` shutdown/startup hooks for warm restarts.
