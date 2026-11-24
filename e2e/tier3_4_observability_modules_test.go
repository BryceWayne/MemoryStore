package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	otelmetrics "github.com/BryceWayne/MemoryStore/metrics/otel"
	prommetrics "github.com/BryceWayne/MemoryStore/metrics/prometheus"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func findOTelMetric(rm *metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func otelSumValue(t *testing.T, m metricdata.Metrics, want attribute.Set) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %s data type = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	t.Fatalf("metric %s missing sum data point for attributes %v", m.Name, want)
	return 0
}

func otelGaugeValue(t *testing.T, m metricdata.Metrics, want attribute.Set) int64 {
	t.Helper()
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %s data type = %T, want metricdata.Gauge[int64]", m.Name, m.Data)
	}
	for _, dp := range gauge.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	t.Fatalf("metric %s missing gauge data point for attributes %v", m.Name, want)
	return 0
}

func otelHistogramPoint(t *testing.T, m metricdata.Metrics, want attribute.Set) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %s data type = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, dp := range hist.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp
		}
	}
	t.Fatalf("metric %s missing histogram data point for attributes %v", m.Name, want)
	return metricdata.HistogramDataPoint[float64]{}
}

func TestTier3_OTel_EndToEnd_ManualReader(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	baseAttr := attribute.String("service", "e2e")
	rec, err := otelmetrics.NewRecorder(
		otelmetrics.WithMeterProvider(mp),
		otelmetrics.WithAttributes(baseAttr),
		otelmetrics.WithLoadDurationBuckets(0, 1, 10, 100),
	)
	if err != nil {
		t.Fatalf("otel.NewRecorder() error = %v", err)
	}
	defer func() { _ = rec.Close() }()

	broker := memorystore.NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	store := mustNewStore(t,
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("e2e:otel"),
		memorystore.WithSyncInvalidation(),
		memorystore.WithMaxItems(3),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithPreciseClock(),
	)

	if err := rec.RegisterStore(store); err != nil {
		t.Fatalf("rec.RegisterStore() error = %v", err)
	}
	if err := rec.RegisterStore(store); !errors.Is(err, otelmetrics.ErrStoreAlreadyRegistered) {
		t.Fatalf("duplicate RegisterStore() error = %v, want ErrStoreAlreadyRegistered", err)
	}

	peer := mustNewStore(t,
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("e2e:otel"),
		memorystore.WithSyncInvalidation(),
	)

	// 1. Miss + Hit + Delete.
	if _, ok := store.Get("missing"); ok {
		t.Fatal("Get(missing) unexpectedly hit")
	}
	if err := store.Set("k1", []byte("vv1"), time.Minute); err != nil {
		t.Fatalf("Set(k1) error = %v", err)
	}
	if got, ok := store.Get("k1"); !ok || string(got) != "vv1" {
		t.Fatalf("Get(k1) = (%q, %v), want (vv1, true)", got, ok)
	}
	if err := store.Delete("k1"); err != nil {
		t.Fatalf("Delete(k1) error = %v", err)
	}

	// 2. Expiration eviction via lazy Get.
	if err := store.Set("k-exp", []byte("exp"), 5*time.Millisecond); err != nil {
		t.Fatalf("Set(k-exp) error = %v", err)
	}
	eventually(t, time.Second, 2*time.Millisecond, func() bool {
		_, ok := store.Get("k-exp")
		return !ok
	}, "k-exp should expire and emit EvictExpired")

	// 3. Peer invalidation received.
	if err := store.Set("k2", []byte("vv2"), time.Minute); err != nil {
		t.Fatalf("Set(k2) error = %v", err)
	}
	if err := store.Set("k3", []byte("vv3"), time.Minute); err != nil {
		t.Fatalf("Set(k3) error = %v", err)
	}
	if err := peer.SetMulti(map[string][]byte{"k2": []byte("new2"), "k3": []byte("new3")}, time.Minute); err != nil {
		t.Fatalf("peer.SetMulti() error = %v", err)
	}
	eventually(t, 2*time.Second, 5*time.Millisecond, func() bool {
		_, ok2 := store.Get("k2")
		_, ok3 := store.Get("k3")
		return !ok2 && !ok3
	}, "peer.SetMulti should invalidate k2 and k3 on store")

	// 4. Capacity eviction (MaxItems=3).
	for _, k := range []string{"a1", "a2", "a3", "a4"} {
		if err := store.Set(k, []byte("val"), time.Minute); err != nil {
			t.Fatalf("Set(%s) error = %v", k, err)
		}
	}

	// 5. Coalesced GetOrLoad (leader + follower) and error load.
	leaderStarted := make(chan struct{})
	followerEntered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, loadErr := store.GetOrLoad(t.Context(), "sf-key", time.Minute, func(context.Context) ([]byte, error) {
			close(leaderStarted)
			<-release
			return []byte("sf-val"), nil
		})
		if loadErr != nil {
			t.Errorf("leader GetOrLoad() error = %v", loadErr)
		}
	}()
	<-leaderStarted
	go func() {
		defer wg.Done()
		close(followerEntered)
		_, loadErr := store.GetOrLoad(t.Context(), "sf-key", time.Minute, func(context.Context) ([]byte, error) {
			return []byte("unexpected-follower-load"), nil
		})
		if loadErr != nil {
			t.Errorf("follower GetOrLoad() error = %v", loadErr)
		}
	}()
	<-followerEntered
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if _, err := store.GetOrLoad(t.Context(), "fail-key", time.Minute, func(context.Context) ([]byte, error) {
		return nil, errors.New("upstream error")
	}); err == nil {
		t.Fatal("GetOrLoad(fail-key) = nil error, want non-nil")
	}

	// Collect and verify all 9 OTel instruments.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("reader.Collect() error = %v", err)
	}
	if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != otelmetrics.DefaultInstrumentationName {
		t.Fatalf("ScopeMetrics = %+v, want single scope %q", rm.ScopeMetrics, otelmetrics.DefaultInstrumentationName)
	}

	baseSet := attribute.NewSet(baseAttr)

	hitsMetric, ok := findOTelMetric(&rm, "memorystore.hits")
	if !ok {
		t.Fatal("missing memorystore.hits")
	}
	if got := otelSumValue(t, hitsMetric, baseSet); got < 1 {
		t.Errorf("memorystore.hits = %d, want >= 1", got)
	}

	missesMetric, ok := findOTelMetric(&rm, "memorystore.misses")
	if !ok {
		t.Fatal("missing memorystore.misses")
	}
	if got := otelSumValue(t, missesMetric, baseSet); got < 4 {
		t.Errorf("memorystore.misses = %d, want >= 4", got)
	}

	evictionsMetric, ok := findOTelMetric(&rm, "memorystore.evictions")
	if !ok {
		t.Fatal("missing memorystore.evictions")
	}
	for _, reason := range []string{"capacity", "expired", "deleted"} {
		set := attribute.NewSet(baseAttr, attribute.String("reason", reason))
		if got := otelSumValue(t, evictionsMetric, set); got < 1 {
			t.Errorf("memorystore.evictions[reason=%s] = %d, want >= 1", reason, got)
		}
	}

	evictedBytesMetric, ok := findOTelMetric(&rm, "memorystore.evicted_bytes")
	if !ok {
		t.Fatal("missing memorystore.evicted_bytes")
	}
	for _, reason := range []string{"capacity", "expired", "deleted"} {
		set := attribute.NewSet(baseAttr, attribute.String("reason", reason))
		if got := otelSumValue(t, evictedBytesMetric, set); got <= 0 {
			t.Errorf("memorystore.evicted_bytes[reason=%s] = %d, want > 0", reason, got)
		}
	}

	loadsMetric, ok := findOTelMetric(&rm, "memorystore.loads")
	if !ok {
		t.Fatal("missing memorystore.loads")
	}
	loadHistMetric, ok := findOTelMetric(&rm, "memorystore.load_duration_ms")
	if !ok {
		t.Fatal("missing memorystore.load_duration_ms")
	}
	for _, lc := range []struct {
		shared bool
		status string
	}{
		{shared: false, status: "ok"},
		{shared: true, status: "ok"},
		{shared: false, status: "error"},
	} {
		set := attribute.NewSet(baseAttr, attribute.Bool("shared", lc.shared), attribute.String("status", lc.status))
		if got := otelSumValue(t, loadsMetric, set); got != 1 {
			t.Errorf("memorystore.loads[shared=%v,status=%s] = %d, want 1", lc.shared, lc.status, got)
		}
		dp := otelHistogramPoint(t, loadHistMetric, set)
		if dp.Count != 1 {
			t.Errorf("memorystore.load_duration_ms[shared=%v,status=%s].Count = %d, want 1", lc.shared, lc.status, dp.Count)
		}
	}

	invMetric, ok := findOTelMetric(&rm, "memorystore.invalidations")
	if !ok {
		t.Fatal("missing memorystore.invalidations")
	}
	if got := otelSumValue(t, invMetric, attribute.NewSet(baseAttr, attribute.String("direction", "sent"))); got < 1 {
		t.Errorf("memorystore.invalidations[sent] = %d, want >= 1", got)
	}
	if got := otelSumValue(t, invMetric, attribute.NewSet(baseAttr, attribute.String("direction", "received"))); got < 2 {
		t.Errorf("memorystore.invalidations[received] = %d, want >= 2", got)
	}

	wantMetrics := store.GetMetrics()
	itemsMetric, ok := findOTelMetric(&rm, "memorystore.items")
	if !ok {
		t.Fatal("missing memorystore.items")
	}
	if got := otelGaugeValue(t, itemsMetric, baseSet); got != int64(wantMetrics.Items) {
		t.Errorf("memorystore.items = %d, want %d", got, wantMetrics.Items)
	}
	bytesMetric, ok := findOTelMetric(&rm, "memorystore.bytes")
	if !ok {
		t.Fatal("missing memorystore.bytes")
	}
	if got := otelGaugeValue(t, bytesMetric, baseSet); got != wantMetrics.Bytes {
		t.Errorf("memorystore.bytes = %d, want %d", got, wantMetrics.Bytes)
	}

	// After store.Stop(), observable gauges report 0.
	if err := store.Stop(); err != nil {
		t.Fatalf("store.Stop() error = %v", err)
	}
	var rmAfterStop metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rmAfterStop); err != nil {
		t.Fatalf("reader.Collect() after Stop error = %v", err)
	}
	if m, ok := findOTelMetric(&rmAfterStop, "memorystore.items"); !ok || otelGaugeValue(t, m, baseSet) != 0 {
		t.Error("expected memorystore.items == 0 after store.Stop()")
	}
	if m, ok := findOTelMetric(&rmAfterStop, "memorystore.bytes"); !ok || otelGaugeValue(t, m, baseSet) != 0 {
		t.Error("expected memorystore.bytes == 0 after store.Stop()")
	}

	// After rec.Close(), observable gauges are unregistered and RegisterStore returns ErrRecorderClosed.
	if err := rec.Close(); err != nil {
		t.Fatalf("rec.Close() error = %v", err)
	}
	if err := rec.RegisterStore(store); !errors.Is(err, otelmetrics.ErrRecorderClosed) {
		t.Fatalf("RegisterStore after Close error = %v, want ErrRecorderClosed", err)
	}
	var rmAfterClose metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rmAfterClose); err != nil {
		t.Fatalf("reader.Collect() after Close error = %v", err)
	}
	if _, ok := findOTelMetric(&rmAfterClose, "memorystore.items"); ok {
		t.Error("memorystore.items still emitted after Recorder.Close()")
	}
	if _, ok := findOTelMetric(&rmAfterClose, "memorystore.bytes"); ok {
		t.Error("memorystore.bytes still emitted after Recorder.Close()")
	}
}

func TestTier3_Prometheus_EndToEnd_PedanticAndTestutil(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	col, err := prommetrics.NewCollector(
		prommetrics.WithRegisterer(reg),
		prommetrics.WithConstLabels(prometheus.Labels{"cluster": "e2e"}),
		prommetrics.WithDurationBuckets([]float64{0.0005, 0.005, 0.05, 0.5}),
	)
	if err != nil {
		t.Fatalf("prometheus.NewCollector() error = %v", err)
	}
	defer func() { _ = col.Close() }()

	// Before SetStore, items/bytes are omitted yet pedantic collection succeeds.
	beforeSeries := testutil.CollectAndCount(reg)
	if beforeSeries == 0 {
		t.Fatal("CollectAndCount(reg) before SetStore = 0, want > 0")
	}

	broker := memorystore.NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	store := mustNewStore(t,
		memorystore.WithMetricsRecorder(col),
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("e2e:prom"),
		memorystore.WithSyncInvalidation(),
		memorystore.WithMaxItems(3),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
		memorystore.WithPreciseClock(),
	)
	col.SetStore(store)

	if got := testutil.CollectAndCount(reg); got != beforeSeries+2 {
		t.Fatalf("CollectAndCount(reg) after SetStore = %d, want %d (+2 gauges)", got, beforeSeries+2)
	}

	peer := mustNewStore(t,
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("e2e:prom"),
		memorystore.WithSyncInvalidation(),
	)

	// 1. Miss + Hit + Delete (5 bytes).
	if _, ok := store.Get("missing"); ok {
		t.Fatal("Get(missing) unexpectedly hit")
	}
	if err := store.Set("k1", []byte("vv1"), time.Minute); err != nil {
		t.Fatalf("Set(k1) error = %v", err)
	}
	if got, ok := store.Get("k1"); !ok || string(got) != "vv1" {
		t.Fatalf("Get(k1) = (%q, %v), want (vv1, true)", got, ok)
	}
	if err := store.Delete("k1"); err != nil {
		t.Fatalf("Delete(k1) error = %v", err)
	}

	// 2. Expiration eviction (8 bytes: "k-exp" + "exp").
	if err := store.Set("k-exp", []byte("exp"), 5*time.Millisecond); err != nil {
		t.Fatalf("Set(k-exp) error = %v", err)
	}
	eventually(t, time.Second, 2*time.Millisecond, func() bool {
		_, ok := store.Get("k-exp")
		return !ok
	}, "k-exp should expire and emit EvictExpired")

	// 3. Peer invalidation received (2 keys).
	if err := store.Set("k2", []byte("vv2"), time.Minute); err != nil {
		t.Fatalf("Set(k2) error = %v", err)
	}
	if err := store.Set("k3", []byte("vv3"), time.Minute); err != nil {
		t.Fatalf("Set(k3) error = %v", err)
	}
	if err := peer.SetMulti(map[string][]byte{"k2": []byte("new2"), "k3": []byte("new3")}, time.Minute); err != nil {
		t.Fatalf("peer.SetMulti() error = %v", err)
	}
	eventually(t, 2*time.Second, 5*time.Millisecond, func() bool {
		_, ok2 := store.Get("k2")
		_, ok3 := store.Get("k3")
		return !ok2 && !ok3
	}, "peer.SetMulti should invalidate k2 and k3 on store")

	// 4. Capacity eviction (4 x 5-byte items into MaxItems=3 -> 1 EvictCapacity of 5 bytes here,
	// plus 1 more when sf-key inserts in step 5 -> 2 total).
	for _, k := range []string{"a1", "a2", "a3", "a4"} {
		if err := store.Set(k, []byte("vv0"), time.Minute); err != nil {
			t.Fatalf("Set(%s) error = %v", k, err)
		}
	}

	// 5. Coalesced GetOrLoad (1 leader + 1 follower) and error load.
	leaderStarted := make(chan struct{})
	followerEntered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, loadErr := store.GetOrLoad(t.Context(), "sf-key", time.Minute, func(context.Context) ([]byte, error) {
			close(leaderStarted)
			<-release
			return []byte("sf-val"), nil
		})
		if loadErr != nil {
			t.Errorf("leader GetOrLoad() error = %v", loadErr)
		}
	}()
	<-leaderStarted
	go func() {
		defer wg.Done()
		close(followerEntered)
		_, loadErr := store.GetOrLoad(t.Context(), "sf-key", time.Minute, func(context.Context) ([]byte, error) {
			return []byte("unexpected-follower-load"), nil
		})
		if loadErr != nil {
			t.Errorf("follower GetOrLoad() error = %v", loadErr)
		}
	}()
	<-followerEntered
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if _, err := store.GetOrLoad(t.Context(), "fail-key", time.Minute, func(context.Context) ([]byte, error) {
		return nil, errors.New("upstream error")
	}); err == nil {
		t.Fatal("GetOrLoad(fail-key) = nil error, want non-nil")
	}

	if problems, err := testutil.CollectAndLint(col); err != nil {
		t.Fatalf("CollectAndLint(col) error = %v", err)
	} else if len(problems) != 0 {
		t.Fatalf("CollectAndLint(col) problems = %+v, want 0", problems)
	}

	snap := store.GetMetrics()
	expected := fmt.Sprintf(`
# HELP memorystore_hits_total Total number of cache hits.
# TYPE memorystore_hits_total counter
memorystore_hits_total{cluster="e2e"} %d
# HELP memorystore_misses_total Total number of cache misses.
# TYPE memorystore_misses_total counter
memorystore_misses_total{cluster="e2e"} %d
# HELP memorystore_evictions_total Total number of cache evictions and deletions.
# TYPE memorystore_evictions_total counter
memorystore_evictions_total{cluster="e2e",reason="capacity"} 2
memorystore_evictions_total{cluster="e2e",reason="deleted"} 3
memorystore_evictions_total{cluster="e2e",reason="expired"} 1
# HELP memorystore_evicted_bytes_total Total payload bytes evicted or deleted from the cache.
# TYPE memorystore_evicted_bytes_total counter
memorystore_evicted_bytes_total{cluster="e2e"} 33
# HELP memorystore_loads_total Total number of cache loader executions.
# TYPE memorystore_loads_total counter
memorystore_loads_total{cluster="e2e",shared="false",status="error"} 1
memorystore_loads_total{cluster="e2e",shared="false",status="ok"} 1
memorystore_loads_total{cluster="e2e",shared="true",status="error"} 0
memorystore_loads_total{cluster="e2e",shared="true",status="ok"} 1
# HELP memorystore_invalidations_total Total number of distributed invalidation keys sent or received.
# TYPE memorystore_invalidations_total counter
memorystore_invalidations_total{cluster="e2e",direction="received"} 2
memorystore_invalidations_total{cluster="e2e",direction="sent"} 10
# HELP memorystore_items Current number of items stored in MemoryStore.
# TYPE memorystore_items gauge
memorystore_items{cluster="e2e"} %d
# HELP memorystore_bytes Current stored key and value payload bytes in MemoryStore.
# TYPE memorystore_bytes gauge
memorystore_bytes{cluster="e2e"} %d
`, snap.Hits, snap.Misses, snap.Items, snap.Bytes)

	if err := testutil.CollectAndCompare(
		reg,
		strings.NewReader(expected),
		"memorystore_hits_total",
		"memorystore_misses_total",
		"memorystore_evictions_total",
		"memorystore_evicted_bytes_total",
		"memorystore_loads_total",
		"memorystore_invalidations_total",
		"memorystore_items",
		"memorystore_bytes",
	); err != nil {
		t.Fatalf("CollectAndCompare() error = %v", err)
	}

	// Verify histogram observations via Gather().
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("reg.Gather() error = %v", err)
	}
	var foundHist bool
	histCounts := make(map[string]uint64)
	for _, mf := range mfs {
		if mf.GetName() != "memorystore_load_duration_seconds" {
			continue
		}
		foundHist = true
		for _, m := range mf.GetMetric() {
			var shared, status string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "shared":
					shared = lp.GetValue()
				case "status":
					status = lp.GetValue()
				}
			}
			histCounts[shared+"/"+status] = m.GetHistogram().GetSampleCount()
		}
	}
	if !foundHist {
		t.Fatal("missing memorystore_load_duration_seconds metric family")
	}
	for _, tc := range []struct {
		key  string
		want uint64
	}{
		{"false/ok", 1},
		{"true/ok", 1},
		{"false/error", 1},
		{"true/error", 0},
	} {
		if got := histCounts[tc.key]; got != tc.want {
			t.Errorf("histogram[%s] sample count = %d, want %d", tc.key, got, tc.want)
		}
	}

	// Multi-store isolation on the same PedanticRegistry + Unregister/Close idempotency.
	col2, err := prommetrics.NewCollector(
		prommetrics.WithRegisterer(reg),
		prommetrics.WithConstLabels(prometheus.Labels{"cluster": "e2e-2"}),
	)
	if err != nil {
		t.Fatalf("NewCollector(col2) error = %v", err)
	}
	col2.RecordMiss()
	if got := testutil.CollectAndCount(reg, "memorystore_misses_total"); got != 2 {
		t.Fatalf("CollectAndCount(misses_total) with 2 collectors = %d, want 2", got)
	}
	if !col2.Unregister() {
		t.Fatal("first col2.Unregister() = false, want true")
	}
	if col2.Unregister() {
		t.Fatal("second col2.Unregister() = true, want false")
	}
	if err := col2.Close(); err != nil {
		t.Fatalf("col2.Close() error = %v", err)
	}
	if got := testutil.CollectAndCount(reg, "memorystore_misses_total"); got != 1 {
		t.Fatalf("CollectAndCount(misses_total) after col2.Close() = %d, want 1", got)
	}
}

func TestTier4_ObservabilityModules_ConcurrentClusterAndGoroutineLeak(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	otelRec, err := otelmetrics.NewRecorder(
		otelmetrics.WithMeterProvider(mp),
		otelmetrics.WithAttributes(attribute.String("node", "node-a")),
	)
	if err != nil {
		t.Fatalf("otel.NewRecorder() error = %v", err)
	}

	promReg := prometheus.NewPedanticRegistry()
	promCol, err := prommetrics.NewCollector(
		prommetrics.WithRegisterer(promReg),
		prommetrics.WithConstLabels(prometheus.Labels{"node": "node-b"}),
	)
	if err != nil {
		t.Fatalf("prometheus.NewCollector() error = %v", err)
	}

	broker := memorystore.NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	nodeA := mustNewStore(t,
		memorystore.WithMetricsRecorder(otelRec),
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("cluster:obs"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithAsyncInvalidation(64),
		memorystore.WithMaxItems(64),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithPreciseClock(),
	)
	if err := otelRec.RegisterStore(nodeA); err != nil {
		t.Fatalf("otelRec.RegisterStore(nodeA) error = %v", err)
	}

	nodeB := mustNewStore(t,
		memorystore.WithMetricsRecorder(promCol),
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("cluster:obs"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithAsyncInvalidation(64),
		memorystore.WithMaxItems(64),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
		memorystore.WithPreciseClock(),
	)
	promCol.SetStore(nodeB)

	const workers = 6
	const iters = 60
	var seq atomic.Int64
	var activeLoaders atomic.Int32
	var wg sync.WaitGroup

	for w := range workers {
		wg.Go(func() {
			for i := range iters {
				key := fmt.Sprintf("item:%d", i%20)
				switch (w + i) % 4 {
				case 0:
					n := seq.Add(1)
					_ = nodeA.Set(key, fmt.Appendf(nil, "val-%d", n), 40*time.Millisecond)
				case 1:
					_, _ = nodeB.Get(key)
				case 2:
					_, _ = nodeA.GetOrLoadWithOptions(
						context.Background(),
						key,
						10*time.Millisecond,
						func(context.Context) ([]byte, error) {
							activeLoaders.Add(1)
							defer activeLoaders.Add(-1)
							n := seq.Add(1)
							return fmt.Appendf(nil, "swr-%d", n), nil
						},
						memorystore.WithStaleWhileRevalidate(50*time.Millisecond),
					)
				case 3:
					_, _ = nodeB.GetOrLoadWithOptions(
						context.Background(),
						key,
						10*time.Millisecond,
						func(context.Context) ([]byte, error) {
							activeLoaders.Add(1)
							defer activeLoaders.Add(-1)
							n := seq.Add(1)
							return fmt.Appendf(nil, "swr-b-%d", n), nil
						},
						memorystore.WithStaleWhileRevalidate(50*time.Millisecond),
					)
				}
				if i%15 == 0 {
					var rm metricdata.ResourceMetrics
					_ = reader.Collect(context.Background(), &rm)
					_, _ = promReg.Gather()
				}
			}
		})
	}
	wg.Wait()

	// Wait for in-flight SWR refreshes and short-TTL windows from the burst to
	// settle, then flush both async WriteThrough queues via long-TTL sentinels
	// before asserting live gauge parity.
	eventually(t, 2*time.Second, 2*time.Millisecond, func() bool {
		return activeLoaders.Load() == 0
	}, "in-flight SWR loaders should finish after workers complete")
	time.Sleep(80 * time.Millisecond)

	if err := nodeA.Set("quiesce-a", []byte("settled-a"), time.Minute); err != nil {
		t.Fatalf("nodeA.Set(quiesce-a) error = %v", err)
	}
	if err := nodeB.Set("quiesce-b", []byte("settled-b"), time.Minute); err != nil {
		t.Fatalf("nodeB.Set(quiesce-b) error = %v", err)
	}
	eventually(t, 2*time.Second, 5*time.Millisecond, func() bool {
		va, okA := nodeB.Get("quiesce-a")
		vb, okB := nodeA.Get("quiesce-b")
		return okA && string(va) == "settled-a" && okB && string(vb) == "settled-b"
	}, "async WriteThrough queues should deliver sentinel keys to peer nodes")

	// Verify live gauge parity on both collectors across a stable, quiesced snapshot before shutdown.
	nodeASet := attribute.NewSet(attribute.String("node", "node-a"))
	eventually(t, 2*time.Second, 5*time.Millisecond, func() bool {
		snapABefore := nodeA.GetMetrics()
		var rmBefore metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &rmBefore); err != nil {
			t.Fatalf("reader.Collect() before Stop error = %v", err)
		}
		snapAAfter := nodeA.GetMetrics()
		if snapABefore.Items == 0 || snapABefore.Bytes == 0 ||
			snapABefore.Items != snapAAfter.Items || snapABefore.Bytes != snapAAfter.Bytes {
			return false
		}
		mItems, okItems := findOTelMetric(&rmBefore, "memorystore.items")
		mBytes, okBytes := findOTelMetric(&rmBefore, "memorystore.bytes")
		if !okItems || !okBytes {
			return false
		}
		if otelGaugeValue(t, mItems, nodeASet) != int64(snapABefore.Items) ||
			otelGaugeValue(t, mBytes, nodeASet) != snapABefore.Bytes {
			return false
		}

		snapBBefore := nodeB.GetMetrics()
		promItems := testutil.ToFloat64(&promFilterCollector{inner: promCol, wantFQName: "memorystore_items"})
		promBytes := testutil.ToFloat64(&promFilterCollector{inner: promCol, wantFQName: "memorystore_bytes"})
		snapBAfter := nodeB.GetMetrics()
		if snapBBefore.Items == 0 || snapBBefore.Bytes == 0 ||
			snapBBefore.Items != snapBAfter.Items || snapBBefore.Bytes != snapBAfter.Bytes {
			return false
		}
		return promItems == float64(snapBBefore.Items) && promBytes == float64(snapBBefore.Bytes)
	}, "live OTel and Prometheus gauges should match settled nodeA/nodeB GetMetrics()")

	if err := nodeA.Stop(); err != nil {
		t.Fatalf("nodeA.Stop() error = %v", err)
	}
	if err := nodeB.Stop(); err != nil {
		t.Fatalf("nodeB.Stop() error = %v", err)
	}

	// Post-Stop gauges must read 0.
	var rmAfterStop metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rmAfterStop); err != nil {
		t.Fatalf("reader.Collect() after Stop error = %v", err)
	}
	if m, ok := findOTelMetric(&rmAfterStop, "memorystore.items"); !ok || otelGaugeValue(t, m, nodeASet) != 0 {
		t.Error("OTel memorystore.items != 0 after nodeA.Stop()")
	}
	if got := testutil.ToFloat64(&promFilterCollector{inner: promCol, wantFQName: "memorystore_items"}); got != 0 {
		t.Errorf("Prometheus memorystore_items = %v after nodeB.Stop(), want 0", got)
	}

	if err := otelRec.Close(); err != nil {
		t.Fatalf("otelRec.Close() error = %v", err)
	}
	if err := promCol.Close(); err != nil {
		t.Fatalf("promCol.Close() error = %v", err)
	}
	if err := mp.Shutdown(t.Context()); err != nil {
		t.Fatalf("mp.Shutdown() error = %v", err)
	}

	if testutil.CollectAndCount(promReg) != 0 {
		t.Errorf("CollectAndCount(promReg) after Close = %d, want 0", testutil.CollectAndCount(promReg))
	}

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		report := buf.String()
		for _, pkg := range []string{
			"github.com/BryceWayne/MemoryStore/v2/memorystore",
			"github.com/BryceWayne/MemoryStore/metrics/otel",
			"github.com/BryceWayne/MemoryStore/metrics/prometheus",
		} {
			if strings.Contains(report, pkg) {
				t.Errorf("detected leaked goroutine in %s:\n%s", pkg, report)
			}
		}
	}
}

type promFilterCollector struct {
	inner      prometheus.Collector
	wantFQName string
}

func (f *promFilterCollector) Describe(ch chan<- *prometheus.Desc) {
	f.inner.Describe(ch)
}

func (f *promFilterCollector) Collect(ch chan<- prometheus.Metric) {
	raw := make(chan prometheus.Metric, 32)
	go func() {
		f.inner.Collect(raw)
		close(raw)
	}()
	for m := range raw {
		if strings.Contains(m.Desc().String(), `fqName: "`+f.wantFQName+`"`) {
			ch <- m
		}
	}
}
