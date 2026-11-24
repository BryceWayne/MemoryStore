// Package prometheus provides a Prometheus [prometheus.Collector],
// [memorystore.MetricsRecorder], and [memorystore.InvalidationDropRecorder]
// implementation for [memorystore.MemoryStore].
//
// Because [memorystore.MetricsRecorder] is a dependency-free interface defined in
// the core module, this package is maintained as an independent Go module so
// core MemoryStore consumers do not incur Prometheus client dependencies unless
// they opt in.
//
// # Registration Behavior
//
// By default, [NewCollector] registers the created [Collector] with
// [prometheus.DefaultRegisterer]. Use [WithRegisterer] to supply a custom
// [prometheus.Registerer] (such as a dedicated *prometheus.Registry) or pass
// nil to skip automatic registration when managing registration manually.
//
// If the target registry already has metrics registered with the same
// fully-qualified names and constant labels, [NewCollector] returns the
// underlying [prometheus.AlreadyRegisteredError]. To instrument multiple
// stores in the same process on a shared registry, distinguish them using
// [WithConstLabels], [WithNamespace], or [WithSubsystem].
//
// # Exported Metrics
//
// With the default namespace ("memorystore") and empty subsystem, [Collector]
// exposes the following Prometheus metrics:
//
//   - memorystore_hits_total (Counter): total cache hits.
//   - memorystore_misses_total (Counter): total cache misses.
//   - memorystore_evictions_total (Counter, label: reason): total cache
//     evictions and deletions ("expired", "capacity", "deleted", or "unknown").
//   - memorystore_evicted_bytes_total (Counter): total payload bytes evicted or
//     deleted (only incremented when bytes > 0).
//   - memorystore_loads_total (Counter, labels: shared, status): total
//     GetOrLoad/GetOrLoadJSON loader executions (shared="true"|"false",
//     status="ok"|"error").
//   - memorystore_load_duration_seconds (Histogram, labels: shared, status):
//     loader execution duration in seconds.
//   - memorystore_invalidations_total (Counter, label: direction): total
//     distributed invalidation keys sent or received (direction="sent"|"received").
//   - memorystore_invalidation_drops_total (Counter): total async invalidation
//     keys dropped due to a full queue.
//   - memorystore_items (Gauge): current item count from
//     [memorystore.MemoryStore.GetMetrics] (emitted once a store is attached).
//   - memorystore_bytes (Gauge): current stored payload bytes from
//     [memorystore.MemoryStore.GetMetrics] (emitted once a store is attached).
//
// # Multi-Store ConstLabels Example
//
//	c, err := prometheus.NewCollector(
//		prometheus.WithRegisterer(reg),
//		prometheus.WithConstLabels(prom.Labels{"cache": "sessions"}),
//	)
//	if err != nil {
//		return err
//	}
//	defer func() { _ = c.Close() }()
//
//	store, err := memorystore.New(
//		memorystore.WithMetricsRecorder(c),
//	)
//	if err != nil {
//		return err
//	}
//	defer func() { _ = store.Stop() }()
//
//	c.SetStore(store)
package prometheus
