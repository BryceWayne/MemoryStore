package e2e_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

func TestTier1_F1_CoreKV(t *testing.T) {
	t.Run("NewDefaultAndGetMiss", func(t *testing.T) {
		s := mustNewStore(t)
		val, ok := s.Get("does-not-exist")
		if ok || val != nil {
			t.Fatalf("Get(missing) = (%v, %v), want (nil, false)", val, ok)
		}
	})

	t.Run("SetAndGetRoundTrip", func(t *testing.T) {
		s := mustNewStore(t)
		if err := s.Set("greeting", []byte("hello-v2"), time.Minute); err != nil {
			t.Fatalf("Set(greeting) error = %v", err)
		}
		got, ok := s.Get("greeting")
		if !ok || !bytes.Equal(got, []byte("hello-v2")) {
			t.Fatalf("Get(greeting) = (%q, %v), want (%q, true)", got, ok, "hello-v2")
		}

		if err := s.Set("empty", []byte{}, time.Minute); err != nil {
			t.Fatalf("Set(empty) error = %v", err)
		}
		emptyGot, ok := s.Get("empty")
		if !ok || len(emptyGot) != 0 {
			t.Fatalf("Get(empty) = (%v, %v), want (empty slice, true)", emptyGot, ok)
		}
	})

	t.Run("DeleteRemovesExistingAndIgnoresMissing", func(t *testing.T) {
		s := mustNewStore(t)
		if err := s.Set("temp", []byte("value"), time.Minute); err != nil {
			t.Fatalf("Set(temp) error = %v", err)
		}
		if err := s.Delete("temp"); err != nil {
			t.Fatalf("Delete(temp) error = %v", err)
		}
		if val, ok := s.Get("temp"); ok || val != nil {
			t.Fatalf("Get(temp) after Delete = (%v, %v), want (nil, false)", val, ok)
		}
		if err := s.Delete("never-existed"); err != nil {
			t.Fatalf("Delete(never-existed) error = %v", err)
		}
	})

	t.Run("SetMultiAndGetMulti", func(t *testing.T) {
		s := mustNewStore(t)
		input := map[string][]byte{
			"alpha": []byte("1"),
			"beta":  []byte("2"),
			"gamma": []byte("3"),
		}
		if err := s.SetMulti(input, time.Minute); err != nil {
			t.Fatalf("SetMulti() error = %v", err)
		}

		got := s.GetMulti([]string{"alpha", "gamma", "missing"})
		if len(got) != 2 {
			t.Fatalf("len(GetMulti) = %d, want 2 (%v)", len(got), got)
		}
		if !bytes.Equal(got["alpha"], []byte("1")) {
			t.Fatalf("GetMulti[alpha] = %q, want 1", got["alpha"])
		}
		if !bytes.Equal(got["gamma"], []byte("3")) {
			t.Fatalf("GetMulti[gamma] = %q, want 3", got["gamma"])
		}
		if _, exists := got["missing"]; exists {
			t.Fatalf("GetMulti unexpectedly included missing key")
		}
	})

	t.Run("NoExpirationPersistsAcrossCleanup", func(t *testing.T) {
		s := mustNewStore(t,
			memorystore.WithCleanupInterval(10*time.Millisecond),
			memorystore.WithPreciseClock(),
		)
		if err := s.Set("permanent", []byte("stay"), memorystore.NoExpiration); err != nil {
			t.Fatalf("Set(permanent) error = %v", err)
		}
		if err := s.Set("ephemeral", []byte("go"), 15*time.Millisecond); err != nil {
			t.Fatalf("Set(ephemeral) error = %v", err)
		}

		eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
			_, ok := s.Get("ephemeral")
			return !ok
		}, "ephemeral key should expire")

		val, ok := s.Get("permanent")
		if !ok || !bytes.Equal(val, []byte("stay")) {
			t.Fatalf("Get(permanent) = (%q, %v), want (%q, true)", val, ok, "stay")
		}
	})

	t.Run("OverwriteUpdatesValueAndTTL", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		if err := s.Set("mutable", []byte("v1"), 25*time.Millisecond); err != nil {
			t.Fatalf("Set(v1) error = %v", err)
		}
		if err := s.Set("mutable", []byte("v2"), time.Minute); err != nil {
			t.Fatalf("Set(v2) error = %v", err)
		}

		time.Sleep(50 * time.Millisecond)

		got, ok := s.Get("mutable")
		if !ok || !bytes.Equal(got, []byte("v2")) {
			t.Fatalf("Get(mutable) after overwrite = (%q, %v), want (%q, true)", got, ok, "v2")
		}
	})
}

func TestTier1_F2_JSONAndLoader(t *testing.T) {
	type user struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}

	t.Run("SetJSONAndGetJSON", func(t *testing.T) {
		s := mustNewStore(t)
		want := user{ID: 42, Name: "Alice"}
		if err := s.SetJSON("user:42", want, time.Minute); err != nil {
			t.Fatalf("SetJSON() error = %v", err)
		}

		var got user
		ok, err := s.GetJSON("user:42", &got)
		if err != nil || !ok {
			t.Fatalf("GetJSON(hit) = (%v, %v), want (true, nil)", ok, err)
		}
		if got != want {
			t.Fatalf("GetJSON(hit) value = %+v, want %+v", got, want)
		}

		var missing user
		ok, err = s.GetJSON("user:missing", &missing)
		if err != nil || ok {
			t.Fatalf("GetJSON(miss) = (%v, %v), want (false, nil)", ok, err)
		}
	})

	t.Run("SetJSONWithOptionsAndGetJSONWithOptions", func(t *testing.T) {
		s := mustNewStore(t)
		input := map[string]int{"b": 2, "a": 1}
		if err := s.SetJSONWithOptions("pair", input, time.Minute, json.Deterministic(true)); err != nil {
			t.Fatalf("SetJSONWithOptions() error = %v", err)
		}

		raw, ok := s.Get("pair")
		if !ok {
			t.Fatalf("Get(pair) missing after SetJSONWithOptions")
		}
		if string(raw) != `{"a":1,"b":2}` {
			t.Fatalf("SetJSONWithOptions deterministic JSON = %s, want {\"a\":1,\"b\":2}", raw)
		}

		type pair struct {
			A int `json:"a"`
			B int `json:"b"`
		}
		var got pair
		ok, err := s.GetJSONWithOptions("pair", &got, json.RejectUnknownMembers(true))
		if err != nil || !ok {
			t.Fatalf("GetJSONWithOptions() = (%v, %v), want (true, nil)", ok, err)
		}
		if got != (pair{A: 1, B: 2}) {
			t.Fatalf("GetJSONWithOptions() value = %+v, want {A:1 B:2}", got)
		}
	})

	t.Run("GetJSONAsTyped", func(t *testing.T) {
		s := mustNewStore(t)
		want := user{ID: 7, Name: "Grace"}
		if err := s.SetJSON("user:7", want, time.Minute); err != nil {
			t.Fatalf("SetJSON() error = %v", err)
		}

		got, ok, err := s.GetJSONAs[user]("user:7")
		if err != nil || !ok {
			t.Fatalf("GetJSONAs(hit) = (%+v, %v, %v), want (user, true, nil)", got, ok, err)
		}
		if got != want {
			t.Fatalf("GetJSONAs(hit) = %+v, want %+v", got, want)
		}

		miss, ok, err := s.GetJSONAs[user]("user:none")
		if err != nil || ok || miss != (user{}) {
			t.Fatalf("GetJSONAs(miss) = (%+v, %v, %v), want (zero, false, nil)", miss, ok, err)
		}
	})

	t.Run("GetOrLoadMissThenHit", func(t *testing.T) {
		s := mustNewStore(t)
		var calls atomic.Int32

		v1, err := s.GetOrLoad(context.Background(), "cfg:1", time.Minute, func(_ context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte("loaded-cfg"), nil
		})
		if err != nil {
			t.Fatalf("GetOrLoad(miss) error = %v", err)
		}
		if !bytes.Equal(v1, []byte("loaded-cfg")) {
			t.Fatalf("GetOrLoad(miss) = %q, want loaded-cfg", v1)
		}

		v2, err := s.GetOrLoad(context.Background(), "cfg:1", time.Minute, func(_ context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte("should-not-run"), nil
		})
		if err != nil {
			t.Fatalf("GetOrLoad(hit) error = %v", err)
		}
		if !bytes.Equal(v2, []byte("loaded-cfg")) {
			t.Fatalf("GetOrLoad(hit) = %q, want loaded-cfg", v2)
		}
		if calls.Load() != 1 {
			t.Fatalf("loader call count = %d, want 1", calls.Load())
		}
	})

	t.Run("GetOrLoadJSONSingleflightCoalescing", func(t *testing.T) {
		s := mustNewStore(t)
		const workers = 16

		var calls atomic.Int32
		startGate := make(chan struct{})
		results := make([]user, workers)
		errs := make([]error, workers)

		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-startGate
				u, err := s.GetOrLoadJSON[user](context.Background(), "sf:user", time.Minute, func(_ context.Context) (user, error) {
					calls.Add(1)
					time.Sleep(30 * time.Millisecond)
					return user{ID: 99, Name: "Coalesced"}, nil
				})
				results[idx] = u
				errs[idx] = err
			}(i)
		}

		close(startGate)
		wg.Wait()

		if calls.Load() != 1 {
			t.Fatalf("singleflight loader calls = %d, want 1", calls.Load())
		}
		want := user{ID: 99, Name: "Coalesced"}
		for i := range workers {
			if errs[i] != nil {
				t.Fatalf("worker %d error = %v", i, errs[i])
			}
			if results[i] != want {
				t.Fatalf("worker %d result = %+v, want %+v", i, results[i], want)
			}
		}
	})
}

func TestTier1_F3_SliceIsolation(t *testing.T) {
	t.Run("DefaultZeroCopyRoundTrip", func(t *testing.T) {
		s := mustNewStore(t)
		in := []byte("original-payload")
		if err := s.Set("k", in, time.Minute); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		got, ok := s.Get("k")
		if !ok || !bytes.Equal(got, []byte("original-payload")) {
			t.Fatalf("Get() = (%q, %v), want (original-payload, true)", got, ok)
		}
	})

	t.Run("CopyOnWriteIsolatesCallerMutation", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithCopyOnWrite(true))
		buf := []byte("safe-write")
		if err := s.Set("cow", buf, time.Minute); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		copy(buf, "CORRUPTED!")

		got, ok := s.Get("cow")
		if !ok || !bytes.Equal(got, []byte("safe-write")) {
			t.Fatalf("Get(cow) = (%q, %v), want (safe-write, true)", got, ok)
		}
	})

	t.Run("CopyOnReadIsolatesReturnedMutation", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithCopyOnRead(true))
		if err := s.Set("cor", []byte("safe-read"), time.Minute); err != nil {
			t.Fatalf("Set() error = %v", err)
		}

		first, ok := s.Get("cor")
		if !ok {
			t.Fatal("first Get(cor) missing")
		}
		copy(first, "MUTATED!!")

		second, ok := s.Get("cor")
		if !ok || !bytes.Equal(second, []byte("safe-read")) {
			t.Fatalf("second Get(cor) = (%q, %v), want (safe-read, true)", second, ok)
		}
	})

	t.Run("CopyOnWriteAndCopyOnReadCombined", func(t *testing.T) {
		s := mustNewStore(t,
			memorystore.WithCopyOnWrite(true),
			memorystore.WithCopyOnRead(true),
		)
		in := []byte("double-isolated")
		if err := s.Set("both", in, time.Minute); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		copy(in, "AAAAAAAAAAAAAAA")

		first, ok := s.Get("both")
		if !ok || !bytes.Equal(first, []byte("double-isolated")) {
			t.Fatalf("first Get(both) = (%q, %v), want (double-isolated, true)", first, ok)
		}
		copy(first, "BBBBBBBBBBBBBBB")

		second, ok := s.Get("both")
		if !ok || !bytes.Equal(second, []byte("double-isolated")) {
			t.Fatalf("second Get(both) = (%q, %v), want (double-isolated, true)", second, ok)
		}
	})

	t.Run("SetMultiCopyOnWriteIsolation", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithCopyOnWrite(true))
		v1 := []byte("multi-one")
		v2 := []byte("multi-two")
		err := s.SetMulti(map[string][]byte{
			"m1": v1,
			"m2": v2,
		}, time.Minute)
		if err != nil {
			t.Fatalf("SetMulti() error = %v", err)
		}

		copy(v1, "XXXXXXXXX")
		copy(v2, "YYYYYYYYY")

		got := s.GetMulti([]string{"m1", "m2"})
		if !bytes.Equal(got["m1"], []byte("multi-one")) {
			t.Fatalf("GetMulti[m1] = %q, want multi-one", got["m1"])
		}
		if !bytes.Equal(got["m2"], []byte("multi-two")) {
			t.Fatalf("GetMulti[m2] = %q, want multi-two", got["m2"])
		}
	})
}

func TestTier1_F4_LifecycleAndStop(t *testing.T) {
	t.Run("NewStoreStartsUnstopped", func(t *testing.T) {
		s := mustNewStore(t)
		if s == nil {
			t.Fatal("mustNewStore returned nil")
		}
		if s.IsStopped() {
			t.Fatal("new store reported IsStopped() == true")
		}
	})

	t.Run("StopIsIdempotentAndSetsIsStopped", func(t *testing.T) {
		s := mustNewStore(t)
		if err := s.Stop(); err != nil {
			t.Fatalf("first Stop() error = %v", err)
		}
		if !s.IsStopped() {
			t.Fatal("IsStopped() = false after first Stop()")
		}
		if err := s.Stop(); err != nil {
			t.Fatalf("second Stop() error = %v", err)
		}
		if !s.IsStopped() {
			t.Fatal("IsStopped() = false after second Stop()")
		}
	})

	t.Run("InstanceIDUniqueNonEmptyPerStore", func(t *testing.T) {
		s1 := mustNewStore(t)
		s2 := mustNewStore(t)

		id1 := s1.InstanceID()
		id2 := s2.InstanceID()
		if id1 == "" || id2 == "" {
			t.Fatalf("expected non-empty InstanceIDs, got %q and %q", id1, id2)
		}
		if id1 == id2 {
			t.Fatalf("expected distinct InstanceIDs across stores, both were %q", id1)
		}
	})

	t.Run("GetReturnsMissAfterStop", func(t *testing.T) {
		s := mustNewStore(t)
		if err := s.Set("k", []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}

		val, ok := s.Get("k")
		if ok || val != nil {
			t.Fatalf("Get(k) after Stop = (%v, %v), want (nil, false)", val, ok)
		}
		multi := s.GetMulti([]string{"k"})
		if len(multi) != 0 {
			t.Fatalf("GetMulti(k) after Stop = %v, want empty map", multi)
		}
	})

	t.Run("MutatingMethodsReturnErrStoreStoppedAfterStop", func(t *testing.T) {
		s := mustNewStore(t)
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}

		if err := s.Set("k", []byte("v"), time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
			t.Fatalf("Set after Stop error = %v, want ErrStoreStopped", err)
		}
		if err := s.SetMulti(map[string][]byte{"k": []byte("v")}, time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
			t.Fatalf("SetMulti after Stop error = %v, want ErrStoreStopped", err)
		}
		if err := s.Delete("k"); !errors.Is(err, memorystore.ErrStoreStopped) {
			t.Fatalf("Delete after Stop error = %v, want ErrStoreStopped", err)
		}

		var loaderCalled atomic.Bool
		_, err := s.GetOrLoad(context.Background(), "k", time.Minute, func(_ context.Context) ([]byte, error) {
			loaderCalled.Store(true)
			return []byte("never"), nil
		})
		if !errors.Is(err, memorystore.ErrStoreStopped) {
			t.Fatalf("GetOrLoad after Stop error = %v, want ErrStoreStopped", err)
		}
		if loaderCalled.Load() {
			t.Fatal("GetOrLoad invoked loader on stopped store")
		}
	})
}

func TestTier1_F5_SetOptionsAndSliding(t *testing.T) {
	t.Run("SetWithOptionsBasic", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		if err := s.SetWithOptions("opt:basic", []byte("val"), 30*time.Millisecond); err != nil {
			t.Fatalf("SetWithOptions() error = %v", err)
		}
		got, ok := s.Get("opt:basic")
		if !ok || !bytes.Equal(got, []byte("val")) {
			t.Fatalf("Get(opt:basic) = (%q, %v), want (val, true)", got, ok)
		}
		eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
			_, ok := s.Get("opt:basic")
			return !ok
		}, "opt:basic should expire after 30ms")
	})

	t.Run("WithNoExpirationOverridesFiniteTTL", func(t *testing.T) {
		s := mustNewStore(t,
			memorystore.WithCleanupInterval(10*time.Millisecond),
			memorystore.WithPreciseClock(),
		)
		if err := s.SetWithOptions("opt:perm", []byte("pinned"), 15*time.Millisecond, memorystore.WithNoExpiration()); err != nil {
			t.Fatalf("SetWithOptions(WithNoExpiration) error = %v", err)
		}
		if err := s.Set("opt:short", []byte("bye"), 15*time.Millisecond); err != nil {
			t.Fatalf("Set(opt:short) error = %v", err)
		}

		eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
			_, ok := s.Get("opt:short")
			return !ok
		}, "opt:short should expire")

		got, ok := s.Get("opt:perm")
		if !ok || !bytes.Equal(got, []byte("pinned")) {
			t.Fatalf("Get(opt:perm) = (%q, %v), want (pinned, true)", got, ok)
		}
	})

	t.Run("WithSlidingExpirationExtendsTTLAcrossGets", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		const ttl = 120 * time.Millisecond
		if err := s.SetWithOptions("slide", []byte("alive"), ttl, memorystore.WithSlidingExpiration()); err != nil {
			t.Fatalf("SetWithOptions(WithSlidingExpiration) error = %v", err)
		}

		// Repeatedly access at ~45ms intervals (> 25% elapsed so coalesced refresh triggers),
		// spanning 135ms total (> initial 120ms TTL).
		for step := range 3 {
			time.Sleep(45 * time.Millisecond)
			got, ok := s.Get("slide")
			if !ok || !bytes.Equal(got, []byte("alive")) {
				t.Fatalf("step %d: Get(slide) = (%q, %v), want (alive, true)", step, got, ok)
			}
		}

		// Stop reading so the sliding TTL expires naturally without being refreshed by polling.
		time.Sleep(160 * time.Millisecond)
		if got, ok := s.Get("slide"); ok {
			t.Fatalf("Get(slide) after idle expiry = (%q, true), want miss", got)
		}
	})

	t.Run("SetWithOptionsWithTTLJitter", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		const ttl = 40 * time.Millisecond
		for i := range 5 {
			k := fmt.Sprintf("jitter-%d", i)
			if err := s.SetWithOptions(k, []byte("j"), ttl, memorystore.WithTTLJitter(0.2)); err != nil {
				t.Fatalf("SetWithOptions(%s, WithTTLJitter) error = %v", k, err)
			}
			if _, ok := s.Get(k); !ok {
				t.Fatalf("Get(%s) immediately after SetWithOptions failed", k)
			}
		}
		eventually(t, 500*time.Millisecond, 10*time.Millisecond, func() bool {
			for i := range 5 {
				if _, ok := s.Get(fmt.Sprintf("jitter-%d", i)); ok {
					return false
				}
			}
			return true
		}, "all jittered keys should expire within upper bound")
	})

	t.Run("WithSlidingExpirationKeyOverwrite", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		if err := s.Set("ow", []byte("old"), 20*time.Millisecond); err != nil {
			t.Fatalf("Set(old) error = %v", err)
		}
		if err := s.SetWithOptions("ow", []byte("new-sliding"), 120*time.Millisecond, memorystore.WithSlidingExpiration()); err != nil {
			t.Fatalf("SetWithOptions(overwrite) error = %v", err)
		}

		for range 3 {
			time.Sleep(45 * time.Millisecond)
			got, ok := s.Get("ow")
			if !ok || !bytes.Equal(got, []byte("new-sliding")) {
				t.Fatalf("Get(ow) = (%q, %v), want (new-sliding, true)", got, ok)
			}
		}
	})
}

func TestTier1_F6_Clocks(t *testing.T) {
	t.Run("DefaultCoarseClockExpiresNormalTTL", func(t *testing.T) {
		s := mustNewStore(t)
		if err := s.Set("coarse", []byte("val"), 50*time.Millisecond); err != nil {
			t.Fatalf("Set(coarse) error = %v", err)
		}
		if got, ok := s.Get("coarse"); !ok || !bytes.Equal(got, []byte("val")) {
			t.Fatalf("immediate Get(coarse) = (%q, %v), want (val, true)", got, ok)
		}
		eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
			_, ok := s.Get("coarse")
			return !ok
		}, "coarse-clock entry with 50ms TTL should expire")
	})

	t.Run("WithPreciseClockExpires15msTTL", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		if err := s.Set("precise", []byte("p"), 15*time.Millisecond); err != nil {
			t.Fatalf("Set(precise) error = %v", err)
		}
		if got, ok := s.Get("precise"); !ok || !bytes.Equal(got, []byte("p")) {
			t.Fatalf("immediate Get(precise) = (%q, %v), want (p, true)", got, ok)
		}
		eventually(t, 500*time.Millisecond, 2*time.Millisecond, func() bool {
			_, ok := s.Get("precise")
			return !ok
		}, "precise-clock entry with 15ms TTL should expire")
	})

	t.Run("Sub10msPreciseTTLFallbackOnDefaultClock", func(t *testing.T) {
		s := mustNewStore(t)
		if err := s.Set("sub10", []byte("fast"), 5*time.Millisecond); err != nil {
			t.Fatalf("Set(sub10) error = %v", err)
		}
		eventually(t, 150*time.Millisecond, 2*time.Millisecond, func() bool {
			_, ok := s.Get("sub10")
			return !ok
		}, "sub-10ms entry on default coarse clock should use preciseTTL fallback and expire promptly")
	})

	t.Run("WithPreciseClockSetMulti", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		err := s.SetMulti(map[string][]byte{
			"pm1": []byte("1"),
			"pm2": []byte("2"),
		}, 20*time.Millisecond)
		if err != nil {
			t.Fatalf("SetMulti() error = %v", err)
		}
		if got := s.GetMulti([]string{"pm1", "pm2"}); len(got) != 2 {
			t.Fatalf("immediate GetMulti len = %d, want 2", len(got))
		}
		eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
			return len(s.GetMulti([]string{"pm1", "pm2"})) == 0
		}, "SetMulti entries under WithPreciseClock should expire")
	})

	t.Run("WithPreciseClockAndSlidingExpiration", func(t *testing.T) {
		s := mustNewStore(t, memorystore.WithPreciseClock())
		if err := s.SetWithOptions("pslide", []byte("ok"), 80*time.Millisecond, memorystore.WithSlidingExpiration()); err != nil {
			t.Fatalf("SetWithOptions() error = %v", err)
		}

		for step := range 3 {
			time.Sleep(35 * time.Millisecond)
			got, ok := s.Get("pslide")
			if !ok || !bytes.Equal(got, []byte("ok")) {
				t.Fatalf("step %d: Get(pslide) = (%q, %v), want (ok, true)", step, got, ok)
			}
		}

		time.Sleep(110 * time.Millisecond)
		if got, ok := s.Get("pslide"); ok {
			t.Fatalf("Get(pslide) after idle sleep = (%q, true), want miss", got)
		}
	})
}
