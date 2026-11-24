package prometheus_test

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

	prommetrics "github.com/BryceWayne/MemoryStore/metrics/prometheus"
	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewCollector_RegistrationAndLifecycle(t *testing.T) {
	t.Run("default registerer and AlreadyRegisteredError", func(t *testing.T) {
		uniqueNS := "memorystore_default_reg_test"
		c1, err := prommetrics.NewCollector(prommetrics.WithNamespace(uniqueNS))
		if err != nil {
			t.Fatalf("NewCollector(c1) error = %v", err)
		}
		defer func() { _ = c1.Close() }()

		if _, err := prommetrics.NewCollector(prommetrics.WithNamespace(uniqueNS)); err == nil {
			t.Fatal("expected AlreadyRegisteredError on duplicate DefaultRegisterer registration")
		} else {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				t.Fatalf("duplicate registration error = %T (%v), want AlreadyRegisteredError", err, err)
			}
		}

		if !c1.Unregister() {
			t.Fatal("first Unregister() = false, want true")
		}
		if c1.Unregister() {
			t.Fatal("second Unregister() = true, want false")
		}
		if err := c1.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		c2, err := prommetrics.NewCollector(prommetrics.WithNamespace(uniqueNS))
		if err != nil {
			t.Fatalf("re-registration after Unregister() error = %v", err)
		}
		_ = c2.Close()
	})

	t.Run("WithRegisterer nil and typed nil skip registration", func(t *testing.T) {
		var typedNil *prometheus.Registry

		for _, tc := range []struct {
			name string
			reg  prometheus.Registerer
		}{
			{name: "untyped nil", reg: nil},
			{name: "typed nil", reg: typedNil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, err := prommetrics.NewCollector(
					nil,
					prommetrics.WithRegisterer(tc.reg),
				)
				if err != nil {
					t.Fatalf("NewCollector() error = %v", err)
				}
				if c.Unregister() {
					t.Fatal("Unregister() with nil registerer = true, want false")
				}
				if err := c.Close(); err != nil {
					t.Fatalf("Close() error = %v", err)
				}

				ped := prometheus.NewPedanticRegistry()
				if err := ped.Register(c); err != nil {
					t.Fatalf("manual Register() error = %v", err)
				}
				if _, err := ped.Gather(); err != nil {
					t.Fatalf("Gather() error = %v", err)
				}
			})
		}
	})
}

func TestNewCollector_OptionsAndValidation(t *testing.T) {
	t.Run("validation errors including with nil registerer", func(t *testing.T) {
		cases := []struct {
			name string
			opts []prommetrics.Option
		}{
			{name: "nil store", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithStore(nil)}},
			{name: "empty buckets", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithDurationBuckets(nil)}},
			{name: "zero bucket", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithDurationBuckets([]float64{0, 1})}},
			{name: "negative bucket", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithDurationBuckets([]float64{-0.1, 1})}},
			{name: "NaN bucket", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithDurationBuckets([]float64{math.NaN(), 1})}},
			{name: "Inf bucket", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithDurationBuckets([]float64{1, math.Inf(1)})}},
			{name: "non-increasing buckets", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithDurationBuckets([]float64{1, 1})}},
			{name: "reserved le", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"le": "1"})}},
			{name: "reserved reason", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"reason": "x"})}},
			{name: "reserved shared", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"shared": "x"})}},
			{name: "reserved status", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"status": "x"})}},
			{name: "reserved direction", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"direction": "x"})}},
			{name: "empty const label", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"": "x"})}},
			{name: "invalid const label", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"bad-key": "x"})}},
			{name: "internal __ const label", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"__name__": "x"})}},
			{name: "invalid UTF-8 const label value", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithConstLabels(prometheus.Labels{"env": "\xff"})}},
			{name: "invalid namespace", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithNamespace("bad-ns")}},
			{name: "invalid subsystem", opts: []prommetrics.Option{prommetrics.WithRegisterer(nil), prommetrics.WithSubsystem("bad-sub")}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := prommetrics.NewCollector(tc.opts...); err == nil {
					t.Fatalf("NewCollector(%s) = nil error, want error", tc.name)
				}
			})
		}
	})

	t.Run("multi-store isolation with ConstLabels and PedanticRegistry", func(t *testing.T) {
		reg := prometheus.NewPedanticRegistry()

		c1, err := prommetrics.NewCollector(
			prommetrics.WithRegisterer(reg),
			prommetrics.WithNamespace("svc"),
			prommetrics.WithSubsystem("cache"),
			prommetrics.WithConstLabels(prometheus.Labels{"store": "users"}),
			prommetrics.WithDurationBuckets([]float64{0.001, 0.01, 0.1}),
		)
		if err != nil {
			t.Fatalf("NewCollector(c1) error = %v", err)
		}
		defer func() { _ = c1.Close() }()

		c2, err := prommetrics.NewCollector(
			prommetrics.WithRegisterer(reg),
			prommetrics.WithNamespace("svc"),
			prommetrics.WithSubsystem("cache"),
			prommetrics.WithConstLabels(prometheus.Labels{"store": "sessions"}),
			prommetrics.WithDurationBuckets([]float64{0.001, 0.01, 0.1}),
		)
		if err != nil {
			t.Fatalf("NewCollector(c2) error = %v", err)
		}
		defer func() { _ = c2.Close() }()

		// Before SetStore, items/bytes are omitted yet pedantic registry Gather succeeds.
		// 1+1+3+1+4+4+2+1 = 17 series per collector = 34 total.
		if got := testutil.CollectAndCount(reg); got != 34 {
			t.Fatalf("CollectAndCount(reg) before SetStore = %d, want 34", got)
		}

		s1, err := memorystore.New(memorystore.WithMetricsRecorder(c1))
		if err != nil {
			t.Fatalf("memorystore.New(s1) error = %v", err)
		}
		defer func() { _ = s1.Stop() }()
		c1.SetStore(s1)

		if err := s1.Set("u1", []byte("alice"), time.Minute); err != nil {
			t.Fatalf("s1.Set() error = %v", err)
		}
		_, _ = s1.Get("u1")
		c2.RecordMiss()

		// Now c1 emits +2 gauges -> 36 series.
		if got := testutil.CollectAndCount(reg); got != 36 {
			t.Fatalf("CollectAndCount(reg) after SetStore = %d, want 36", got)
		}

		// CollectAndLint checks naming, help, units, and types.
		if problems, err := testutil.CollectAndLint(c1); err != nil {
			t.Fatalf("CollectAndLint(c1) error = %v", err)
		} else if len(problems) != 0 {
			t.Fatalf("CollectAndLint(c1) problems = %+v, want 0", problems)
		}
	})

	t.Run("WithStore option attaches store immediately", func(t *testing.T) {
		reg := prometheus.NewPedanticRegistry()
		store, err := memorystore.New()
		if err != nil {
			t.Fatalf("memorystore.New() error = %v", err)
		}
		defer func() { _ = store.Stop() }()
		if err := store.Set("k", []byte("val"), time.Minute); err != nil {
			t.Fatalf("store.Set() error = %v", err)
		}

		c, err := prommetrics.NewCollector(
			prommetrics.WithRegisterer(reg),
			prommetrics.WithStore(store),
		)
		if err != nil {
			t.Fatalf("NewCollector(WithStore) error = %v", err)
		}
		defer func() { _ = c.Close() }()

		expected := `
# HELP memorystore_items Current number of items stored in MemoryStore.
# TYPE memorystore_items gauge
memorystore_items 1
# HELP memorystore_bytes Current stored key and value payload bytes in MemoryStore.
# TYPE memorystore_bytes gauge
memorystore_bytes 4
`
		if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "memorystore_items", "memorystore_bytes"); err != nil {
			t.Fatalf("CollectAndCompare() error = %v", err)
		}
	})
}

func TestCollector_DefensiveGuardsAndDirectMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c, err := prommetrics.NewCollector(
		prommetrics.WithRegisterer(reg),
		prommetrics.WithDurationBuckets([]float64{0.005, 0.05, 0.5}),
	)
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	c.RecordHit()
	c.RecordHit()
	c.RecordMiss()

	c.RecordEvict(memorystore.EvictExpired, 100)
	c.RecordEvict(memorystore.EvictCapacity, 0)
	c.RecordEvict(memorystore.EvictDeleted, -100)
	c.RecordEvict(memorystore.EvictReason(99), 25)

	c.RecordLoad(-time.Millisecond, false, nil)
	c.RecordLoad(20*time.Millisecond, true, errors.New("err"))

	c.RecordInvalidation(true, 0)
	c.RecordInvalidation(true, -5)
	c.RecordInvalidation(true, 3)
	c.RecordInvalidation(false, 4)

	c.RecordInvalidationDrop(0)
	c.RecordInvalidationDrop(-2)
	c.RecordInvalidationDrop(5)
	c.RecordInvalidationDrop(3)

	expected := `
# HELP memorystore_hits_total Total number of cache hits.
# TYPE memorystore_hits_total counter
memorystore_hits_total 2
# HELP memorystore_misses_total Total number of cache misses.
# TYPE memorystore_misses_total counter
memorystore_misses_total 1
# HELP memorystore_evictions_total Total number of cache evictions and deletions.
# TYPE memorystore_evictions_total counter
memorystore_evictions_total{reason="capacity"} 1
memorystore_evictions_total{reason="deleted"} 1
memorystore_evictions_total{reason="expired"} 1
memorystore_evictions_total{reason="unknown"} 1
# HELP memorystore_evicted_bytes_total Total payload bytes evicted or deleted from the cache.
# TYPE memorystore_evicted_bytes_total counter
memorystore_evicted_bytes_total 125
# HELP memorystore_loads_total Total number of cache loader executions.
# TYPE memorystore_loads_total counter
memorystore_loads_total{shared="false",status="error"} 0
memorystore_loads_total{shared="false",status="ok"} 1
memorystore_loads_total{shared="true",status="error"} 1
memorystore_loads_total{shared="true",status="ok"} 0
# HELP memorystore_invalidations_total Total number of distributed invalidation keys sent or received.
# TYPE memorystore_invalidations_total counter
memorystore_invalidations_total{direction="received"} 4
memorystore_invalidations_total{direction="sent"} 3
# HELP memorystore_invalidation_drops_total Total number of async invalidation keys dropped due to a full queue.
# TYPE memorystore_invalidation_drops_total counter
memorystore_invalidation_drops_total 8
`
	if err := testutil.CollectAndCompare(
		reg,
		strings.NewReader(expected),
		"memorystore_hits_total",
		"memorystore_misses_total",
		"memorystore_evictions_total",
		"memorystore_evicted_bytes_total",
		"memorystore_loads_total",
		"memorystore_invalidations_total",
		"memorystore_invalidation_drops_total",
	); err != nil {
		t.Fatalf("CollectAndCompare() error = %v", err)
	}

	// Verify histogram observations via Gather().
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	var foundHist bool
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
			h := m.GetHistogram()
			switch {
			case shared == "false" && status == "ok":
				if h.GetSampleCount() != 1 || h.GetSampleSum() != 0 {
					t.Errorf("hist(false, ok) = (%d, %v), want (1, 0)", h.GetSampleCount(), h.GetSampleSum())
				}
			case shared == "true" && status == "error":
				if h.GetSampleCount() != 1 || math.Abs(h.GetSampleSum()-0.02) > 1e-6 {
					t.Errorf("hist(true, error) = (%d, %v), want (1, 0.02)", h.GetSampleCount(), h.GetSampleSum())
				}
			default:
				if h.GetSampleCount() != 0 {
					t.Errorf("hist(%s, %s) count = %d, want 0", shared, status, h.GetSampleCount())
				}
			}
		}
	}
	if !foundHist {
		t.Fatal("missing memorystore_load_duration_seconds")
	}

	if problems, err := testutil.CollectAndLint(c); err != nil {
		t.Fatalf("CollectAndLint() error = %v", err)
	} else if len(problems) != 0 {
		t.Fatalf("CollectAndLint() problems = %+v, want 0", problems)
	}
}

func TestCollector_MemoryStoreIntegration(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c, err := prommetrics.NewCollector(prommetrics.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	broker := memorystore.NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	store, err := memorystore.New(
		memorystore.WithMetricsRecorder(c),
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("inv:prom"),
		memorystore.WithMaxItems(2),
	)
	if err != nil {
		t.Fatalf("memorystore.New(store) error = %v", err)
	}
	defer func() { _ = store.Stop() }()
	c.SetStore(store)

	peer, err := memorystore.New(
		memorystore.WithPubSub(broker),
		memorystore.WithInvalidationTopic("inv:prom"),
	)
	if err != nil {
		t.Fatalf("memorystore.New(peer) error = %v", err)
	}
	defer func() { _ = peer.Stop() }()

	_, _ = store.Get("missing")
	_ = store.Set("a", []byte("1"), time.Minute)
	_, _ = store.Get("a")
	_ = store.Set("b", []byte("2"), time.Minute)
	_ = store.Set("d", []byte("4"), time.Minute) // capacity eviction
	_ = store.Delete("d")

	_ = peer.SetMulti(map[string][]byte{"a": []byte("11"), "b": []byte("22")}, time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store.Get("a"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, _ = store.GetOrLoad(t.Context(), "ldr", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("val"), nil
	})

	if problems, err := testutil.CollectAndLint(c); err != nil {
		t.Fatalf("CollectAndLint() error = %v", err)
	} else if len(problems) != 0 {
		t.Fatalf("CollectAndLint() problems = %+v, want 0", problems)
	}

	m := store.GetMetrics()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	var gotItems, gotBytes float64
	for _, mf := range mfs {
		switch mf.GetName() {
		case "memorystore_items":
			gotItems = mf.GetMetric()[0].GetGauge().GetValue()
		case "memorystore_bytes":
			gotBytes = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if gotItems != float64(m.Items) {
		t.Errorf("memorystore_items = %v, want %d", gotItems, m.Items)
	}
	if gotBytes != float64(m.Bytes) {
		t.Errorf("memorystore_bytes = %v, want %d", gotBytes, m.Bytes)
	}
}

type stalledPromPubSub struct {
	once       sync.Once
	pubStarted chan struct{}
}

func (s *stalledPromPubSub) Subscribe(ctx context.Context, _ string) (<-chan []byte, error) {
	ch := make(chan []byte)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (s *stalledPromPubSub) Publish(ctx context.Context, _ string, _ []byte) error {
	s.once.Do(func() {
		close(s.pubStarted)
	})
	<-ctx.Done()
	return ctx.Err()
}

func (s *stalledPromPubSub) Unsubscribe(context.Context, string) error { return nil }
func (s *stalledPromPubSub) Close() error                              { return nil }

func TestCollector_AsyncInvalidationDropsIntegration(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c, err := prommetrics.NewCollector(prommetrics.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	ps := &stalledPromPubSub{pubStarted: make(chan struct{})}
	store, err := memorystore.New(
		memorystore.WithMetricsRecorder(c),
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("inv:prom:drop"),
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

	dropsOnly := &metricNameFilterCollector{inner: c, wantFQName: "memorystore_invalidation_drops_total"}
	if got := testutil.ToFloat64(dropsOnly); got != float64(wantDrops) {
		t.Errorf("memorystore_invalidation_drops_total = %v, want %d", got, wantDrops)
	}
}

func TestCollector_ToFloat64AndZeroHotPathAllocs(t *testing.T) {
	c, err := prommetrics.NewCollector(prommetrics.WithRegisterer(nil))
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	errSample := errors.New("sample")
	checks := []struct {
		name string
		fn   func()
	}{
		{name: "RecordHit", fn: func() { c.RecordHit() }},
		{name: "RecordMiss", fn: func() { c.RecordMiss() }},
		{name: "RecordEvict", fn: func() { c.RecordEvict(memorystore.EvictCapacity, 64) }},
		{name: "RecordLoad_OK", fn: func() { c.RecordLoad(time.Millisecond, false, nil) }},
		{name: "RecordLoad_ErrShared", fn: func() { c.RecordLoad(time.Millisecond, true, errSample) }},
		{name: "RecordInvalidation", fn: func() { c.RecordInvalidation(true, 4) }},
		{name: "RecordInvalidationDrop", fn: func() { c.RecordInvalidationDrop(4) }},
	}

	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, tc.fn)
			if allocs != 0 {
				t.Errorf("%s allocated %v times per call, want 0", tc.name, allocs)
			}
		})
	}

	reg := prometheus.NewPedanticRegistry()
	c2, err := prommetrics.NewCollector(prommetrics.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("NewCollector(c2) error = %v", err)
	}
	defer func() { _ = c2.Close() }()
	c2.RecordHit()
	c2.RecordHit()
	c2.RecordMiss()

	hitsOnly := &metricNameFilterCollector{inner: c2, wantFQName: "memorystore_hits_total"}
	if got := testutil.ToFloat64(hitsOnly); got != 2 {
		t.Fatalf("ToFloat64(hits_total) = %v, want 2", got)
	}
	missesOnly := &metricNameFilterCollector{inner: c2, wantFQName: "memorystore_misses_total"}
	if got := testutil.ToFloat64(missesOnly); got != 1 {
		t.Fatalf("ToFloat64(misses_total) = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(reg, "memorystore_hits_total"); got != 1 {
		t.Fatalf("CollectAndCount(hits_total) = %d, want 1", got)
	}
}

type metricNameFilterCollector struct {
	inner      prometheus.Collector
	wantFQName string
}

func (f *metricNameFilterCollector) Describe(ch chan<- *prometheus.Desc) {
	f.inner.Describe(ch)
}

func (f *metricNameFilterCollector) Collect(ch chan<- prometheus.Metric) {
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

func TestCollector_ConcurrentRaceAndGoroutineLeak(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c, err := prommetrics.NewCollector(prommetrics.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}
	defer func() { _ = c.Close() }()

	store, err := memorystore.New(
		memorystore.WithMetricsRecorder(c),
		memorystore.WithMaxItems(32),
	)
	if err != nil {
		t.Fatalf("memorystore.New() error = %v", err)
	}
	c.SetStore(store)

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
					_, _ = reg.Gather()
				}
				if id == 1 && i%25 == 0 {
					c.SetStore(store)
				}
			}
		}(g)
	}
	wg.Wait()

	if err := store.Stop(); err != nil {
		t.Fatalf("store.Stop() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/metrics/prometheus") {
			t.Errorf("detected leaked goroutine in metrics/prometheus:\n%s", buf.String())
		}
	}
}
