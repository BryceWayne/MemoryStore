package e2e_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

// -----------------------------------------------------------------------------
// F1: Core KV & Multi-Key Boundaries
// -----------------------------------------------------------------------------

func TestTier2_F1_ZeroAndNegativeTTL_DeletesExistingWithoutDeadEntry(t *testing.T) {
	s := mustNewStore(t)

	// Setting a non-existing key with ttl=0 must not insert a dead entry.
	if err := s.Set("ghost-zero", []byte("v"), 0); err != nil {
		t.Fatalf("Set(ttl=0) error = %v", err)
	}
	if val, ok := s.Get("ghost-zero"); ok || val != nil {
		t.Fatalf("Get(ghost-zero) = (%v, %v), want (nil, false)", val, ok)
	}
	if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("metrics after Set(ttl=0) on missing = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}

	// Setting an existing key with ttl=0 must delete the existing entry.
	if err := s.Set("live", []byte("initial"), time.Minute); err != nil {
		t.Fatalf("Set(live) error = %v", err)
	}
	if err := s.Set("live", []byte("replaced"), 0); err != nil {
		t.Fatalf("Set(live, ttl=0) error = %v", err)
	}
	if val, ok := s.Get("live"); ok || val != nil {
		t.Fatalf("Get(live) after ttl=0 = (%v, %v), want (nil, false)", val, ok)
	}
	if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("metrics after Set(live, ttl=0) = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}

	// Negative TTL != NoExpiration (-1) must also expire immediately and delete existing.
	for _, negTTL := range []time.Duration{-2 * time.Nanosecond, -time.Millisecond, -5 * time.Second} {
		if err := s.Set("neg", []byte("before"), time.Minute); err != nil {
			t.Fatalf("Set(neg) setup error = %v", err)
		}
		if err := s.Set("neg", []byte("after"), negTTL); err != nil {
			t.Fatalf("Set(neg, %v) error = %v", negTTL, err)
		}
		if val, ok := s.Get("neg"); ok || val != nil {
			t.Fatalf("Get(neg) after ttl=%v = (%v, %v), want (nil, false)", negTTL, val, ok)
		}
		if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
			t.Fatalf("metrics after ttl=%v = (Items=%d, Bytes=%d), want (0, 0)", negTTL, m.Items, m.Bytes)
		}
	}
}

func TestTier2_F1_EmptyKeyAndEmptyOrNilValue(t *testing.T) {
	s := mustNewStore(t)

	if err := s.Set("", []byte("empty-key-val"), time.Minute); err != nil {
		t.Fatalf("Set empty key error = %v", err)
	}
	if val, ok := s.Get(""); !ok || string(val) != "empty-key-val" {
		t.Fatalf("Get(\"\") = (%q, %v), want (%q, true)", val, ok, "empty-key-val")
	}

	if err := s.Set("empty-slice", []byte{}, time.Minute); err != nil {
		t.Fatalf("Set empty slice error = %v", err)
	}
	if val, ok := s.Get("empty-slice"); !ok || len(val) != 0 {
		t.Fatalf("Get(empty-slice) = (%v, %v), want (len 0, true)", val, ok)
	}

	if err := s.Set("nil-slice", nil, time.Minute); err != nil {
		t.Fatalf("Set nil slice error = %v", err)
	}
	if val, ok := s.Get("nil-slice"); !ok || len(val) != 0 {
		t.Fatalf("Get(nil-slice) = (%v, %v), want (len 0, true)", val, ok)
	}

	wantBytes := int64(len("empty-key-val") + len("empty-slice") + len("nil-slice"))
	if m := s.GetMetrics(); m.Items != 3 || m.Bytes != wantBytes {
		t.Fatalf("metrics = (Items=%d, Bytes=%d), want (3, %d)", m.Items, m.Bytes, wantBytes)
	}

	for _, k := range []string{"", "empty-slice", "nil-slice"} {
		if err := s.Delete(k); err != nil {
			t.Fatalf("Delete(%q) error = %v", k, err)
		}
	}
	if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("metrics after deletes = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}
}

func TestTier2_F1_LargePayload1MiB(t *testing.T) {
	s := mustNewStore(t)

	const size = 1 << 20 // 1 MiB
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	payload[0] = 0x00
	payload[size/2] = '\n'
	payload[size-1] = 0xFF

	const key = "payload-1mib"
	if err := s.Set(key, payload, time.Minute); err != nil {
		t.Fatalf("Set(1MiB) error = %v", err)
	}
	got, ok := s.Get(key)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatalf("Get(1MiB) ok=%v equal=%v (len=%d)", ok, bytes.Equal(got, payload), len(got))
	}

	wantBytes := int64(len(key) + size)
	if m := s.GetMetrics(); m.Items != 1 || m.Bytes != wantBytes {
		t.Fatalf("metrics = (Items=%d, Bytes=%d), want (1, %d)", m.Items, m.Bytes, wantBytes)
	}

	if err := s.Delete(key); err != nil {
		t.Fatalf("Delete(1MiB) error = %v", err)
	}
	if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("metrics after Delete = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}
}

func TestTier2_F1_SetMulti_EmptyAndNilMap(t *testing.T) {
	s := mustNewStore(t)

	if err := s.Set("anchor", []byte("keep"), time.Minute); err != nil {
		t.Fatalf("Set(anchor) error = %v", err)
	}

	if err := s.SetMulti(nil, time.Minute); err != nil {
		t.Fatalf("SetMulti(nil) error = %v", err)
	}
	if err := s.SetMulti(map[string][]byte{}, time.Minute); err != nil {
		t.Fatalf("SetMulti(empty) error = %v", err)
	}
	if err := s.SetMulti(map[string][]byte{"anchor": []byte("gone"), "new": []byte("dead")}, 0); err != nil {
		t.Fatalf("SetMulti(ttl=0) error = %v", err)
	}

	if val, ok := s.Get("anchor"); ok || val != nil {
		t.Fatalf("Get(anchor) after SetMulti(ttl=0) = (%v, %v), want (nil, false)", val, ok)
	}
	if val, ok := s.Get("new"); ok || val != nil {
		t.Fatalf("Get(new) after SetMulti(ttl=0) = (%v, %v), want (nil, false)", val, ok)
	}
	if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("metrics = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}
}

func TestTier2_F1_GetMulti_MixedMissingExpiredPresent(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	if err := s.Set("present-1", []byte("one"), time.Minute); err != nil {
		t.Fatalf("Set(present-1) error = %v", err)
	}
	if err := s.Set("present-2", []byte("two"), time.Minute); err != nil {
		t.Fatalf("Set(present-2) error = %v", err)
	}
	if err := s.Set("expired-soon", []byte("old"), 5*time.Millisecond); err != nil {
		t.Fatalf("Set(expired-soon) error = %v", err)
	}

	eventually(t, 200*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := s.Get("expired-soon")
		return !ok
	}, "expired-soon did not expire")

	got := s.GetMulti([]string{"present-1", "missing", "expired-soon", "present-2", "present-1", ""})
	if got == nil {
		t.Fatal("GetMulti returned nil map, want non-nil")
	}
	if len(got) != 2 {
		t.Fatalf("len(GetMulti) = %d, want 2 (%v)", len(got), got)
	}
	if string(got["present-1"]) != "one" || string(got["present-2"]) != "two" {
		t.Fatalf("GetMulti values = %v, want present-1=one, present-2=two", got)
	}

	if empty := s.GetMulti(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("GetMulti(nil) = %#v, want non-nil empty map", empty)
	}
	if empty := s.GetMulti([]string{}); empty == nil || len(empty) != 0 {
		t.Fatalf("GetMulti([]) = %#v, want non-nil empty map", empty)
	}
}

func TestTier2_F1_SpecialCharsAndControlBytesRoundtrip(t *testing.T) {
	s := mustNewStore(t)

	cases := map[string][]byte{
		"nul\x00key\x00end":     {0x00, 0x01, 0x02, 0xff, '\n', '\r', '\t'},
		"unicode:键:🔑":           []byte("val-\u2603-\x00-\"quoted\"-\\slash"),
		"quotes'\"and\nnewline": []byte(`{"raw":"json\n\t\u0000"}`),
	}
	if err := s.SetMulti(cases, time.Minute); err != nil {
		t.Fatalf("SetMulti special chars error = %v", err)
	}

	keys := make([]string, 0, len(cases))
	for k, want := range cases {
		keys = append(keys, k)
		got, ok := s.Get(k)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("Get(%q) = (%v, %v), want (%v, true)", k, got, ok, want)
		}
	}

	multi := s.GetMulti(keys)
	if len(multi) != len(cases) {
		t.Fatalf("len(GetMulti) = %d, want %d", len(multi), len(cases))
	}
	for k, want := range cases {
		if !bytes.Equal(multi[k], want) {
			t.Fatalf("GetMulti[%q] = %v, want %v", k, multi[k], want)
		}
	}
}

// -----------------------------------------------------------------------------
// F2: Typed JSON & Loader / Singleflight Boundaries
// -----------------------------------------------------------------------------

func TestTier2_F2_GetOrLoad_LoaderErrorDoesNotCache(t *testing.T) {
	s := mustNewStore(t)

	sentinel := errors.New("upstream unavailable")
	var calls atomic.Int32

	val, err := s.GetOrLoad(t.Context(), "err-key", time.Minute, func(_ context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("partial"), sentinel
	})
	if !errors.Is(err, sentinel) || val != nil {
		t.Fatalf("GetOrLoad() on loader error = (%v, %v), want (nil, %v)", val, err, sentinel)
	}
	if _, ok := s.Get("err-key"); ok {
		t.Fatal("failed GetOrLoad must not populate cache")
	}

	// Next call must invoke loader again and cache on success.
	val, err = s.GetOrLoad(t.Context(), "err-key", time.Minute, func(_ context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("ok"), nil
	})
	if err != nil || string(val) != "ok" {
		t.Fatalf("second GetOrLoad() = (%q, %v), want (ok, nil)", val, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("loader calls = %d, want 2", calls.Load())
	}

	// Same contract for GetOrLoadJSON.
	var jsonCalls atomic.Int32
	if _, err := s.GetOrLoadJSON[int](t.Context(), "err-json", time.Minute, func(_ context.Context) (int, error) {
		jsonCalls.Add(1)
		return 99, sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("GetOrLoadJSON() error = %v, want %v", err, sentinel)
	}
	got, err := s.GetOrLoadJSON[int](t.Context(), "err-json", time.Minute, func(_ context.Context) (int, error) {
		jsonCalls.Add(1)
		return 42, nil
	})
	if err != nil || got != 42 || jsonCalls.Load() != 2 {
		t.Fatalf("recovery GetOrLoadJSON() = (%d, %v) calls=%d, want (42, nil) calls=2", got, err, jsonCalls.Load())
	}
}

func TestTier2_F2_GetOrLoad_PanicPropagatesAndCleansSingleflight(t *testing.T) {
	s := mustNewStore(t)

	const callers = 12
	started := make(chan struct{})
	release := make(chan struct{})
	var leaderCalls atomic.Int32

	panics := make([]any, callers)
	results := make([][]byte, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() {
			panics[0] = recover()
		}()
		results[0], errs[0] = s.GetOrLoad(t.Context(), "panic-bytes", time.Minute, func(_ context.Context) ([]byte, error) {
			leaderCalls.Add(1)
			close(started)
			<-release
			panic("loader-panic")
		})
	})

	<-started
	for i := 1; i < callers; i++ {
		idx := i
		wg.Go(func() {
			defer func() {
				panics[idx] = recover()
			}()
			results[idx], errs[idx] = s.GetOrLoad(t.Context(), "panic-bytes", time.Minute, func(_ context.Context) ([]byte, error) {
				leaderCalls.Add(1)
				return []byte("unexpected"), nil
			})
		})
	}

	time.Sleep(15 * time.Millisecond)
	close(release)
	wg.Wait()

	if leaderCalls.Load() != 1 {
		t.Fatalf("leaderCalls = %d, want 1", leaderCalls.Load())
	}
	for i := range callers {
		if panics[i] != "loader-panic" {
			t.Fatalf("caller %d panic = %v (val=%v, err=%v), want %q", i, panics[i], results[i], errs[i], "loader-panic")
		}
	}
	if _, ok := s.Get("panic-bytes"); ok {
		t.Fatal("panicked GetOrLoad must not populate cache")
	}

	val, err := s.GetOrLoad(t.Context(), "panic-bytes", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("healed"), nil
	})
	if err != nil || string(val) != "healed" {
		t.Fatalf("post-panic GetOrLoad() = (%q, %v), want (healed, nil)", val, err)
	}
}

func TestTier2_F2_GetOrLoadJSON_PanicPropagatesAndCleansSingleflight(t *testing.T) {
	type record struct {
		ID int `json:"id"`
	}

	s := mustNewStore(t)

	const callers = 10
	started := make(chan struct{})
	release := make(chan struct{})
	panics := make([]any, callers)
	results := make([]record, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() {
			panics[0] = recover()
		}()
		results[0], errs[0] = s.GetOrLoadJSON[record](t.Context(), "panic-json", time.Minute, func(_ context.Context) (record, error) {
			close(started)
			<-release
			panic("json-loader-panic")
		})
	})

	<-started
	for i := 1; i < callers; i++ {
		idx := i
		wg.Go(func() {
			defer func() {
				panics[idx] = recover()
			}()
			results[idx], errs[idx] = s.GetOrLoadJSON[record](t.Context(), "panic-json", time.Minute, func(_ context.Context) (record, error) {
				return record{ID: 999}, nil
			})
		})
	}

	time.Sleep(15 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range callers {
		if panics[i] != "json-loader-panic" {
			t.Fatalf("caller %d panic = %v (res=%+v, err=%v), want %q", i, panics[i], results[i], errs[i], "json-loader-panic")
		}
	}

	got, err := s.GetOrLoadJSON[record](t.Context(), "panic-json", time.Minute, func(_ context.Context) (record, error) {
		return record{ID: 7}, nil
	})
	if err != nil || got.ID != 7 {
		t.Fatalf("post-panic GetOrLoadJSON() = (%+v, %v), want ({ID:7}, nil)", got, err)
	}
}

func TestTier2_F2_GetOrLoad_CallerContextCancellation(t *testing.T) {
	s := mustNewStore(t)

	// 1. Pre-cancelled context on cache miss returns context.Canceled without running loader.
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	var ran atomic.Bool
	val, err := s.GetOrLoad(canceledCtx, "miss-canceled", time.Minute, func(_ context.Context) ([]byte, error) {
		ran.Store(true)
		return []byte("nope"), nil
	})
	if !errors.Is(err, context.Canceled) || val != nil {
		t.Fatalf("GetOrLoad(canceled ctx) = (%v, %v), want (nil, context.Canceled)", val, err)
	}
	if ran.Load() {
		t.Fatal("loader ran despite cancelled context on cache miss")
	}

	// 2. Coalesced waiter timing out does not cancel leader flight.
	started := make(chan struct{})
	release := make(chan struct{})
	var leaderErr error
	var leaderVal []byte
	var wg sync.WaitGroup

	wg.Go(func() {
		leaderVal, leaderErr = s.GetOrLoad(t.Context(), "slow-key", time.Minute, func(_ context.Context) ([]byte, error) {
			close(started)
			<-release
			return []byte("completed"), nil
		})
	})

	<-started
	waiterCtx, waiterCancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer waiterCancel()

	wVal, wErr := s.GetOrLoad(waiterCtx, "slow-key", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("should-not-run"), nil
	})
	if !errors.Is(wErr, context.DeadlineExceeded) || wVal != nil {
		t.Fatalf("waiter GetOrLoad() = (%v, %v), want (nil, context.DeadlineExceeded)", wVal, wErr)
	}

	close(release)
	wg.Wait()
	if leaderErr != nil || string(leaderVal) != "completed" {
		t.Fatalf("leader GetOrLoad() = (%q, %v), want (completed, nil)", leaderVal, leaderErr)
	}
}

func TestTier2_F2_GetJSONAs_TypeMismatchAndCorruptJSON(t *testing.T) {
	s := mustNewStore(t)

	// Missing key -> (zero, false, nil).
	gotMissing, ok, err := s.GetJSONAs[int]("does-not-exist")
	if ok || err != nil || gotMissing != 0 {
		t.Fatalf("GetJSONAs(missing) = (%d, %v, %v), want (0, false, nil)", gotMissing, ok, err)
	}

	// Valid JSON object decoded as incompatible primitive type -> (zero, true, err != nil).
	type user struct {
		Name string `json:"name"`
	}
	if err := s.SetJSON("user", user{Name: "alice"}, time.Minute); err != nil {
		t.Fatalf("SetJSON error = %v", err)
	}
	gotInt, ok, err := s.GetJSONAs[int]("user")
	if !ok || err == nil || gotInt != 0 {
		t.Fatalf("GetJSONAs[int](object) = (%d, %v, %v), want (0, true, non-nil err)", gotInt, ok, err)
	}

	// Corrupt raw JSON bytes -> (zero, true, err != nil).
	if err := s.Set("corrupt", []byte("{not-json"), time.Minute); err != nil {
		t.Fatalf("Set(corrupt) error = %v", err)
	}
	gotUser, ok, err := s.GetJSONAs[user]("corrupt")
	if !ok || err == nil || gotUser != (user{}) {
		t.Fatalf("GetJSONAs[user](corrupt) = (%+v, %v, %v), want ({}, true, non-nil err)", gotUser, ok, err)
	}
}

func TestTier2_F2_SetJSON_UnmarshalableTypeReturnsError(t *testing.T) {
	s := mustNewStore(t)

	if err := s.SetJSON("chan", make(chan int), time.Minute); err == nil {
		t.Fatal("SetJSON(chan) succeeded, want error")
	}
	if err := s.SetJSON("func", func() {}, time.Minute); err == nil {
		t.Fatal("SetJSON(func) succeeded, want error")
	}
	if err := s.SetJSONWithOptions("chan-opt", make(chan int), time.Minute, json.Deterministic(true)); err == nil {
		t.Fatal("SetJSONWithOptions(chan) succeeded, want error")
	}

	if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("metrics after failed SetJSON = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}
}

// -----------------------------------------------------------------------------
// F3: Slice Isolation Boundaries
// -----------------------------------------------------------------------------

func TestTier2_F3_DefaultZeroCopy_AliasingBehavior(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []memorystore.Option
	}{
		{name: "default"},
		{name: "explicit-false", opts: []memorystore.Option{memorystore.WithCopyOnWrite(false), memorystore.WithCopyOnRead(false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustNewStore(t, tc.opts...)
			src := []byte("hello")
			if err := s.Set("k", src, time.Minute); err != nil {
				t.Fatalf("Set error = %v", err)
			}
			src[0] = 'X'

			got, ok := s.Get("k")
			if !ok || got[0] != 'X' {
				t.Fatalf("zero-copy Get() = (%q, %v), expected caller mutation aliasing 'X'", got, ok)
			}

			got[1] = 'Y'
			got2, ok2 := s.Get("k")
			if !ok2 || got2[1] != 'Y' {
				t.Fatalf("zero-copy second Get() = (%q, %v), expected read mutation aliasing 'Y'", got2, ok2)
			}
		})
	}
}

func TestTier2_F3_CopyOnWrite_IsolatesCallerInputMutation(t *testing.T) {
	s := mustNewStore(t, memorystore.WithCopyOnWrite(true), memorystore.WithCopyOnRead(false))

	src := []byte("original-secret")
	if err := s.Set("k", src, time.Minute); err != nil {
		t.Fatalf("Set error = %v", err)
	}
	copy(src, "MUTATED-PAYLOAD")

	got, ok := s.Get("k")
	if !ok || string(got) != "original-secret" {
		t.Fatalf("Get() = (%q, %v), want %q", got, ok, "original-secret")
	}

	// Verify SetMulti also isolates caller slices under CopyOnWrite.
	b1 := []byte("batch-one")
	b2 := []byte("batch-two")
	if err := s.SetMulti(map[string][]byte{"b1": b1, "b2": b2}, time.Minute); err != nil {
		t.Fatalf("SetMulti error = %v", err)
	}
	b1[0] = '!'
	b2[0] = '!'

	if v, ok := s.Get("b1"); !ok || string(v) != "batch-one" {
		t.Fatalf("Get(b1) = (%q, %v), want batch-one", v, ok)
	}
	if v, ok := s.Get("b2"); !ok || string(v) != "batch-two" {
		t.Fatalf("Get(b2) = (%q, %v), want batch-two", v, ok)
	}
}

func TestTier2_F3_CopyOnRead_GetAndGetMultiIsolation(t *testing.T) {
	s := mustNewStore(t, memorystore.WithCopyOnRead(true))

	if err := s.Set("a", []byte("alpha"), time.Minute); err != nil {
		t.Fatalf("Set(a) error = %v", err)
	}
	if err := s.Set("b", []byte("bravo"), time.Minute); err != nil {
		t.Fatalf("Set(b) error = %v", err)
	}

	vA, ok := s.Get("a")
	if !ok || string(vA) != "alpha" {
		t.Fatalf("Get(a) = (%q, %v), want (alpha, true)", vA, ok)
	}
	copy(vA, "XXXXX")

	vA2, ok := s.Get("a")
	if !ok || string(vA2) != "alpha" {
		t.Fatalf("Get(a) after caller mutation = (%q, %v), want (alpha, true)", vA2, ok)
	}

	multi := s.GetMulti([]string{"a", "b"})
	if string(multi["a"]) != "alpha" || string(multi["b"]) != "bravo" {
		t.Fatalf("GetMulti() = %v, want alpha/bravo", multi)
	}
	copy(multi["a"], "ZZZZZ")
	copy(multi["b"], "YYYYY")

	multi2 := s.GetMulti([]string{"a", "b"})
	if string(multi2["a"]) != "alpha" || string(multi2["b"]) != "bravo" {
		t.Fatalf("GetMulti() after caller mutation = %v, want alpha/bravo", multi2)
	}
}

func TestTier2_F3_CopyOnWrite_EmptyAndNilSlices(t *testing.T) {
	s := mustNewStore(t, memorystore.WithCopyOnWrite(true), memorystore.WithCopyOnRead(true))

	if err := s.Set("nil-key", nil, time.Minute); err != nil {
		t.Fatalf("Set(nil) error = %v", err)
	}
	if err := s.Set("empty-key", []byte{}, time.Minute); err != nil {
		t.Fatalf("Set(empty) error = %v", err)
	}

	vNil, ok := s.Get("nil-key")
	if !ok || len(vNil) != 0 {
		t.Fatalf("Get(nil-key) = (%v, %v), want (len 0, true)", vNil, ok)
	}
	vNil = append(vNil, 'z')
	_ = vNil

	vEmpty, ok := s.Get("empty-key")
	if !ok || len(vEmpty) != 0 {
		t.Fatalf("Get(empty-key) = (%v, %v), want (len 0, true)", vEmpty, ok)
	}
	vEmpty = append(vEmpty, 'y')
	_ = vEmpty

	if again, ok := s.Get("nil-key"); !ok || len(again) != 0 {
		t.Fatalf("second Get(nil-key) = (%v, %v), want (len 0, true)", again, ok)
	}
	if again, ok := s.Get("empty-key"); !ok || len(again) != 0 {
		t.Fatalf("second Get(empty-key) = (%v, %v), want (len 0, true)", again, ok)
	}
}

func TestTier2_F3_CopyOnRead_ConcurrentMutatingReadersRaceFree(t *testing.T) {
	s := mustNewStore(t, memorystore.WithCopyOnWrite(true), memorystore.WithCopyOnRead(true))

	valA := bytes.Repeat([]byte{'A'}, 64)
	if err := s.Set("shared", valA, time.Minute); err != nil {
		t.Fatalf("Set error = %v", err)
	}

	var wg sync.WaitGroup
	stopWriters := make(chan struct{})

	for g := range 12 {
		wg.Go(func() {
			for i := range 200 {
				var got []byte
				if (g+i)%2 == 0 {
					var ok bool
					got, ok = s.Get("shared")
					if !ok {
						t.Errorf("Get(shared) missed unexpectedly")
						return
					}
				} else {
					m := s.GetMulti([]string{"shared"})
					got = m["shared"]
				}
				if len(got) != 64 {
					t.Errorf("len(got) = %d, want 64", len(got))
					return
				}
				first := got[0]
				if first != 'A' && first != 'B' {
					t.Errorf("corrupted byte %q observed", first)
					return
				}
				for _, b := range got {
					if b != first {
						t.Errorf("torn slice observed: first=%q saw=%q", first, b)
						return
					}
				}
				// Aggressively mutate returned slice to test isolation under -race.
				for idx := range got {
					got[idx] = 'X'
				}
			}
		})
	}

	var writerWG sync.WaitGroup
	writerWG.Go(func() {
		buf := make([]byte, 64)
		for i := 0; ; i++ {
			select {
			case <-stopWriters:
				return
			default:
			}
			ch := byte('A')
			if i%2 == 1 {
				ch = 'B'
			}
			for idx := range buf {
				buf[idx] = ch
			}
			_ = s.Set("shared", buf, time.Minute)
			// Immediately overwrite caller buffer to also stress CopyOnWrite.
			for idx := range buf {
				buf[idx] = 'W'
			}
		}
	})

	wg.Wait()
	close(stopWriters)
	writerWG.Wait()
}

// -----------------------------------------------------------------------------
// F4: Lifecycle, Config Validation & Stop() Safety
// -----------------------------------------------------------------------------

func TestTier2_F4_New_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		opts []memorystore.Option
	}{
		{
			name: "negative max items",
			opts: []memorystore.Option{memorystore.WithMaxItems(-1)},
		},
		{
			name: "negative max bytes",
			opts: []memorystore.Option{memorystore.WithMaxBytes(-1)},
		},
		{
			name: "negative cleanup interval",
			opts: []memorystore.Option{memorystore.WithCleanupInterval(-time.Second)},
		},
		{
			name: "S3FIFO without bounds",
			opts: []memorystore.Option{memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO)},
		},
		{
			name: "TinyLFU without bounds",
			opts: []memorystore.Option{memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU)},
		},
		{
			name: "invalidation topic without PubSub",
			opts: []memorystore.Option{memorystore.WithInvalidationTopic("orphan-topic")},
		},
		{
			name: "negative jitter",
			opts: []memorystore.Option{memorystore.WithDefaultTTLJitter(-0.01)},
		},
		{
			name: "jitter above 0.5",
			opts: []memorystore.Option{memorystore.WithDefaultTTLJitter(0.51)},
		},
		{
			name: "jitter >= 1.0",
			opts: []memorystore.Option{memorystore.WithDefaultTTLJitter(1.0)},
		},
		{
			name: "jitter NaN",
			opts: []memorystore.Option{memorystore.WithDefaultTTLJitter(math.NaN())},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := memorystore.New(tc.opts...)
			if err == nil {
				_ = s.Stop()
				t.Fatalf("New(%s) succeeded, want validation error", tc.name)
			}
			if s != nil {
				_ = s.Stop()
				t.Fatalf("New(%s) returned non-nil store alongside error %v", tc.name, err)
			}
		})
	}
}

func TestTier2_F4_Stop_IdempotentAndReadBehavior(t *testing.T) {
	s := mustNewStore(t)
	if err := s.Set("k", []byte(`{"id":1}`), time.Minute); err != nil {
		t.Fatalf("Set error = %v", err)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("first Stop() error = %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if !s.IsStopped() {
		t.Fatal("IsStopped() = false after Stop()")
	}

	before := s.GetMetrics()
	if before.Items != 0 || before.Bytes != 0 {
		t.Fatalf("post-Stop metrics = (Items=%d, Bytes=%d), want (0, 0)", before.Items, before.Bytes)
	}

	if v, ok := s.Get("k"); ok || v != nil {
		t.Fatalf("Get() after Stop = (%v, %v), want (nil, false)", v, ok)
	}
	if m := s.GetMulti([]string{"k"}); m == nil || len(m) != 0 {
		t.Fatalf("GetMulti() after Stop = %#v, want non-nil empty map", m)
	}
	var dst map[string]int
	if ok, err := s.GetJSON("k", &dst); ok || err != nil {
		t.Fatalf("GetJSON() after Stop = (%v, %v), want (false, nil)", ok, err)
	}
	if val, ok, err := s.GetJSONAs[map[string]int]("k"); ok || err != nil || val != nil {
		t.Fatalf("GetJSONAs() after Stop = (%v, %v, %v), want (nil, false, nil)", val, ok, err)
	}

	after := s.GetMetrics()
	if after.Misses != before.Misses {
		t.Fatalf("post-Stop reads modified Misses: before=%d after=%d", before.Misses, after.Misses)
	}
}

func TestTier2_F4_PostStop_ErrStoreStoppedOnAllMutatingLoadSnapshotPubSub(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	defer func() { _ = ps.Close() }()

	s := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("stop:inv"),
	)
	if err := s.Set("seed", []byte("val"), time.Minute); err != nil {
		t.Fatalf("Set(seed) error = %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	var loaderCalls atomic.Int32
	byteLoader := func(_ context.Context) ([]byte, error) {
		loaderCalls.Add(1)
		return []byte("x"), nil
	}
	intLoader := func(_ context.Context) (int, error) {
		loaderCalls.Add(1)
		return 1, nil
	}

	if err := s.Set("k", []byte("v"), time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("Set = %v, want ErrStoreStopped", err)
	}
	if err := s.SetWithOptions("k", []byte("v"), time.Minute, memorystore.WithNoExpiration()); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("SetWithOptions = %v, want ErrStoreStopped", err)
	}
	if err := s.SetMulti(map[string][]byte{"k": []byte("v")}, time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("SetMulti = %v, want ErrStoreStopped", err)
	}
	if err := s.SetJSON("k", 1, time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("SetJSON = %v, want ErrStoreStopped", err)
	}
	if err := s.SetJSONWithOptions("k", 1, time.Minute, json.Deterministic(true)); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("SetJSONWithOptions = %v, want ErrStoreStopped", err)
	}
	if err := s.Delete("seed"); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("Delete = %v, want ErrStoreStopped", err)
	}
	if _, err := s.GetOrLoad(t.Context(), "k", time.Minute, byteLoader); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("GetOrLoad = %v, want ErrStoreStopped", err)
	}
	if _, err := s.GetOrLoadWithOptions(t.Context(), "k", time.Minute, byteLoader, memorystore.WithStaleWhileRevalidate(time.Second)); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("GetOrLoadWithOptions = %v, want ErrStoreStopped", err)
	}
	if _, err := s.GetOrLoadJSON[int](t.Context(), "k", time.Minute, intLoader); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("GetOrLoadJSON = %v, want ErrStoreStopped", err)
	}
	if _, err := s.GetOrLoadJSONWithOptions[int](t.Context(), "k", time.Minute, intLoader, []memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(time.Second)}); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("GetOrLoadJSONWithOptions = %v, want ErrStoreStopped", err)
	}
	var snapBuf bytes.Buffer
	if err := s.Save(&snapBuf); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("Save = %v, want ErrStoreStopped", err)
	}
	if err := s.Load(bytes.NewReader(nil)); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("Load = %v, want ErrStoreStopped", err)
	}
	if _, err := s.Subscribe("t"); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("Subscribe = %v, want ErrStoreStopped", err)
	}
	if err := s.Publish("t", []byte("msg")); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("Publish = %v, want ErrStoreStopped", err)
	}
	if err := s.Unsubscribe("t"); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Errorf("Unsubscribe = %v, want ErrStoreStopped", err)
	}
	if loaderCalls.Load() != 0 {
		t.Errorf("loaderCalls after Stop = %d, want 0", loaderCalls.Load())
	}
}

func TestTier2_F4_ConcurrentStop_RacingReadsWritesDeletesSave(t *testing.T) {
	for round := range 8 {
		ps := memorystore.NewInMemoryPubSub()
		opts := []memorystore.Option{
			memorystore.WithPubSub(ps),
			memorystore.WithInvalidationTopic("race:inv"),
			memorystore.WithCleanupInterval(3 * time.Millisecond),
		}
		if round%2 == 0 {
			opts = append(opts, memorystore.WithMaxItems(32), memorystore.WithMaxBytes(4096))
		}
		s := mustNewStore(t, opts...)

		start := make(chan struct{})
		var wg sync.WaitGroup

		for g := range 12 {
			wg.Go(func() {
				<-start
				for i := range 50 {
					key := fmt.Sprintf("k-%d-%d", g, i%6)
					switch i % 7 {
					case 0:
						if err := s.Set(key, []byte("val"), 20*time.Millisecond); err != nil && !errors.Is(err, memorystore.ErrStoreStopped) {
							t.Errorf("unexpected Set error: %v", err)
						}
					case 1:
						if err := s.SetWithOptions(key, []byte("opt"), time.Minute, memorystore.WithSlidingExpiration()); err != nil && !errors.Is(err, memorystore.ErrStoreStopped) {
							t.Errorf("unexpected SetWithOptions error: %v", err)
						}
					case 2:
						if err := s.SetMulti(map[string][]byte{key: []byte("m1"), "m2": []byte("v2")}, time.Minute); err != nil && !errors.Is(err, memorystore.ErrStoreStopped) {
							t.Errorf("unexpected SetMulti error: %v", err)
						}
					case 3:
						_, _ = s.Get(key)
						_ = s.GetMulti([]string{key, "m2"})
					case 4:
						if err := s.Delete(key); err != nil && !errors.Is(err, memorystore.ErrStoreStopped) {
							t.Errorf("unexpected Delete error: %v", err)
						}
					case 5:
						_, err := s.GetOrLoad(context.Background(), key, time.Minute, func(_ context.Context) ([]byte, error) {
							return []byte("loaded"), nil
						})
						if err != nil && !errors.Is(err, memorystore.ErrStoreStopped) {
							t.Errorf("unexpected GetOrLoad error: %v", err)
						}
					case 6:
						var buf bytes.Buffer
						if err := s.Save(&buf); err != nil && !errors.Is(err, memorystore.ErrStoreStopped) {
							t.Errorf("unexpected Save error: %v", err)
						}
					}
					_ = s.GetMetrics()
					_ = s.IsStopped()
				}
			})
		}

		for range 2 {
			wg.Go(func() {
				<-start
				time.Sleep(time.Millisecond)
				if err := s.Stop(); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})
		}

		close(start)
		wg.Wait()
		_ = ps.Close()

		if !s.IsStopped() {
			t.Fatalf("round %d: IsStopped() = false after concurrent Stop", round)
		}
	}
}

func TestTier2_F4_StopDuringInFlightLoadAndOnEvictCallbackSafety(t *testing.T) {
	var s *memorystore.MemoryStore
	var evictCalled atomic.Bool
	s = mustNewStore(t,
		memorystore.WithMaxItems(2),
		memorystore.WithOnEvict(func(_ string, _ []byte, _ memorystore.EvictReason) {
			evictCalled.Store(true)
			// Querying store status inside OnEvict must never deadlock.
			_ = s.IsStopped()
			_ = s.GetMetrics()
		}),
	)

	if err := s.Set("e1", []byte("v1"), time.Minute); err != nil {
		t.Fatalf("Set(e1) error = %v", err)
	}
	if err := s.Set("e2", []byte("v2"), time.Minute); err != nil {
		t.Fatalf("Set(e2) error = %v", err)
	}
	if err := s.Set("e3", []byte("v3"), time.Minute); err != nil {
		t.Fatalf("Set(e3) error = %v", err)
	}
	if !evictCalled.Load() {
		t.Fatal("expected OnEvict callback on bounded eviction")
	}

	// Stop during an active GetOrLoad loader must return ErrStoreStopped instead of caching into a stopped store.
	_, err := s.GetOrLoad(t.Context(), "mid-stop", time.Minute, func(_ context.Context) ([]byte, error) {
		if stopErr := s.Stop(); stopErr != nil {
			return nil, stopErr
		}
		return []byte("late"), nil
	})
	if !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("GetOrLoad when Stop called inside loader = %v, want ErrStoreStopped", err)
	}
}

// -----------------------------------------------------------------------------
// F5: Per-Item Set Options & Sliding Expiration
// -----------------------------------------------------------------------------

func TestTier2_F5_SetWithOptions_InvalidTTLJitterReturnsError(t *testing.T) {
	s := mustNewStore(t)

	for _, bad := range []float64{-0.1, -0.001, 0.51, 0.6, 1.0, math.NaN()} {
		if err := s.SetWithOptions("bad-jitter", []byte("v"), time.Minute, memorystore.WithTTLJitter(bad)); err == nil {
			t.Fatalf("SetWithOptions(WithTTLJitter(%v)) succeeded, want error", bad)
		}
	}
	if _, ok := s.Get("bad-jitter"); ok {
		t.Fatal("failed SetWithOptions must not store key")
	}

	// Boundary valid values [0.0, 0.5] must succeed.
	if err := s.SetWithOptions("j0", []byte("v"), time.Minute, memorystore.WithTTLJitter(0.0)); err != nil {
		t.Fatalf("WithTTLJitter(0.0) error = %v", err)
	}
	if err := s.SetWithOptions("j05", []byte("v"), time.Minute, memorystore.WithTTLJitter(0.5)); err != nil {
		t.Fatalf("WithTTLJitter(0.5) error = %v", err)
	}
}

func TestTier2_F5_SlidingExpiration_CombinedWithNoExpiration(t *testing.T) {
	s := mustNewStore(t,
		memorystore.WithCleanupInterval(5*time.Millisecond),
		memorystore.WithPreciseClock(),
	)

	if err := s.SetWithOptions("perm-slide-opt", []byte("p1"), 10*time.Millisecond,
		memorystore.WithSlidingExpiration(),
		memorystore.WithNoExpiration(),
	); err != nil {
		t.Fatalf("SetWithOptions(sliding+noexp) error = %v", err)
	}
	if err := s.SetWithOptions("perm-slide-sentinel", []byte("p2"), memorystore.NoExpiration,
		memorystore.WithSlidingExpiration(),
	); err != nil {
		t.Fatalf("SetWithOptions(NoExpiration+sliding) error = %v", err)
	}

	time.Sleep(40 * time.Millisecond)

	if v, ok := s.Get("perm-slide-opt"); !ok || string(v) != "p1" {
		t.Fatalf("Get(perm-slide-opt) = (%q, %v), want (p1, true)", v, ok)
	}
	if v, ok := s.Get("perm-slide-sentinel"); !ok || string(v) != "p2" {
		t.Fatalf("Get(perm-slide-sentinel) = (%q, %v), want (p2, true)", v, ok)
	}
}

func TestTier2_F5_SlidingExpiration_ZeroOrNegativeTTLExpiresImmediately(t *testing.T) {
	s := mustNewStore(t)

	if err := s.Set("k", []byte("live"), time.Minute); err != nil {
		t.Fatalf("Set error = %v", err)
	}
	if err := s.SetWithOptions("k", []byte("dead"), 0, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions(ttl=0, sliding) error = %v", err)
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("Get(k) hit after SetWithOptions(ttl=0, sliding)")
	}

	if err := s.SetWithOptions("k2", []byte("dead"), -10*time.Millisecond, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions(ttl<0, sliding) error = %v", err)
	}
	if _, ok := s.Get("k2"); ok {
		t.Fatal("Get(k2) hit after SetWithOptions(ttl<0, sliding)")
	}
	if m := s.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("metrics = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}
}

func TestTier2_F5_SlidingExpiration_HotReadsUnderCoalescingThresholdStillRefresh(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	const ttl = 80 * time.Millisecond
	if err := s.SetWithOptions("hot-slide", []byte("alive"), ttl, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions error = %v", err)
	}

	// Read every 2ms for 130ms (> 1.5x ttl). Initial reads happen while remaining TTL >= 3/4*ttl
	// (coalesced), and subsequent reads once remaining TTL < 3/4*ttl must still refresh before expiry.
	deadline := time.Now().Add(130 * time.Millisecond)
	for time.Now().Before(deadline) {
		if v, ok := s.Get("hot-slide"); !ok || string(v) != "alive" {
			t.Fatalf("hot sliding key expired prematurely during continuous reads: ok=%v val=%q", ok, v)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Once reads stop for longer than ttl, the key must expire (do not poll Get inside eventually,
	// as each Get hit would refresh the sliding TTL).
	time.Sleep(ttl + 40*time.Millisecond)
	if _, ok := s.Get("hot-slide"); ok {
		t.Fatal("sliding key did not expire after reads stopped")
	}
}

func TestTier2_F5_SlidingExpiration_RefreshesOnGetAndGetMulti(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	const ttl = 80 * time.Millisecond
	if err := s.SetWithOptions("slide-get", []byte("g"), ttl, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions(slide-get) error = %v", err)
	}
	if err := s.SetWithOptions("slide-multi", []byte("m"), ttl, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions(slide-multi) error = %v", err)
	}
	if err := s.Set("non-slide", []byte("f"), ttl); err != nil {
		t.Fatalf("Set(non-slide) error = %v", err)
	}

	// Wait 45ms (> 1/4 of 80ms so remaining TTL < 3/4*ttl refresh coalescing threshold).
	time.Sleep(45 * time.Millisecond)

	if v, ok := s.Get("slide-get"); !ok || string(v) != "g" {
		t.Fatalf("Get(slide-get) at 45ms = (%q, %v), want (g, true)", v, ok)
	}
	if multi := s.GetMulti([]string{"slide-multi", "non-slide"}); string(multi["slide-multi"]) != "m" || string(multi["non-slide"]) != "f" {
		t.Fatalf("GetMulti at 45ms = %v, want slide-multi=m and non-slide=f", multi)
	}

	// Wait another 50ms (total 95ms > 80ms original TTL, but only 50ms < 80ms since refresh).
	time.Sleep(50 * time.Millisecond)

	if v, ok := s.Get("slide-get"); !ok || string(v) != "g" {
		t.Fatalf("Get(slide-get) at 95ms = (%q, %v), want refreshed hit", v, ok)
	}
	if v, ok := s.Get("slide-multi"); !ok || string(v) != "m" {
		t.Fatalf("Get(slide-multi) at 95ms = (%q, %v), want refreshed hit from GetMulti", v, ok)
	}
	if _, ok := s.Get("non-slide"); ok {
		t.Fatal("non-sliding key unexpectedly remained alive past TTL")
	}
}

// -----------------------------------------------------------------------------
// F6: Coarse & Precise Clocks
// -----------------------------------------------------------------------------

func TestTier2_F6_Sub10msTTLs_CoarseClock_PreciseFallback(t *testing.T) {
	// Default store uses 1ms coarse clock, with automatic per-entry preciseTTL fallback for ttl < 10ms.
	s := mustNewStore(t)

	for _, ttl := range []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 9 * time.Millisecond} {
		key := fmt.Sprintf("sub10-%s", ttl)
		if err := s.Set(key, []byte("short"), ttl); err != nil {
			t.Fatalf("Set(%s) error = %v", key, err)
		}
		// Must expire promptly on Get via precise fallback without waiting for a cleanup sweep.
		eventually(t, 40*time.Millisecond, time.Millisecond, func() bool {
			_, ok := s.Get(key)
			return !ok
		}, fmt.Sprintf("sub-10ms key %s did not expire promptly under coarse clock", key))
	}

	// Verify 9ms is readable before expiry when not heavily delayed.
	start := time.Now()
	if err := s.Set("readable-9ms", []byte("ok"), 9*time.Millisecond); err != nil {
		t.Fatalf("Set(9ms) error = %v", err)
	}
	val, ok := s.Get("readable-9ms")
	if time.Since(start) < 6*time.Millisecond && (!ok || string(val) != "ok") {
		t.Fatalf("immediate Get(9ms) = (%q, %v), want (ok, true)", val, ok)
	}
}

func TestTier2_F6_WithPreciseClock_1msBoundary(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	if err := s.Set("precise-1ms", []byte("fast"), time.Millisecond); err != nil {
		t.Fatalf("Set(1ms) error = %v", err)
	}
	if err := s.Set("precise-100ms", []byte("slow"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set(100ms) error = %v", err)
	}

	eventually(t, 30*time.Millisecond, time.Millisecond, func() bool {
		_, ok := s.Get("precise-1ms")
		return !ok
	}, "1ms entry did not expire promptly under WithPreciseClock")

	if v, ok := s.Get("precise-100ms"); !ok || string(v) != "slow" {
		t.Fatalf("100ms entry = (%q, %v), want (slow, true)", v, ok)
	}
}

func TestTier2_F6_10msBoundary_CoarseVsPreciseClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []memorystore.Option
	}{
		{name: "coarse-default"},
		{name: "precise-opt-out", opts: []memorystore.Option{memorystore.WithPreciseClock()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustNewStore(t, tc.opts...)

			if err := s.Set("k9", []byte("9"), 9*time.Millisecond); err != nil {
				t.Fatalf("Set(9ms) error = %v", err)
			}
			if err := s.Set("k10", []byte("10"), 10*time.Millisecond); err != nil {
				t.Fatalf("Set(10ms) error = %v", err)
			}

			// Both 9ms (precise fallback) and 10ms (coarse boundary) must expire promptly.
			eventually(t, 60*time.Millisecond, 2*time.Millisecond, func() bool {
				_, ok9 := s.Get("k9")
				_, ok10 := s.Get("k10")
				return !ok9 && !ok10
			}, "9ms/10ms boundary entries did not expire")
		})
	}
}

func TestTier2_F6_NoExpiration_SurvivesMultipleCleanupTicks(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []memorystore.Option
	}{
		{
			name: "coarse-clock",
			opts: []memorystore.Option{memorystore.WithCleanupInterval(5 * time.Millisecond)},
		},
		{
			name: "precise-clock",
			opts: []memorystore.Option{memorystore.WithCleanupInterval(5 * time.Millisecond), memorystore.WithPreciseClock()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingMetrics()
			s := mustNewStore(t, append(tc.opts, memorystore.WithMetricsRecorder(rec))...)

			if err := s.Set("perm", []byte("forever"), memorystore.NoExpiration); err != nil {
				t.Fatalf("Set(NoExpiration) error = %v", err)
			}
			if err := s.Set("temp", []byte("ephemeral"), 8*time.Millisecond); err != nil {
				t.Fatalf("Set(temp) error = %v", err)
			}

			eventually(t, 200*time.Millisecond, 5*time.Millisecond, func() bool {
				return s.GetMetrics().Items == 1 && rec.EvictsByReason(memorystore.EvictExpired) == 1
			}, "temporary item was not swept by cleanup ticks")

			// Wait across multiple additional cleanup ticks.
			time.Sleep(35 * time.Millisecond)

			if v, ok := s.Get("perm"); !ok || string(v) != "forever" {
				t.Fatalf("NoExpiration entry lost after cleanup ticks: (%q, %v)", v, ok)
			}
			if m := s.GetMetrics(); m.Items != 1 {
				t.Fatalf("Items = %d, want 1", m.Items)
			}
		})
	}
}

func TestTier2_F6_CoarseClock_RapidCreateStopLifecycle(t *testing.T) {
	for i := range 20 {
		s, err := memorystore.New()
		if err != nil {
			t.Fatalf("iter %d: New() error = %v", i, err)
		}
		if err := s.Set("k", []byte("v"), 50*time.Millisecond); err != nil {
			t.Fatalf("iter %d: Set error = %v", i, err)
		}
		_, _ = s.Get("k")
		if err := s.Stop(); err != nil {
			t.Fatalf("iter %d: Stop error = %v", i, err)
		}
	}
}
