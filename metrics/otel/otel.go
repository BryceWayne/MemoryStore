package otel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DefaultInstrumentationName is the default OpenTelemetry Meter name used
// when [WithMeter] is not supplied.
const DefaultInstrumentationName = "github.com/BryceWayne/MemoryStore/metrics/otel"

var (
	// ErrRecorderClosed is returned when [Recorder.RegisterStore] is called after
	// [Recorder.Close].
	ErrRecorderClosed = errors.New("otel recorder is closed")
	// ErrStoreAlreadyRegistered is returned when [Recorder.RegisterStore] is
	// called when a store is already registered with the recorder.
	ErrStoreAlreadyRegistered = errors.New("store is already registered")
)

var (
	bgCtx = context.Background()

	defaultLoadDurationBuckets = []float64{
		0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000,
	}
)

var (
	attrReason    = attribute.Key("reason")
	attrShared    = attribute.Key("shared")
	attrStatus    = attribute.Key("status")
	attrDirection = attribute.Key("direction")
)

type config struct {
	meterProvider metric.MeterProvider
	meter         metric.Meter
	store         *memorystore.MemoryStore
	attrs         []attribute.KeyValue
	buckets       []float64
}

// Option configures a [Recorder] created by [NewRecorder].
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

// WithMeterProvider sets the OpenTelemetry MeterProvider used to obtain a
// Meter named [DefaultInstrumentationName] when [WithMeter] is not provided.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) error {
		if isNilInterface(mp) {
			return errors.New("meter provider must not be nil")
		}
		c.meterProvider = mp
		return nil
	}
}

// WithMeter sets an explicit OpenTelemetry Meter for all created instruments.
// When provided, it takes precedence over [WithMeterProvider].
func WithMeter(m metric.Meter) Option {
	return func(c *config) error {
		if isNilInterface(m) {
			return errors.New("meter must not be nil")
		}
		c.meter = m
		return nil
	}
}

// WithStore attaches a [memorystore.MemoryStore] during construction so
// memorystore.items and memorystore.bytes observable gauges are registered
// immediately. For stores constructed with [memorystore.WithMetricsRecorder],
// call [Recorder.RegisterStore] after [memorystore.New] instead.
func WithStore(store *memorystore.MemoryStore) Option {
	return func(c *config) error {
		if store == nil {
			return errors.New("store must not be nil")
		}
		c.store = store
		return nil
	}
}

// WithAttributes appends base attributes attached to every emitted metric.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *config) error {
		c.attrs = append(c.attrs, attrs...)
		return nil
	}
}

// WithLoadDurationBuckets overrides the explicit histogram bucket boundaries
// (in milliseconds) for memorystore.load_duration_ms. Bounds must be non-empty,
// finite, non-negative, and strictly increasing.
func WithLoadDurationBuckets(bounds ...float64) Option {
	return func(c *config) error {
		if len(bounds) == 0 {
			return errors.New("load duration buckets must not be empty")
		}
		for i, b := range bounds {
			if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 {
				return fmt.Errorf("invalid load duration bucket %v at index %d: must be finite and >= 0", b, i)
			}
			if i > 0 && b <= bounds[i-1] {
				return fmt.Errorf("load duration buckets must be strictly increasing: %v <= %v at index %d", b, bounds[i-1], i)
			}
		}
		c.buckets = slices.Clone(bounds)
		return nil
	}
}

// Recorder implements [memorystore.MetricsRecorder] and
// [memorystore.InvalidationDropRecorder] using OpenTelemetry metric instruments
// and optional observable gauges for [memorystore.MemoryStore].
type Recorder struct {
	meter metric.Meter

	hits              metric.Int64Counter
	misses            metric.Int64Counter
	evictions         metric.Int64Counter
	evictedBytes      metric.Int64Counter
	loads             metric.Int64Counter
	loadDuration      metric.Float64Histogram
	invalidations     metric.Int64Counter
	invalidationDrops metric.Int64Counter
	items             metric.Int64ObservableGauge
	bytes             metric.Int64ObservableGauge

	baseAddOpts      []metric.AddOption
	baseObserveOpts  []metric.ObserveOption
	evictOpts        [4][]metric.AddOption
	loadAddOpts      [2][2][]metric.AddOption
	loadRecordOpts   [2][2][]metric.RecordOption
	invalidationOpts [2][]metric.AddOption

	mu     sync.Mutex
	store  *memorystore.MemoryStore
	reg    metric.Registration
	closed bool
}

var (
	_ memorystore.MetricsRecorder          = (*Recorder)(nil)
	_ memorystore.InvalidationDropRecorder = (*Recorder)(nil)
)

// NewRecorder creates a new OpenTelemetry [Recorder] configured by opts.
// Nil options are skipped.
func NewRecorder(opts ...Option) (*Recorder, error) {
	cfg := config{
		buckets: defaultLoadDurationBuckets,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	meter := cfg.meter
	if meter == nil {
		mp := cfg.meterProvider
		if mp == nil {
			mp = otel.GetMeterProvider()
		}
		meter = mp.Meter(DefaultInstrumentationName)
	}

	hits, err := meter.Int64Counter(
		"memorystore.hits",
		metric.WithDescription("Total number of cache hits."),
		metric.WithUnit("{hit}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.hits counter: %w", err)
	}

	misses, err := meter.Int64Counter(
		"memorystore.misses",
		metric.WithDescription("Total number of cache misses."),
		metric.WithUnit("{miss}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.misses counter: %w", err)
	}

	evictions, err := meter.Int64Counter(
		"memorystore.evictions",
		metric.WithDescription("Total number of cache evictions and deletions."),
		metric.WithUnit("{eviction}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.evictions counter: %w", err)
	}

	evictedBytes, err := meter.Int64Counter(
		"memorystore.evicted_bytes",
		metric.WithDescription("Total payload bytes evicted or deleted from the cache."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.evicted_bytes counter: %w", err)
	}

	loads, err := meter.Int64Counter(
		"memorystore.loads",
		metric.WithDescription("Total number of cache loader executions."),
		metric.WithUnit("{load}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.loads counter: %w", err)
	}

	loadDuration, err := meter.Float64Histogram(
		"memorystore.load_duration_ms",
		metric.WithDescription("Duration of cache loader executions in milliseconds."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(cfg.buckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.load_duration_ms histogram: %w", err)
	}

	invalidations, err := meter.Int64Counter(
		"memorystore.invalidations",
		metric.WithDescription("Total number of distributed invalidation keys sent or received."),
		metric.WithUnit("{key}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.invalidations counter: %w", err)
	}

	invalidationDrops, err := meter.Int64Counter(
		"memorystore.invalidation_drops",
		metric.WithDescription("Total number of async invalidation keys dropped due to a full queue."),
		metric.WithUnit("{key}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.invalidation_drops counter: %w", err)
	}

	items, err := meter.Int64ObservableGauge(
		"memorystore.items",
		metric.WithDescription("Current number of items stored in MemoryStore."),
		metric.WithUnit("{item}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.items gauge: %w", err)
	}

	bytesGauge, err := meter.Int64ObservableGauge(
		"memorystore.bytes",
		metric.WithDescription("Current stored key and value payload bytes in MemoryStore."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, fmt.Errorf("create memorystore.bytes gauge: %w", err)
	}

	r := &Recorder{
		meter:             meter,
		hits:              hits,
		misses:            misses,
		evictions:         evictions,
		evictedBytes:      evictedBytes,
		loads:             loads,
		loadDuration:      loadDuration,
		invalidations:     invalidations,
		invalidationDrops: invalidationDrops,
		items:             items,
		bytes:             bytesGauge,
	}

	if len(cfg.attrs) > 0 {
		baseSet := attribute.NewSet(cfg.attrs...)
		r.baseAddOpts = []metric.AddOption{metric.WithAttributeSet(baseSet)}
		r.baseObserveOpts = []metric.ObserveOption{metric.WithAttributeSet(baseSet)}
	}

	evictReasons := [4]string{"unknown", "expired", "capacity", "deleted"}
	for i, reason := range evictReasons {
		set := buildAttributeSet(cfg.attrs, attrReason.String(reason))
		r.evictOpts[i] = []metric.AddOption{metric.WithAttributeSet(set)}
	}

	sharedVals := [2]bool{false, true}
	statusVals := [2]string{"ok", "error"}
	for si, shared := range sharedVals {
		for sti, status := range statusVals {
			set := buildAttributeSet(cfg.attrs, attrShared.Bool(shared), attrStatus.String(status))
			opt := metric.WithAttributeSet(set)
			r.loadAddOpts[si][sti] = []metric.AddOption{opt}
			r.loadRecordOpts[si][sti] = []metric.RecordOption{opt}
		}
	}

	directions := [2]string{"received", "sent"}
	for i, dir := range directions {
		set := buildAttributeSet(cfg.attrs, attrDirection.String(dir))
		r.invalidationOpts[i] = []metric.AddOption{metric.WithAttributeSet(set)}
	}

	if cfg.store != nil {
		if err := r.RegisterStore(cfg.store); err != nil {
			return nil, err
		}
	}

	return r, nil
}

func buildAttributeSet(base []attribute.KeyValue, extra ...attribute.KeyValue) attribute.Set {
	merged := make([]attribute.KeyValue, 0, len(base)+len(extra))
	merged = append(merged, base...)
	merged = append(merged, extra...)
	return attribute.NewSet(merged...)
}

// RegisterStore binds store to the recorder's memorystore.items and
// memorystore.bytes observable gauges. At most one store may be registered per
// Recorder. Calling RegisterStore with nil, after another store is registered,
// or after [Recorder.Close] returns an error.
func (r *Recorder) RegisterStore(store *memorystore.MemoryStore) error {
	if store == nil {
		return errors.New("store must not be nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return ErrRecorderClosed
	}
	if r.store != nil {
		return ErrStoreAlreadyRegistered
	}

	reg, err := r.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		m := store.GetMetrics()
		o.ObserveInt64(r.items, int64(m.Items), r.baseObserveOpts...)
		o.ObserveInt64(r.bytes, m.Bytes, r.baseObserveOpts...)
		return nil
	}, r.items, r.bytes)
	if err != nil {
		return fmt.Errorf("register observable gauge callback: %w", err)
	}

	r.store = store
	r.reg = reg
	return nil
}

// Close unregisters any observable gauge callback registered for an attached
// store and marks the recorder closed. Subsequent calls to Close are no-ops and
// return nil.
func (r *Recorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	reg := r.reg
	r.reg = nil
	r.store = nil
	r.mu.Unlock()

	if reg != nil {
		return reg.Unregister()
	}
	return nil
}

// RecordHit records a single cache hit.
func (r *Recorder) RecordHit() {
	r.hits.Add(bgCtx, 1, r.baseAddOpts...)
}

// RecordMiss records a single cache miss.
func (r *Recorder) RecordMiss() {
	r.misses.Add(bgCtx, 1, r.baseAddOpts...)
}

// RecordEvict records an eviction or deletion with its reason and payload size.
func (r *Recorder) RecordEvict(reason memorystore.EvictReason, bytes int64) {
	idx := 0
	switch reason {
	case memorystore.EvictExpired:
		idx = 1
	case memorystore.EvictCapacity:
		idx = 2
	case memorystore.EvictDeleted:
		idx = 3
	}
	opts := r.evictOpts[idx]
	r.evictions.Add(bgCtx, 1, opts...)
	if bytes > 0 {
		r.evictedBytes.Add(bgCtx, bytes, opts...)
	}
}

// RecordLoad records a singleflight loader execution, its duration in
// milliseconds, whether the result was shared with coalesced callers, and
// whether it succeeded or failed.
func (r *Recorder) RecordLoad(duration time.Duration, shared bool, err error) {
	sharedIdx := 0
	if shared {
		sharedIdx = 1
	}
	statusIdx := 0
	if err != nil {
		statusIdx = 1
	}
	if duration < 0 {
		duration = 0
	}
	ms := float64(duration) / float64(time.Millisecond)
	r.loads.Add(bgCtx, 1, r.loadAddOpts[sharedIdx][statusIdx]...)
	r.loadDuration.Record(bgCtx, ms, r.loadRecordOpts[sharedIdx][statusIdx]...)
}

// RecordInvalidation records the number of keys sent or received in a
// distributed invalidation batch. Non-positive key counts are ignored.
func (r *Recorder) RecordInvalidation(sent bool, keyCount int) {
	if keyCount <= 0 {
		return
	}
	dirIdx := 0
	if sent {
		dirIdx = 1
	}
	r.invalidations.Add(bgCtx, int64(keyCount), r.invalidationOpts[dirIdx]...)
}

// RecordInvalidationDrop records the number of async invalidation keys dropped
// due to a full invalidation queue. Non-positive key counts are ignored.
func (r *Recorder) RecordInvalidationDrop(keyCount int) {
	if keyCount <= 0 {
		return
	}
	r.invalidationDrops.Add(bgCtx, int64(keyCount), r.baseAddOpts...)
}
