package benchmarks

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
	"github.com/allegro/bigcache/v3"
	"github.com/coocood/freecache"
	"github.com/dgraph-io/ristretto/v2"
	"github.com/maypok86/otter/v2"
	gocache "github.com/patrickmn/go-cache"
)

const (
	numKeys  = 1024
	keyMask  = numKeys - 1
	ttl      = 5 * time.Minute
	valBytes = "benchmark-payload-value-for-in-memory-cache-comparison"
)

var (
	benchKeys     = make([]string, numKeys)
	benchKeyBytes = make([][]byte, numKeys)
	benchValBytes = []byte(valBytes)
)

func init() {
	for i := range numKeys {
		k := fmt.Sprintf("bench:cache:key:%04d", i)
		benchKeys[i] = k
		benchKeyBytes[i] = []byte(k)
	}
}

type cacheHarness struct {
	name    string
	set     func(idx int)
	get     func(idx int) bool
	cleanup func()
}

func newHarnesses(b *testing.B, preload bool) []cacheHarness {
	b.Helper()

	// 1. MemoryStore
	ms, err := memorystore.New()
	if err != nil {
		b.Fatal(err)
	}
	if preload {
		for i := range numKeys {
			if err := ms.Set(benchKeys[i], benchValBytes, ttl); err != nil {
				b.Fatal(err)
			}
		}
	}

	// 2. patrickmn/go-cache
	gc := gocache.New(ttl, 10*time.Minute)
	if preload {
		for i := range numKeys {
			gc.Set(benchKeys[i], benchValBytes, ttl)
		}
	}

	// 3. allegro/bigcache/v3
	bcCfg := bigcache.DefaultConfig(ttl)
	bcCfg.Verbose = false
	bc, err := bigcache.New(context.Background(), bcCfg)
	if err != nil {
		b.Fatal(err)
	}
	if preload {
		for i := range numKeys {
			if err := bc.Set(benchKeys[i], benchValBytes); err != nil {
				b.Fatal(err)
			}
		}
	}

	// 4. coocood/freecache (16 MiB ring buffer)
	fc := freecache.NewCache(16 * 1024 * 1024)
	if preload {
		for i := range numKeys {
			if err := fc.Set(benchKeyBytes[i], benchValBytes, int(ttl.Seconds())); err != nil {
				b.Fatal(err)
			}
		}
	}

	// 5. maypok86/otter/v2
	ot := otter.Must(&otter.Options[string, []byte]{
		MaximumSize:      10_000,
		ExpiryCalculator: otter.ExpiryWriting[string, []byte](ttl),
	})
	if preload {
		for i := range numKeys {
			ot.Set(benchKeys[i], benchValBytes)
		}
	}

	// 6. dgraph-io/ristretto/v2
	rc, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 100_000,
		MaxCost:     16 * 1024 * 1024,
		BufferItems: 64,
	})
	if err != nil {
		b.Fatal(err)
	}
	if preload {
		for i := range numKeys {
			rc.SetWithTTL(benchKeys[i], benchValBytes, int64(len(benchValBytes)), ttl)
		}
		rc.Wait()
	}

	return []cacheHarness{
		{
			name: "MemoryStore",
			set:  func(i int) { _ = ms.Set(benchKeys[i], benchValBytes, ttl) },
			get: func(i int) bool {
				_, ok := ms.Get(benchKeys[i])
				return ok
			},
			cleanup: func() { _ = ms.Stop() },
		},
		{
			name: "go-cache",
			set:  func(i int) { gc.Set(benchKeys[i], benchValBytes, ttl) },
			get: func(i int) bool {
				_, ok := gc.Get(benchKeys[i])
				return ok
			},
			cleanup: func() {},
		},
		{
			name: "bigcache",
			set:  func(i int) { _ = bc.Set(benchKeys[i], benchValBytes) },
			get: func(i int) bool {
				_, err := bc.Get(benchKeys[i])
				return err == nil
			},
			cleanup: func() { _ = bc.Close() },
		},
		{
			name: "freecache",
			set:  func(i int) { _ = fc.Set(benchKeyBytes[i], benchValBytes, int(ttl.Seconds())) },
			get: func(i int) bool {
				_, err := fc.Get(benchKeyBytes[i])
				return err == nil
			},
			cleanup: func() {},
		},
		{
			name: "otter",
			set:  func(i int) { ot.Set(benchKeys[i], benchValBytes) },
			get: func(i int) bool {
				_, ok := ot.GetIfPresent(benchKeys[i])
				return ok
			},
			cleanup: func() {},
		},
		{
			name: "ristretto",
			set:  func(i int) { rc.SetWithTTL(benchKeys[i], benchValBytes, int64(len(benchValBytes)), ttl) },
			get: func(i int) bool {
				_, ok := rc.Get(benchKeys[i])
				return ok
			},
			cleanup: func() { rc.Close() },
		},
	}
}

func BenchmarkCompare_Get(b *testing.B) {
	harnesses := newHarnesses(b, true)
	for _, h := range harnesses {
		b.Cleanup(h.cleanup)
		b.Run(h.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !h.get(i & keyMask) {
					b.Fatal("unexpected cache miss")
				}
			}
		})
	}
}

func BenchmarkCompare_Set(b *testing.B) {
	harnesses := newHarnesses(b, false)
	for _, h := range harnesses {
		b.Cleanup(h.cleanup)
		b.Run(h.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.set(i & keyMask)
			}
		})
	}
}

func BenchmarkCompare_GetParallel(b *testing.B) {
	harnesses := newHarnesses(b, true)
	for _, h := range harnesses {
		b.Cleanup(h.cleanup)
		b.Run(h.name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if !h.get(i & keyMask) {
						b.Fatal("unexpected cache miss")
					}
					i++
				}
			})
		})
	}
}

func BenchmarkCompare_MixedParallel(b *testing.B) {
	harnesses := newHarnesses(b, true)
	for _, h := range harnesses {
		b.Cleanup(h.cleanup)
		b.Run(h.name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					idx := i & keyMask
					if i&7 == 0 {
						h.set(idx)
					} else {
						h.get(idx)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkCompare_BoundedGetParallel(b *testing.B) {
	const boundedMaxItems = 10_000

	msS3, err := memorystore.New(
		memorystore.WithMaxItems(boundedMaxItems),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
	)
	if err != nil {
		b.Fatal(err)
	}
	msTiny, err := memorystore.New(
		memorystore.WithMaxItems(boundedMaxItems),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
	)
	if err != nil {
		b.Fatal(err)
	}
	ot := otter.Must(&otter.Options[string, []byte]{
		MaximumSize:      boundedMaxItems,
		ExpiryCalculator: otter.ExpiryWriting[string, []byte](ttl),
	})
	rc, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters:        boundedMaxItems * 10,
		MaxCost:            boundedMaxItems,
		BufferItems:        64,
		IgnoreInternalCost: true,
	})
	if err != nil {
		b.Fatal(err)
	}

	for i := range numKeys {
		_ = msS3.Set(benchKeys[i], benchValBytes, ttl)
		_ = msTiny.Set(benchKeys[i], benchValBytes, ttl)
		ot.Set(benchKeys[i], benchValBytes)
		rc.SetWithTTL(benchKeys[i], benchValBytes, 1, ttl)
		if i&31 == 31 {
			rc.Wait()
		}
	}
	rc.Wait()

	bounded := []cacheHarness{
		{
			name:    "MemoryStore-S3FIFO",
			get:     func(i int) bool { _, ok := msS3.Get(benchKeys[i]); return ok },
			cleanup: func() { _ = msS3.Stop() },
		},
		{
			name:    "MemoryStore-TinyLFU",
			get:     func(i int) bool { _, ok := msTiny.Get(benchKeys[i]); return ok },
			cleanup: func() { _ = msTiny.Stop() },
		},
		{
			name:    "otter",
			get:     func(i int) bool { _, ok := ot.GetIfPresent(benchKeys[i]); return ok },
			cleanup: func() {},
		},
		{
			name:    "ristretto",
			get:     func(i int) bool { _, ok := rc.Get(benchKeys[i]); return ok },
			cleanup: func() { rc.Close() },
		},
	}

	for _, h := range bounded {
		b.Cleanup(h.cleanup)
		b.Run(h.name, func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if !h.get(i & keyMask) {
						b.Fatal("unexpected cache miss")
					}
					i++
				}
			})
		})
	}
}

func buildZipfianTrace(universe, ops int, s float64) ([]string, []int) {
	keys := make([]string, universe)
	for i := range keys {
		keys[i] = fmt.Sprintf("zipf:%06d", i)
	}
	cdf := make([]float64, universe)
	var sum float64
	for i := range universe {
		sum += 1.0 / math.Pow(float64(i+1), s)
		cdf[i] = sum
	}
	for i := range universe {
		cdf[i] /= sum
	}
	cdf[universe-1] = 1.0

	rng := rand.New(rand.NewPCG(42, 99))
	trace := make([]int, ops)
	for i := range ops {
		idx := sort.SearchFloat64s(cdf, rng.Float64())
		if idx >= universe {
			idx = universe - 1
		}
		trace[i] = idx
	}
	return keys, trace
}

type hitRatioHarness struct {
	name    string
	get     func(k string) bool
	set     func(k string)
	cleanup func()
}

func newHitRatioHarnesses(tb testing.TB, maxItems int) []hitRatioHarness {
	tb.Helper()

	msVol, err := memorystore.New(
		memorystore.WithMaxItems(maxItems),
		memorystore.WithEvictionPolicy(memorystore.EvictionVolatileTTL),
	)
	if err != nil {
		tb.Fatal(err)
	}
	msS3, err := memorystore.New(
		memorystore.WithMaxItems(maxItems),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
	)
	if err != nil {
		tb.Fatal(err)
	}
	msTiny, err := memorystore.New(
		memorystore.WithMaxItems(maxItems),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
	)
	if err != nil {
		tb.Fatal(err)
	}
	ot := otter.Must(&otter.Options[string, []byte]{
		MaximumSize:      maxItems,
		ExpiryCalculator: otter.ExpiryWriting[string, []byte](ttl),
	})
	rc, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters:        int64(maxItems * 10),
		MaxCost:            int64(maxItems),
		BufferItems:        64,
		IgnoreInternalCost: true,
	})
	if err != nil {
		tb.Fatal(err)
	}

	return []hitRatioHarness{
		{
			name:    "MemoryStore-VolatileTTL",
			get:     func(k string) bool { _, ok := msVol.Get(k); return ok },
			set:     func(k string) { _ = msVol.Set(k, benchValBytes, ttl) },
			cleanup: func() { _ = msVol.Stop() },
		},
		{
			name:    "MemoryStore-S3FIFO",
			get:     func(k string) bool { _, ok := msS3.Get(k); return ok },
			set:     func(k string) { _ = msS3.Set(k, benchValBytes, ttl) },
			cleanup: func() { _ = msS3.Stop() },
		},
		{
			name:    "MemoryStore-TinyLFU",
			get:     func(k string) bool { _, ok := msTiny.Get(k); return ok },
			set:     func(k string) { _ = msTiny.Set(k, benchValBytes, ttl) },
			cleanup: func() { _ = msTiny.Stop() },
		},
		{
			name:    "otter",
			get:     func(k string) bool { _, ok := ot.GetIfPresent(k); return ok },
			set:     func(k string) { ot.Set(k, benchValBytes) },
			cleanup: func() {},
		},
		{
			name: "ristretto",
			get:  func(k string) bool { _, ok := rc.Get(k); return ok },
			set: func(k string) {
				rc.SetWithTTL(k, benchValBytes, 1, ttl)
				rc.Wait()
			},
			cleanup: func() { rc.Close() },
		},
	}
}

func measureZipfHitRatio(h hitRatioHarness, keys []string, trace []int) float64 {
	var hits, total int
	warmup := len(trace) / 5
	for i, idx := range trace {
		k := keys[idx]
		if h.get(k) {
			if i >= warmup {
				hits++
				total++
			}
		} else {
			if i >= warmup {
				total++
			}
			h.set(k)
		}
	}
	return float64(hits) / float64(total)
}

func measureScanHitRatio(h hitRatioHarness, maxItems int) float64 {
	hotCount := maxItems / 2
	scanSize := maxItems * 5
	hotKeys := make([]string, hotCount)
	for i := range hotKeys {
		hotKeys[i] = fmt.Sprintf("hot-%04d", i)
		h.set(hotKeys[i])
	}
	for range 4 {
		for _, k := range hotKeys {
			_ = h.get(k)
		}
	}
	var hits, total int
	for i := range scanSize {
		h.set(fmt.Sprintf("scan-%05d", i))
		hk := hotKeys[i%hotCount]
		total++
		if h.get(hk) {
			hits++
		} else {
			h.set(hk)
		}
	}
	return float64(hits) / float64(total)
}

func BenchmarkCompare_ZipfianHitRatio(b *testing.B) {
	const (
		maxItems = 1000
		universe = 50_000
		ops      = 100_000
	)
	keys, trace := buildZipfianTrace(universe, ops, 0.99)
	names := []string{"MemoryStore-VolatileTTL", "MemoryStore-S3FIFO", "MemoryStore-TinyLFU", "otter", "ristretto"}
	for idx, name := range names {
		b.Run(name, func(b *testing.B) {
			var hitPct float64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				harnesses := newHitRatioHarnesses(b, maxItems)
				h := harnesses[idx]
				b.StartTimer()
				hitPct = measureZipfHitRatio(h, keys, trace) * 100
				b.StopTimer()
				for _, hh := range harnesses {
					hh.cleanup()
				}
			}
			b.ReportMetric(hitPct, "hit%")
		})
	}
}

func BenchmarkCompare_ScanHitRatio(b *testing.B) {
	const maxItems = 1000
	names := []string{"MemoryStore-VolatileTTL", "MemoryStore-S3FIFO", "MemoryStore-TinyLFU", "otter", "ristretto"}
	for idx, name := range names {
		b.Run(name, func(b *testing.B) {
			var hitPct float64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				harnesses := newHitRatioHarnesses(b, maxItems)
				h := harnesses[idx]
				b.StartTimer()
				hitPct = measureScanHitRatio(h, maxItems) * 100
				b.StopTimer()
				for _, hh := range harnesses {
					hh.cleanup()
				}
			}
			b.ReportMetric(hitPct, "hit%")
		})
	}
}

func TestCompare_ZipfianAndScanHitRatio(t *testing.T) {
	const (
		maxItems = 1000
		universe = 50_000
		ops      = 100_000
	)
	keys, trace := buildZipfianTrace(universe, ops, 0.99)
	zipfHarnesses := newHitRatioHarnesses(t, maxItems)
	zipfRes := make(map[string]float64, len(zipfHarnesses))
	for _, h := range zipfHarnesses {
		zipfRes[h.name] = measureZipfHitRatio(h, keys, trace)
		h.cleanup()
	}
	t.Logf("Zipfian(s=0.99, cap=%d, univ=%d) hit ratios: %+v", maxItems, universe, zipfRes)

	volZ := zipfRes["MemoryStore-VolatileTTL"]
	for _, name := range []string{"MemoryStore-S3FIFO", "MemoryStore-TinyLFU"} {
		relGain := (zipfRes[name] - volZ) / volZ
		absGain := zipfRes[name] - volZ
		t.Logf("%s Zipfian vs VolatileTTL: abs=+%.2f%% rel=+%.2f%%", name, absGain*100, relGain*100)
		if relGain < 0.15 {
			t.Errorf("%s Zipfian relative gain = +%.2f%% (abs=+%.2f%%), want >= +15.00%%", name, relGain*100, absGain*100)
		}
	}

	scanHarnesses := newHitRatioHarnesses(t, maxItems)
	scanRes := make(map[string]float64, len(scanHarnesses))
	for _, h := range scanHarnesses {
		scanRes[h.name] = measureScanHitRatio(h, maxItems)
		h.cleanup()
	}
	t.Logf("Scan(cap=%d) hit ratios: %+v", maxItems, scanRes)

	volS := scanRes["MemoryStore-VolatileTTL"]
	for _, name := range []string{"MemoryStore-S3FIFO", "MemoryStore-TinyLFU"} {
		absGain := scanRes[name] - volS
		if absGain < 0.15 {
			t.Errorf("%s Scan gain = +%.2f%%, want >= +15.00%%", name, absGain*100)
		}
	}
}
