package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

// ============================================================================
// F7: Observability, Metrics & Health
// ============================================================================

func TestTier1_F7_GetMetrics_UnboundedAndBounded(t *testing.T) {
	// Unbounded store tracks Items, Bytes, Hits, Misses, and Evictions accurately.
	unbounded := mustNewStore(t)
	if err := unbounded.Set("k1", []byte("val1"), time.Minute); err != nil {
		t.Fatalf("Set(k1) failed: %v", err)
	}
	if err := unbounded.Set("k2", []byte("val22"), time.Minute); err != nil {
		t.Fatalf("Set(k2) failed: %v", err)
	}

	if v, ok := unbounded.Get("k1"); !ok || string(v) != "val1" {
		t.Fatalf("Get(k1) = (%q, %v), want (\"val1\", true)", v, ok)
	}
	if v, ok := unbounded.Get("k2"); !ok || string(v) != "val22" {
		t.Fatalf("Get(k2) = (%q, %v), want (\"val22\", true)", v, ok)
	}
	if _, ok := unbounded.Get("missing"); ok {
		t.Fatal("Get(missing) = true, want false")
	}

	wantBytes := int64(len("k1") + len("val1") + len("k2") + len("val22"))
	m := unbounded.GetMetrics()
	if m.Items != 2 || m.Bytes != wantBytes || m.Hits != 2 || m.Misses != 1 || m.Evictions != 0 {
		t.Fatalf("unbounded GetMetrics() = %+v, want Items=2 Bytes=%d Hits=2 Misses=1 Evictions=0", m, wantBytes)
	}

	// Overwrite k1 with a larger value and verify Bytes updates accurately.
	if err := unbounded.Set("k1", []byte("longer-v1"), time.Minute); err != nil {
		t.Fatalf("overwrite Set(k1) failed: %v", err)
	}
	wantBytesAfter := int64(len("k1") + len("longer-v1") + len("k2") + len("val22"))
	m = unbounded.GetMetrics()
	if m.Items != 2 || m.Bytes != wantBytesAfter {
		t.Fatalf("after overwrite GetMetrics() = %+v, want Items=2 Bytes=%d", m, wantBytesAfter)
	}

	// Bounded store tracks capacity evictions and stays within MaxItems.
	bounded := mustNewStore(t, memorystore.WithMaxItems(2), memorystore.WithPreciseClock())
	for i := range 5 {
		key := fmt.Sprintf("bk-%d", i)
		if err := bounded.Set(key, []byte("payload"), time.Minute); err != nil {
			t.Fatalf("bounded Set(%s) failed: %v", key, err)
		}
	}
	bm := bounded.GetMetrics()
	if bm.Items > 2 {
		t.Fatalf("bounded GetMetrics().Items = %d, want <= 2", bm.Items)
	}
	if bm.Evictions < 3 {
		t.Fatalf("bounded GetMetrics().Evictions = %d, want >= 3", bm.Evictions)
	}
	if bm.Bytes <= 0 {
		t.Fatalf("bounded GetMetrics().Bytes = %d, want > 0", bm.Bytes)
	}
}

func TestTier1_F7_MetricsRecorder_HitMissEvict(t *testing.T) {
	rec := newRecordingMetrics()
	s := mustNewStore(
		t,
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithMaxItems(2),
		memorystore.WithEvictionPolicy(memorystore.EvictionVolatileTTL),
		memorystore.WithCleanupInterval(15*time.Millisecond),
		memorystore.WithPreciseClock(),
	)

	if err := s.Set("short", []byte("1234"), 30*time.Millisecond); err != nil {
		t.Fatalf("Set(short) failed: %v", err)
	}
	if err := s.Set("keep", []byte("5678"), time.Minute); err != nil {
		t.Fatalf("Set(keep) failed: %v", err)
	}

	if _, ok := s.Get("short"); !ok {
		t.Fatal("Get(short) = false, want true")
	}
	if _, ok := s.Get("absent"); ok {
		t.Fatal("Get(absent) = true, want false")
	}

	if rec.Hits() != 1 {
		t.Fatalf("rec.Hits() = %d, want 1", rec.Hits())
	}
	if rec.Misses() != 1 {
		t.Fatalf("rec.Misses() = %d, want 1", rec.Misses())
	}

	// Wait for "short" to be swept as EvictExpired.
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		return rec.EvictsByReason(memorystore.EvictExpired) >= 1
	}, "expected MetricsRecorder to observe EvictExpired")

	// Exceed MaxItems(2) to trigger EvictCapacity.
	for i := range 4 {
		k := fmt.Sprintf("cap-%d", i)
		if err := s.Set(k, []byte("abcdefgh"), time.Minute); err != nil {
			t.Fatalf("Set(%s) failed: %v", k, err)
		}
	}
	if rec.EvictsByReason(memorystore.EvictCapacity) < 1 {
		t.Fatalf("rec.EvictsByReason(EvictCapacity) = %d, want >= 1", rec.EvictsByReason(memorystore.EvictCapacity))
	}
	if rec.Evicts() < 2 {
		t.Fatalf("rec.Evicts() = %d, want >= 2", rec.Evicts())
	}
	if rec.EvictedBytes() <= 0 {
		t.Fatalf("rec.EvictedBytes() = %d, want > 0", rec.EvictedBytes())
	}
}

func TestTier1_F7_MetricsRecorder_LoadAndInvalidation(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	t.Cleanup(func() { _ = ps.Close() })

	rec := newRecordingMetrics()
	s := mustNewStore(
		t,
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f7:inv"),
		memorystore.WithSyncInvalidation(),
	)

	// Coalesce concurrent GetOrLoad calls so at least one follower has shared=true.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	const callers = 4

	for range callers {
		wg.Go(func() {
			val, err := s.GetOrLoad(t.Context(), "load-key", time.Minute, func(_ context.Context) ([]byte, error) {
				once.Do(func() { close(entered) })
				<-release
				return []byte("loaded-val"), nil
			})
			if err != nil {
				t.Errorf("GetOrLoad() error = %v", err)
				return
			}
			if string(val) != "loaded-val" {
				t.Errorf("GetOrLoad() = %q, want %q", val, "loaded-val")
			}
		})
	}

	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if rec.Loads() < 2 {
		t.Fatalf("rec.Loads() = %d, want >= 2", rec.Loads())
	}
	if rec.SharedLoads() < 1 {
		t.Fatalf("rec.SharedLoads() = %d, want >= 1", rec.SharedLoads())
	}
	if rec.FailedLoads() != 0 {
		t.Fatalf("rec.FailedLoads() = %d, want 0", rec.FailedLoads())
	}

	// Verify failed load increments FailedLoads.
	wantErr := errors.New("db error")
	if _, err := s.GetOrLoad(t.Context(), "fail-key", time.Minute, func(_ context.Context) ([]byte, error) {
		return nil, wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("GetOrLoad(fail-key) err = %v, want %v", err, wantErr)
	}
	if rec.FailedLoads() != 1 {
		t.Fatalf("rec.FailedLoads() = %d, want 1", rec.FailedLoads())
	}

	// Verify RecordInvalidation is called on distributed invalidation.
	beforeInv := rec.InvalidationsSent()
	beforeKeys := rec.InvalidatedKeys()
	if err := s.Set("inv-key", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set(inv-key) failed: %v", err)
	}
	if rec.InvalidationsSent() <= beforeInv {
		t.Fatalf("rec.InvalidationsSent() = %d, want > %d", rec.InvalidationsSent(), beforeInv)
	}
	if rec.InvalidatedKeys() <= beforeKeys {
		t.Fatalf("rec.InvalidatedKeys() = %d, want > %d", rec.InvalidatedKeys(), beforeKeys)
	}
}

func TestTier1_F7_InMemoryPubSub_DroppedCount(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	t.Cleanup(func() { _ = ps.Close() })

	if _, err := ps.Subscribe(t.Context(), "drop-topic"); err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}
	if ps.DroppedCount() != 0 {
		t.Fatalf("initial DroppedCount() = %d, want 0", ps.DroppedCount())
	}

	// Publish more messages than subscriber channel buffer without reading.
	for i := range 250 {
		if err := ps.Publish(t.Context(), "drop-topic", fmt.Appendf(nil, "msg-%d", i)); err != nil {
			t.Fatalf("Publish(%d) failed: %v", i, err)
		}
	}

	if ps.DroppedCount() <= 0 {
		t.Fatalf("DroppedCount() = %d, want > 0 when subscriber channel overflows", ps.DroppedCount())
	}
}

func TestTier1_F7_Health_StandaloneAndWithPinger(t *testing.T) {
	standalone := mustNewStore(t)
	if err := standalone.Health(t.Context()); err != nil {
		t.Fatalf("standalone Health() error = %v, want nil", err)
	}

	ps := newControllablePubSub()
	ps.SetPingError(nil)
	withPinger := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f7:health"),
	)
	if err := withPinger.Health(t.Context()); err != nil {
		t.Fatalf("withPinger Health() error = %v, want nil", err)
	}
}

// ============================================================================
// F8: Bounded Capacity & Pluggable Eviction
// ============================================================================

func TestTier1_F8_MaxItems_BoundsAndOnEvict(t *testing.T) {
	var mu sync.Mutex
	var capEvicted []string

	s := mustNewStore(
		t,
		memorystore.WithMaxItems(5),
		memorystore.WithOnEvict(func(key string, _ []byte, reason memorystore.EvictReason) {
			if reason == memorystore.EvictCapacity {
				mu.Lock()
				capEvicted = append(capEvicted, key)
				mu.Unlock()
			}
		}),
	)

	for i := range 20 {
		k := fmt.Sprintf("item-%02d", i)
		if err := s.Set(k, []byte("val"), time.Minute); err != nil {
			t.Fatalf("Set(%s) failed: %v", k, err)
		}
	}

	if m := s.GetMetrics(); m.Items > 5 {
		t.Fatalf("GetMetrics().Items = %d, want <= 5", m.Items)
	}

	mu.Lock()
	evictCount := len(capEvicted)
	mu.Unlock()
	if evictCount < 15 {
		t.Fatalf("EvictCapacity callback count = %d, want >= 15", evictCount)
	}
}

func TestTier1_F8_MaxBytes_BoundsPayload(t *testing.T) {
	s := mustNewStore(t, memorystore.WithMaxBytes(200))

	payload := bytes.Repeat([]byte("x"), 40)
	for i := range 20 {
		k := fmt.Sprintf("k%02d", i)
		if err := s.Set(k, payload, time.Minute); err != nil {
			t.Fatalf("Set(%s) failed: %v", k, err)
		}
	}

	m := s.GetMetrics()
	if m.Bytes > 200 {
		t.Fatalf("GetMetrics().Bytes = %d, want <= 200", m.Bytes)
	}
	if m.Evictions == 0 {
		t.Fatal("GetMetrics().Evictions = 0, want > 0")
	}
}

func TestTier1_F8_VolatileTTL_EvictsShortestTTLFirst(t *testing.T) {
	const capItems = 512
	s := mustNewStore(
		t,
		memorystore.WithMaxItems(capItems),
		memorystore.WithEvictionPolicy(memorystore.EvictionVolatileTTL),
		memorystore.WithPreciseClock(),
	)

	// Populate capacity evenly with long-TTL and short-TTL keys.
	const half = capItems / 2
	for i := range half {
		if err := s.Set(fmt.Sprintf("long-%04d", i), []byte("v"), 10*time.Minute); err != nil {
			t.Fatalf("Set(long-%d) failed: %v", i, err)
		}
		if err := s.Set(fmt.Sprintf("short-%04d", i), []byte("v"), 5*time.Second); err != nil {
			t.Fatalf("Set(short-%d) failed: %v", i, err)
		}
	}

	// Insert additional very-long-TTL entries to force capacity eviction.
	for i := range half {
		if err := s.Set(fmt.Sprintf("vlong-%04d", i), []byte("v"), 20*time.Minute); err != nil {
			t.Fatalf("Set(vlong-%d) failed: %v", i, err)
		}
	}

	shortSurvived := 0
	longSurvived := 0
	for i := range half {
		if _, ok := s.Get(fmt.Sprintf("short-%04d", i)); ok {
			shortSurvived++
		}
		if _, ok := s.Get(fmt.Sprintf("long-%04d", i)); ok {
			longSurvived++
		}
	}

	if longSurvived <= shortSurvived {
		t.Fatalf("EvictionVolatileTTL kept long=%d vs short=%d, expected long > short", longSurvived, shortSurvived)
	}
}

func runZipfScanWorkload(t *testing.T, s *memorystore.MemoryStore) (hotKept int, totalHot int) {
	t.Helper()
	samples := zipfKeys(42, 0.99, 2000, 10000)
	counts := make(map[string]int, 2000)
	for _, k := range samples {
		counts[k]++
		if _, ok := s.Get(k); !ok {
			if err := s.Set(k, []byte("val"), time.Minute); err != nil {
				t.Fatalf("Set(%s) failed: %v", k, err)
			}
		}
	}

	// Scan 500 cold one-hit-wonder keys after the Zipfian phase.
	for i := range 500 {
		if err := s.Set(fmt.Sprintf("cold-scan-%04d", i), []byte("scan"), time.Minute); err != nil {
			t.Fatalf("Set(cold-scan-%d) failed: %v", i, err)
		}
	}

	type kv struct {
		key   string
		count int
	}
	ranked := make([]kv, 0, len(counts))
	for k, c := range counts {
		ranked = append(ranked, kv{key: k, count: c})
	}
	slices.SortFunc(ranked, func(a, b kv) int {
		if a.count != b.count {
			return b.count - a.count
		}
		if a.key < b.key {
			return -1
		}
		if a.key > b.key {
			return 1
		}
		return 0
	})

	topN := min(20, len(ranked))
	kept := 0
	for i := range topN {
		if _, ok := s.Get(ranked[i].key); ok {
			kept++
		}
	}
	return kept, topN
}

func TestTier1_F8_S3FIFO_DefaultKeepsHotZipfianWorkingSet(t *testing.T) {
	// Bounded store without WithEvictionPolicy defaults to EvictionS3FIFO.
	s := mustNewStore(t, memorystore.WithMaxItems(200))
	kept, topN := runZipfScanWorkload(t, s)
	if kept < topN/2 {
		t.Fatalf("default S3FIFO kept %d/%d hottest Zipfian keys across scan, want >= %d", kept, topN, topN/2)
	}
}

func TestTier1_F8_TinyLFU_KeepsHotZipfianWorkingSet(t *testing.T) {
	s := mustNewStore(
		t,
		memorystore.WithMaxItems(200),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
	)
	kept, topN := runZipfScanWorkload(t, s)
	if kept < topN/2 {
		t.Fatalf("TinyLFU kept %d/%d hottest Zipfian keys across scan, want >= %d", kept, topN, topN/2)
	}
}

// ============================================================================
// F9: Stale-While-Revalidate (SWR)
// ============================================================================

func TestTier1_F9_GetOrLoadWithOptions_ServesStaleAndRefreshes(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	val, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-bytes",
		40*time.Millisecond,
		func(_ context.Context) ([]byte, error) {
			return []byte("v1"), nil
		},
		memorystore.WithStaleWhileRevalidate(500*time.Millisecond),
	)
	if err != nil || string(val) != "v1" {
		t.Fatalf("initial GetOrLoadWithOptions() = (%q, %v), want (\"v1\", nil)", val, err)
	}

	// Wait until soft TTL expires so Get misses while stale window remains active.
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s.Get("swr-bytes")
		return !ok
	}, "expected key to reach soft expiration")

	refreshDone := make(chan struct{})
	var calls atomic.Int32
	staleVal, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-bytes",
		time.Minute,
		func(_ context.Context) ([]byte, error) {
			if calls.Add(1) == 1 {
				defer close(refreshDone)
			}
			return []byte("v2"), nil
		},
		memorystore.WithStaleWhileRevalidate(500*time.Millisecond),
	)
	if err != nil || string(staleVal) != "v1" {
		t.Fatalf("stale GetOrLoadWithOptions() = (%q, %v), want (\"v1\", nil)", staleVal, err)
	}

	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for background SWR refresh")
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s.Get("swr-bytes")
		return ok && string(v) == "v2"
	}, "expected background SWR refresh to store v2")
}

func TestTier1_F9_GetOrLoadJSONWithOptions_ServesStaleAndRefreshes(t *testing.T) {
	type swrDoc struct {
		Version int    `json:"version"`
		Label   string `json:"label"`
	}

	s := mustNewStore(t, memorystore.WithPreciseClock())
	loadOpts := []memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(500 * time.Millisecond)}

	doc, err := s.GetOrLoadJSONWithOptions[swrDoc](
		t.Context(),
		"swr-json",
		40*time.Millisecond,
		func(_ context.Context) (swrDoc, error) {
			return swrDoc{Version: 1, Label: "initial"}, nil
		},
		loadOpts,
	)
	if err != nil || doc.Version != 1 {
		t.Fatalf("initial GetOrLoadJSONWithOptions() = (%+v, %v), want Version=1", doc, err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s.Get("swr-json")
		return !ok
	}, "expected swr-json to reach soft expiration")

	staleDoc, err := s.GetOrLoadJSONWithOptions[swrDoc](
		t.Context(),
		"swr-json",
		time.Minute,
		func(_ context.Context) (swrDoc, error) {
			return swrDoc{Version: 2, Label: "refreshed"}, nil
		},
		loadOpts,
	)
	if err != nil || staleDoc.Version != 1 || staleDoc.Label != "initial" {
		t.Fatalf("stale GetOrLoadJSONWithOptions() = (%+v, %v), want Version=1 Label=\"initial\"", staleDoc, err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		got, ok, getErr := s.GetJSONAs[swrDoc]("swr-json")
		return getErr == nil && ok && got.Version == 2 && got.Label == "refreshed"
	}, "expected background JSON SWR refresh to populate Version=2")
}

func TestTier1_F9_Get_TreatsStaleAsMiss(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	if _, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-get-miss",
		40*time.Millisecond,
		func(_ context.Context) ([]byte, error) {
			return []byte("initial"), nil
		},
		memorystore.WithStaleWhileRevalidate(800*time.Millisecond),
	); err != nil {
		t.Fatalf("initial GetOrLoadWithOptions() failed: %v", err)
	}

	if v, ok := s.Get("swr-get-miss"); !ok || string(v) != "initial" {
		t.Fatalf("fresh Get() = (%q, %v), want (\"initial\", true)", v, ok)
	}

	// Wait until softExpiresAt passes: Get() must return (nil, false).
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s.Get("swr-get-miss")
		return !ok && v == nil
	}, "expected Get() to treat stale entry as miss (nil, false)")

	// Verify GetOrLoadWithOptions still serves stale immediately while loader is blocked.
	gate := make(chan struct{})
	val, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-get-miss",
		time.Minute,
		func(_ context.Context) ([]byte, error) {
			<-gate
			return []byte("reloaded"), nil
		},
		memorystore.WithStaleWhileRevalidate(800*time.Millisecond),
	)
	if err != nil || string(val) != "initial" {
		close(gate)
		t.Fatalf("GetOrLoadWithOptions() during stale window = (%q, %v), want (\"initial\", nil)", val, err)
	}
	close(gate)

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s.Get("swr-get-miss")
		return ok && string(v) == "reloaded"
	}, "expected background refresh after gate release")
}

func TestTier1_F9_DefaultStaleWhileRevalidate_AppliesToStandardGetOrLoad(t *testing.T) {
	type item struct {
		Val string `json:"val"`
	}

	s := mustNewStore(
		t,
		memorystore.WithPreciseClock(),
		memorystore.WithDefaultStaleWhileRevalidate(600*time.Millisecond),
	)

	// 1. Standard GetOrLoad inherits store-level SWR grace.
	b, err := s.GetOrLoad(t.Context(), "def-bytes", 40*time.Millisecond, func(_ context.Context) ([]byte, error) {
		return []byte("b1"), nil
	})
	if err != nil || string(b) != "b1" {
		t.Fatalf("initial GetOrLoad() = (%q, %v), want (\"b1\", nil)", b, err)
	}

	// 2. Standard GetOrLoadJSON[T] inherits store-level SWR grace.
	j, err := s.GetOrLoadJSON[item](t.Context(), "def-json", 40*time.Millisecond, func(_ context.Context) (item, error) {
		return item{Val: "j1"}, nil
	})
	if err != nil || j.Val != "j1" {
		t.Fatalf("initial GetOrLoadJSON() = (%+v, %v), want Val=\"j1\"", j, err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok1 := s.Get("def-bytes")
		_, ok2 := s.Get("def-json")
		return !ok1 && !ok2
	}, "expected both keys to reach soft expiration")

	staleB, err := s.GetOrLoad(t.Context(), "def-bytes", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("b2"), nil
	})
	if err != nil || string(staleB) != "b1" {
		t.Fatalf("stale GetOrLoad() = (%q, %v), want (\"b1\", nil)", staleB, err)
	}

	staleJ, err := s.GetOrLoadJSON[item](t.Context(), "def-json", time.Minute, func(_ context.Context) (item, error) {
		return item{Val: "j2"}, nil
	})
	if err != nil || staleJ.Val != "j1" {
		t.Fatalf("stale GetOrLoadJSON() = (%+v, %v), want Val=\"j1\"", staleJ, err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		vb, ok1 := s.Get("def-bytes")
		vj, ok2, err2 := s.GetJSONAs[item]("def-json")
		return ok1 && string(vb) == "b2" && err2 == nil && ok2 && vj.Val == "j2"
	}, "expected background refreshes for GetOrLoad and GetOrLoadJSON")
}

func TestTier1_F9_FailedBackgroundRefreshPreservesStaleValue(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	if _, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-fail-preserve",
		40*time.Millisecond,
		func(_ context.Context) ([]byte, error) {
			return []byte("stale-ok"), nil
		},
		memorystore.WithStaleWhileRevalidate(800*time.Millisecond),
	); err != nil {
		t.Fatalf("initial GetOrLoadWithOptions() failed: %v", err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s.Get("swr-fail-preserve")
		return !ok
	}, "expected key to become stale")

	var failRan atomic.Bool
	val1, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-fail-preserve",
		time.Minute,
		func(_ context.Context) ([]byte, error) {
			failRan.Store(true)
			return nil, errors.New("upstream unavailable")
		},
		memorystore.WithStaleWhileRevalidate(800*time.Millisecond),
	)
	if err != nil || string(val1) != "stale-ok" {
		t.Fatalf("first stale call = (%q, %v), want (\"stale-ok\", nil)", val1, err)
	}

	eventually(t, time.Second, 5*time.Millisecond, failRan.Load, "expected failing background refresh to run")

	// Stale value must still be preserved inside hardExpiresAt after failed refresh.
	val2, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-fail-preserve",
		time.Minute,
		func(_ context.Context) ([]byte, error) {
			return []byte("recovered"), nil
		},
		memorystore.WithStaleWhileRevalidate(800*time.Millisecond),
	)
	if err != nil || string(val2) != "stale-ok" {
		t.Fatalf("second stale call after failed refresh = (%q, %v), want (\"stale-ok\", nil)", val2, err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s.Get("swr-fail-preserve")
		return ok && string(v) == "recovered"
	}, "expected subsequent recovery refresh to succeed")
}

func TestTier1_F9_SetWithOptions_WithStaleWindow(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	if err := s.SetWithOptions(
		"swr-set-window",
		[]byte("seed-v1"),
		40*time.Millisecond,
		memorystore.WithStaleWindow(600*time.Millisecond),
	); err != nil {
		t.Fatalf("SetWithOptions(WithStaleWindow) error = %v", err)
	}

	if v, ok := s.Get("swr-set-window"); !ok || string(v) != "seed-v1" {
		t.Fatalf("fresh Get() = (%q, %v), want (\"seed-v1\", true)", v, ok)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s.Get("swr-set-window")
		return !ok
	}, "expected entry written with WithStaleWindow to reach soft expiration")

	refreshDone := make(chan struct{})
	var calls atomic.Int32
	staleVal, err := s.GetOrLoad(t.Context(), "swr-set-window", time.Minute, func(_ context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			defer close(refreshDone)
		}
		return []byte("refreshed-v2"), nil
	})
	if err != nil || string(staleVal) != "seed-v1" {
		t.Fatalf("stale GetOrLoad() = (%q, %v), want (\"seed-v1\", nil)", staleVal, err)
	}

	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for background SWR refresh triggered by WithStaleWindow entry")
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s.Get("swr-set-window")
		return ok && string(v) == "refreshed-v2"
	}, "expected background refresh to store refreshed-v2")
}

// ============================================================================
// F10: TTL Jitter
// ============================================================================

func measureExpirations(t *testing.T, s *memorystore.MemoryStore, keys []string, setFn func(key string), pollTimeout time.Duration) (minExp, maxExp time.Duration) {
	t.Helper()
	start := time.Now()
	for _, k := range keys {
		setFn(k)
	}

	expirations := make(map[string]time.Duration, len(keys))
	deadline := time.Now().Add(pollTimeout)
	for len(expirations) < len(keys) && time.Now().Before(deadline) {
		for _, k := range keys {
			if _, recorded := expirations[k]; recorded {
				continue
			}
			if _, ok := s.Get(k); !ok {
				expirations[k] = time.Since(start)
			}
		}
		if len(expirations) < len(keys) {
			time.Sleep(5 * time.Millisecond)
		}
	}

	if len(expirations) != len(keys) {
		t.Fatalf("only %d/%d keys expired within %v", len(expirations), len(keys), pollTimeout)
	}

	minExp = time.Hour
	for _, d := range expirations {
		if d < minExp {
			minExp = d
		}
		if d > maxExp {
			maxExp = d
		}
	}
	return minExp, maxExp
}

func TestTier1_F10_DefaultTTLJitter_DistributesExpirations(t *testing.T) {
	const baseTTL = 300 * time.Millisecond
	s := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithDefaultTTLJitter(0.3))

	keys := make([]string, 30)
	for i := range keys {
		keys[i] = fmt.Sprintf("jit-def-%02d", i)
	}

	minExp, maxExp := measureExpirations(t, s, keys, func(k string) {
		if err := s.Set(k, []byte("v"), baseTTL); err != nil {
			t.Fatalf("Set(%s) failed: %v", k, err)
		}
	}, time.Second)

	// Lower bound of [0.7*300ms, 1.3*300ms] is 210ms.
	if minExp < 200*time.Millisecond {
		t.Fatalf("minExp = %v, expected >= 200ms (0.7 * %v)", minExp, baseTTL)
	}
	if maxExp-minExp < 40*time.Millisecond {
		t.Fatalf("expiration spread max-min = %v (%v..%v), expected >= 40ms spread across [0.7, 1.3]*TTL", maxExp-minExp, minExp, maxExp)
	}
}

func TestTier1_F10_SetWithOptions_PerItemJitter(t *testing.T) {
	const baseTTL = 300 * time.Millisecond
	s := mustNewStore(t, memorystore.WithPreciseClock())

	keys := make([]string, 30)
	for i := range keys {
		keys[i] = fmt.Sprintf("jit-item-%02d", i)
	}

	minExp, maxExp := measureExpirations(t, s, keys, func(k string) {
		if err := s.SetWithOptions(k, []byte("v"), baseTTL, memorystore.WithTTLJitter(0.25)); err != nil {
			t.Fatalf("SetWithOptions(%s) failed: %v", k, err)
		}
	}, time.Second)

	// Lower bound of [0.75*300ms, 1.25*300ms] is 225ms.
	if minExp < 215*time.Millisecond {
		t.Fatalf("minExp = %v, expected >= 215ms (0.75 * %v)", minExp, baseTTL)
	}
	if maxExp-minExp < 30*time.Millisecond {
		t.Fatalf("per-item jitter spread max-min = %v (%v..%v), expected >= 30ms", maxExp-minExp, minExp, maxExp)
	}
}

func TestTier1_F10_PerItemZeroOverridesGlobalJitter(t *testing.T) {
	const baseTTL = 150 * time.Millisecond
	s := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithDefaultTTLJitter(0.4))

	keys := make([]string, 20)
	for i := range keys {
		keys[i] = fmt.Sprintf("jit-zero-ov-%02d", i)
	}

	minExp, maxExp := measureExpirations(t, s, keys, func(k string) {
		if err := s.SetWithOptions(k, []byte("v"), baseTTL, memorystore.WithTTLJitter(0.0)); err != nil {
			t.Fatalf("SetWithOptions(%s) failed: %v", k, err)
		}
	}, time.Second)

	if minExp < 140*time.Millisecond {
		t.Fatalf("minExp = %v, expected >= 140ms when per-item jitter is 0.0", minExp)
	}
	if maxExp-minExp > 40*time.Millisecond {
		t.Fatalf("spread = %v (%v..%v), expected synchronized expiry when per-item WithTTLJitter(0.0) overrides global", maxExp-minExp, minExp, maxExp)
	}
}

func TestTier1_F10_PerItemLargerOverridesSmallerGlobalJitter(t *testing.T) {
	const baseTTL = 400 * time.Millisecond
	// Global 0.05 has max theoretical span 0.1*400ms = 40ms; per-item 0.4 spans 320ms.
	s := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithDefaultTTLJitter(0.05))

	keys := make([]string, 40)
	for i := range keys {
		keys[i] = fmt.Sprintf("jit-big-ov-%02d", i)
	}

	minExp, maxExp := measureExpirations(t, s, keys, func(k string) {
		if err := s.SetWithOptions(k, []byte("v"), baseTTL, memorystore.WithTTLJitter(0.4)); err != nil {
			t.Fatalf("SetWithOptions(%s) failed: %v", k, err)
		}
	}, 1500*time.Millisecond)

	if maxExp-minExp < 80*time.Millisecond {
		t.Fatalf("spread = %v (%v..%v), expected >= 80ms proving per-item 0.4 overrode global 0.05", maxExp-minExp, minExp, maxExp)
	}
}

func TestTier1_F10_DefaultZeroDisablesJitter(t *testing.T) {
	const baseTTL = 120 * time.Millisecond
	s := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithDefaultTTLJitter(0.0))

	keys := make([]string, 20)
	for i := range keys {
		keys[i] = fmt.Sprintf("jit-disabled-%02d", i)
	}

	minExp, maxExp := measureExpirations(t, s, keys, func(k string) {
		if err := s.Set(k, []byte("v"), baseTTL); err != nil {
			t.Fatalf("Set(%s) failed: %v", k, err)
		}
	}, time.Second)

	if minExp < 110*time.Millisecond {
		t.Fatalf("minExp = %v, expected >= 110ms with jitter disabled", minExp)
	}
	if maxExp-minExp > 40*time.Millisecond {
		t.Fatalf("spread = %v (%v..%v), expected deterministic expiry with WithDefaultTTLJitter(0.0)", maxExp-minExp, minExp, maxExp)
	}
}

func TestTier1_F10_WithTTLJitterSeed_Deterministic(t *testing.T) {
	const baseTTL = 200 * time.Millisecond
	const jitterFraction = 0.4

	keys := make([]string, 8)
	for i := range keys {
		keys[i] = fmt.Sprintf("jit-seed-%02d", i)
	}

	measureForSeed := func(seed uint64) []time.Duration {
		s := mustNewStore(
			t,
			memorystore.WithPreciseClock(),
			memorystore.WithDefaultTTLJitter(jitterFraction),
			memorystore.WithTTLJitterSeed(seed),
		)
		start := time.Now()
		for _, k := range keys {
			if err := s.Set(k, []byte("v"), baseTTL); err != nil {
				t.Fatalf("Set(%s) failed: %v", k, err)
			}
		}

		expirations := make([]time.Duration, len(keys))
		recorded := make([]bool, len(keys))
		remaining := len(keys)
		deadline := time.Now().Add(time.Second)
		for remaining > 0 && time.Now().Before(deadline) {
			for i, k := range keys {
				if recorded[i] {
					continue
				}
				if _, ok := s.Get(k); !ok {
					expirations[i] = time.Since(start)
					recorded[i] = true
					remaining--
				}
			}
			if remaining > 0 {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if remaining > 0 {
			t.Fatalf("seed %d: %d keys did not expire within 1s", seed, remaining)
		}
		return expirations
	}

	expA1 := measureForSeed(42)
	expA2 := measureForSeed(42)
	expB := measureForSeed(999)

	for i := range keys {
		diff := expA1[i] - expA2[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > 30*time.Millisecond {
			t.Fatalf("key %s: same seed=42 produced divergent expirations %v vs %v (diff=%v > 30ms)", keys[i], expA1[i], expA2[i], diff)
		}
	}

	var maxDiffOtherSeed time.Duration
	for i := range keys {
		diff := expA1[i] - expB[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > maxDiffOtherSeed {
			maxDiffOtherSeed = diff
		}
	}
	if maxDiffOtherSeed < 25*time.Millisecond {
		t.Fatalf("seed=42 (%v) vs seed=999 (%v) max per-key diff = %v, expected >= 25ms divergence across distinct seeds", expA1, expB, maxDiffOtherSeed)
	}
}

// ============================================================================
// F11: Distributed Invalidation, Write-Through & Resilient Backplane
// ============================================================================

func TestTier1_F11_InvalidateOnly_PeerInvalidatedAndEchoSuppressed(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	t.Cleanup(func() { _ = ps.Close() })

	s1 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f11:inv"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeInvalidateOnly),
		memorystore.WithSyncInvalidation(),
	)
	s2 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f11:inv"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeInvalidateOnly),
		memorystore.WithSyncInvalidation(),
	)

	// Populate keys on peer s2 first.
	if err := s2.Set("shared-1", []byte("old"), time.Minute); err != nil {
		t.Fatalf("s2.Set(shared-1) failed: %v", err)
	}
	if err := s2.Set("shared-2", []byte("to-delete"), time.Minute); err != nil {
		t.Fatalf("s2.Set(shared-2) failed: %v", err)
	}

	// s1.Set invalidates s2 while suppressing echo on s1.
	if err := s1.Set("shared-1", []byte("new"), time.Minute); err != nil {
		t.Fatalf("s1.Set(shared-1) failed: %v", err)
	}
	if v, ok := s1.Get("shared-1"); !ok || string(v) != "new" {
		t.Fatalf("origin s1.Get(shared-1) = (%q, %v), want (\"new\", true) (echo suppressed)", v, ok)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s2.Get("shared-1")
		return !ok
	}, "expected s2 shared-1 to be invalidated after s1.Set")

	// s1.Delete invalidates s2.
	if err := s1.Delete("shared-2"); err != nil {
		t.Fatalf("s1.Delete(shared-2) failed: %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s2.Get("shared-2")
		return !ok
	}, "expected s2 shared-2 to be invalidated after s1.Delete")
}

func TestTier1_F11_WriteThrough_PropagatesValueAndRemainingTTL(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	t.Cleanup(func() { _ = ps.Close() })

	s1 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f11:wt"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithSyncInvalidation(),
		memorystore.WithPreciseClock(),
	)
	s2 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f11:wt"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithSyncInvalidation(),
		memorystore.WithPreciseClock(),
	)

	if err := s1.Set("wt-key", []byte("wt-val"), 200*time.Millisecond); err != nil {
		t.Fatalf("s1.Set(wt-key) failed: %v", err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s2.Get("wt-key")
		return ok && string(v) == "wt-val"
	}, "expected WriteThrough to populate wt-key on peer s2")

	// Origin s1 also retains the value.
	if v, ok := s1.Get("wt-key"); !ok || string(v) != "wt-val" {
		t.Fatalf("s1.Get(wt-key) = (%q, %v), want (\"wt-val\", true)", v, ok)
	}

	// Remaining TTL propagated to s2 expires naturally.
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		_, ok := s2.Get("wt-key")
		return !ok
	}, "expected WriteThrough entry on peer s2 to expire according to remaining TTL")
}

func TestTier1_F11_WriteThrough_PropagatesSetMultiBatch(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	t.Cleanup(func() { _ = ps.Close() })

	s1 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f11:wt-batch"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithSyncInvalidation(),
	)
	s2 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f11:wt-batch"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithSyncInvalidation(),
	)

	batch := map[string][]byte{
		"b1": []byte("one"),
		"b2": []byte("two"),
		"b3": []byte("three"),
	}
	if err := s1.SetMulti(batch, time.Minute); err != nil {
		t.Fatalf("s1.SetMulti() failed: %v", err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		got := s2.GetMulti([]string{"b1", "b2", "b3"})
		return string(got["b1"]) == "one" && string(got["b2"]) == "two" && string(got["b3"]) == "three"
	}, "expected WriteThrough SetMulti batch to propagate to peer s2")
}

func TestTier1_F11_AsyncInvalidation_NonBlockingOnSlowPublish(t *testing.T) {
	ps := newControllablePubSub()
	ps.SetPublishDelay(200 * time.Millisecond)

	s := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f11:async"),
		memorystore.WithAsyncInvalidation(64),
	)

	start := time.Now()
	if err := s.Set("async-k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set(async-k) failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("Set(async-k) blocked for %v despite WithAsyncInvalidation, want < 50ms", elapsed)
	}

	eventually(t, 2*time.Second, 10*time.Millisecond, func() bool {
		return ps.PublishCount() >= 1
	}, "expected async invalidation worker to publish to PubSubClient")
	ps.SetPublishDelay(0)
}

func TestTier1_F11_UserPubSub_SubscribePublishUnsubscribeCount(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	t.Cleanup(func() { _ = ps.Close() })

	s1 := mustNewStore(t, memorystore.WithPubSub(ps))
	s2 := mustNewStore(t, memorystore.WithPubSub(ps))

	ch, err := s2.Subscribe("events:orders")
	if err != nil {
		t.Fatalf("s2.Subscribe() failed: %v", err)
	}

	if got := s1.SubscriberCount("events:orders"); got != 1 {
		t.Fatalf("s1.SubscriberCount() = %d, want 1", got)
	}
	if got := s2.SubscriberCount("events:orders"); got != 1 {
		t.Fatalf("s2.SubscriberCount() = %d, want 1", got)
	}

	if err := s1.Publish("events:orders", []byte("order-123")); err != nil {
		t.Fatalf("s1.Publish() failed: %v", err)
	}

	select {
	case msg := <-ch:
		if string(msg) != "order-123" {
			t.Fatalf("received payload = %q, want \"order-123\"", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published message")
	}

	if err := s2.Unsubscribe("events:orders"); err != nil {
		t.Fatalf("s2.Unsubscribe() failed: %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return s1.SubscriberCount("events:orders") == 0
	}, "expected SubscriberCount to drop to 0 after Unsubscribe")
}

// ============================================================================
// F12: Streaming Snapshot Persistence
// ============================================================================

func TestTier1_F12_SaveLoad_BinaryRoundtripAndNoExpiration(t *testing.T) {
	s1 := mustNewStore(t)
	if err := s1.Set("perm", []byte("forever"), memorystore.NoExpiration); err != nil {
		t.Fatalf("Set(perm) failed: %v", err)
	}
	if err := s1.Set("ttl", []byte("temp"), time.Minute); err != nil {
		t.Fatalf("Set(ttl) failed: %v", err)
	}
	binVal := []byte{0x00, 0x01, 0xfe, 0xff}
	if err := s1.Set("bin", binVal, time.Minute); err != nil {
		t.Fatalf("Set(bin) failed: %v", err)
	}

	var buf bytes.Buffer
	if err := s1.Save(&buf); err != nil {
		t.Fatalf("s1.Save() failed: %v", err)
	}

	s2 := mustNewStore(t, memorystore.WithCleanupInterval(15*time.Millisecond), memorystore.WithPreciseClock())
	if err := s2.Load(&buf); err != nil {
		t.Fatalf("s2.Load() failed: %v", err)
	}

	if v, ok := s2.Get("ttl"); !ok || string(v) != "temp" {
		t.Fatalf("s2.Get(ttl) = (%q, %v), want (\"temp\", true)", v, ok)
	}
	if v, ok := s2.Get("bin"); !ok || !bytes.Equal(v, binVal) {
		t.Fatalf("s2.Get(bin) = (%x, %v), want (%x, true)", v, ok, binVal)
	}

	// Wait past multiple cleanup cycles to verify NoExpiration survived roundtrip.
	time.Sleep(40 * time.Millisecond)
	if v, ok := s2.Get("perm"); !ok || string(v) != "forever" {
		t.Fatalf("s2.Get(perm) after cleanup = (%q, %v), want (\"forever\", true)", v, ok)
	}
}

func TestTier1_F12_SaveLoad_JSONRoundtripAndRelativeTTL(t *testing.T) {
	s1 := mustNewStore(t, memorystore.WithPreciseClock())
	if err := s1.Set("j-short", []byte("ephemeral"), 180*time.Millisecond); err != nil {
		t.Fatalf("Set(j-short) failed: %v", err)
	}
	if err := s1.Set("j-long", []byte("durable"), time.Minute); err != nil {
		t.Fatalf("Set(j-long) failed: %v", err)
	}
	if err := s1.Set("j-perm", []byte("always"), memorystore.NoExpiration); err != nil {
		t.Fatalf("Set(j-perm) failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	var buf bytes.Buffer
	if err := s1.Save(&buf, memorystore.WithSnapshotFormat(memorystore.SnapshotJSON)); err != nil {
		t.Fatalf("s1.Save(SnapshotJSON) failed: %v", err)
	}

	s2 := mustNewStore(t, memorystore.WithPreciseClock())
	if err := s2.Load(&buf, memorystore.WithSnapshotFormat(memorystore.SnapshotJSON)); err != nil {
		t.Fatalf("s2.Load(SnapshotJSON) failed: %v", err)
	}

	if v, ok := s2.Get("j-short"); !ok || string(v) != "ephemeral" {
		t.Fatalf("s2.Get(j-short) = (%q, %v), want (\"ephemeral\", true)", v, ok)
	}
	if v, ok := s2.Get("j-long"); !ok || string(v) != "durable" {
		t.Fatalf("s2.Get(j-long) = (%q, %v), want (\"durable\", true)", v, ok)
	}
	if v, ok := s2.Get("j-perm"); !ok || string(v) != "always" {
		t.Fatalf("s2.Get(j-perm) = (%q, %v), want (\"always\", true)", v, ok)
	}

	// Remaining relative TTL of j-short (~130ms) should expire promptly in s2.
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		_, ok := s2.Get("j-short")
		return !ok
	}, "expected j-short to expire in s2 according to preserved remaining TTL")

	if _, ok := s2.Get("j-long"); !ok {
		t.Fatal("expected j-long to remain valid in s2")
	}
	if _, ok := s2.Get("j-perm"); !ok {
		t.Fatal("expected j-perm (NoExpiration) to remain valid in s2")
	}
}

func TestTier1_F12_SaveLoad_GzipCompressedRoundtrip(t *testing.T) {
	s1 := mustNewStore(t)
	for i := range 50 {
		k := fmt.Sprintf("gz-%02d", i)
		v := fmt.Appendf(nil, "payload-%02d", i)
		if err := s1.Set(k, v, time.Minute); err != nil {
			t.Fatalf("Set(%s) failed: %v", k, err)
		}
	}

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	if err := s1.Save(gzw); err != nil {
		t.Fatalf("s1.Save(gzip) failed: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzw.Close() failed: %v", err)
	}

	gzr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip.NewReader() failed: %v", err)
	}
	s2 := mustNewStore(t)
	if err := s2.Load(gzr); err != nil {
		_ = gzr.Close()
		t.Fatalf("s2.Load(gzip) failed: %v", err)
	}
	if err := gzr.Close(); err != nil {
		t.Fatalf("gzr.Close() failed: %v", err)
	}

	for i := range 50 {
		k := fmt.Sprintf("gz-%02d", i)
		want := fmt.Sprintf("payload-%02d", i)
		if v, ok := s2.Get(k); !ok || string(v) != want {
			t.Fatalf("s2.Get(%s) = (%q, %v), want (%q, true)", k, v, ok, want)
		}
	}
}

type slowEOFReader struct {
	r        io.Reader
	eofDelay time.Duration
	delayed  bool
}

func (s *slowEOFReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if errors.Is(err, io.EOF) && !s.delayed {
		s.delayed = true
		time.Sleep(s.eofDelay)
	}
	return n, err
}

func TestTier1_F12_Load_SkipsExpiredEntries(t *testing.T) {
	s1 := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithCleanupInterval(time.Hour))
	if err := s1.Set("expired-before-save", []byte("gone1"), 20*time.Millisecond); err != nil {
		t.Fatalf("Set(expired-before-save) failed: %v", err)
	}
	if err := s1.Set("still-valid", []byte("kept"), time.Minute); err != nil {
		t.Fatalf("Set(still-valid) failed: %v", err)
	}

	// Wait until the first entry expires prior to Save.
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s1.Get("expired-before-save")
		return !ok
	}, "expected expired-before-save to expire prior to Save")

	if err := s1.Set("expires-before-load-finishes", []byte("gone2"), 50*time.Millisecond); err != nil {
		t.Fatalf("Set(expires-before-load-finishes) failed: %v", err)
	}

	var buf bytes.Buffer
	if err := s1.Save(&buf); err != nil {
		t.Fatalf("s1.Save() failed: %v", err)
	}

	s2 := mustNewStore(t, memorystore.WithPreciseClock())
	if err := s2.Load(&slowEOFReader{r: &buf, eofDelay: 90 * time.Millisecond}); err != nil {
		t.Fatalf("s2.Load() failed: %v", err)
	}

	if _, ok := s2.Get("expired-before-save"); ok {
		t.Fatal("s2.Get(expired-before-save) = true, want false")
	}
	if _, ok := s2.Get("expires-before-load-finishes"); ok {
		t.Fatal("s2.Get(expires-before-load-finishes) = true, want false")
	}
	if v, ok := s2.Get("still-valid"); !ok || string(v) != "kept" {
		t.Fatalf("s2.Get(still-valid) = (%q, %v), want (\"kept\", true)", v, ok)
	}
}

func TestTier1_F12_Load_DoesNotBroadcastInvalidations(t *testing.T) {
	src := mustNewStore(t)
	if err := src.Set("snap-k", []byte("snap-v"), time.Minute); err != nil {
		t.Fatalf("src.Set() failed: %v", err)
	}
	var buf bytes.Buffer
	if err := src.Save(&buf); err != nil {
		t.Fatalf("src.Save() failed: %v", err)
	}

	ps := newControllablePubSub()
	rec2 := newRecordingMetrics()
	s2 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f12:no-inv"),
		memorystore.WithSyncInvalidation(),
		memorystore.WithMetricsRecorder(rec2),
	)
	s3 := mustNewStore(
		t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("f12:no-inv"),
		memorystore.WithSyncInvalidation(),
	)

	if err := s3.Set("snap-k", []byte("peer-val"), time.Minute); err != nil {
		t.Fatalf("s3.Set() failed: %v", err)
	}
	eventually(t, time.Second, 2*time.Millisecond, func() bool {
		return rec2.InvalidationsReceived() >= 1
	}, "s2 backplane should process s3 invalidation before Load")
	beforePublishes := ps.PublishCount()

	if err := s2.Load(&buf); err != nil {
		t.Fatalf("s2.Load() failed: %v", err)
	}

	if ps.PublishCount() != beforePublishes {
		t.Fatalf("Load broadcast %d invalidation(s), want 0", ps.PublishCount()-beforePublishes)
	}
	if v, ok := s3.Get("snap-k"); !ok || string(v) != "peer-val" {
		t.Fatalf("peer s3.Get(snap-k) = (%q, %v), want (\"peer-val\", true)", v, ok)
	}
	if v, ok := s2.Get("snap-k"); !ok || string(v) != "snap-v" {
		t.Fatalf("s2.Get(snap-k) = (%q, %v), want (\"snap-v\", true)", v, ok)
	}
}
