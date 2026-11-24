package memorystore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ==================== Issue #44: Validation & Stringers ====================

func TestM4_OptionValidationAndStringers(t *testing.T) {
	if got := InvalidationModeInvalidateOnly.String(); got != "invalidate-only" {
		t.Errorf("InvalidationModeInvalidateOnly.String() = %q", got)
	}
	if got := InvalidationModeWriteThrough.String(); got != "write-through" {
		t.Errorf("InvalidationModeWriteThrough.String() = %q", got)
	}
	if got := InvalidationMode(99).String(); got != "unknown" {
		t.Errorf("InvalidationMode(99).String() = %q", got)
	}

	tests := []struct {
		name string
		opts []Option
	}{
		{"negative DefaultStaleWhileRevalidate", []Option{WithDefaultStaleWhileRevalidate(-time.Millisecond)}},
		{"unknown InvalidationMode", []Option{WithInvalidationMode(InvalidationMode(99))}},
		{"negative WriteThroughMaxBytes", []Option{WithWriteThroughMaxBytes(-1)}},
		{"zero AsyncInvalidation", []Option{WithAsyncInvalidation(0)}},
		{"negative AsyncInvalidation", []Option{WithAsyncInvalidation(-5)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := New(tt.opts...)
			if err == nil {
				_ = ms.Stop()
				t.Fatalf("New() succeeded, expected validation error")
			}
		})
	}

	t.Run("default PubSubTimeout 5s when InvalidationTopic set", func(t *testing.T) {
		ps := NewInMemoryPubSub()
		defer func() { _ = ps.Close() }()

		ms, err := New(WithPubSub(ps), WithInvalidationTopic("m4:inv:timeout"))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		if ms.pubSubTimeout != defaultInvalidationPubSubTimeout {
			t.Errorf("pubSubTimeout = %v, want %v", ms.pubSubTimeout, defaultInvalidationPubSubTimeout)
		}
		if !ms.asyncInvalidation || cap(ms.invalidationQueue) != defaultInvalidationQueueSize {
			t.Errorf("asyncInvalidation=%v cap=%d, want true/%d", ms.asyncInvalidation, cap(ms.invalidationQueue), defaultInvalidationQueueSize)
		}
		if ms.writeThroughMaxBytes != defaultWriteThroughMaxBytes {
			t.Errorf("writeThroughMaxBytes = %d, want %d", ms.writeThroughMaxBytes, defaultWriteThroughMaxBytes)
		}
	})

	t.Run("WithWriteThroughMaxBytes(0) and WithSyncInvalidation", func(t *testing.T) {
		ps := NewInMemoryPubSub()
		defer func() { _ = ps.Close() }()

		ms, err := New(
			WithPubSub(ps),
			WithInvalidationTopic("m4:inv:sync"),
			WithInvalidationMode(InvalidationModeWriteThrough),
			WithWriteThroughMaxBytes(0),
			WithSyncInvalidation(),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		if ms.writeThroughMaxBytes != 0 {
			t.Errorf("writeThroughMaxBytes = %d, want 0", ms.writeThroughMaxBytes)
		}
		if ms.asyncInvalidation {
			t.Error("expected asyncInvalidation=false with WithSyncInvalidation()")
		}
	})

	t.Run("SetOption and LoadOption validation", func(t *testing.T) {
		ms, err := New(WithPreciseClock())
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		if err := ms.SetWithOptions("k", []byte("v"), time.Second, WithStaleWindow(-time.Millisecond)); err == nil {
			t.Fatal("SetWithOptions(WithStaleWindow(-1ms)) succeeded, want error")
		}
		if _, err := ms.GetOrLoadWithOptions(t.Context(), "k", time.Second, func(context.Context) ([]byte, error) {
			return []byte("v"), nil
		}, WithStaleWhileRevalidate(-time.Millisecond)); err == nil {
			t.Fatal("GetOrLoadWithOptions(WithStaleWhileRevalidate(-1ms)) succeeded, want error")
		}
		if _, err := ms.GetOrLoadJSONWithOptions[string](t.Context(), "k", time.Second, func(context.Context) (string, error) {
			return "v", nil
		}, []LoadOption{WithStaleWhileRevalidate(-time.Millisecond)}); err == nil {
			t.Fatal("GetOrLoadJSONWithOptions(WithStaleWhileRevalidate(-1ms)) succeeded, want error")
		}
	})
}

// ==================== Issue #44: Stale-While-Revalidate ====================

func TestSWR_GetFreshOnlyVsGetOrLoadStaleCoalescing(t *testing.T) {
	rec := &spyRecorder{}
	ms, err := New(
		WithPreciseClock(),
		WithCleanupInterval(15*time.Millisecond),
		WithDefaultStaleWhileRevalidate(500*time.Millisecond),
		WithMetricsRecorder(rec),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	if err := ms.Set("swr-key", []byte("v1"), 35*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Wait until soft TTL expires, while still well within 500ms stale grace
	// and after cleanup worker runs at least once.
	time.Sleep(60 * time.Millisecond)

	// Plain Get and GetMulti MUST treat soft-expired entry as a miss,
	// without evicting it before hardExpiresAt.
	if val, ok := ms.Get("swr-key"); ok || val != nil {
		t.Fatalf("Get() during stale window = (%q, %v), want (nil, false)", string(val), ok)
	}
	if multi := ms.GetMulti([]string{"swr-key"}); len(multi) != 0 {
		t.Fatalf("GetMulti() during stale window = %v, want empty", multi)
	}

	// Concurrent GetOrLoad calls should all receive "v1" immediately while
	// coalescing background refresh to exactly 1 loader execution, using a
	// detached context even if callers cancel their own context right after.
	var loads atomic.Int32
	gate := make(chan struct{})
	var ctxCancelledInLoader atomic.Bool

	const callers = 8
	var wg sync.WaitGroup
	results := make([][]byte, callers)
	errs := make([]error, callers)

	for i := range callers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			reqCtx, cancel := context.WithCancel(context.Background())
			defer cancel()

			val, lerr := ms.GetOrLoad(reqCtx, "swr-key", time.Second, func(bgCtx context.Context) ([]byte, error) {
				loads.Add(1)
				<-gate
				if bgCtx.Err() != nil {
					ctxCancelledInLoader.Store(true)
				}
				return []byte("v2"), nil
			})
			results[idx] = val
			errs[idx] = lerr
			cancel() // cancel caller context immediately
		}(i)
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d GetOrLoad error = %v", i, errs[i])
		}
		if string(results[i]) != "v1" {
			t.Fatalf("caller %d got %q, want stale %q", i, string(results[i]), "v1")
		}
	}

	close(gate)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, ok := ms.Get("swr-key"); ok && string(v) == "v2" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v, ok := ms.Get("swr-key"); !ok || string(v) != "v2" {
		t.Fatalf("expected background refresh to update key to v2, got (%q, %v)", string(v), ok)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loader invocations = %d, want 1", got)
	}
	if ctxCancelledInLoader.Load() {
		t.Fatal("background SWR refresh received cancelled caller context instead of detached m.ctx")
	}
}

func TestSWR_WithOptionsJSONAndPreservesStaleOnErrorOrPanic(t *testing.T) {
	type payload struct {
		Version int `json:"version"`
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rec := &spyRecorder{}

	ms, err := New(
		WithPreciseClock(),
		WithLogger(logger),
		WithMetricsRecorder(rec),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	// Seed via GetOrLoadJSONWithOptions with per-call SWR window.
	got, err := GetOrLoadJSONWithOptions[payload](
		ms,
		t.Context(),
		"json-swr",
		30*time.Millisecond,
		func(context.Context) (payload, error) {
			return payload{Version: 1}, nil
		},
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	)
	if err != nil || got.Version != 1 {
		t.Fatalf("initial GetOrLoadJSONWithOptions = (%+v, %v), want v1", got, err)
	}

	time.Sleep(50 * time.Millisecond)

	// Plain GetJSONAs must miss during stale window.
	if _, ok, jerr := ms.GetJSONAs[payload]("json-swr"); ok || jerr != nil {
		t.Fatalf("GetJSONAs during stale window = (ok=%v, err=%v), want (false, nil)", ok, jerr)
	}

	// Background refresh fails -> stale v1 must remain preserved!
	failErr := errors.New("upstream unavailable")
	var failCalls atomic.Int32
	got, err = ms.GetOrLoadJSONWithOptions(
		t.Context(),
		"json-swr",
		time.Second,
		func(context.Context) (payload, error) {
			failCalls.Add(1)
			return payload{}, failErr
		},
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	)
	if err != nil || got.Version != 1 {
		t.Fatalf("stale GetOrLoadJSONWithOptions = (%+v, %v), want v1", got, err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.loads)
		rec.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Next call with a panicking loader still gets preserved stale v1!
	var panicDone atomic.Bool
	got, err = ms.GetOrLoadJSONWithOptions(
		t.Context(),
		"json-swr",
		time.Second,
		func(context.Context) (payload, error) {
			defer panicDone.Store(true)
			panic("boom in swr loader")
		},
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	)
	if err != nil || got.Version != 1 {
		t.Fatalf("stale after failed refresh = (%+v, %v), want v1", got, err)
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !panicDone.Load() {
		time.Sleep(5 * time.Millisecond)
	}

	// Verify WithStaleWhileRevalidate(0) bypasses stale serving and reloads synchronously.
	got, err = ms.GetOrLoadJSONWithOptions(
		t.Context(),
		"json-swr",
		time.Second,
		func(context.Context) (payload, error) {
			return payload{Version: 3}, nil
		},
		[]LoadOption{WithStaleWhileRevalidate(0)},
	)
	if err != nil || got.Version != 3 {
		t.Fatalf("WithStaleWhileRevalidate(0) = (%+v, %v), want v3", got, err)
	}

	if !strings.Contains(logBuf.String(), "stale-while-revalidate background refresh") {
		t.Errorf("expected warning logs on SWR refresh failure/panic, got: %s", logBuf.String())
	}
}

// Helper wrapper so generic method call compiles cleanly in test helper.
func GetOrLoadJSONWithOptions[T any](
	ms *MemoryStore,
	ctx context.Context,
	key string,
	ttl time.Duration,
	loader func(context.Context) (T, error),
	loadOpts []LoadOption,
) (T, error) {
	return ms.GetOrLoadJSONWithOptions[T](ctx, key, ttl, loader, loadOpts)
}

func TestSWR_StopCancelsAndAwaitsActiveRefresh_NoGoroutineLeak(t *testing.T) {
	ms, err := New(WithPreciseClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := ms.SetWithOptions("k", []byte("stale-val"), 20*time.Millisecond, WithStaleWindow(time.Second)); err != nil {
		t.Fatalf("SetWithOptions() error = %v", err)
	}
	time.Sleep(35 * time.Millisecond)

	started := make(chan struct{})
	exited := make(chan struct{})

	val, err := ms.GetOrLoadWithOptions(t.Context(), "k", time.Second, func(bgCtx context.Context) ([]byte, error) {
		close(started)
		<-bgCtx.Done()
		close(exited)
		return nil, bgCtx.Err()
	})
	if err != nil || string(val) != "stale-val" {
		t.Fatalf("GetOrLoadWithOptions() = (%q, %v), want stale-val", string(val), err)
	}

	<-started
	if err := ms.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("background SWR refresh did not observe m.ctx cancellation on Stop()")
	}

	if _, err := ms.GetOrLoadWithOptions(t.Context(), "k", time.Second, func(context.Context) ([]byte, error) {
		return nil, nil
	}); !errors.Is(err, ErrStoreStopped) {
		t.Fatalf("post-Stop GetOrLoadWithOptions = %v, want ErrStoreStopped", err)
	}
	if _, err := ms.GetOrLoadJSONWithOptions[int](t.Context(), "k", time.Second, func(context.Context) (int, error) {
		return 0, nil
	}, nil); !errors.Is(err, ErrStoreStopped) {
		t.Fatalf("post-Stop GetOrLoadJSONWithOptions = %v, want ErrStoreStopped", err)
	}

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/v2/memorystore") {
			t.Errorf("detected leaked goroutine in memorystore:\n%s", buf.String())
		}
	}
}

// ==================== Issue #44: Deterministic TTL Jitter ====================

func TestTTLJitter_DeterministicSeedAndBounds(t *testing.T) {
	newSeeded := func() *MemoryStore {
		ms, err := New(
			WithDefaultTTLJitter(0.25),
			WithTTLJitterSeed(123456789),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		return ms
	}

	ms1 := newSeeded()
	defer func() { _ = ms1.Stop() }()
	ms2 := newSeeded()
	defer func() { _ = ms2.Stop() }()

	baseTTL := 10 * time.Second
	for i := range 50 {
		d1 := ms1.applyJitter(baseTTL, 0.25)
		d2 := ms2.applyJitter(baseTTL, 0.25)
		if d1 != d2 {
			t.Fatalf("iter %d: seeded jitter mismatch %v vs %v", i, d1, d2)
		}
		minExpected := time.Duration(float64(baseTTL) * 0.75)
		maxExpected := time.Duration(float64(baseTTL) * 1.25)
		if d1 < minExpected || d1 > maxExpected {
			t.Fatalf("iter %d: jittered TTL %v outside [%v, %v]", i, d1, minExpected, maxExpected)
		}
	}

	// Minimum 1ns clamp for tiny positive TTLs.
	if d := ms1.applyJitter(1*time.Nanosecond, 0.5); d < 1 {
		t.Fatalf("applyJitter(1ns, 0.5) = %v, want >= 1ns", d)
	}

	// Per-item WithTTLJitter(0) overrides store DefaultTTLJitter(0.5).
	if err := ms1.SetWithOptions("exact", []byte("v"), time.Hour, WithTTLJitter(0)); err != nil {
		t.Fatalf("SetWithOptions() error = %v", err)
	}
	// Verify entry was inserted and is within tight tolerance around 1h.
	if v, ok := ms1.Get("exact"); !ok || string(v) != "v" {
		t.Fatalf("Get(exact) = (%q, %v), want (v, true)", string(v), ok)
	}
}

// ==================== Issue #44: WriteThrough, Async Drops & Reconnect ====================

func TestWriteThroughInvalidation_WarmPeerFallbackAndEchoSuppression(t *testing.T) {
	broker := NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	nodeA, err := New(
		WithPreciseClock(),
		WithPubSub(broker),
		WithInvalidationTopic("wt:topic"),
		WithInvalidationMode(InvalidationModeWriteThrough),
		WithWriteThroughMaxBytes(256),
		WithSyncInvalidation(),
	)
	if err != nil {
		t.Fatalf("New(nodeA) error = %v", err)
	}
	defer func() { _ = nodeA.Stop() }()

	nodeB, err := New(
		WithPreciseClock(),
		WithPubSub(broker),
		WithInvalidationTopic("wt:topic"),
		WithInvalidationMode(InvalidationModeWriteThrough),
		WithWriteThroughMaxBytes(256),
		WithSyncInvalidation(),
	)
	if err != nil {
		t.Fatalf("New(nodeB) error = %v", err)
	}
	defer func() { _ = nodeB.Stop() }()

	// 1. Small Set warms peer nodeB with remaining TTL, Sliding, and StaleWindow.
	if err := nodeA.SetWithOptions("small", []byte("warm-value"), 2*time.Second, WithSlidingExpiration(), WithStaleWindow(500*time.Millisecond)); err != nil {
		t.Fatalf("nodeA.SetWithOptions() error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, ok := nodeB.Get("small"); ok && string(v) == "warm-value" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v, ok := nodeB.Get("small"); !ok || string(v) != "warm-value" {
		t.Fatalf("nodeB did not receive write-through entry: got (%q, %v)", string(v), ok)
	}
	if e := nodeB.lookupEntry("small"); e == nil || !e.isSliding() || e.staleGrace != int64(500*time.Millisecond) {
		t.Fatalf("nodeB write-through entry metadata not preserved: %+v", e)
	}

	// 2. Oversized Set falls back to InvalidateOnly (evicts stale entry on peer).
	if err := nodeB.Set("big", []byte("old"), time.Minute); err != nil {
		t.Fatalf("nodeB.Set(big) error = %v", err)
	}
	time.Sleep(20 * time.Millisecond) // settle nodeB's write-through to nodeA

	oversized := bytes.Repeat([]byte("x"), 512)
	if err := nodeA.Set("big", oversized, time.Minute); err != nil {
		t.Fatalf("nodeA.Set(big) error = %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := nodeB.Get("big"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := nodeB.Get("big"); ok {
		t.Fatal("expected oversized write-through fallback to invalidate nodeB's stale entry")
	}
	// Echo suppression: nodeA still retains its oversized entry.
	if v, ok := nodeA.Get("big"); !ok || !bytes.Equal(v, oversized) {
		t.Fatal("nodeA lost its own oversized entry")
	}

	// 3. SetMulti mixed batch: small keys warm peer, oversized keys invalidate peer.
	_ = nodeB.Set("m-big", []byte("stale-mbig"), time.Minute)
	time.Sleep(20 * time.Millisecond)

	if err := nodeA.SetMulti(map[string][]byte{
		"m-s1":  []byte("v1"),
		"m-s2":  []byte("v2"),
		"m-big": bytes.Repeat([]byte("y"), 400),
	}, time.Minute); err != nil {
		t.Fatalf("nodeA.SetMulti() error = %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v1, ok1 := nodeB.Get("m-s1")
		v2, ok2 := nodeB.Get("m-s2")
		_, okBig := nodeB.Get("m-big")
		if ok1 && string(v1) == "v1" && ok2 && string(v2) == "v2" && !okBig {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v1, ok1 := nodeB.Get("m-s1"); !ok1 || string(v1) != "v1" {
		t.Fatalf("nodeB m-s1 = (%q, %v), want v1", string(v1), ok1)
	}
	if _, okBig := nodeB.Get("m-big"); okBig {
		t.Fatal("expected m-big to fall back to InvalidateOnly")
	}

	// 4. Delete always emits InvalidateOnly even in WriteThrough mode.
	if err := nodeA.Delete("small"); err != nil {
		t.Fatalf("nodeA.Delete() error = %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := nodeB.Get("small"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := nodeB.Get("small"); ok {
		t.Fatal("expected nodeA.Delete to invalidate nodeB")
	}
}

type stalledDropPubSub struct {
	mu      sync.Mutex
	ch      chan []byte
	pubHit  chan struct{}
	pubOnce sync.Once
	closed  bool
}

func (s *stalledDropPubSub) Subscribe(_ context.Context, _ string) (<-chan []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ch = make(chan []byte, 1)
	return s.ch, nil
}

func (s *stalledDropPubSub) Publish(ctx context.Context, _ string, _ []byte) error {
	s.pubOnce.Do(func() { close(s.pubHit) })
	<-ctx.Done()
	return ctx.Err()
}

func (s *stalledDropPubSub) Unsubscribe(_ context.Context, _ string) error { return nil }
func (s *stalledDropPubSub) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch != nil && !s.closed {
		close(s.ch)
		s.closed = true
	}
	return nil
}

type dropSpyRecorder struct {
	spyRecorder
	drops atomic.Int64
}

func (d *dropSpyRecorder) RecordInvalidationDrop(keyCount int) {
	d.drops.Add(int64(keyCount))
}

func TestAsyncInvalidation_StalledPubSubLatencyDropsAndFastStop(t *testing.T) {
	ps := &stalledDropPubSub{pubHit: make(chan struct{})}
	rec := &dropSpyRecorder{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ms, err := New(
		WithPubSub(ps),
		WithInvalidationTopic("stalled:inv"),
		WithAsyncInvalidation(4),
		WithPubSubTimeout(5*time.Second),
		WithMetricsRecorder(rec),
		WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Trigger worker to block inside stalled Publish.
	if err := ms.Set("seed", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set(seed) error = %v", err)
	}
	<-ps.pubHit

	// Flood past queue size (4): Set and Delete must remain non-blocking
	// even though PubSub.Publish is stalled.
	start := time.Now()
	const writes = 200
	for i := range writes {
		if err := ms.Set("hot", []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set #%d error = %v", i, err)
		}
		if err := ms.Delete("hot"); err != nil {
			t.Fatalf("Delete #%d error = %v", i, err)
		}
	}
	elapsed := time.Since(start)
	perOp := elapsed / (2 * writes)
	if perOp > 50*time.Microsecond {
		t.Fatalf("stalled PubSub caused slow Set/Delete: %v/op (total %v)", perOp, elapsed)
	}

	metrics := ms.GetMetrics()
	if metrics.InvalidationDrops == 0 || rec.drops.Load() != metrics.InvalidationDrops {
		t.Fatalf("drops mismatch: StoreMetrics=%d recorder=%d (want > 0)", metrics.InvalidationDrops, rec.drops.Load())
	}

	// Stop() must cancel m.ctx and unblock stalled Publish immediately (<< 5s PubSubTimeout).
	stopStart := time.Now()
	if err := ms.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if d := time.Since(stopStart); d > time.Second {
		t.Fatalf("Stop() took %v waiting on stalled Publish", d)
	}

	if !strings.Contains(logBuf.String(), "invalidation queue full") {
		t.Errorf("expected rate-limited queue drop warning log, got: %s", logBuf.String())
	}
}

type reconnectPubSub struct {
	mu       sync.Mutex
	attempts int
	failFor  int
	ch       chan []byte
	closed   bool
}

func (r *reconnectPubSub) Subscribe(_ context.Context, _ string) (<-chan []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if r.attempts <= r.failFor {
		return nil, errors.New("broker temporarily down")
	}
	r.ch = make(chan []byte, 8)
	return r.ch, nil
}

func (r *reconnectPubSub) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (r *reconnectPubSub) Unsubscribe(_ context.Context, _ string) error       { return nil }
func (r *reconnectPubSub) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ch != nil && !r.closed {
		close(r.ch)
		r.closed = true
	}
	return nil
}

func TestInvalidationBackplane_ExponentialBackoffReconnect(t *testing.T) {
	ps := &reconnectPubSub{failFor: 2}
	ms, err := New(
		WithPubSub(ps),
		WithInvalidationTopic("reconnect:topic"),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	// Initially unhealthy because initial Subscribe failed.
	if err := ms.Health(t.Context()); err == nil {
		t.Fatal("expected initial Health() error after Subscribe failure")
	}

	// Wait for backoff reconnect (100ms + 200ms ~= 300ms) to recover.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ms.Health(t.Context()) == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if err := ms.Health(t.Context()); err != nil {
		t.Fatalf("expected backplane Health() to recover after reconnect, got: %v", err)
	}

	// Verify live invalidation works over the reconnected channel.
	if err := ms.Set("victim", []byte("stale"), time.Minute); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	ps.mu.Lock()
	ch := ps.ch
	ps.mu.Unlock()
	ch <- []byte(`{"o":"remote-node","k":["victim"]}`)

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ms.Get("victim"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := ms.Get("victim"); ok {
		t.Fatal("expected reconnected backplane to process remote invalidation")
	}
}

func BenchmarkAsyncInvalidation_StalledPubSub(b *testing.B) {
	ps := &stalledDropPubSub{pubHit: make(chan struct{})}
	ms, err := New(
		WithPubSub(ps),
		WithInvalidationTopic("bench:stalled"),
		WithAsyncInvalidation(64),
	)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = ms.Stop() }()

	val := []byte("value")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ms.Set("hot", val, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSWR_JSONBackgroundErrorPanicAndStopDrain(t *testing.T) {
	ms, err := New(WithPreciseClock())
	if err != nil {
		t.Fatal(err)
	}
	type doc struct {
		V int `json:"v"`
	}

	if _, err := ms.GetOrLoadJSONWithOptions(context.Background(), "k-swr", 15*time.Millisecond,
		func(context.Context) (doc, error) { return doc{V: 1}, nil },
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)

	// 1. Background loader error keeps stale value intact.
	got, err := ms.GetOrLoadJSONWithOptions(context.Background(), "k-swr", 15*time.Millisecond,
		func(context.Context) (doc, error) { return doc{}, errors.New("upstream down") },
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	)
	if err != nil || got.V != 1 {
		t.Fatalf("stale hit = (%+v, %v), want ({V:1}, nil)", got, err)
	}
	time.Sleep(25 * time.Millisecond)

	// 2. Background loader panic is recovered without crashing or leaking goroutines.
	got, err = ms.GetOrLoadJSONWithOptions(context.Background(), "k-swr", 15*time.Millisecond,
		func(context.Context) (doc, error) { panic("swr-json-panic") },
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	)
	if err != nil || got.V != 1 {
		t.Fatalf("stale hit before panic = (%+v, %v), want ({V:1}, nil)", got, err)
	}
	time.Sleep(25 * time.Millisecond)

	// 3. Background JSON marshal error is handled cleanly.
	type badDoc struct {
		V  int      `json:"v"`
		Ch chan int `json:"ch,omitempty"`
	}
	if err := ms.SetWithOptions("k-bad-json", []byte(`{"v":2}`), 15*time.Millisecond, WithStaleWindow(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	gotBad, err := ms.GetOrLoadJSONWithOptions(context.Background(), "k-bad-json", 15*time.Millisecond,
		func(context.Context) (badDoc, error) { return badDoc{V: 3, Ch: make(chan int)}, nil },
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	)
	if err != nil || gotBad.V != 2 {
		t.Fatalf("stale hit before marshal error = (%+v, %v), want ({V:2}, nil)", gotBad, err)
	}
	time.Sleep(25 * time.Millisecond)

	// 4. Stop() while background JSON refresh returns a value hits setInternal ErrStoreStopped cleanly.
	started := make(chan struct{})
	release := make(chan struct{})
	got, err = ms.GetOrLoadJSONWithOptions(context.Background(), "k-swr", 15*time.Millisecond,
		func(context.Context) (doc, error) {
			close(started)
			<-release
			return doc{V: 99}, nil
		},
		[]LoadOption{WithStaleWhileRevalidate(500 * time.Millisecond)},
	)
	if err != nil || got.V != 1 {
		t.Fatalf("stale hit before stop = (%+v, %v), want ({V:1}, nil)", got, err)
	}
	<-started
	close(release)
	if err := ms.Stop(); err != nil {
		t.Fatal(err)
	}
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/v2/memorystore") {
			t.Errorf("detected leaked goroutine in memorystore:\n%s", buf.String())
		}
	}
}
