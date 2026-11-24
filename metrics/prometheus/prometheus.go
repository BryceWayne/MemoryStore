package prometheus

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"github.com/prometheus/client_golang/prometheus"
)

// DefaultNamespace is the default Prometheus metric namespace.
const DefaultNamespace = "memorystore"

// DefaultDurationBuckets defines the default histogram bucket boundaries (in
// seconds) for memorystore_load_duration_seconds.
var DefaultDurationBuckets = []float64{
	0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

type config struct {
	registerer  prometheus.Registerer
	namespace   string
	subsystem   string
	constLabels prometheus.Labels
	store       *memorystore.MemoryStore
	buckets     []float64
}

// Option configures a [Collector] created by [NewCollector].
type Option func(*config) error

func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

func isValidMetricName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == ':'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

func isValidLabelName(name string) bool {
	if name == "" || strings.HasPrefix(name, "__") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

// WithRegisterer sets the [prometheus.Registerer] used to automatically register
// the [Collector] in [NewCollector]. Passing nil (or a typed nil) skips
// automatic registration. By default, [prometheus.DefaultRegisterer] is used.
func WithRegisterer(reg prometheus.Registerer) Option {
	return func(c *config) error {
		if isNilInterface(reg) {
			c.registerer = nil
		} else {
			c.registerer = reg
		}
		return nil
	}
}

// WithNamespace overrides the Prometheus metric namespace (default
// [DefaultNamespace]).
func WithNamespace(namespace string) Option {
	return func(c *config) error {
		c.namespace = namespace
		return nil
	}
}

// WithSubsystem sets the Prometheus metric subsystem.
func WithSubsystem(subsystem string) Option {
	return func(c *config) error {
		c.subsystem = subsystem
		return nil
	}
}

// WithConstLabels attaches constant labels to all metrics exported by the
// [Collector]. Reserved label keys ("le", "reason", "shared", "status",
// "direction"), Prometheus internal "__*" keys, invalid label names, and
// non-UTF-8 values are rejected.
func WithConstLabels(labels prometheus.Labels) Option {
	return func(c *config) error {
		for k, v := range labels {
			switch k {
			case "le", "reason", "shared", "status", "direction":
				return fmt.Errorf("const label %q is reserved", k)
			}
			if !isValidLabelName(k) {
				return fmt.Errorf("invalid const label name %q", k)
			}
			if !utf8.ValidString(v) {
				return fmt.Errorf("const label %q value is not valid UTF-8", k)
			}
		}
		c.constLabels = maps.Clone(labels)
		return nil
	}
}

// WithStore attaches a [memorystore.MemoryStore] during construction so
// memorystore_items and memorystore_bytes gauges are populated immediately.
// When wiring [memorystore.WithMetricsRecorder], call [Collector.SetStore]
// after [memorystore.New] instead.
func WithStore(store *memorystore.MemoryStore) Option {
	return func(c *config) error {
		if store == nil {
			return errors.New("store must not be nil")
		}
		c.store = store
		return nil
	}
}

// WithDurationBuckets overrides the histogram bucket boundaries (in seconds)
// for memorystore_load_duration_seconds. Buckets must be non-empty, finite,
// strictly positive, and strictly increasing.
func WithDurationBuckets(buckets []float64) Option {
	return func(c *config) error {
		if len(buckets) == 0 {
			return errors.New("duration buckets must not be empty")
		}
		for i, b := range buckets {
			if math.IsNaN(b) || math.IsInf(b, 0) || b <= 0 {
				return fmt.Errorf("invalid duration bucket %v at index %d: must be finite and > 0", b, i)
			}
			if i > 0 && b <= buckets[i-1] {
				return fmt.Errorf("duration buckets must be strictly increasing: %v <= %v at index %d", b, buckets[i-1], i)
			}
		}
		c.buckets = slices.Clone(buckets)
		return nil
	}
}

// Collector implements [prometheus.Collector], [memorystore.MetricsRecorder],
// and [memorystore.InvalidationDropRecorder] for [memorystore.MemoryStore].
type Collector struct {
	registerer   prometheus.Registerer
	unregistered atomic.Bool

	store atomic.Pointer[memorystore.MemoryStore]

	hits              prometheus.Counter
	misses            prometheus.Counter
	evictions         *prometheus.CounterVec
	evictedBytes      prometheus.Counter
	loads             *prometheus.CounterVec
	loadDuration      *prometheus.HistogramVec
	invalidations     *prometheus.CounterVec
	invalidationDrops prometheus.Counter

	// Pre-resolved child metrics for zero-lookup hot-path recording.
	evictExpired         prometheus.Counter
	evictCapacity        prometheus.Counter
	evictDeleted         prometheus.Counter
	loadCounters         [2][2]prometheus.Counter
	loadDurations        [2][2]prometheus.Observer
	invalidationSent     prometheus.Counter
	invalidationReceived prometheus.Counter

	itemsDesc *prometheus.Desc
	bytesDesc *prometheus.Desc
}

var (
	_ prometheus.Collector                 = (*Collector)(nil)
	_ memorystore.MetricsRecorder          = (*Collector)(nil)
	_ memorystore.InvalidationDropRecorder = (*Collector)(nil)
)

// NewCollector creates a new Prometheus [Collector] configured by opts.
// Nil options are skipped. Unless [WithRegisterer](nil) is supplied, the
// collector is registered immediately with the configured [prometheus.Registerer]
// (defaulting to [prometheus.DefaultRegisterer]).
func NewCollector(opts ...Option) (*Collector, error) {
	cfg := config{
		registerer: prometheus.DefaultRegisterer,
		namespace:  DefaultNamespace,
		buckets:    DefaultDurationBuckets,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	fq := func(name string) string {
		return prometheus.BuildFQName(cfg.namespace, cfg.subsystem, name)
	}
	sampleName := fq("hits_total")
	if !isValidMetricName(sampleName) {
		return nil, fmt.Errorf("invalid prometheus metric prefix (namespace=%q, subsystem=%q)", cfg.namespace, cfg.subsystem)
	}

	hits := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "hits_total",
		Help:        "Total number of cache hits.",
		ConstLabels: cfg.constLabels,
	})
	misses := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "misses_total",
		Help:        "Total number of cache misses.",
		ConstLabels: cfg.constLabels,
	})
	evictions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "evictions_total",
		Help:        "Total number of cache evictions and deletions.",
		ConstLabels: cfg.constLabels,
	}, []string{"reason"})
	evictedBytes := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "evicted_bytes_total",
		Help:        "Total payload bytes evicted or deleted from the cache.",
		ConstLabels: cfg.constLabels,
	})
	loads := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "loads_total",
		Help:        "Total number of cache loader executions.",
		ConstLabels: cfg.constLabels,
	}, []string{"shared", "status"})
	loadDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "load_duration_seconds",
		Help:        "Duration of cache loader executions in seconds.",
		ConstLabels: cfg.constLabels,
		Buckets:     cfg.buckets,
	}, []string{"shared", "status"})
	invalidations := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "invalidations_total",
		Help:        "Total number of distributed invalidation keys sent or received.",
		ConstLabels: cfg.constLabels,
	}, []string{"direction"})
	invalidationDrops := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   cfg.namespace,
		Subsystem:   cfg.subsystem,
		Name:        "invalidation_drops_total",
		Help:        "Total number of async invalidation keys dropped due to a full queue.",
		ConstLabels: cfg.constLabels,
	})

	itemsDesc := prometheus.NewDesc(
		fq("items"),
		"Current number of items stored in MemoryStore.",
		nil,
		cfg.constLabels,
	)
	bytesDesc := prometheus.NewDesc(
		fq("bytes"),
		"Current stored key and value payload bytes in MemoryStore.",
		nil,
		cfg.constLabels,
	)

	c := &Collector{
		registerer:           cfg.registerer,
		hits:                 hits,
		misses:               misses,
		evictions:            evictions,
		evictedBytes:         evictedBytes,
		loads:                loads,
		loadDuration:         loadDuration,
		invalidations:        invalidations,
		invalidationDrops:    invalidationDrops,
		evictExpired:         evictions.WithLabelValues(memorystore.EvictExpired.String()),
		evictCapacity:        evictions.WithLabelValues(memorystore.EvictCapacity.String()),
		evictDeleted:         evictions.WithLabelValues(memorystore.EvictDeleted.String()),
		invalidationSent:     invalidations.WithLabelValues("sent"),
		invalidationReceived: invalidations.WithLabelValues("received"),
		itemsDesc:            itemsDesc,
		bytesDesc:            bytesDesc,
	}

	sharedLabels := [2]string{"false", "true"}
	statusLabels := [2]string{"ok", "error"}
	for si, shared := range sharedLabels {
		for sti, status := range statusLabels {
			c.loadCounters[si][sti] = loads.WithLabelValues(shared, status)
			c.loadDurations[si][sti] = loadDuration.WithLabelValues(shared, status)
		}
	}

	if cfg.store != nil {
		c.store.Store(cfg.store)
	}

	if cfg.registerer != nil {
		if err := cfg.registerer.Register(c); err != nil {
			return nil, fmt.Errorf("register prometheus collector: %w", err)
		}
	} else {
		// Validate all descriptors eagerly even when auto-registration is disabled.
		if err := prometheus.NewPedanticRegistry().Register(c); err != nil {
			return nil, fmt.Errorf("validate prometheus collector descriptors: %w", err)
		}
	}

	return c, nil
}

// SetStore binds (or replaces) the [memorystore.MemoryStore] queried for
// memorystore_items and memorystore_bytes during Prometheus scrapes. Passing nil
// detaches the store.
func (c *Collector) SetStore(store *memorystore.MemoryStore) {
	c.store.Store(store)
}

// Unregister idempotently unregisters the collector from its configured
// [prometheus.Registerer]. It returns false if the collector was created with
// [WithRegisterer](nil) or was already unregistered.
func (c *Collector) Unregister() bool {
	if c == nil || c.registerer == nil {
		return false
	}
	if !c.unregistered.CompareAndSwap(false, true) {
		return false
	}
	return c.registerer.Unregister(c)
}

// Close unregisters the collector from its [prometheus.Registerer] and detaches
// any bound store. Subsequent calls to Close are no-ops and return nil.
func (c *Collector) Close() error {
	c.Unregister()
	c.store.Store(nil)
	return nil
}

// Describe implements [prometheus.Collector]. It unconditionally sends all
// ten metric descriptors so pedantic registries accept scrapes before and after
// [Collector.SetStore] is called.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	c.hits.Describe(ch)
	c.misses.Describe(ch)
	c.evictions.Describe(ch)
	c.evictedBytes.Describe(ch)
	c.loads.Describe(ch)
	c.loadDuration.Describe(ch)
	c.invalidations.Describe(ch)
	c.invalidationDrops.Describe(ch)
	ch <- c.itemsDesc
	ch <- c.bytesDesc
}

// Collect implements [prometheus.Collector].
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.hits.Collect(ch)
	c.misses.Collect(ch)
	c.evictions.Collect(ch)
	c.evictedBytes.Collect(ch)
	c.loads.Collect(ch)
	c.loadDuration.Collect(ch)
	c.invalidations.Collect(ch)
	c.invalidationDrops.Collect(ch)

	if store := c.store.Load(); store != nil {
		m := store.GetMetrics()
		ch <- prometheus.MustNewConstMetric(c.itemsDesc, prometheus.GaugeValue, float64(m.Items))
		ch <- prometheus.MustNewConstMetric(c.bytesDesc, prometheus.GaugeValue, float64(m.Bytes))
	}
}

// RecordHit records a single cache hit.
func (c *Collector) RecordHit() {
	c.hits.Inc()
}

// RecordMiss records a single cache miss.
func (c *Collector) RecordMiss() {
	c.misses.Inc()
}

// RecordEvict records an eviction or deletion with its reason and payload size.
func (c *Collector) RecordEvict(reason memorystore.EvictReason, bytes int64) {
	switch reason {
	case memorystore.EvictExpired:
		c.evictExpired.Inc()
	case memorystore.EvictCapacity:
		c.evictCapacity.Inc()
	case memorystore.EvictDeleted:
		c.evictDeleted.Inc()
	default:
		c.evictions.WithLabelValues(reason.String()).Inc()
	}
	if bytes > 0 {
		c.evictedBytes.Add(float64(bytes))
	}
}

// RecordLoad records a singleflight loader execution, its duration in seconds,
// whether the result was shared with coalesced callers, and whether it
// succeeded or failed.
func (c *Collector) RecordLoad(duration time.Duration, shared bool, err error) {
	si := 0
	if shared {
		si = 1
	}
	sti := 0
	if err != nil {
		sti = 1
	}
	c.loadCounters[si][sti].Inc()
	c.loadDurations[si][sti].Observe(max(duration.Seconds(), 0))
}

// RecordInvalidation records the number of keys sent or received in a
// distributed invalidation batch. Non-positive key counts are ignored.
func (c *Collector) RecordInvalidation(sent bool, keyCount int) {
	if keyCount <= 0 {
		return
	}
	if sent {
		c.invalidationSent.Add(float64(keyCount))
		return
	}
	c.invalidationReceived.Add(float64(keyCount))
}

// RecordInvalidationDrop records the number of async invalidation keys dropped
// due to a full invalidation queue. Non-positive key counts are ignored.
func (c *Collector) RecordInvalidationDrop(keyCount int) {
	if keyCount <= 0 {
		return
	}
	c.invalidationDrops.Add(float64(keyCount))
}
