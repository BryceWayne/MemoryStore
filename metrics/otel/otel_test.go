package otel_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/metrics/otel"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func findMetric(rm *metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func sumDataPointValue(t *testing.T, m metricdata.Metrics, want attribute.Set) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %s has data type %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	t.Fatalf("metric %s missing data point with attributes %v", m.Name, want)
	return 0
}

func gaugeDataPointValue(t *testing.T, m metricdata.Metrics, want attribute.Set) int64 {
	t.Helper()
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %s has data type %T, want metricdata.Gauge[int64]", m.Name, m.Data)
	}
	for _, dp := range gauge.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	t.Fatalf("metric %s missing gauge data point with attributes %v", m.Name, want)
	return 0
}

func histogramDataPoint(t *testing.T, m metricdata.Metrics, want attribute.Set) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %s has data type %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, dp := range hist.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp
		}
	}
	t.Fatalf("metric %s missing histogram data point with attributes %v", m.Name, want)
	return metricdata.HistogramDataPoint[float64]{}
}

func TestNewRecorder_ValidationAndOptions(t *testing.T) {
	t.Run("rejects nil and invalid options", func(t *testing.T) {
		var typedNilMP *sdkmetric.MeterProvider
		var typedNilMeter metric.Meter

		cases := []struct {
			name string
			opt  otel.Option
		}{
			{name: "untyped nil meter provider", opt: otel.WithMeterProvider(nil)},
			{name: "typed nil meter provider", opt: otel.WithMeterProvider(typedNilMP)},
			{name: "untyped nil meter", opt: otel.WithMeter(nil)},
			{name: "typed nil meter", opt: otel.WithMeter(typedNilMeter)},
			{name: "nil store", opt: otel.WithStore(nil)},
			{name: "empty buckets", opt: otel.WithLoadDurationBuckets()},
			{name: "negative bucket", opt: otel.WithLoadDurationBuckets(-1, 1)},
			{name: "NaN bucket", opt: otel.WithLoadDurationBuckets(math.NaN(), 1)},
			{name: "Inf bucket", opt: otel.WithLoadDurationBuckets(1, math.Inf(1))},
			{name: "equal buckets", opt: otel.WithLoadDurationBuckets(1, 1)},
			{name: "decreasing buckets", opt: otel.WithLoadDurationBuckets(5, 1)},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := otel.NewRecorder(tc.opt); err == nil {
					t.Fatalf("NewRecorder(%s) = nil error, want non-nil", tc.name)
				}
			})
		}
	})

	t.Run("skips nil options and honors WithMeter precedence", func(t *testing.T) {
		readerIgnored := sdkmetric.NewManualReader()
		mpIgnored := sdkmetric.NewMeterProvider(sdkmetric.WithReader(readerIgnored))
		defer func() { _ = mpIgnored.Shutdown(t.Context()) }()

		readerUsed := sdkmetric.NewManualReader()
		mpUsed := sdkmetric.NewMeterProvider(sdkmetric.WithReader(readerUsed))
		defer func() { _ = mpUsed.Shutdown(t.Context()) }()

		customMeter := mpUsed.Meter("custom.meter")
		rec, err := otel.NewRecorder(
			nil,
			otel.WithMeterProvider(mpIgnored),
			otel.WithMeter(customMeter),
			otel.WithLoadDurationBuckets(0, 1, 10),
		)
		if err != nil {
			t.Fatalf("NewRecorder() error = %v", err)
		}
		defer func() { _ = rec.Close() }()

		rec.RecordHit()

		var rmIgnored, rmUsed metricdata.ResourceMetrics
		if err := readerIgnored.Collect(t.Context(), &rmIgnored); err != nil {
			t.Fatalf("readerIgnored.Collect() error = %v", err)
		}
		if len(rmIgnored.ScopeMetrics) != 0 {
			t.Fatalf("expected ignored MeterProvider to have 0 scope metrics, got %d", len(rmIgnored.ScopeMetrics))
		}

		if err := readerUsed.Collect(t.Context(), &rmUsed); err != nil {
			t.Fatalf("readerUsed.Collect() error = %v", err)
		}
		if len(rmUsed.ScopeMetrics) != 1 || rmUsed.ScopeMetrics[0].Scope.Name != "custom.meter" {
			t.Fatalf("unexpected ScopeMetrics: %+v", rmUsed.ScopeMetrics)
		}
	})

	t.Run("default global provider fallback", func(t *testing.T) {
		rec, err := otel.NewRecorder()
		if err != nil {
			t.Fatalf("NewRecorder() error = %v", err)
		}
		rec.RecordHit()
		if err := rec.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	t.Run("RegisterStore and Close lifecycle", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		defer func() { _ = mp.Shutdown(t.Context()) }()

		store, err := memorystore.New()
		if err != nil {
			t.Fatalf("memorystore.New() error = %v", err)
		}
		defer func() { _ = store.Stop() }()

		rec, err := otel.NewRecorder(otel.WithMeterProvider(mp))
		if err != nil {
			t.Fatalf("NewRecorder() error = %v", err)
		}

		if err := rec.RegisterStore(nil); err == nil {
			t.Fatal("RegisterStore(nil) = nil, want error")
		}
		if err := rec.RegisterStore(store); err != nil {
			t.Fatalf("RegisterStore(store) error = %v", err)
		}
		if err := rec.RegisterStore(store); !errors.Is(err, otel.ErrStoreAlreadyRegistered) {
			t.Fatalf("second RegisterStore error = %v, want ErrStoreAlreadyRegistered", err)
		}

		if err := rec.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		if err := rec.Close(); err != nil {
			t.Fatalf("second Close() error = %v", err)
		}
		if err := rec.RegisterStore(store); !errors.Is(err, otel.ErrRecorderClosed) {
			t.Fatalf("RegisterStore after Close error = %v, want ErrRecorderClosed", err)
		}

		// WithStore followed by RegisterStore also returns ErrStoreAlreadyRegistered.
		rec2, err := otel.NewRecorder(otel.WithMeterProvider(mp), otel.WithStore(store))
		if err != nil {
			t.Fatalf("NewRecorder(WithStore) error = %v", err)
		}
		defer func() { _ = rec2.Close() }()
		if err := rec2.RegisterStore(store); !errors.Is(err, otel.ErrStoreAlreadyRegistered) {
			t.Fatalf("RegisterStore after WithStore error = %v, want ErrStoreAlreadyRegistered", err)
		}
	})
}

func TestRecorder_DirectMetrics_ManualReader(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(t.Context()) }()

	baseAttr := attribute.String("cache", "users")
	rec, err := otel.NewRecorder(
		otel.WithMeterProvider(mp),
		otel.WithAttributes(baseAttr, attribute.String("reason", "must_be_overridden")),
		otel.WithLoadDurationBuckets(1, 5, 20),
	)
	if err != nil {
		t.Fatalf("NewRecorder() error = %v", err)
	}
	defer func() { _ = rec.Close() }()

	rec.RecordHit()
	rec.RecordHit()
	rec.RecordMiss()

	rec.RecordEvict(memorystore.EvictExpired, 128)
	rec.RecordEvict(memorystore.EvictCapacity, 256)
	rec.RecordEvict(memorystore.EvictDeleted, 0)
	rec.RecordEvict(memorystore.EvictReason(0), -10)
	rec.RecordEvict(memorystore.EvictReason(99), 32)

	errBoom := errors.New("boom")
	rec.RecordLoad(2*time.Millisecond, false, nil)
	rec.RecordLoad(10*time.Millisecond, true, nil)
	rec.RecordLoad(3*time.Millisecond, false, errBoom)
	rec.RecordLoad(-5*time.Millisecond, true, errBoom)

	rec.RecordInvalidation(true, 3)
	rec.RecordInvalidation(false, 2)
	rec.RecordInvalidation(true, 0)
	rec.RecordInvalidation(false, -1)

	rec.RecordInvalidationDrop(5)
	rec.RecordInvalidationDrop(2)
	rec.RecordInvalidationDrop(0)
	rec.RecordInvalidationDrop(-3)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("reader.Collect() error = %v", err)
	}

	baseSet := attribute.NewSet(baseAttr, attribute.String("reason", "must_be_overridden"))

	hits, ok := findMetric(&rm, "memorystore.hits")
	if !ok {
		t.Fatal("missing memorystore.hits")
	}
	if got := sumDataPointValue(t, hits, baseSet); got != 2 {
		t.Errorf("memorystore.hits = %d, want 2", got)
	}

	misses, ok := findMetric(&rm, "memorystore.misses")
	if !ok {
		t.Fatal("missing memorystore.misses")
	}
	if got := sumDataPointValue(t, misses, baseSet); got != 1 {
		t.Errorf("memorystore.misses = %d, want 1", got)
	}

	evictions, ok := findMetric(&rm, "memorystore.evictions")
	if !ok {
		t.Fatal("missing memorystore.evictions")
	}
	if got := sumDataPointValue(t, evictions, attribute.NewSet(baseAttr, attribute.String("reason", "expired"))); got != 1 {
		t.Errorf("evictions[expired] = %d, want 1", got)
	}
	if got := sumDataPointValue(t, evictions, attribute.NewSet(baseAttr, attribute.String("reason", "capacity"))); got != 1 {
		t.Errorf("evictions[capacity] = %d, want 1", got)
	}
	if got := sumDataPointValue(t, evictions, attribute.NewSet(baseAttr, attribute.String("reason", "deleted"))); got != 1 {
		t.Errorf("evictions[deleted] = %d, want 1", got)
	}
	if got := sumDataPointValue(t, evictions, attribute.NewSet(baseAttr, attribute.String("reason", "unknown"))); got != 2 {
		t.Errorf("evictions[unknown] = %d, want 2", got)
	}

	evictedBytes, ok := findMetric(&rm, "memorystore.evicted_bytes")
	if !ok {
		t.Fatal("missing memorystore.evicted_bytes")
	}
	if got := sumDataPointValue(t, evictedBytes, attribute.NewSet(baseAttr, attribute.String("reason", "expired"))); got != 128 {
		t.Errorf("evicted_bytes[expired] = %d, want 128", got)
	}
	if got := sumDataPointValue(t, evictedBytes, attribute.NewSet(baseAttr, attribute.String("reason", "capacity"))); got != 256 {
		t.Errorf("evicted_bytes[capacity] = %d, want 256", got)
	}
	if got := sumDataPointValue(t, evictedBytes, attribute.NewSet(baseAttr, attribute.String("reason", "unknown"))); got != 32 {
		t.Errorf("evicted_bytes[unknown] = %d, want 32", got)
	}

	loads, ok := findMetric(&rm, "memorystore.loads")
	if !ok {
		t.Fatal("missing memorystore.loads")
	}
	loadHist, ok := findMetric(&rm, "memorystore.load_duration_ms")
	if !ok {
		t.Fatal("missing memorystore.load_duration_ms")
	}

	loadCases := []struct {
		shared  bool
		status  string
		wantSum float64
	}{
		{shared: false, status: "ok", wantSum: 2.0},
		{shared: true, status: "ok", wantSum: 10.0},
		{shared: false, status: "error", wantSum: 3.0},
		{shared: true, status: "error", wantSum: 0.0},
	}
	for _, tc := range loadCases {
		set := attribute.NewSet(baseAttr, attribute.String("reason", "must_be_overridden"), attribute.Bool("shared", tc.shared), attribute.String("status", tc.status))
		if got := sumDataPointValue(t, loads, set); got != 1 {
			t.Errorf("loads[shared=%v,status=%s] = %d, want 1", tc.shared, tc.status, got)
		}
		dp := histogramDataPoint(t, loadHist, set)
		if dp.Count != 1 || math.Abs(dp.Sum-tc.wantSum) > 1e-6 {
			t.Errorf("load_duration_ms[shared=%v,status=%s] = (count=%d, sum=%v), want (1, %v)", tc.shared, tc.status, dp.Count, dp.Sum, tc.wantSum)
		}
	}

	invalidations, ok := findMetric(&rm, "memorystore.invalidations")
	if !ok {
		t.Fatal("missing memorystore.invalidations")
	}
	if got := sumDataPointValue(t, invalidations, attribute.NewSet(baseAttr, attribute.String("reason", "must_be_overridden"), attribute.String("direction", "sent"))); got != 3 {
		t.Errorf("invalidations[sent] = %d, want 3", got)
	}
	if got := sumDataPointValue(t, invalidations, attribute.NewSet(baseAttr, attribute.String("reason", "must_be_overridden"), attribute.String("direction", "received"))); got != 2 {
		t.Errorf("invalidations[received] = %d, want 2", got)
	}

	invalidationDrops, ok := findMetric(&rm, "memorystore.invalidation_drops")
	if !ok {
		t.Fatal("missing memorystore.invalidation_drops")
	}
	if got := sumDataPointValue(t, invalidationDrops, baseSet); got != 7 {
		t.Errorf("memorystore.invalidation_drops = %d, want 7", got)
	}
}

type stalledOTelPubSub struct {
	once       sync.Once
	pubStarted chan struct{}
}

func (s *stalledOTelPubSub) Subscribe(ctx context.Context, _ string) (<-chan []byte, error) {
	ch := make(chan []byte)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (s *stalledOTelPubSub) Publish(ctx context.Context, _ string, _ []byte) error {
	s.once.Do(func() {
		close(s.pubStarted)
	})
	<-ctx.Done()
	return ctx.Err()
}

func (s *stalledOTelPubSub) Unsubscribe(context.Context, string) error { return nil }
func (s *stalledOTelPubSub) Close() error                              { return nil }

func TestRecorder_AsyncInvalidationDropsIntegration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(t.Context()) }()

	baseAttr := attribute.String("node", "drop-test")
	rec, err := otel.NewRecorder(
		otel.WithMeterProvider(mp),
		otel.WithAttributes(baseAttr),
	)
	if err != nil {
		t.Fatalf("NewRecorder() error = %v", err)
	}
	defer func() { _ = rec.Close() }()

	ps := &stalledOTelPubSub{pubStarted: make(chan struct{})}
	store, err := memorystore.New(
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("inv:otel:drop"),
		memorystore.WithAsyncInvalidation(1),
		memorystore.WithPubSubTimeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("memorystore.New() error = %v", err)
	}
	defer func() { _ = store.Stop() }()

	if err := store.Set("seed", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set(seed) error = %v", err)
	}
	<-ps.pubStarted

	for range 20 {
		if err := store.Set("hot", []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set(hot) error = %v", err)
		}
	}

	wantDrops := store.GetMetrics().InvalidationDrops
	if wantDrops == 0 {
		t.Fatal("expected InvalidationDrops > 0 when async queue depth is 1 and Publish is stalled")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("reader.Collect() error = %v", err)
	}
	dropsMetric, ok := findMetric(&rm, "memorystore.invalidation_drops")
	if !ok {
		t.Fatal("missing memorystore.invalidation_drops")
	}
	if got := sumDataPointValue(t, dropsMetric, attribute.NewSet(baseAttr)); got != wantDrops {
		t.Errorf("memorystore.invalidation_drops = %d, want %d", got, wantDrops)
	}
}

func TestRecorder_MemoryStoreIntegrationAndGauges(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(t.Context()) }()

	rec, err := otel.NewRecorder(
		otel.WithMeterProvider(mp),
		otel.WithAttributes(attribute.String("node", "primary")),
	)
	if err != nil {
		t.Fatalf("NewRecorder() error = %v", err)
	}
	defer func() { _ = rec.Close() }()

	broker := memorystore.NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	store, err := memorystore.New(
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("inv:otel"),
		memorystore.WithMaxItems(3),
	)
	if err != nil {
		t.Fatalf("memorystore.New(store) error = %v", err)
	}
	defer func() { _ = store.Stop() }()

	if err := rec.RegisterStore(store); err != nil {
		t.Fatalf("RegisterStore() error = %v", err)
	}

	peer, err := memorystore.New(
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("inv:otel"),
	)
	if err != nil {
		t.Fatalf("memorystore.New(peer) error = %v", err)
	}
	defer func() { _ = peer.Stop() }()

	// Hits & misses
	if _, ok := store.Get("missing"); ok {
		t.Fatal("expected miss on missing key")
	}
	if err := store.Set("k1", []byte("v1"), time.Minute); err != nil {
		t.Fatalf("Set(k1) error = %v", err)
	}
	if got, ok := store.Get("k1"); !ok || string(got) != "v1" {
		t.Fatalf("Get(k1) = (%q, %v), want (v1, true)", got, ok)
	}

	// Capacity eviction (MaxItems=3)
	for _, k := range []string{"k2", "k3", "k4"} {
		if err := store.Set(k, []byte("val"), time.Minute); err != nil {
			t.Fatalf("Set(%s) error = %v", k, err)
		}
	}

	// Delete -> EvictDeleted + invalidation sent
	if err := store.Delete("k4"); err != nil {
		t.Fatalf("Delete(k4) error = %v", err)
	}

	// Peer invalidation -> invalidation received
	if err := peer.SetMulti(map[string][]byte{"k2": []byte("new2"), "k3": []byte("new3")}, time.Minute); err != nil {
		t.Fatalf("peer.SetMulti() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store.Get("k2"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// GetOrLoad leader + coalesced waiter + error
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			_, _ = store.GetOrLoad(t.Context(), "loaded", time.Minute, func(context.Context) ([]byte, error) {
				select {
				case <-started:
				default:
					close(started)
				}
				<-release
				return []byte("loaded-val"), nil
			})
		}()
	}
	<-started
	time.Sleep(15 * time.Millisecond)
	close(release)
	wg.Wait()

	_, _ = store.GetOrLoad(t.Context(), "fail", time.Minute, func(context.Context) ([]byte, error) {
		return nil, errors.New("load failed")
	})

	// Collect metrics and validate gauges match store.GetMetrics()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("reader.Collect() error = %v", err)
	}

	baseSet := attribute.NewSet(attribute.String("node", "primary"))
	wantMetrics := store.GetMetrics()

	itemsMetric, ok := findMetric(&rm, "memorystore.items")
	if !ok {
		t.Fatal("missing memorystore.items gauge")
	}
	if got := gaugeDataPointValue(t, itemsMetric, baseSet); got != int64(wantMetrics.Items) {
		t.Errorf("memorystore.items = %d, want %d", got, wantMetrics.Items)
	}

	bytesMetric, ok := findMetric(&rm, "memorystore.bytes")
	if !ok {
		t.Fatal("missing memorystore.bytes gauge")
	}
	if got := gaugeDataPointValue(t, bytesMetric, baseSet); got != wantMetrics.Bytes {
		t.Errorf("memorystore.bytes = %d, want %d", got, wantMetrics.Bytes)
	}

	loadsMetric, ok := findMetric(&rm, "memorystore.loads")
	if !ok {
		t.Fatal("missing memorystore.loads")
	}
	if got := sumDataPointValue(t, loadsMetric, attribute.NewSet(attribute.String("node", "primary"), attribute.Bool("shared", false), attribute.String("status", "ok"))); got < 1 {
		t.Errorf("loads[shared=false,status=ok] = %d, want >= 1", got)
	}
	if got := sumDataPointValue(t, loadsMetric, attribute.NewSet(attribute.String("node", "primary"), attribute.Bool("shared", true), attribute.String("status", "ok"))); got < 1 {
		t.Errorf("loads[shared=true,status=ok] = %d, want >= 1", got)
	}
	if got := sumDataPointValue(t, loadsMetric, attribute.NewSet(attribute.String("node", "primary"), attribute.Bool("shared", false), attribute.String("status", "error"))); got != 1 {
		t.Errorf("loads[shared=false,status=error] = %d, want 1", got)
	}

	invMetric, ok := findMetric(&rm, "memorystore.invalidations")
	if !ok {
		t.Fatal("missing memorystore.invalidations")
	}
	if got := sumDataPointValue(t, invMetric, attribute.NewSet(attribute.String("node", "primary"), attribute.String("direction", "sent"))); got < 1 {
		t.Errorf("invalidations[sent] = %d, want >= 1", got)
	}
	if got := sumDataPointValue(t, invMetric, attribute.NewSet(attribute.String("node", "primary"), attribute.String("direction", "received"))); got < 2 {
		t.Errorf("invalidations[received] = %d, want >= 2", got)
	}

	// After store.Stop(), gauges report 0 items and 0 bytes.
	if err := store.Stop(); err != nil {
		t.Fatalf("store.Stop() error = %v", err)
	}
	var rmAfterStop metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rmAfterStop); err != nil {
		t.Fatalf("reader.Collect() after Stop error = %v", err)
	}
	if m, ok := findMetric(&rmAfterStop, "memorystore.items"); !ok || gaugeDataPointValue(t, m, baseSet) != 0 {
		t.Errorf("expected memorystore.items == 0 after Stop")
	}
	if m, ok := findMetric(&rmAfterStop, "memorystore.bytes"); !ok || gaugeDataPointValue(t, m, baseSet) != 0 {
		t.Errorf("expected memorystore.bytes == 0 after Stop")
	}

	// After rec.Close(), observable gauges are unregistered and no longer emitted.
	if err := rec.Close(); err != nil {
		t.Fatalf("rec.Close() error = %v", err)
	}
	var rmAfterClose metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rmAfterClose); err != nil {
		t.Fatalf("reader.Collect() after Close error = %v", err)
	}
	if _, ok := findMetric(&rmAfterClose, "memorystore.items"); ok {
		t.Error("memorystore.items still emitted after Recorder.Close()")
	}
	if _, ok := findMetric(&rmAfterClose, "memorystore.bytes"); ok {
		t.Error("memorystore.bytes still emitted after Recorder.Close()")
	}
}

func TestRecorder_ZeroHotPathAllocations(t *testing.T) {
	mp := noop.NewMeterProvider()
	rec, err := otel.NewRecorder(
		otel.WithMeterProvider(mp),
		otel.WithAttributes(attribute.String("env", "test")),
	)
	if err != nil {
		t.Fatalf("NewRecorder() error = %v", err)
	}
	defer func() { _ = rec.Close() }()

	errSample := errors.New("sample")

	checks := []struct {
		name string
		fn   func()
	}{
		{name: "RecordHit", fn: func() { rec.RecordHit() }},
		{name: "RecordMiss", fn: func() { rec.RecordMiss() }},
		{name: "RecordEvict", fn: func() { rec.RecordEvict(memorystore.EvictCapacity, 64) }},
		{name: "RecordLoad_OK", fn: func() { rec.RecordLoad(time.Millisecond, false, nil) }},
		{name: "RecordLoad_ErrShared", fn: func() { rec.RecordLoad(time.Millisecond, true, errSample) }},
		{name: "RecordInvalidation", fn: func() { rec.RecordInvalidation(true, 4) }},
		{name: "RecordInvalidationDrop", fn: func() { rec.RecordInvalidationDrop(4) }},
	}

	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, tc.fn)
			if allocs != 0 {
				t.Errorf("%s allocated %v times per call, want 0", tc.name, allocs)
			}
		})
	}
}

func TestRecorder_ConcurrentRaceAndGoroutineLeak(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(t.Context()) }()

	rec, err := otel.NewRecorder(
		otel.WithMeterProvider(mp),
		otel.WithAttributes(attribute.String("suite", "race")),
	)
	if err != nil {
		t.Fatalf("NewRecorder() error = %v", err)
	}

	store, err := memorystore.New(
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithMaxItems(32),
	)
	if err != nil {
		t.Fatalf("memorystore.New() error = %v", err)
	}
	if err := rec.RegisterStore(store); err != nil {
		t.Fatalf("RegisterStore() error = %v", err)
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 50 {
				key := "k" + string(rune('a'+(i%16)))
				_ = store.Set(key, []byte("payload"), 50*time.Millisecond)
				_, _ = store.Get(key)
				if i%10 == 0 {
					_ = store.Delete(key)
				}
				if i%15 == 0 {
					_, _ = store.GetOrLoad(context.Background(), "shared", 50*time.Millisecond, func(context.Context) ([]byte, error) {
						return []byte("v"), nil
					})
				}
				if id == 0 && i%10 == 0 {
					var rm metricdata.ResourceMetrics
					_ = reader.Collect(context.Background(), &rm)
				}
			}
		}(g)
	}

	// Concurrently close the recorder mid-flight to verify Close vs Collect/Record safety.
	time.Sleep(2 * time.Millisecond)
	if err := rec.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	wg.Wait()

	if err := store.Stop(); err != nil {
		t.Fatalf("store.Stop() error = %v", err)
	}
	if err := mp.Shutdown(t.Context()); err != nil {
		t.Fatalf("mp.Shutdown() error = %v", err)
	}

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/metrics/otel") {
			t.Errorf("detected leaked goroutine in metrics/otel:\n%s", buf.String())
		}
	}
}
