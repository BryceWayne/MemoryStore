package memorystore

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"
)

type benchSmallStruct struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

type benchComplexStruct struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Email     string         `json:"email"`
	Tags      []string       `json:"tags"`
	Scores    []float64      `json:"scores"`
	Metadata  map[string]any `json:"metadata"`
	Active    bool           `json:"active"`
	CreatedAt time.Time      `json:"created_at"`
	Address   struct {
		Street  string `json:"street"`
		City    string `json:"city"`
		State   string `json:"state"`
		ZipCode string `json:"zip_code"`
	} `json:"address"`
}

func newBenchComplexStruct() benchComplexStruct {
	s := benchComplexStruct{
		ID:     42001,
		Name:   "Alice Example",
		Email:  "alice@example.com",
		Tags:   []string{"golang", "cache", "memorystore", "performance", "json"},
		Scores: []float64{98.5, 99.1, 87.3, 94.0, 100.0},
		Metadata: map[string]any{
			"role":       "admin",
			"region":     "us-central1",
			"tier":       "enterprise",
			"retryCount": 3,
		},
		Active:    true,
		CreatedAt: time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC),
	}
	s.Address.Street = "123 Performance Way"
	s.Address.City = "Boulder"
	s.Address.State = "CO"
	s.Address.ZipCode = "80302"
	return s
}

func newSilentBenchStore(b *testing.B) *MemoryStore {
	b.Helper()
	ms, err := New()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = ms.Stop()
	})
	return ms
}

func BenchmarkMemoryStore_Set(b *testing.B) {
	ms := newSilentBenchStore(b)
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-key-%d", i)
	}
	val := []byte("benchmark payload value for memorystore")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ms.Set(keys[i&1023], val, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStore_Get(b *testing.B) {
	ms := newSilentBenchStore(b)
	keys := make([]string, 1024)
	val := []byte("benchmark payload value for memorystore")
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-key-%d", i)
		if err := ms.Set(keys[i], val, time.Minute); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := ms.Get(keys[i&1023]); !ok {
			b.Fatal("expected hit")
		}
	}
}

func BenchmarkMemoryStore_GetParallel(b *testing.B) {
	ms := newSilentBenchStore(b)
	keys := make([]string, 1024)
	val := []byte("benchmark payload value for memorystore")
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-key-%d", i)
		if err := ms.Set(keys[i], val, time.Minute); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, ok := ms.Get(keys[i&1023]); !ok {
				b.Fatal("expected hit")
			}
			i++
		}
	})
}

func BenchmarkMemoryStore_GetParallel_NilRecorder(b *testing.B) {
	ms, err := New(WithMetricsRecorder(nil))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ms.Stop() })

	keys := make([]string, 1024)
	val := []byte("benchmark payload value for memorystore")
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-key-%d", i)
		if err := ms.Set(keys[i], val, time.Minute); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, ok := ms.Get(keys[i&1023]); !ok {
				b.Fatal("expected hit")
			}
			i++
		}
	})
}

func benchmarkBoundedGetParallel(b *testing.B, pol EvictionPolicy) {
	b.Helper()
	ms, err := New(WithMaxItems(10_000), WithEvictionPolicy(pol))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ms.Stop() })

	keys := make([]string, 1024)
	val := []byte("benchmark payload value for memorystore")
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-key-%d", i)
		if err := ms.Set(keys[i], val, time.Minute); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, ok := ms.Get(keys[i&1023]); !ok {
				b.Fatal("expected hit")
			}
			i++
		}
	})
}

func BenchmarkMemoryStore_BoundedGetParallel_S3FIFO(b *testing.B) {
	benchmarkBoundedGetParallel(b, EvictionS3FIFO)
}

func BenchmarkMemoryStore_BoundedGetParallel_TinyLFU(b *testing.B) {
	benchmarkBoundedGetParallel(b, EvictionTinyLFU)
}

func BenchmarkMemoryStore_SetGetParallel(b *testing.B) {
	ms := newSilentBenchStore(b)
	keys := make([]string, 1024)
	val := []byte("benchmark payload value for memorystore")
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-key-%d", i)
		if err := ms.Set(keys[i], val, time.Minute); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i&1023]
			if i&7 == 0 {
				if err := ms.Set(k, val, time.Minute); err != nil {
					b.Fatal(err)
				}
			} else {
				ms.Get(k)
			}
			i++
		}
	})
}

func BenchmarkMemoryStore_SetJSON_Small(b *testing.B) {
	ms := newSilentBenchStore(b)
	payload := benchSmallStruct{ID: 42, Name: "Alice", Active: true}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ms.SetJSON("json-small", &payload, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStore_GetJSON_Small(b *testing.B) {
	ms := newSilentBenchStore(b)
	payload := benchSmallStruct{ID: 42, Name: "Alice", Active: true}
	if err := ms.SetJSON("json-small", &payload, time.Minute); err != nil {
		b.Fatal(err)
	}

	var dst benchSmallStruct
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok, err := ms.GetJSON("json-small", &dst)
		if err != nil || !ok {
			b.Fatalf("GetJSON failed: ok=%v err=%v", ok, err)
		}
	}
}

func BenchmarkMemoryStore_SetJSON_Complex(b *testing.B) {
	ms := newSilentBenchStore(b)
	payload := newBenchComplexStruct()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ms.SetJSON("json-complex", &payload, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStore_GetJSON_Complex(b *testing.B) {
	ms := newSilentBenchStore(b)
	payload := newBenchComplexStruct()
	if err := ms.SetJSON("json-complex", &payload, time.Minute); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var dst benchComplexStruct
		ok, err := ms.GetJSON("json-complex", &dst)
		if err != nil || !ok {
			b.Fatalf("GetJSON failed: ok=%v err=%v", ok, err)
		}
	}
}

func BenchmarkMemoryStore_SetMulti(b *testing.B) {
	ms := newSilentBenchStore(b)
	items := make(map[string][]byte, 100)
	for i := 0; i < 100; i++ {
		items[fmt.Sprintf("batch-key-%d", i)] = []byte("batch-value-payload")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ms.SetMulti(items, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStore_GetMulti(b *testing.B) {
	ms := newSilentBenchStore(b)
	items := make(map[string][]byte, 100)
	keys := make([]string, 100)
	for i := 0; i < 100; i++ {
		k := fmt.Sprintf("batch-key-%d", i)
		keys[i] = k
		items[k] = []byte("batch-value-payload")
	}
	if err := ms.SetMulti(items, time.Minute); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := ms.GetMulti(keys)
		if len(res) != 100 {
			b.Fatalf("expected 100 items, got %d", len(res))
		}
	}
}

func BenchmarkMemoryStore_PubSub_ExactManyTopics(b *testing.B) {
	ms := newSilentBenchStore(b)
	for i := range 100 {
		ch, err := ms.Subscribe(fmt.Sprintf("orders:region-%d", i))
		if err != nil {
			b.Fatal(err)
		}
		go func(c <-chan []byte) {
			for range c {
			}
		}(ch)
	}
	msg := []byte("order-created")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ms.Publish("orders:region-42", msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStore_SnapshotSave_100K(b *testing.B) {
	ms := newSilentBenchStore(b)
	val := bytes.Repeat([]byte("a"), 256)
	for i := range 100_000 {
		if err := ms.Set(fmt.Sprintf("snap-key-%06d", i), val, time.Hour); err != nil {
			b.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		format SnapshotFormat
	}{
		{"Binary", SnapshotBinary},
		{"JSON", SnapshotJSON},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ms.Save(io.Discard, WithSnapshotFormat(tc.format)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemoryStore_SnapshotLoad_100K(b *testing.B) {
	src := newSilentBenchStore(b)
	val := bytes.Repeat([]byte("a"), 256)
	for i := range 100_000 {
		if err := src.Set(fmt.Sprintf("snap-key-%06d", i), val, time.Hour); err != nil {
			b.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		format SnapshotFormat
	}{
		{"Binary", SnapshotBinary},
		{"JSON", SnapshotJSON},
	} {
		var buf bytes.Buffer
		if err := src.Save(&buf, WithSnapshotFormat(tc.format)); err != nil {
			b.Fatal(err)
		}
		raw := buf.Bytes()

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dst, err := New()
				if err != nil {
					b.Fatal(err)
				}
				r := bytes.NewReader(raw)
				b.StartTimer()
				if err := dst.Load(r, WithSnapshotFormat(tc.format)); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				_ = dst.Stop()
			}
		})
	}
}
