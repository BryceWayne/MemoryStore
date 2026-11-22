package benchmarks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/memorystore"
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
	ms := memorystore.NewMemoryStore()
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
