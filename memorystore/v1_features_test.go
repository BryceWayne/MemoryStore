package memorystore

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ==================== Pillar #1: Distributed Invalidation ====================

func TestDistributedInvalidation_TwoNodes(t *testing.T) {
	sharedBroker := NewInMemoryPubSub()
	defer func() { _ = sharedBroker.Close() }()

	var nodeBEvicted sync.Map
	nodeA, err := New(
		WithPubSub(sharedBroker),
		WithInvalidationTopic("cache:inv"),
	)
	if err != nil {
		t.Fatalf("New(nodeA) error = %v", err)
	}
	defer func() {
		// Prevent closing sharedBroker prematurely while nodeB is active; stop both cleanly below.
	}()

	nodeB, err := New(
		WithPubSub(sharedBroker),
		WithInvalidationTopic("cache:inv"),
		WithOnEvict(func(key string, _ []byte, reason EvictReason) {
			nodeBEvicted.Store(key, reason)
		}),
	)
	if err != nil {
		t.Fatalf("New(nodeB) error = %v", err)
	}

	if nodeA.InstanceID() == "" || nodeB.InstanceID() == "" || nodeA.InstanceID() == nodeB.InstanceID() {
		t.Fatalf("expected distinct non-empty instance IDs, got A=%q B=%q", nodeA.InstanceID(), nodeB.InstanceID())
	}

	// Populate nodeA first and wait for its invalidation broadcast to settle,
	// then write k1 on nodeB.
	_ = nodeA.Set("k1", []byte("v1-a"), time.Minute)
	time.Sleep(20 * time.Millisecond)
	_ = nodeB.Set("k1", []byte("v1-b"), time.Minute)
	time.Sleep(30 * time.Millisecond)

	// Because nodeB just called Set("k1"), nodeA should have evicted its stale k1,
	// while nodeB MUST still hold its own "v1-b" (echo suppression).
	if v, ok := nodeB.Get("k1"); !ok || string(v) != "v1-b" {
		t.Fatalf("echo suppression failed on nodeB: got (%q, %v), want (%q, true)", string(v), ok, "v1-b")
	}
	if _, ok := nodeA.Get("k1"); ok {
		t.Fatal("expected nodeA to invalidate k1 after nodeB wrote k1")
	}

	// Now nodeA deletes k1 -> nodeB must evict k1 and invoke OnEvict(EvictDeleted).
	if err := nodeA.Delete("k1"); err != nil {
		t.Fatalf("nodeA.Delete() error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := nodeB.Get("k1"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := nodeB.Get("k1"); ok {
		t.Fatal("expected nodeB to evict k1 after nodeA.Delete")
	}
	if reason, ok := nodeBEvicted.Load("k1"); !ok || reason != EvictDeleted {
		t.Errorf("nodeB OnEvict(k1) = (%v, %v), want (EvictDeleted, true)", reason, ok)
	}

	// SetMulti (small <=4 and large >4) broadcasts invalidation for all keys.
	_ = nodeB.Set("m1", []byte("stale"), time.Minute)
	_ = nodeB.Set("m2", []byte("stale"), time.Minute)
	time.Sleep(20 * time.Millisecond)
	_ = nodeA.SetMulti(map[string][]byte{
		"m1": []byte("new1"),
		"m2": []byte("new2"),
	}, time.Minute)

	batch := make(map[string][]byte, 10)
	for i := range 10 {
		k := fmt.Sprintf("bulk-%d", i)
		_ = nodeB.Set(k, []byte("stale"), time.Minute)
		batch[k] = []byte("fresh")
	}
	time.Sleep(20 * time.Millisecond)
	_ = nodeA.SetMulti(batch, time.Minute)

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := nodeB.Get("bulk-9"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := nodeB.Get("m1"); ok {
		t.Error("expected nodeB m1 to be invalidated by nodeA SetMulti (small)")
	}
	if _, ok := nodeB.Get("bulk-9"); ok {
		t.Error("expected nodeB bulk-9 to be invalidated by nodeA SetMulti (large)")
	}

	// Malformed invalidation message must be ignored safely.
	_ = sharedBroker.Publish(t.Context(), "cache:inv", []byte("{not-json"))
	time.Sleep(20 * time.Millisecond)

	_ = nodeA.Stop()
	_ = nodeB.Stop()

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/v2/memorystore") {
			t.Errorf("detected leaked goroutine after stopping invalidation nodes:\n%s", buf.String())
		}
	}
}

// ==================== Pillar #2: Go 1.27 Generic Methods & Singleflight ====================

func TestMemoryStore_GetJSONAs(t *testing.T) {
	type profile struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}

	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	// Miss
	got, ok, err := ms.GetJSONAs[profile]("missing")
	if err != nil || ok || got != (profile{}) {
		t.Fatalf("GetJSONAs(missing) = (%+v, %v, %v), want zero/false/nil", got, ok, err)
	}

	// Hit
	want := profile{ID: 7, Name: "Grace"}
	if err := ms.SetJSON("p:7", &want, time.Minute); err != nil {
		t.Fatalf("SetJSON() error = %v", err)
	}
	got, ok, err = ms.GetJSONAs[profile]("p:7")
	if err != nil || !ok || got != want {
		t.Fatalf("GetJSONAs(p:7) = (%+v, %v, %v), want (%+v, true, nil)", got, ok, err, want)
	}

	// Options respected (RejectUnknownMembers)
	_ = ms.Set("p:extra", []byte(`{"id":8,"name":"Ada","extra":true}`), time.Minute)
	if _, ok, err := ms.GetJSONAs[profile]("p:extra", json.RejectUnknownMembers(true)); !ok || err == nil {
		t.Errorf("expected unknown member error, got ok=%v err=%v", ok, err)
	}
}

func TestMemoryStore_GetOrLoad_Singleflight(t *testing.T) {
	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	var calls atomic.Int32
	start := make(chan struct{})
	const workers = 20

	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)

	for i := range workers {
		wg.Go(func() {
			<-start
			val, err := ms.GetOrLoad(t.Context(), "hot-key", time.Minute, func(_ context.Context) ([]byte, error) {
				calls.Add(1)
				time.Sleep(30 * time.Millisecond)
				return []byte("computed-once"), nil
			})
			results[i] = string(val)
			errs[i] = err
		})
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("loader invoked %d times, want 1 (singleflight)", got)
	}
	for i := range workers {
		if errs[i] != nil || results[i] != "computed-once" {
			t.Errorf("worker %d got (%q, %v), want (computed-once, nil)", i, results[i], errs[i])
		}
	}

	// Subsequent call hits cache without invoking loader.
	val, err := ms.GetOrLoad(t.Context(), "hot-key", time.Minute, func(_ context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("should-not-run"), nil
	})
	if err != nil || string(val) != "computed-once" || calls.Load() != 1 {
		t.Errorf("fast-path hit failed: val=%q err=%v calls=%d", string(val), err, calls.Load())
	}

	// Loader errors are not cached.
	boom := errors.New("db down")
	if _, err := ms.GetOrLoad(t.Context(), "err-key", time.Minute, func(_ context.Context) ([]byte, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Errorf("expected boom error, got %v", err)
	}
	if _, ok := ms.Get("err-key"); ok {
		t.Error("loader error should not populate cache")
	}

	// Context cancellation on waiter returns promptly.
	block := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = ms.GetOrLoad(context.Background(), "slow-key", time.Minute, func(_ context.Context) ([]byte, error) {
			<-block
			return []byte("done"), nil
		})
	}()
	time.Sleep(15 * time.Millisecond)

	waitCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := ms.GetOrLoad(waitCtx, "slow-key", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("never"), nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded for timed-out waiter, got %v", err)
	}
	close(block)
	<-leaderDone
}

func TestMemoryStore_GetOrLoadJSON_Singleflight(t *testing.T) {
	type account struct {
		ID      string `json:"id"`
		Balance int    `json:"balance"`
	}

	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	var calls atomic.Int32
	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	got := make([]account, workers)
	errs := make([]error, workers)

	for i := range workers {
		wg.Go(func() {
			<-start
			acc, err := ms.GetOrLoadJSON[account](t.Context(), "acct:42", time.Minute, func(_ context.Context) (account, error) {
				calls.Add(1)
				time.Sleep(25 * time.Millisecond)
				return account{ID: "42", Balance: 500}, nil
			})
			got[i] = acc
			errs[i] = err
		})
	}
	close(start)
	wg.Wait()

	if calls.Load() != 1 {
		t.Errorf("JSON loader called %d times, want 1", calls.Load())
	}
	want := account{ID: "42", Balance: 500}
	for i := range workers {
		if errs[i] != nil || got[i] != want {
			t.Errorf("worker %d got (%+v, %v), want (%+v, nil)", i, got[i], errs[i], want)
		}
	}

	// Hit path via GetOrLoadJSON skips loader.
	acc, err := ms.GetOrLoadJSON[account](t.Context(), "acct:42", time.Minute, func(_ context.Context) (account, error) {
		calls.Add(1)
		return account{}, errors.New("should not run")
	})
	if err != nil || acc != want || calls.Load() != 1 {
		t.Errorf("GetOrLoadJSON cache hit failed: acc=%+v err=%v calls=%d", acc, err, calls.Load())
	}

	// Canceled context before load.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ms.GetOrLoadJSON[account](canceled, "acct:missing", time.Minute, func(_ context.Context) (account, error) {
		return account{}, nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// ==================== Pillar #3: Bounded Capacity & OnEvict ====================

func TestMemoryStore_MaxItemsAndOnEvict(t *testing.T) {
	var capacityEvictions atomic.Int64
	var ms *MemoryStore
	var err error
	ms, err = New(
		WithMaxItems(20),
		WithOnEvict(func(_ string, _ []byte, reason EvictReason) {
			// Verify calling store methods inside OnEvict never deadlocks.
			_ = ms.IsStopped()
			if reason == EvictCapacity {
				capacityEvictions.Add(1)
			}
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	for i := range 100 {
		if err := ms.Set(fmt.Sprintf("item-%d", i), []byte("payload"), time.Duration(i+1)*time.Second); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	metrics := ms.GetMetrics()
	if metrics.Items > 20 {
		t.Errorf("Items = %d, want <= 20", metrics.Items)
	}
	if capacityEvictions.Load() < 80 {
		t.Errorf("capacityEvictions = %d, want >= 80", capacityEvictions.Load())
	}
	if metrics.Evictions < 80 {
		t.Errorf("metrics.Evictions = %d, want >= 80", metrics.Evictions)
	}
}

func TestMemoryStore_MaxBytesAndUpdateTracking(t *testing.T) {
	ms, err := New(WithMaxBytes(500))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	val := bytes.Repeat([]byte("x"), 40)
	for i := range 50 {
		_ = ms.Set(fmt.Sprintf("bkey-%02d", i), val, time.Minute)
	}

	metrics := ms.GetMetrics()
	if metrics.Bytes > 500 {
		t.Errorf("Bytes = %d, want <= 500", metrics.Bytes)
	}
	if metrics.Items == 0 {
		t.Error("expected some items to remain within MaxBytes budget")
	}

	// Overwrite with smaller payload should shrink tracked Bytes accurately.
	_ = ms.Set("shrink", bytes.Repeat([]byte("a"), 100), time.Minute)
	before := ms.GetMetrics().Bytes
	_ = ms.Set("shrink", []byte("a"), time.Minute)
	after := ms.GetMetrics().Bytes
	if after >= before {
		t.Errorf("expected Bytes to decrease on overwrite: before=%d after=%d", before, after)
	}

	// SetMulti respects MaxBytes.
	batch := make(map[string][]byte, 20)
	for i := range 20 {
		batch[fmt.Sprintf("mb-%02d", i)] = bytes.Repeat([]byte("z"), 40)
	}
	_ = ms.SetMulti(batch, time.Minute)
	if got := ms.GetMetrics().Bytes; got > 500 {
		t.Errorf("Bytes after SetMulti = %d, want <= 500", got)
	}
}

func TestEvictReason_String(t *testing.T) {
	tests := []struct {
		r    EvictReason
		want string
	}{
		{EvictExpired, "expired"},
		{EvictCapacity, "capacity"},
		{EvictDeleted, "deleted"},
		{EvictReason(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.r.String(); got != tt.want {
			t.Errorf("EvictReason(%d).String() = %q, want %q", tt.r, got, tt.want)
		}
	}
}
