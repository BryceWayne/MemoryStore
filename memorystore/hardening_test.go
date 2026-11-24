package memorystore

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryStore_PostStopBehavior(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		name := "unbounded"
		if bounded {
			name = "bounded"
		}
		t.Run(name, func(t *testing.T) {
			mock := newMockPubSub()
			opts := []Option{
				WithPubSub(mock),
				WithInvalidationTopic("inv:test"),
			}
			if bounded {
				opts = append(opts, WithMaxItems(100), WithMaxBytes(4096))
			}
			ms, err := New(opts...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			if err := ms.Set("existing", []byte("val"), time.Minute); err != nil {
				t.Fatalf("Set() setup error = %v", err)
			}
			if err := ms.Set("k1", []byte("v1"), time.Minute); err != nil {
				t.Fatalf("Set() setup error = %v", err)
			}
			if err := ms.SetJSON("existing-json", map[string]int{"a": 42}, time.Minute); err != nil {
				t.Fatalf("SetJSON() setup error = %v", err)
			}

			if err := ms.Stop(); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
			if err := ms.Stop(); err != nil {
				t.Fatalf("second Stop() error = %v", err)
			}
			if !ms.IsStopped() {
				t.Fatal("IsStopped() = false after Stop()")
			}

			beforeMetrics := ms.GetMetrics()
			if beforeMetrics.Items != 0 || beforeMetrics.Bytes != 0 {
				t.Fatalf("post-Stop metrics Items/Bytes = (%d, %d), want (0, 0)", beforeMetrics.Items, beforeMetrics.Bytes)
			}

			if err := ms.Set("new", []byte("v"), time.Minute); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("Set() after Stop = %v, want ErrStoreStopped", err)
			}
			if err := ms.SetMulti(nil, time.Minute); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("SetMulti(nil) after Stop = %v, want ErrStoreStopped", err)
			}
			if err := ms.SetMulti(map[string][]byte{"a": []byte("1"), "b": []byte("2")}, time.Minute); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("SetMulti(small) after Stop = %v, want ErrStoreStopped", err)
			}
			large := make(map[string][]byte, 10)
			for i := range 10 {
				large[fmt.Sprintf("large-%d", i)] = []byte("v")
			}
			if err := ms.SetMulti(large, time.Minute); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("SetMulti(large) after Stop = %v, want ErrStoreStopped", err)
			}

			if err := ms.SetJSON("json", map[string]int{"a": 1}, time.Minute); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("SetJSON() after Stop = %v, want ErrStoreStopped", err)
			}
			if err := ms.SetJSON("bad-json", make(chan int), time.Minute); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("SetJSON(unserializable) after Stop = %v, want ErrStoreStopped", err)
			}
			if err := ms.SetJSONWithOptions("json-opt", map[string]int{"a": 1}, time.Minute, json.Deterministic(true)); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("SetJSONWithOptions() after Stop = %v, want ErrStoreStopped", err)
			}

			if val, ok := ms.Get("existing"); ok || val != nil {
				t.Errorf("Get() after Stop = (%v, %v), want (nil, false)", val, ok)
			}
			if res := ms.GetMulti(nil); res == nil || len(res) != 0 {
				t.Errorf("GetMulti(nil) after Stop = %#v, want non-nil empty map", res)
			}
			if res := ms.GetMulti([]string{"existing", "k1", "a", "b", "c", "d", "e"}); res == nil || len(res) != 0 {
				t.Errorf("GetMulti(keys) after Stop = %#v, want non-nil empty map", res)
			}
			var jsonDst map[string]int
			if ok, err := ms.GetJSON("existing-json", &jsonDst); ok || err != nil {
				t.Errorf("GetJSON() after Stop = (%v, %v), want (false, nil)", ok, err)
			}
			if ok, err := ms.GetJSONWithOptions("existing-json", &jsonDst); ok || err != nil {
				t.Errorf("GetJSONWithOptions() after Stop = (%v, %v), want (false, nil)", ok, err)
			}
			if got, ok, err := ms.GetJSONAs[map[string]int]("existing-json"); ok || err != nil || got != nil {
				t.Errorf("GetJSONAs() after Stop = (%v, %v, %v), want (nil, false, nil)", got, ok, err)
			}

			mock.mu.Lock()
			pubBefore := len(mock.published["inv:test"])
			mock.mu.Unlock()
			if err := ms.Delete("existing"); !errors.Is(err, ErrStoreStopped) {
				t.Errorf("Delete() after Stop = %v, want ErrStoreStopped", err)
			}
			mock.mu.Lock()
			pubAfter := len(mock.published["inv:test"])
			mock.mu.Unlock()
			if pubAfter != pubBefore {
				t.Errorf("Delete() after Stop should not publish invalidations: before=%d after=%d", pubBefore, pubAfter)
			}

			var loaderCalled atomic.Bool
			if val, err := ms.GetOrLoad(t.Context(), "existing", time.Minute, func(_ context.Context) ([]byte, error) {
				loaderCalled.Store(true)
				return []byte("nope"), nil
			}); !errors.Is(err, ErrStoreStopped) || val != nil {
				t.Errorf("GetOrLoad() after Stop = (%v, %v), want (nil, ErrStoreStopped)", val, err)
			}
			if loaderCalled.Load() {
				t.Error("GetOrLoad() invoked loader after Stop")
			}

			if val, err := ms.GetOrLoadJSON[int](t.Context(), "existing", time.Minute, func(_ context.Context) (int, error) {
				loaderCalled.Store(true)
				return 42, nil
			}); !errors.Is(err, ErrStoreStopped) || val != 0 {
				t.Errorf("GetOrLoadJSON() after Stop = (%d, %v), want (0, ErrStoreStopped)", val, err)
			}
			if loaderCalled.Load() {
				t.Error("GetOrLoadJSON() invoked loader after Stop")
			}

			// Safe no-op on invalidation delivery and background sweeps after Stop.
			ms.handleInvalidationMessage([]byte(`{"o":"other","k":["existing"]}`))
			ms.cleanupExpiredItems()
			ms.enforceBounds(0, ms.nowNano(), "")

			afterMetrics := ms.GetMetrics()
			if afterMetrics.Misses != beforeMetrics.Misses {
				t.Errorf("post-Stop Get/GetMulti modified Misses: before=%d after=%d", beforeMetrics.Misses, afterMetrics.Misses)
			}
		})
	}
}

func TestMemoryStore_ConcurrentStopRace(t *testing.T) {
	for round := range 10 {
		var ms *MemoryStore
		var err error
		opts := []Option{
			WithPubSub(NewInMemoryPubSub()),
			WithInvalidationTopic("race:inv"),
			WithCleanupInterval(5 * time.Millisecond),
		}
		if round%2 == 0 {
			opts = append(opts,
				WithMaxItems(16),
				WithMaxBytes(1024),
				WithOnEvict(func(_ string, _ []byte, _ EvictReason) {
					// Calling IsStopped inside OnEvict during Stop() must never deadlock.
					_ = ms.IsStopped()
				}),
			)
		}
		ms, err = New(opts...)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}

		largeBatch := make(map[string][]byte, 8)
		for i := range 8 {
			largeBatch[fmt.Sprintf("lb-%d", i)] = []byte("large-batch-value")
		}
		smallBatch := map[string][]byte{
			"sb-1": []byte("v1"),
			"sb-2": []byte("v2"),
		}

		start := make(chan struct{})
		var wg sync.WaitGroup

		for g := range 12 {
			wg.Go(func() {
				<-start
				for i := range 40 {
					k := fmt.Sprintf("k-%d-%d", g, i%6)
					switch i % 8 {
					case 0:
						_ = ms.Set(k, []byte("payload"), 20*time.Millisecond)
					case 1:
						_ = ms.SetMulti(smallBatch, time.Minute)
					case 2:
						_ = ms.SetMulti(largeBatch, time.Minute)
					case 3:
						_ = ms.SetJSON(k, map[string]int{"i": i}, time.Minute)
					case 4:
						_, _ = ms.Get(k)
						_ = ms.GetMulti([]string{k, "sb-1", "lb-0", "lb-1", "lb-2"})
					case 5:
						_ = ms.Delete(k)
					case 6:
						_, _ = ms.GetOrLoad(context.Background(), k, time.Minute, func(_ context.Context) ([]byte, error) {
							return []byte("loaded"), nil
						})
					case 7:
						_, _ = ms.GetOrLoadJSON[int](context.Background(), "json-"+k, time.Minute, func(_ context.Context) (int, error) {
							return i, nil
						})
					}
					_ = ms.IsStopped()
					_ = ms.GetMetrics()
				}
			})
		}

		// Concurrent Stop callers.
		for range 2 {
			wg.Go(func() {
				<-start
				time.Sleep(time.Millisecond)
				_ = ms.Stop()
			})
		}

		close(start)
		wg.Wait()

		if !ms.IsStopped() {
			t.Fatalf("round %d: expected store to be stopped", round)
		}
	}
}

func TestMemoryStore_GetOrLoad_PanicPropagation(t *testing.T) {
	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	const callers = 12
	loaderStarted := make(chan struct{})
	releasePanic := make(chan struct{})
	var leaderCalls atomic.Int32

	panics := make([]any, callers)
	results := make([][]byte, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	// Start leader first so all waiters deterministically join the same flight.
	wg.Go(func() {
		defer func() {
			panics[0] = recover()
		}()
		results[0], errs[0] = ms.GetOrLoad(t.Context(), "panic-key", time.Minute, func(_ context.Context) ([]byte, error) {
			leaderCalls.Add(1)
			close(loaderStarted)
			<-releasePanic
			panic("loader-boom")
		})
	})

	<-loaderStarted
	for i := 1; i < callers; i++ {
		idx := i
		wg.Go(func() {
			defer func() {
				panics[idx] = recover()
			}()
			results[idx], errs[idx] = ms.GetOrLoad(t.Context(), "panic-key", time.Minute, func(_ context.Context) ([]byte, error) {
				leaderCalls.Add(1)
				return []byte("should-not-run"), nil
			})
		})
	}

	// Give waiters time to block on c.done.
	time.Sleep(15 * time.Millisecond)
	close(releasePanic)
	wg.Wait()

	if got := leaderCalls.Load(); got != 1 {
		t.Errorf("leaderCalls = %d, want 1", got)
	}
	for i := range callers {
		if panics[i] != "loader-boom" {
			t.Errorf("caller %d panic = %v (val=%v, err=%v), want %q", i, panics[i], results[i], errs[i], "loader-boom")
		}
	}

	if _, ok := ms.Get("panic-key"); ok {
		t.Error("panicked load should not populate cache")
	}

	// Subsequent call on same key must recover cleanly.
	val, err := ms.GetOrLoad(t.Context(), "panic-key", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("recovered"), nil
	})
	if err != nil || string(val) != "recovered" {
		t.Fatalf("recovery GetOrLoad() = (%q, %v), want (recovered, nil)", string(val), err)
	}
}

func TestMemoryStore_GetOrLoadJSON_PanicPropagation(t *testing.T) {
	type payload struct {
		OK bool `json:"ok"`
	}

	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	const callers = 8
	loaderStarted := make(chan struct{})
	releasePanic := make(chan struct{})
	panics := make([]any, callers)

	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() {
			panics[0] = recover()
		}()
		_, _ = ms.GetOrLoadJSON[payload](t.Context(), "json-panic", time.Minute, func(_ context.Context) (payload, error) {
			close(loaderStarted)
			<-releasePanic
			panic("json-boom")
		})
	})

	<-loaderStarted
	for i := 1; i < callers; i++ {
		idx := i
		wg.Go(func() {
			defer func() {
				panics[idx] = recover()
			}()
			_, _ = ms.GetOrLoadJSON[payload](t.Context(), "json-panic", time.Minute, func(_ context.Context) (payload, error) {
				return payload{OK: true}, nil
			})
		})
	}

	time.Sleep(15 * time.Millisecond)
	close(releasePanic)
	wg.Wait()

	for i := range callers {
		if panics[i] != "json-boom" {
			t.Errorf("caller %d panic = %v, want json-boom", i, panics[i])
		}
	}

	got, err := ms.GetOrLoadJSON[payload](t.Context(), "json-panic", time.Minute, func(_ context.Context) (payload, error) {
		return payload{OK: true}, nil
	})
	if err != nil || !got.OK {
		t.Fatalf("recovery GetOrLoadJSON() = (%+v, %v), want ({OK:true}, nil)", got, err)
	}
}

func TestSingleflight_Goexit(t *testing.T) {
	var sf singleflightGroup
	started := make(chan struct{})
	release := make(chan struct{})

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _, _ = sf.do(t.Context(), "goexit-key", func(_ context.Context) ([]byte, error) {
			close(started)
			<-release
			runtime.Goexit()
			return nil, nil
		})
	}()

	<-started
	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := sf.do(t.Context(), "goexit-key", func(_ context.Context) ([]byte, error) {
			return []byte("unexpected"), nil
		})
		waiterDone <- err
	}()

	time.Sleep(10 * time.Millisecond)
	close(release)
	<-leaderDone

	if err := <-waiterDone; !errors.Is(err, errGoexit) {
		t.Fatalf("waiter error = %v, want %v", err, errGoexit)
	}

	val, _, err := sf.do(t.Context(), "goexit-key", func(_ context.Context) ([]byte, error) {
		return []byte("ok"), nil
	})
	if err != nil || string(val) != "ok" {
		t.Fatalf("subsequent sf.do = (%q, %v), want (ok, nil)", string(val), err)
	}
}

func TestMemoryStore_GetOrLoadJSON_LeaderSkipsUnmarshalAndPropagatesSetError(t *testing.T) {
	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	type itemVal struct {
		ID int `json:"id"`
	}
	var hookCalls atomic.Int32
	spyOpt := json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, dst *itemVal) error {
		hookCalls.Add(1)
		type alias itemVal
		var a alias
		if err := json.UnmarshalDecode(dec, &a); err != nil {
			return err
		}
		*dst = itemVal(a)
		return nil
	}))

	got, err := ms.GetOrLoadJSON[itemVal](t.Context(), "leader-only", time.Minute, func(_ context.Context) (itemVal, error) {
		return itemVal{ID: 99}, nil
	}, spyOpt)
	if err != nil || got.ID != 99 {
		t.Fatalf("GetOrLoadJSON() = (%+v, %v), want ({ID:99}, nil)", got, err)
	}
	if c := hookCalls.Load(); c != 0 {
		t.Errorf("leader invoked json.Unmarshal %d times, want 0", c)
	}

	// Verify coalesced waiter still unmarshals cleanly while leader skips unmarshal.
	hookCalls.Store(0)
	started := make(chan struct{})
	release := make(chan struct{})
	var leaderRes, waiterRes itemVal
	var leaderErr, waiterErr error
	var wg sync.WaitGroup

	wg.Go(func() {
		leaderRes, leaderErr = ms.GetOrLoadJSON[itemVal](t.Context(), "leader-waiter", time.Minute, func(_ context.Context) (itemVal, error) {
			close(started)
			<-release
			return itemVal{ID: 7}, nil
		}, spyOpt)
	})
	<-started
	wg.Go(func() {
		waiterRes, waiterErr = ms.GetOrLoadJSON[itemVal](t.Context(), "leader-waiter", time.Minute, func(_ context.Context) (itemVal, error) {
			return itemVal{ID: 999}, nil
		}, spyOpt)
	})
	time.Sleep(15 * time.Millisecond)
	close(release)
	wg.Wait()

	if leaderErr != nil || leaderRes.ID != 7 {
		t.Errorf("leader = (%+v, %v), want ({ID:7}, nil)", leaderRes, leaderErr)
	}
	if waiterErr != nil || waiterRes.ID != 7 {
		t.Errorf("waiter = (%+v, %v), want ({ID:7}, nil)", waiterRes, waiterErr)
	}
	if c := hookCalls.Load(); c != 1 {
		t.Errorf("expected exactly 1 unmarshal (waiter only), got %d", c)
	}

	// Verify ErrStoreStopped from m.Set inside GetOrLoadJSON propagates instead of returning leaderVal.
	got, err = ms.GetOrLoadJSON[itemVal](t.Context(), "stop-mid-load", time.Minute, func(_ context.Context) (itemVal, error) {
		_ = ms.Stop()
		return itemVal{ID: 123}, nil
	})
	if !errors.Is(err, ErrStoreStopped) || got != (itemVal{}) {
		t.Errorf("GetOrLoadJSON(stop during load) = (%+v, %v), want ({}, ErrStoreStopped)", got, err)
	}
}

func TestMemoryStore_UnboundedMetricsBytesAccuracy(t *testing.T) {
	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	check := func(step string, wantItems int, wantBytes int64) {
		t.Helper()
		m := ms.GetMetrics()
		if m.Items != wantItems || m.Bytes != wantBytes {
			t.Fatalf("%s: GetMetrics() = (Items=%d, Bytes=%d), want (%d, %d)", step, m.Items, m.Bytes, wantItems, wantBytes)
		}
		if m.Bytes < 0 {
			t.Fatalf("%s: negative Bytes=%d", step, m.Bytes)
		}
	}

	check("initial", 0, 0)

	// Set insert
	_ = ms.Set("k1", []byte("12345"), time.Minute) // len(k1)=2 + 5 = 7
	check("after insert k1", 1, 7)

	// Overwrite larger
	_ = ms.Set("k1", []byte("1234567890"), time.Minute) // 2 + 10 = 12
	check("after grow k1", 1, 12)

	// Overwrite smaller
	_ = ms.Set("k1", []byte("x"), time.Minute) // 2 + 1 = 3
	check("after shrink k1", 1, 3)

	// Small SetMulti (<= 4): overwrite k1 + insert k2, k3
	_ = ms.SetMulti(map[string][]byte{
		"k1": []byte("abcd"), // 2 + 4 = 6
		"k2": []byte("ef"),   // 2 + 2 = 4
		"k3": []byte("ghij"), // 2 + 4 = 6
	}, time.Minute)
	check("after small SetMulti", 3, 16)

	// Large SetMulti (> 4): overwrite k2 + insert 5 new keys
	large := map[string][]byte{
		"k2": []byte("z"),   // 2 + 1 = 3 (was 4 -> delta -1)
		"b1": []byte("111"), // 2 + 3 = 5
		"b2": []byte("222"), // 2 + 3 = 5
		"b3": []byte("333"), // 2 + 3 = 5
		"b4": []byte("444"), // 2 + 3 = 5
		"b5": []byte("555"), // 2 + 3 = 5
	}
	_ = ms.SetMulti(large, time.Minute)
	check("after large SetMulti", 8, 40)

	// Delete missing and existing keys
	_ = ms.Delete("does-not-exist")
	check("after delete missing", 8, 40)

	_ = ms.Delete("k1") // -6
	_ = ms.Delete("b5") // -5
	check("after delete k1,b5", 6, 29)

	// Expiration via cleanupExpiredItems
	_ = ms.Set("exp", []byte("expire-me"), 5*time.Millisecond) // +12 -> 41
	check("after set expiring", 7, 41)
	time.Sleep(20 * time.Millisecond)
	ms.cleanupExpiredItems()
	check("after cleanupExpiredItems", 6, 29)

	// Delete all remaining keys -> exact 0
	for _, k := range []string{"k2", "k3", "b1", "b2", "b3", "b4"} {
		_ = ms.Delete(k)
	}
	check("after delete all", 0, 0)

	_ = ms.Stop()
	check("after Stop", 0, 0)
}
