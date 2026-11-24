package memorystore

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSwissTable_CollisionsResizeAndDeleteInOverflowChains(t *testing.T) {
	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	// Force all keys onto shard 0 so a single shard undergoes multiple resizes
	// and builds overflow chains across buckets.
	var shard0Keys []string
	for i := 0; len(shard0Keys) < 300; i++ {
		k := fmt.Sprintf("collision-key-%d", i)
		if ms.shardIndex(k) == 0 {
			shard0Keys = append(shard0Keys, k)
		}
	}

	var wantBytes int64
	for i, k := range shard0Keys {
		val := fmt.Appendf(nil, "value-%03d", i)
		if err := ms.Set(k, val, time.Minute); err != nil {
			t.Fatalf("Set(%q) error = %v", k, err)
		}
		wantBytes += payloadSize(k, val)
	}

	if m := ms.GetMetrics(); m.Items != len(shard0Keys) || m.Bytes != wantBytes {
		t.Fatalf("after inserts: metrics=(Items=%d, Bytes=%d), want (%d, %d)",
			m.Items, m.Bytes, len(shard0Keys), wantBytes)
	}

	// Delete every second key (preserving overflowBit for keys later in chains).
	for i := 0; i < len(shard0Keys); i += 2 {
		k := shard0Keys[i]
		e := ms.lookupEntry(k)
		if e == nil {
			t.Fatalf("missing key %q before delete", k)
		}
		wantBytes -= payloadSize(k, e.value)
		if err := ms.Delete(k); err != nil {
			t.Fatalf("Delete(%q) error = %v", k, err)
		}
		if _, ok := ms.Get(k); ok {
			t.Fatalf("Get(%q) hit after delete", k)
		}
	}

	// All surviving keys must remain reachable across deleted slots in overflow chains.
	for i := 1; i < len(shard0Keys); i += 2 {
		k := shard0Keys[i]
		want := fmt.Sprintf("value-%03d", i)
		got, ok := ms.Get(k)
		if !ok || string(got) != want {
			t.Fatalf("Get(%q) after interleaved deletes = (%q, %v), want (%q, true)", k, got, ok, want)
		}
	}

	if m := ms.GetMetrics(); m.Items != len(shard0Keys)/2 || m.Bytes != wantBytes {
		t.Fatalf("after deletes: metrics=(Items=%d, Bytes=%d), want (%d, %d)",
			m.Items, m.Bytes, len(shard0Keys)/2, wantBytes)
	}

	// Re-insert deleted keys (reusing tombstone-free 0 slots) and verify resize
	// prunes expired churn without unnecessary table growth.
	var shortKeys []string
	for i := 0; len(shortKeys) < 120; i++ {
		k := fmt.Sprintf("churn-short-%d", i)
		if ms.shardIndex(k) == 0 {
			shortKeys = append(shortKeys, k)
		}
	}
	for _, k := range shortKeys {
		_ = ms.Set(k, []byte("short"), 5*time.Millisecond)
	}
	time.Sleep(15 * time.Millisecond)

	s0 := &ms.shards[0]
	s0.mu.Lock()
	capBefore := len(s0.table.Load().buckets)
	s0.mu.Unlock()

	for i := 0; i < 40; i++ {
		k := shard0Keys[i*2]
		_ = ms.Set(k, []byte("reused"), time.Minute)
	}

	s0.mu.Lock()
	capAfter := len(s0.table.Load().buckets)
	s0.mu.Unlock()
	if capAfter > capBefore {
		t.Errorf("expired churn caused unnecessary table growth: before=%d after=%d", capBefore, capAfter)
	}
}

func TestCoarseClock_WithPreciseClockAndSub10msFallback(t *testing.T) {
	t.Run("default coarse clock and <10ms fallback on Set, SetWithOptions, SetMulti, Get, GetMulti", func(t *testing.T) {
		ms, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		if err := ms.Set("coarse", []byte("c"), time.Minute); err != nil {
			t.Fatalf("Set(coarse) error = %v", err)
		}
		if e := ms.lookupEntry("coarse"); e == nil || e.isPreciseTTL() {
			t.Fatalf("expected coarse entry to NOT have preciseTTL flag, got e=%v", e)
		}

		// Set < 10ms triggers preciseTTL flag and accurate expiry on Get.
		if err := ms.Set("fast", []byte("f"), 5*time.Millisecond); err != nil {
			t.Fatalf("Set(fast) error = %v", err)
		}
		if e := ms.lookupEntry("fast"); e == nil || !e.isPreciseTTL() {
			t.Fatalf("expected fast entry to have preciseTTL flag, got e=%v", e)
		}
		if v, ok := ms.Get("fast"); !ok || string(v) != "f" {
			t.Fatalf("immediate Get(fast) = (%q, %v), want (f, true)", v, ok)
		}
		time.Sleep(8 * time.Millisecond)
		if _, ok := ms.Get("fast"); ok {
			t.Error("expected <10ms preciseTTL entry to expire promptly on Get")
		}

		// SetWithOptions post-jitter < 10ms also sets preciseTTL.
		sawPrecise := false
		for i := range 64 {
			k := fmt.Sprintf("jit-sub10-%d", i)
			if err := ms.SetWithOptions(k, []byte("j"), 12*time.Millisecond, WithTTLJitter(0.5)); err != nil {
				t.Fatalf("SetWithOptions() error = %v", err)
			}
			if e := ms.lookupEntry(k); e != nil && e.isPreciseTTL() {
				sawPrecise = true
				break
			}
		}
		if !sawPrecise {
			t.Error("expected post-jitter effectiveTTL < 10ms to activate entryFlagPreciseTTL")
		}

		// SetMulti (< 10ms) + GetMulti (>4 keys) honors preciseTTL.
		batch := map[string][]byte{
			"m1": []byte("1"),
			"m2": []byte("2"),
			"m3": []byte("3"),
			"m4": []byte("4"),
			"m5": []byte("5"),
		}
		if err := ms.SetMulti(batch, 5*time.Millisecond); err != nil {
			t.Fatalf("SetMulti() error = %v", err)
		}
		for k := range batch {
			if e := ms.lookupEntry(k); e == nil || !e.isPreciseTTL() {
				t.Fatalf("SetMulti entry %q missing preciseTTL flag", k)
			}
		}
		time.Sleep(8 * time.Millisecond)
		if got := ms.GetMulti([]string{"m1", "m2", "m3", "m4", "m5"}); len(got) != 0 {
			t.Errorf("GetMulti after 5ms TTL expiry returned %d items, want 0", len(got))
		}
	})

	t.Run("WithPreciseClock opt-out", func(t *testing.T) {
		ms, err := New(WithPreciseClock())
		if err != nil {
			t.Fatalf("New(WithPreciseClock) error = %v", err)
		}
		if !ms.preciseClock {
			t.Fatal("expected ms.preciseClock == true")
		}
		if err := ms.Set("k", []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		if e := ms.lookupEntry("k"); e == nil || !e.isPreciseTTL() {
			t.Fatalf("expected entry under WithPreciseClock to have preciseTTL flag")
		}
		if err := ms.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	})
}

func TestSlidingTTL_CoalescingThreshold(t *testing.T) {
	ms, err := New(WithPreciseClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	const ttl = 200 * time.Millisecond
	if err := ms.SetWithOptions("slide", []byte("v"), ttl, WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions() error = %v", err)
	}
	e := ms.lookupEntry("slide")
	if e == nil {
		t.Fatal("lookupEntry(slide) = nil")
	}
	initialExp := e.expiresAt.Load()

	// Within first 25% of TTL (remaining > 3/4 TTL): Get and GetMulti must NOT refresh expiresAt.
	time.Sleep(15 * time.Millisecond)
	if _, ok := ms.Get("slide"); !ok {
		t.Fatal("Get(slide) missed early")
	}
	if gotExp := e.expiresAt.Load(); gotExp != initialExp {
		t.Fatalf("sliding TTL refreshed too early (remaining >= 3/4): got %d, want %d", gotExp, initialExp)
	}

	// Set 5 additional sliding keys to exercise large GetMulti coalescing too.
	multiKeys := []string{"slide", "sm1", "sm2", "sm3", "sm4", "sm5"}
	multiExps := make([]int64, len(multiKeys))
	multiExps[0] = initialExp
	for i := 1; i < len(multiKeys); i++ {
		if err := ms.SetWithOptions(multiKeys[i], []byte("m"), ttl, WithSlidingExpiration()); err != nil {
			t.Fatalf("SetWithOptions(%s) error = %v", multiKeys[i], err)
		}
		multiExps[i] = ms.lookupEntry(multiKeys[i]).expiresAt.Load()
	}

	// Cross the 25% elapsed boundary (remaining < 3/4 TTL = 150ms).
	time.Sleep(65 * time.Millisecond)
	if _, ok := ms.Get("slide"); !ok {
		t.Fatal("Get(slide) missed after 80ms")
	}
	if gotExp := e.expiresAt.Load(); gotExp <= initialExp {
		t.Fatalf("sliding TTL did not refresh after >25%% elapsed: got %d <= %d", gotExp, initialExp)
	}

	gotMulti := ms.GetMulti(multiKeys)
	if len(gotMulti) != len(multiKeys) {
		t.Fatalf("GetMulti len = %d, want %d", len(gotMulti), len(multiKeys))
	}
	for i := 1; i < len(multiKeys); i++ {
		if cur := ms.lookupEntry(multiKeys[i]).expiresAt.Load(); cur <= multiExps[i] {
			t.Errorf("GetMulti sliding key %s did not refresh after >25%% elapsed: cur=%d <= init=%d",
				multiKeys[i], cur, multiExps[i])
		}
	}
}

func TestEviction_ZeroTTLDeleteNotInsertAndLazyExpiry(t *testing.T) {
	policies := []EvictionPolicy{EvictionVolatileTTL, EvictionS3FIFO, EvictionTinyLFU}
	for _, pol := range policies {
		t.Run(pol.String(), func(t *testing.T) {
			var mu sync.Mutex
			var events []EvictReason
			ms, err := New(
				WithMaxItems(10),
				WithEvictionPolicy(pol),
				WithOnEvict(func(_ string, _ []byte, r EvictReason) {
					mu.Lock()
					events = append(events, r)
					mu.Unlock()
				}),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer func() { _ = ms.Stop() }()

			for i := range 10 {
				if err := ms.Set(fmt.Sprintf("k-%d", i), []byte("live"), time.Minute); err != nil {
					t.Fatalf("Set() error = %v", err)
				}
			}

			// ttl <= 0 deletes existing key k-0 with EvictExpired without evicting live k-1..k-9.
			if err := ms.Set("k-0", []byte("dead"), 0); err != nil {
				t.Fatalf("Set(k-0, 0) error = %v", err)
			}
			if err := ms.Set("never-existed", []byte("dead"), -time.Second); err != nil {
				t.Fatalf("Set(never-existed, -1s) error = %v", err)
			}
			if err := ms.SetMulti(map[string][]byte{"k-1": []byte("dead"), "absent": []byte("x")}, 0); err != nil {
				t.Fatalf("SetMulti(0) error = %v", err)
			}

			if _, ok := ms.Get("k-0"); ok {
				t.Error("k-0 should be deleted by ttl=0")
			}
			if _, ok := ms.Get("k-1"); ok {
				t.Error("k-1 should be deleted by SetMulti ttl=0")
			}
			for i := 2; i < 10; i++ {
				if _, ok := ms.Get(fmt.Sprintf("k-%d", i)); !ok {
					t.Errorf("live key k-%d was erroneously evicted", i)
				}
			}

			if m := ms.GetMetrics(); m.Items != 8 {
				t.Errorf("Items = %d, want 8", m.Items)
			}

			mu.Lock()
			if len(events) != 2 || events[0] != EvictExpired || events[1] != EvictExpired {
				t.Errorf("events = %v, want [EvictExpired EvictExpired]", events)
			}
			events = nil
			mu.Unlock()

			// Lazy expiry on Get / GetMulti removes expired items from storage and accounting.
			for i := 2; i < 10; i++ {
				_ = ms.Delete(fmt.Sprintf("k-%d", i))
			}
			mu.Lock()
			events = nil
			mu.Unlock()

			for i := range 6 {
				if err := ms.Set(fmt.Sprintf("exp-%d", i), []byte("val"), 5*time.Millisecond); err != nil {
					t.Fatalf("Set(exp-%d) error = %v", i, err)
				}
			}
			time.Sleep(15 * time.Millisecond)

			if _, ok := ms.Get("exp-0"); ok {
				t.Error("exp-0 should be expired")
			}
			if got := ms.GetMetrics().Items; got != 5 {
				t.Errorf("Items after lazy Get expiry = %d, want 5", got)
			}
			if res := ms.GetMulti([]string{"exp-1", "exp-2", "exp-3", "exp-4", "exp-5"}); len(res) != 0 {
				t.Errorf("GetMulti returned %d expired items", len(res))
			}
			if m := ms.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
				t.Errorf("metrics after lazy GetMulti expiry = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
			}
			mu.Lock()
			expCount := 0
			for _, r := range events {
				if r == EvictExpired {
					expCount++
				}
			}
			mu.Unlock()
			if expCount != 6 {
				t.Errorf("lazy expiry emitted %d EvictExpired events, want 6", expCount)
			}
		})
	}
}

func TestEviction_SmallBoundsMaxBytesAndNoExpiration(t *testing.T) {
	policies := []EvictionPolicy{EvictionVolatileTTL, EvictionS3FIFO, EvictionTinyLFU}
	for _, pol := range policies {
		t.Run(pol.String(), func(t *testing.T) {
			for _, maxItems := range []int{1, 2, 10, 100} {
				var capEvicts atomic.Int64
				var expEvicts atomic.Int64
				ms, err := New(
					WithMaxItems(maxItems),
					WithEvictionPolicy(pol),
					WithOnEvict(func(_ string, _ []byte, r EvictReason) {
						switch r {
						case EvictCapacity:
							capEvicts.Add(1)
						case EvictExpired:
							expEvicts.Add(1)
						}
					}),
				)
				if err != nil {
					t.Fatalf("New(maxItems=%d) error = %v", maxItems, err)
				}

				total := maxItems + 50
				for i := range total {
					if err := ms.Set(fmt.Sprintf("perm-%d", i), []byte("v"), NoExpiration); err != nil {
						t.Fatalf("Set() error = %v", err)
					}
					if i%3 == 0 {
						_, _ = ms.Get(fmt.Sprintf("perm-%d", i))
					}
				}
				ms.cleanupExpiredItems()

				if got := ms.GetMetrics().Items; got > maxItems {
					t.Errorf("maxItems=%d: Items=%d exceeds limit", maxItems, got)
				}
				if capEvicts.Load() < 50 {
					t.Errorf("maxItems=%d: capEvicts=%d, want >= 50", maxItems, capEvicts.Load())
				}
				if expEvicts.Load() != 0 {
					t.Errorf("maxItems=%d: NoExpiration triggered %d EvictExpired", maxItems, expEvicts.Load())
				}
				_ = ms.Stop()
			}

			// MaxBytes enforcement + shrinking overwrite.
			msBytes, err := New(WithMaxBytes(400), WithEvictionPolicy(pol))
			if err != nil {
				t.Fatalf("New(MaxBytes) error = %v", err)
			}
			val := bytes.Repeat([]byte("a"), 40)
			for i := range 40 {
				_ = msBytes.Set(fmt.Sprintf("bk-%02d", i), val, time.Minute)
			}
			if m := msBytes.GetMetrics(); m.Bytes > 400 || m.Items == 0 {
				t.Errorf("MaxBytes=400 violated: metrics=%+v", m)
			}
			_ = msBytes.Stop()
		})
	}
}

func buildZipfCDF(n int, s float64) []float64 {
	cdf := make([]float64, n)
	var sum float64
	for i := range n {
		sum += 1.0 / math.Pow(float64(i+1), s)
		cdf[i] = sum
	}
	for i := range n {
		cdf[i] /= sum
	}
	cdf[n-1] = 1.0
	return cdf
}

func runZipfianHitRatio(t *testing.T, pol EvictionPolicy, maxItems, universe, ops int, cdf []float64) float64 {
	t.Helper()
	ms, err := New(WithMaxItems(maxItems), WithEvictionPolicy(pol))
	if err != nil {
		t.Fatalf("New(%v) error = %v", pol, err)
	}
	defer func() { _ = ms.Stop() }()

	keys := make([]string, universe)
	for i := range keys {
		keys[i] = fmt.Sprintf("zkey:%05d", i)
	}
	val := []byte("zipf-payload-value")

	rng := rand.New(rand.NewPCG(42, 99))

	var hits, total int
	for i := range ops {
		u := rng.Float64()
		idx := sort.SearchFloat64s(cdf, u)
		if idx >= universe {
			idx = universe - 1
		}
		k := keys[idx]
		if _, ok := ms.Get(k); ok {
			if i >= ops/5 { // measure steady-state after 20% warmup
				hits++
				total++
			}
		} else {
			if i >= ops/5 {
				total++
			}
			if err := ms.Set(k, val, 5*time.Minute); err != nil {
				t.Fatalf("Set() error = %v", err)
			}
		}
	}
	return float64(hits) / float64(total)
}

func TestEvictionPolicy_ZipfianHitRatio(t *testing.T) {
	const (
		maxItems = 1000
		universe = 50000
		ops      = 100000
	)
	cdf := buildZipfCDF(universe, 0.99)

	volHit := runZipfianHitRatio(t, EvictionVolatileTTL, maxItems, universe, ops, cdf)
	s3Hit := runZipfianHitRatio(t, EvictionS3FIFO, maxItems, universe, ops, cdf)
	tinyHit := runZipfianHitRatio(t, EvictionTinyLFU, maxItems, universe, ops, cdf)

	s3Rel := (s3Hit - volHit) / volHit
	tinyRel := (tinyHit - volHit) / volHit

	t.Logf("Zipfian(s=0.99, cap=%d, univ=%d) hit ratios: VolatileTTL=%.2f%% S3FIFO=%.2f%% (abs=+%.2f%%, rel=+%.2f%%) TinyLFU=%.2f%% (abs=+%.2f%%, rel=+%.2f%%)",
		maxItems, universe, volHit*100, s3Hit*100, (s3Hit-volHit)*100, s3Rel*100, tinyHit*100, (tinyHit-volHit)*100, tinyRel*100)

	if s3Rel < 0.15 {
		t.Errorf("S3FIFO Zipfian relative gain = +%.2f%%, want >= +15.00%% (VolatileTTL=%.2f%%, S3FIFO=%.2f%%)",
			s3Rel*100, volHit*100, s3Hit*100)
	}
	if tinyRel < 0.15 {
		t.Errorf("TinyLFU Zipfian relative gain = +%.2f%%, want >= +15.00%% (VolatileTTL=%.2f%%, TinyLFU=%.2f%%)",
			tinyRel*100, volHit*100, tinyHit*100)
	}
}

func runScanHitRatio(t *testing.T, pol EvictionPolicy) float64 {
	t.Helper()
	const (
		maxItems = 1000
		hotCount = 500
		scanSize = 5000
	)
	ms, err := New(WithMaxItems(maxItems), WithEvictionPolicy(pol))
	if err != nil {
		t.Fatalf("New(%v) error = %v", pol, err)
	}
	defer func() { _ = ms.Stop() }()

	val := []byte("scan-payload")
	hotKeys := make([]string, hotCount)
	for i := range hotKeys {
		hotKeys[i] = fmt.Sprintf("hot-%04d", i)
		_ = ms.Set(hotKeys[i], val, 5*time.Minute)
	}
	// Warm hot keys so frequency/protection state stabilizes.
	for range 4 {
		for _, k := range hotKeys {
			_, _ = ms.Get(k)
		}
	}

	// Sequential one-hit scan burst interleaved with hot-set reads.
	var hits, total int
	for i := range scanSize {
		scanKey := fmt.Sprintf("scan-%05d", i)
		_ = ms.Set(scanKey, val, 5*time.Minute)
		hotKey := hotKeys[i%hotCount]
		total++
		if _, ok := ms.Get(hotKey); ok {
			hits++
		} else {
			_ = ms.Set(hotKey, val, 5*time.Minute)
		}
	}
	return float64(hits) / float64(total)
}

func TestEvictionPolicy_ScanResistanceHitRatio(t *testing.T) {
	volHit := runScanHitRatio(t, EvictionVolatileTTL)
	s3Hit := runScanHitRatio(t, EvictionS3FIFO)
	tinyHit := runScanHitRatio(t, EvictionTinyLFU)

	t.Logf("Scan-resistant hit ratios: VolatileTTL=%.2f%% S3FIFO=%.2f%% (+%.2f%%) TinyLFU=%.2f%% (+%.2f%%)",
		volHit*100, s3Hit*100, (s3Hit-volHit)*100, tinyHit*100, (tinyHit-volHit)*100)

	if s3Hit-volHit < 0.15 {
		t.Errorf("S3FIFO scan gain = +%.2f%%, want >= +15.00%%", (s3Hit-volHit)*100)
	}
	if tinyHit-volHit < 0.15 {
		t.Errorf("TinyLFU scan gain = +%.2f%%, want >= +15.00%%", (tinyHit-volHit)*100)
	}
}

func TestEviction_ConcurrentRaceAndGoroutineLeak(t *testing.T) {
	for _, pol := range []EvictionPolicy{EvictionVolatileTTL, EvictionS3FIFO, EvictionTinyLFU} {
		t.Run(pol.String(), func(t *testing.T) {
			ms, err := New(
				WithMaxItems(64),
				WithMaxBytes(4096),
				WithEvictionPolicy(pol),
				WithCleanupInterval(5*time.Millisecond),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			var wg sync.WaitGroup
			for g := range 8 {
				wg.Go(func() {
					for i := range 400 {
						k := fmt.Sprintf("race-key-%d", (g*17+i)%128)
						switch i % 6 {
						case 0, 1:
							_ = ms.Set(k, []byte("payload-value"), 20*time.Millisecond)
						case 2, 3:
							_, _ = ms.Get(k)
						case 4:
							_ = ms.GetMulti([]string{k, "race-key-1", "race-key-2", "race-key-3", "race-key-4"})
						case 5:
							_ = ms.Delete(k)
						}
					}
				})
			}
			wg.Wait()
			if err := ms.Stop(); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
		})
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

func TestAdaptiveActiveShards_AndEvictorSizing(t *testing.T) {
	cases := []struct {
		name       string
		opts       []Option
		wantShards int
	}{
		{name: "unbounded", opts: nil, wantShards: 256},
		{name: "maxItems=128,maxBytes=32768", opts: []Option{WithMaxItems(128), WithMaxBytes(32768)}, wantShards: 1},
		{name: "maxItems=200", opts: []Option{WithMaxItems(200)}, wantShards: 1},
		{name: "maxItems=256", opts: []Option{WithMaxItems(256)}, wantShards: 4},
		{name: "maxItems=1000", opts: []Option{WithMaxItems(1000)}, wantShards: 8},
		{name: "maxItems=10000", opts: []Option{WithMaxItems(10000)}, wantShards: 128},
		{name: "maxItems=16384", opts: []Option{WithMaxItems(16384)}, wantShards: 256},
		{name: "maxBytes=400", opts: []Option{WithMaxBytes(400)}, wantShards: 1},
		{name: "maxBytes=32768", opts: []Option{WithMaxBytes(32768)}, wantShards: 4},
		{name: "maxBytes=2MiB", opts: []Option{WithMaxBytes(2 * 1024 * 1024)}, wantShards: 256},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms, err := New(tc.opts...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer func() { _ = ms.Stop() }()
			if got := ms.activeShards(); got != tc.wantShards {
				t.Fatalf("activeShards() = %d, want %d (shardMask=%d)", got, tc.wantShards, ms.shardMask)
			}
		})
	}

	// Verify MaxBytes-only evictor sizing (shardTargetItems == 0 && shardTargetBytes > 0).
	msS3, err := New(WithMaxBytes(32768), WithEvictionPolicy(EvictionS3FIFO))
	if err != nil {
		t.Fatalf("New(S3FIFO MaxBytes) error = %v", err)
	}
	defer func() { _ = msS3.Stop() }()
	if got := msS3.shards[0].evict.ghost.cap; got != 128 {
		t.Fatalf("MaxBytes-only S3FIFO ghost.cap = %d, want 128", got)
	}

	msTiny, err := New(WithMaxBytes(32768), WithEvictionPolicy(EvictionTinyLFU))
	if err != nil {
		t.Fatalf("New(TinyLFU MaxBytes) error = %v", err)
	}
	defer func() { _ = msTiny.Stop() }()
	if got := msTiny.shards[0].evict.cmsTargetItems(); got != 128 {
		t.Fatalf("MaxBytes-only TinyLFU cmsTargetItems() = %d, want 128", got)
	}
}

func TestEnforceBounds_MaxItems128AndMaxBytes32768(t *testing.T) {
	const (
		maxItems = 128
		maxBytes = int64(32 * 1024)
	)
	val := bytes.Repeat([]byte("x"), 96)

	for _, pol := range []EvictionPolicy{EvictionVolatileTTL, EvictionS3FIFO, EvictionTinyLFU} {
		t.Run(pol.String(), func(t *testing.T) {
			ms, err := New(
				WithMaxItems(maxItems),
				WithMaxBytes(maxBytes),
				WithEvictionPolicy(pol),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer func() { _ = ms.Stop() }()

			for i := range 500 {
				if err := ms.Set(fmt.Sprintf("k-%04d", i), val, 5*time.Minute); err != nil {
					t.Fatalf("Set(%d) error = %v", i, err)
				}
				m := ms.GetMetrics()
				if m.Items > maxItems || m.Bytes > maxBytes {
					t.Fatalf("after Set(%d): metrics=(Items=%d, Bytes=%d) exceeds (%d, %d)",
						i, m.Items, m.Bytes, maxItems, maxBytes)
				}
			}

			// Verify Save/Load round-trip preserves bounded invariants on reduced active shards.
			for _, fmtOpt := range []SnapshotFormat{SnapshotBinary, SnapshotJSON} {
				var snap bytes.Buffer
				if err := ms.Save(&snap, WithSnapshotFormat(fmtOpt)); err != nil {
					t.Fatalf("Save(%v) error = %v", fmtOpt, err)
				}
				dst, err := New(
					WithMaxItems(maxItems),
					WithMaxBytes(maxBytes),
					WithEvictionPolicy(pol),
				)
				if err != nil {
					t.Fatalf("New(dst) error = %v", err)
				}
				if err := dst.Load(&snap, WithSnapshotFormat(fmtOpt)); err != nil {
					_ = dst.Stop()
					t.Fatalf("Load(%v) error = %v", fmtOpt, err)
				}
				m := dst.GetMetrics()
				_ = dst.Stop()
				if m.Items == 0 || m.Items > maxItems || m.Bytes > maxBytes {
					t.Fatalf("Load(%v) metrics=(Items=%d, Bytes=%d) invalid for bounds (%d, %d)",
						fmtOpt, m.Items, m.Bytes, maxItems, maxBytes)
				}
			}

			// Single oversized protectedKey entry (> MaxBytes) must be evicted in pass == 1.
			oversized := bytes.Repeat([]byte("Z"), int(maxBytes)+1024)
			if err := ms.Set("oversized-key", oversized, time.Minute); err != nil {
				t.Fatalf("Set(oversized) error = %v", err)
			}
			if m := ms.GetMetrics(); m.Bytes > maxBytes || m.Items > maxItems {
				t.Fatalf("after oversized Set: metrics=(Items=%d, Bytes=%d) exceeds (%d, %d)",
					m.Items, m.Bytes, maxItems, maxBytes)
			}
		})
	}

	// Verify pass == 0 -> pass == 1 fallback even when shardTargetItems == 0 on multi-shard store.
	msFallback, err := New(WithMaxItems(512), WithMaxBytes(64*1024), WithEvictionPolicy(EvictionVolatileTTL))
	if err != nil {
		t.Fatalf("New(fallback) error = %v", err)
	}
	defer func() { _ = msFallback.Stop() }()
	// Force shardTargetItems = 0 while keeping activeShards = 8 to exercise the white-box pass 0/1 path.
	msFallback.shardTargetItems = 0
	for i := range 600 {
		if err := msFallback.Set(fmt.Sprintf("fb-%04d", i), val, time.Minute); err != nil {
			t.Fatalf("Set(%d) error = %v", i, err)
		}
	}
	if m := msFallback.GetMetrics(); m.Items > 512 || m.Bytes > 64*1024 {
		t.Fatalf("fallback bounds violated: metrics=(Items=%d, Bytes=%d)", m.Items, m.Bytes)
	}
}

func TestEvictionPolicy_SmallCapacityZipfianAndScanBurst(t *testing.T) {
	t.Run("MaxItems=128 Zipfian + Scan Burst Hit Ratio", func(t *testing.T) {
		const (
			maxItems = 128
			universe = 1500
			nZipf    = 6000
			nScan    = 3000
		)
		cdf := buildZipfCDF(universe, 0.99)
		keys := make([]string, universe)
		for i := range keys {
			keys[i] = fmt.Sprintf("szipf:%04d", i)
		}

		rng := rand.New(rand.NewPCG(123, 456))
		workload := make([]string, 0, nZipf*2+nScan)
		for range nZipf {
			idx := min(sort.SearchFloat64s(cdf, rng.Float64()), universe-1)
			workload = append(workload, keys[idx])
		}
		for i := range nScan {
			workload = append(workload, fmt.Sprintf("sscan-%04d", i))
		}
		for range nZipf {
			idx := min(sort.SearchFloat64s(cdf, rng.Float64()), universe-1)
			workload = append(workload, keys[idx])
		}

		run := func(pol EvictionPolicy) float64 {
			ms, err := New(WithMaxItems(maxItems), WithEvictionPolicy(pol))
			if err != nil {
				t.Fatalf("New(%v) error = %v", pol, err)
			}
			defer func() { _ = ms.Stop() }()
			var hits int
			val := []byte("v")
			for _, k := range workload {
				if _, ok := ms.Get(k); ok {
					hits++
				} else if err := ms.Set(k, val, 5*time.Minute); err != nil {
					t.Fatalf("Set(%s) error = %v", k, err)
				}
			}
			return float64(hits) / float64(len(workload))
		}

		vol := run(EvictionVolatileTTL)
		s3 := run(EvictionS3FIFO)
		tiny := run(EvictionTinyLFU)
		t.Logf("SmallCap(128) hit ratios: VolatileTTL=%.4f S3FIFO=%.4f TinyLFU=%.4f", vol, s3, tiny)

		if s3 <= vol*1.15 {
			t.Errorf("S3FIFO hit ratio %.4f did not exceed VolatileTTL %.4f by >15%%", s3, vol)
		}
		if tiny <= vol*1.15 {
			t.Errorf("TinyLFU hit ratio %.4f did not exceed VolatileTTL %.4f by >15%%", tiny, vol)
		}
	})

	t.Run("MaxItems=200 Hot Key Retention Across Scan", func(t *testing.T) {
		const (
			maxItems = 200
			universe = 2000
			nZipf    = 10000
			nScan    = 500
		)
		cdf := buildZipfCDF(universe, 0.99)
		keys := make([]string, universe)
		for i := range keys {
			keys[i] = fmt.Sprintf("rzipf:%04d", i)
		}

		for _, pol := range []EvictionPolicy{EvictionS3FIFO, EvictionTinyLFU} {
			t.Run(pol.String(), func(t *testing.T) {
				ms, err := New(WithMaxItems(maxItems), WithEvictionPolicy(pol))
				if err != nil {
					t.Fatalf("New(%v) error = %v", pol, err)
				}
				defer func() { _ = ms.Stop() }()

				rng := rand.New(rand.NewPCG(99, 77))
				counts := make(map[string]int, universe)
				for range nZipf {
					idx := min(sort.SearchFloat64s(cdf, rng.Float64()), universe-1)
					k := keys[idx]
					counts[k]++
					if _, ok := ms.Get(k); !ok {
						if err := ms.Set(k, []byte("val"), time.Minute); err != nil {
							t.Fatalf("Set(%s) error = %v", k, err)
						}
					}
				}
				for i := range nScan {
					if err := ms.Set(fmt.Sprintf("cold-%04d", i), []byte("scan"), time.Minute); err != nil {
						t.Fatalf("Set(cold) error = %v", err)
					}
				}

				type kv struct {
					k string
					c int
				}
				ranked := make([]kv, 0, len(counts))
				for k, c := range counts {
					ranked = append(ranked, kv{k: k, c: c})
				}
				sort.Slice(ranked, func(i, j int) bool {
					if ranked[i].c != ranked[j].c {
						return ranked[i].c > ranked[j].c
					}
					return ranked[i].k < ranked[j].k
				})
				kept := 0
				for i := range 20 {
					if _, ok := ms.Get(ranked[i].k); ok {
						kept++
					}
				}
				if kept < 10 {
					t.Errorf("%v kept %d/20 hottest keys across scan, want >= 10", pol, kept)
				}
			})
		}
	})
}

func TestGhostRing_NoOrphansOrUnboundedGrowth(t *testing.T) {
	g := &ghostRing{cap: 4}
	for i := uint64(1); i <= 4; i++ {
		g.add(i)
	}
	// Remove an item not at g.pos (g.pos == 0 -> item 1), then churn.
	if !g.remove(2) {
		t.Fatal("expected remove(2) == true")
	}
	g.add(5)
	if _, orphan := g.set[1]; orphan {
		t.Fatal("key 1 was overwritten in ring and must not remain orphaned in g.set")
	}

	// Re-adding a previously removed hash before pos wraps must not get prematurely evicted.
	g.add(2)
	for i := uint64(6); i <= 100; i++ {
		if i%3 == 0 {
			g.remove(i - 2)
		}
		g.add(i)
		if len(g.set) > g.cap {
			t.Fatalf("len(g.set) = %d exceeded ghostRing cap %d", len(g.set), g.cap)
		}
		for h, slotIdx := range g.set {
			slot := g.ring[slotIdx]
			if !slot.live || slot.hash != h {
				t.Fatalf("ghostRing bijection broken: hash %d -> slot %d = %+v", h, slotIdx, slot)
			}
		}
	}
}

func TestNewShardEvictor_GhostCapBoundedOnHugeMaxBytes(t *testing.T) {
	ms, err := New(
		WithMaxBytes(16<<30), // 16 GiB MaxBytes-only store
		WithEvictionPolicy(EvictionS3FIFO),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	if capVal := ms.shards[0].evict.ghost.cap; capVal > maxGhostCapPerShard {
		t.Fatalf("ghost.cap = %d on 16 GiB MaxBytes store, want bounded <= %d", capVal, maxGhostCapPerShard)
	}

	msHugeItems, err := New(
		WithMaxItems(100_000_000),
		WithEvictionPolicy(EvictionS3FIFO),
	)
	if err != nil {
		t.Fatalf("New(MaxItems=100M) error = %v", err)
	}
	defer func() { _ = msHugeItems.Stop() }()

	if capVal := msHugeItems.shards[0].evict.ghost.cap; capVal > maxGhostCapPerShard {
		t.Fatalf("ghost.cap = %d on 100M MaxItems store, want bounded <= %d", capVal, maxGhostCapPerShard)
	}
}

func TestEnforceBounds_MultiShardProtectedKeyAndOversized(t *testing.T) {
	// 1. Entry larger than shardTargetBytes (8 KiB) but <= MaxBytes (32 KiB) across 4 shards
	// must NEVER be self-evicted during its own Set().
	for trial := range 32 {
		key := fmt.Sprintf("medium-%d", trial)
		ms, err := New(WithMaxBytes(32 * 1024))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		small := make([]byte, 1024)
		for i := range 22 {
			if err := ms.Set(fmt.Sprintf("s-%d-%d", trial, i), small, time.Minute); err != nil {
				_ = ms.Stop()
				t.Fatalf("Set(small) error = %v", err)
			}
		}
		if err := ms.Set(key, make([]byte, 20*1024), time.Minute); err != nil {
			_ = ms.Stop()
			t.Fatalf("Set(medium) error = %v", err)
		}
		if _, ok := ms.Get(key); !ok {
			_ = ms.Stop()
			t.Fatalf("trial %d: newly inserted key (%dB <= MaxBytes) was self-evicted in pass==1", trial, 20*1024)
		}
		if m := ms.GetMetrics(); m.Bytes > 32*1024 {
			_ = ms.Stop()
			t.Fatalf("trial %d: Bytes = %d exceeds MaxBytes 32768", trial, m.Bytes)
		}
		_ = ms.Stop()
	}

	// 2. Single oversized entry (> MaxBytes) must NOT wipe out healthy entries in other shards,
	// and must delete any prior entry for the same key and emit EvictCapacity.
	for trial := range 32 {
		var capEvicts atomic.Int64
		ms, err := New(
			WithMaxBytes(32*1024),
			WithOnEvict(func(_ string, _ []byte, r EvictReason) {
				if r == EvictCapacity {
					capEvicts.Add(1)
				}
			}),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		small := make([]byte, 1024)
		for i := range 20 {
			if err := ms.Set(fmt.Sprintf("keep-%d-%d", trial, i), small, time.Minute); err != nil {
				_ = ms.Stop()
				t.Fatalf("Set(keep) error = %v", err)
			}
		}
		hugeKey := fmt.Sprintf("huge-%d", trial)
		if err := ms.Set(hugeKey, []byte("prior"), time.Minute); err != nil {
			_ = ms.Stop()
			t.Fatalf("Set(prior) error = %v", err)
		}
		beforeCap := capEvicts.Load()
		if err := ms.Set(hugeKey, make([]byte, 40*1024), time.Minute); err != nil {
			_ = ms.Stop()
			t.Fatalf("Set(huge) error = %v", err)
		}
		if _, ok := ms.Get(hugeKey); ok {
			_ = ms.Stop()
			t.Fatalf("trial %d: prior entry for hugeKey remained after oversized Set", trial)
		}
		if capEvicts.Load() <= beforeCap {
			_ = ms.Stop()
			t.Fatalf("trial %d: expected EvictCapacity notification for oversized Set", trial)
		}
		m := ms.GetMetrics()
		_ = ms.Stop()
		if m.Bytes > 32*1024 {
			t.Fatalf("trial %d: Bytes = %d exceeds MaxBytes 32768", trial, m.Bytes)
		}
		if m.Items == 0 {
			t.Fatalf("trial %d: oversized Set wiped all healthy items across all shards", trial)
		}
	}

	// 3. SetMulti oversized entries (small batch and multi-shard batch) do not wipe healthy entries.
	msMulti, err := New(WithMaxBytes(32 * 1024))
	if err != nil {
		t.Fatalf("New(msMulti) error = %v", err)
	}
	defer func() { _ = msMulti.Stop() }()
	for i := range 10 {
		if err := msMulti.Set(fmt.Sprintf("base-%d", i), make([]byte, 512), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := msMulti.SetMulti(map[string][]byte{
		"ok-small":  []byte("hello"),
		"too-large": make([]byte, 40*1024),
	}, time.Minute); err != nil {
		t.Fatal(err)
	}
	largeBatch := make(map[string][]byte, 12)
	for i := range 10 {
		largeBatch[fmt.Sprintf("lb-%d", i)] = []byte("ok")
	}
	largeBatch["lb-huge"] = make([]byte, 40*1024)
	if err := msMulti.SetMulti(largeBatch, time.Minute); err != nil {
		t.Fatal(err)
	}
	if m := msMulti.GetMetrics(); m.Bytes > 32*1024 || m.Items == 0 {
		t.Fatalf("SetMulti oversized left invalid state: %+v", m)
	}
}

func TestEviction_S3FIFOAndTinyLFU_ExpiredAndProtectedPaths(t *testing.T) {
	for _, pol := range []EvictionPolicy{EvictionS3FIFO, EvictionTinyLFU} {
		t.Run(pol.String(), func(t *testing.T) {
			var expiredCount, capCount atomic.Int64
			ms, err := New(
				WithMaxItems(8),
				WithEvictionPolicy(pol),
				WithPreciseClock(),
				WithCleanupInterval(time.Hour),
				WithOnEvict(func(_ string, _ []byte, r EvictReason) {
					switch r {
					case EvictExpired:
						expiredCount.Add(1)
					case EvictCapacity:
						capCount.Add(1)
					}
				}),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer func() { _ = ms.Stop() }()

			// Seed short-TTL entries (which enter small and main after promotion), let expire,
			// then insert new entries to force capacity eviction to encounter expired entries.
			for i := range 8 {
				if err := ms.Set(fmt.Sprintf("exp-%d", i), []byte("v"), 5*time.Millisecond); err != nil {
					t.Fatal(err)
				}
			}
			// Touch half so TinyLFU moves them / S3-FIFO bumps freq -> main on eviction pass.
			for i := range 4 {
				_, _ = ms.Get(fmt.Sprintf("exp-%d", i))
			}
			time.Sleep(15 * time.Millisecond)

			for i := range 12 {
				if err := ms.Set(fmt.Sprintf("live-%d", i), []byte("v"), time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if expiredCount.Load() == 0 {
				t.Fatalf("%s: expected EvictExpired during capacity eviction of expired entries", pol)
			}
			if m := ms.GetMetrics(); m.Items > 8 {
				t.Fatalf("%s: Items = %d, want <= 8", pol, m.Items)
			}
		})
	}
}

func TestEviction_WhiteBox_TinyLFUWrapOverwriteAndS3FIFOProtected(t *testing.T) {
	// Exercise TinyLFU drainLocked readBuf wrap (w-d > 64) and onOverwriteLocked default branch.
	msTiny, err := New(WithMaxItems(16), WithEvictionPolicy(EvictionTinyLFU), WithPreciseClock())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = msTiny.Stop() }()

	if err := msTiny.Set("k1", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	s := &msTiny.shards[msTiny.shardIndex("k1")]
	e1 := msTiny.lookupEntry("k1")
	ev := s.evict
	// Hold s.mu so recordReadAccess TryLock fails and writeIdx wraps past 64.
	s.mu.Lock()
	for range 80 {
		msTiny.recordReadAccess(s, e1)
	}
	ev.drainLocked()
	// Exercise onOverwriteLocked default branch (evictQueueNone).
	orphan := &entry{key: "k1", value: []byte("old"), hash: e1.hash, evictQueue: evictQueueNone}
	ev.onOverwriteLocked(orphan, e1, 5, 4)
	s.mu.Unlock()

	// Exercise S3-FIFO onOverwriteLocked default branch and protectedKey skipping in small & main.
	msS3, err := New(WithMaxItems(8), WithEvictionPolicy(EvictionS3FIFO), WithPreciseClock(), WithCleanupInterval(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = msS3.Stop() }()

	if err := msS3.Set("s0", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	sS3 := &msS3.shards[0]
	eS3 := msS3.lookupEntry("s0")
	sS3.mu.Lock()
	msS3.shards[0].evict.onOverwriteLocked(&entry{key: "s0", value: []byte("old"), hash: eS3.hash, evictQueue: evictQueueNone}, eS3, 5, 3)
	// Seed expired entry in main and protected key in small/main to exercise protectedKey + expired branches.
	eMainExp := &entry{key: "main-exp", value: []byte("v"), hash: 101}
	eMainExp.expiresAt.Store(msS3.nowExact() - 10)
	sS3.table.Load().insertFreshLocked(eMainExp.hash, eMainExp)
	sS3.evict.main.pushHead(eMainExp, evictQueueMain, payloadSize(eMainExp.key, eMainExp.value))
	sS3.count.Store(sS3.count.Load() + 1)
	sS3.bytes.Store(sS3.bytes.Load() + payloadSize(eMainExp.key, eMainExp.value))

	// Put protected entry at head of small and main and force eviction.
	eProtSmall := &entry{key: "prot", value: []byte("v"), hash: 202}
	eProtSmall.expiresAt.Store(msS3.nowExact() + int64(time.Minute))
	sS3.table.Load().insertFreshLocked(eProtSmall.hash, eProtSmall)
	sS3.evict.small.pushHead(eProtSmall, evictQueueSmall, payloadSize(eProtSmall.key, eProtSmall.value))
	sS3.count.Store(sS3.count.Load() + 1)
	sS3.bytes.Store(sS3.bytes.Load() + payloadSize(eProtSmall.key, eProtSmall.value))

	evRes, ok := msS3.evictOneLocked(sS3, msS3.nowExact(), "prot")
	if !ok || evRes.key == "prot" {
		t.Fatalf("evictOneLocked evicted protected key or failed: ok=%v res=%+v", ok, evRes)
	}
	sS3.mu.Unlock()
}

func TestCalcExpiresAtAndApplyJitter_Saturation(t *testing.T) {
	if got := calcExpiresAt(1000, 0); got != 1000 {
		t.Fatalf("calcExpiresAt(1000, 0) = %d, want 1000", got)
	}
	if got := calcExpiresAt(1000, math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("calcExpiresAt(1000, MaxInt64) = %d, want MaxInt64", got)
	}

	ms, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Stop() }()

	for range 16 {
		got := ms.applyJitter(math.MaxInt64, 0.5)
		if got <= 0 {
			t.Fatalf("applyJitter(MaxInt64, 0.5) = %d, want > 0", got)
		}
	}
}

func TestSetMulti_LargeBatchResizeAndOversizedNoTableDesync(t *testing.T) {
	for _, pol := range []EvictionPolicy{EvictionS3FIFO, EvictionTinyLFU, EvictionVolatileTTL} {
		t.Run(pol.String(), func(t *testing.T) {
			ms, err := New(
				WithMaxItems(100),
				WithMaxBytes(4096),
				WithEvictionPolicy(pol),
				WithCleanupInterval(time.Hour),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ms.Stop() }()

			for i := range 10 {
				if err := ms.Set(fmt.Sprintf("pre-%d", i), []byte("val"), time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			for round := range 20 {
				batch := make(map[string][]byte, 22)
				for i := range 12 {
					batch[fmt.Sprintf("new-%d-%d", round, i)] = []byte("x")
				}
				for i := range 10 {
					k := fmt.Sprintf("pre-%d", i)
					if err := ms.Set(k, []byte("existing"), time.Minute); err != nil {
						t.Fatal(err)
					}
					batch[k] = bytes.Repeat([]byte("Z"), 5000)
				}
				if err := ms.SetMulti(batch, time.Minute); err != nil {
					t.Fatal(err)
				}
			}

			for i := range 10 {
				if _, ok := ms.Get(fmt.Sprintf("pre-%d", i)); ok {
					t.Fatalf("oversized pre-%d remained live after SetMulti", i)
				}
			}
			var actualItems, actualBytes int64
			for i := range ms.activeShards() {
				s := &ms.shards[i]
				s.mu.Lock()
				tbl := s.table.Load()
				var sItems, sBytes int64
				for bIdx := range tbl.buckets {
					b := &tbl.buckets[bIdx]
					ctrl := b.ctrl.Load()
					for j := range bucketSlots {
						if (ctrl>>(j*8))&0xFF != 0 {
							if e := b.slots[j].Load(); e != nil {
								sItems++
								sBytes += payloadSize(e.key, e.value)
								if (pol == EvictionS3FIFO || pol == EvictionTinyLFU) && e.evictQueue == evictQueueNone {
									s.mu.Unlock()
									t.Fatalf("phantom entry %q with evictQueueNone", e.key)
								}
							}
						}
					}
				}
				if s.count.Load() != sItems || s.bytes.Load() != sBytes {
					s.mu.Unlock()
					t.Fatalf("shard %d drift: s=(%d,%d) tbl=(%d,%d)", i, s.count.Load(), s.bytes.Load(), sItems, sBytes)
				}
				s.mu.Unlock()
				actualItems += sItems
				actualBytes += sBytes
			}
			gm := ms.GetMetrics()
			if int64(gm.Items) != actualItems || gm.Bytes != actualBytes || gm.Items > 100 || gm.Bytes > 4096 {
				t.Fatalf("metrics mismatch or bounds exceeded: metrics=%+v actual=(%d,%d)", gm, actualItems, actualBytes)
			}
		})
	}
}

func TestSetMulti_LargeBatchJitterNoNegativeMetrics(t *testing.T) {
	ms, err := New(
		WithPreciseClock(),
		WithCleanupInterval(time.Hour),
		WithDefaultTTLJitter(0.2),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Stop() }()

	var s0Keys []string
	for i := 0; len(s0Keys) < 20; i++ {
		k := fmt.Sprintf("k-%d", i)
		if ms.shardIndex(k) == 0 {
			s0Keys = append(s0Keys, k)
		}
	}

	var stop atomic.Bool
	var negSeen atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			m := ms.GetMetrics()
			if m.Items < 0 || m.Bytes < 0 {
				negSeen.Store(true)
			}
		}
	}()

	for range 200 {
		batch := make(map[string][]byte, 20)
		for _, k := range s0Keys {
			batch[k] = []byte("vvvvvvvvvv")
		}
		if err := ms.SetMulti(batch, time.Nanosecond); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatal(err)
		}
		for _, k := range s0Keys {
			if err := ms.Delete(k); err != nil {
				stop.Store(true)
				wg.Wait()
				t.Fatal(err)
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	if negSeen.Load() {
		t.Fatal("GetMetrics() observed negative Items or Bytes during SetMulti")
	}
}

func TestSetMulti_MultiShardProtectsBatchEntriesFromSelfEviction(t *testing.T) {
	for _, pol := range []EvictionPolicy{EvictionS3FIFO, EvictionTinyLFU, EvictionVolatileTTL} {
		t.Run(pol.String()+"/small_batch", func(t *testing.T) {
			for trial := range 32 {
				ms, err := New(WithMaxBytes(32*1024), WithEvictionPolicy(pol))
				if err != nil {
					t.Fatal(err)
				}
				small := make([]byte, 1024)
				for i := range 22 {
					if err := ms.Set(fmt.Sprintf("s-%d-%d", trial, i), small, time.Minute); err != nil {
						_ = ms.Stop()
						t.Fatal(err)
					}
				}
				bigKey := fmt.Sprintf("big-%d", trial)
				if err := ms.SetMulti(map[string][]byte{
					bigKey: make([]byte, 20*1024),
					"tiny": []byte("ok"),
				}, time.Minute); err != nil {
					_ = ms.Stop()
					t.Fatal(err)
				}
				if _, ok := ms.Get(bigKey); !ok {
					_ = ms.Stop()
					t.Fatalf("trial %d: newly inserted batch key <= MaxBytes was self-evicted", trial)
				}
				m := ms.GetMetrics()
				_ = ms.Stop()
				if m.Bytes > 32*1024 {
					t.Fatalf("trial %d: Bytes = %d exceeds MaxBytes 32768", trial, m.Bytes)
				}
			}
		})

		t.Run(pol.String()+"/large_batch", func(t *testing.T) {
			for trial := range 16 {
				ms, err := New(WithMaxBytes(32*1024), WithEvictionPolicy(pol))
				if err != nil {
					t.Fatal(err)
				}
				small := make([]byte, 1024)
				for i := range 22 {
					if err := ms.Set(fmt.Sprintf("ls-%d-%d", trial, i), small, time.Minute); err != nil {
						_ = ms.Stop()
						t.Fatal(err)
					}
				}
				bigKey := fmt.Sprintf("lbig-%d", trial)
				batch := make(map[string][]byte, 12)
				batch[bigKey] = make([]byte, 20*1024)
				for i := range 11 {
					batch[fmt.Sprintf("ltiny-%d-%d", trial, i)] = []byte("ok")
				}
				if err := ms.SetMulti(batch, time.Minute); err != nil {
					_ = ms.Stop()
					t.Fatal(err)
				}
				if _, ok := ms.Get(bigKey); !ok {
					_ = ms.Stop()
					t.Fatalf("trial %d: newly inserted large-batch key <= MaxBytes was self-evicted", trial)
				}
				m := ms.GetMetrics()
				_ = ms.Stop()
				if m.Bytes > 32*1024 {
					t.Fatalf("trial %d: Bytes = %d exceeds MaxBytes 32768", trial, m.Bytes)
				}
			}
		})

		t.Run(pol.String()+"/batch_exceeding_max_bytes_falls_back", func(t *testing.T) {
			ms, err := New(WithMaxBytes(32*1024), WithEvictionPolicy(pol))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ms.Stop() }()
			if err := ms.Set("old", make([]byte, 1024), time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := ms.SetMulti(map[string][]byte{
				"big-a": make([]byte, 20*1024),
				"big-b": make([]byte, 20*1024),
			}, time.Minute); err != nil {
				t.Fatal(err)
			}
			if m := ms.GetMetrics(); m.Bytes > 32*1024 {
				t.Fatalf("Bytes = %d exceeds MaxBytes 32768 after oversized batch fallback", m.Bytes)
			}
		})
	}
}
