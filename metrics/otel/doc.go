// Package otel provides an OpenTelemetry implementation of
// [memorystore.MetricsRecorder] and [memorystore.InvalidationDropRecorder]
// along with optional observable gauges backed by
// [memorystore.MemoryStore.GetMetrics].
//
// Because [memorystore.MetricsRecorder] is a dependency-free interface defined in
// the core module, this package is maintained as an independent Go module so
// core MemoryStore consumers do not incur OpenTelemetry dependencies unless they
// opt in.
//
// # Instruments and Attributes
//
// [Recorder] exports the following OpenTelemetry instruments under
// [DefaultInstrumentationName] (or the meter supplied via [WithMeter]):
//
//   - memorystore.hits (Int64Counter, unit "{hit}"): incremented on cache hits.
//   - memorystore.misses (Int64Counter, unit "{miss}"): incremented on cache misses.
//   - memorystore.evictions (Int64Counter, unit "{eviction}"): incremented on
//     evictions/deletes with attribute reason ("expired", "capacity",
//     "deleted", or "unknown").
//   - memorystore.evicted_bytes (Int64Counter, unit "By"): incremented by
//     payload bytes (when > 0) with attribute reason.
//   - memorystore.loads (Int64Counter, unit "{load}"): incremented on
//     GetOrLoad/GetOrLoadJSON calls with attributes shared (bool) and status
//     ("ok" or "error").
//   - memorystore.load_duration_ms (Float64Histogram, unit "ms"): records load
//     duration in milliseconds with attributes shared and status.
//   - memorystore.invalidations (Int64Counter, unit "{key}"): incremented by the
//     number of invalidated keys (when > 0) with attribute direction ("sent" or
//     "received").
//   - memorystore.invalidation_drops (Int64Counter, unit "{key}"): incremented by
//     the number of async invalidation keys dropped (when > 0) due to a full
//     invalidation queue.
//   - memorystore.items (Int64ObservableGauge, unit "{item}"): current item count
//     reported by [memorystore.MemoryStore.GetMetrics] when a store is attached.
//   - memorystore.bytes (Int64ObservableGauge, unit "By"): current payload bytes
//     reported by [memorystore.MemoryStore.GetMetrics] when a store is attached.
//
// Base attributes configured via [WithAttributes] are attached to all emitted
// instruments. Event-specific attributes (reason, shared, status, direction)
// take precedence if a base attribute uses the same key.
//
// # Thread-Safety and Non-Blocking Guarantees
//
// All [Recorder] methods are safe for concurrent use by multiple goroutines.
// Attribute sets and metric option slices are pre-allocated during [NewRecorder]
// so hot-path Record* calls perform zero heap allocations in this package and
// never block on MemoryStore shard locks.
//
// When a store is attached via [WithStore] or [Recorder.RegisterStore], the
// observable gauge callback calls [memorystore.MemoryStore.GetMetrics] in
// lock-free O(1) time. Because MemoryStore never
// holds shard locks while invoking [memorystore.MetricsRecorder], no lock-order
// cycle exists. Call [Recorder.Close] during shutdown to idempotently
// unregister the observable gauge callback.
//
// # Wiring Example
//
//	rec, err := otel.NewRecorder(
//		otel.WithMeterProvider(mp),
//		otel.WithAttributes(attribute.String("cache", "users")),
//	)
//	if err != nil {
//		return err
//	}
//	defer func() { _ = rec.Close() }()
//
//	store, err := memorystore.New(
//		memorystore.WithMetricsRecorder(rec),
//	)
//	if err != nil {
//		return err
//	}
//	defer func() { _ = store.Stop() }()
//
//	if err := rec.RegisterStore(store); err != nil {
//		return err
//	}
package otel
