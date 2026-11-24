package e2e_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

// -----------------------------------------------------------------------------
// F7: Observability, Metrics & Health
// -----------------------------------------------------------------------------

func TestTier2_F7_WithMetricsRecorderNil_Safe(t *testing.T) {
	s := mustNewStore(t,
		memorystore.WithMetricsRecorder(nil),
		memorystore.WithMaxItems(2),
		memorystore.WithPreciseClock(),
	)

	if err := s.Set("k1", []byte("v1"), time.Minute); err != nil {
		t.Fatalf("Set(k1) error = %v", err)
	}
	if _, ok := s.Get("k1"); !ok {
		t.Fatal("Get(k1) missed unexpectedly")
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get(missing) hit unexpectedly")
	}
	if _, err := s.GetOrLoad(t.Context(), "loaded", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("val"), nil
	}); err != nil {
		t.Fatalf("GetOrLoad error = %v", err)
	}
	// Trigger capacity eviction and expiration/delete paths with nil MetricsRecorder.
	if err := s.Set("k3", []byte("v3"), time.Minute); err != nil {
		t.Fatalf("Set(k3) error = %v", err)
	}
	if err := s.Set("k3", []byte("dead"), 0); err != nil {
		t.Fatalf("Set(k3, 0) error = %v", err)
	}
	if err := s.Delete("k1"); err != nil {
		t.Fatalf("Delete(k1) error = %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestTier2_F7_Health_ReturnsErrorAfterStop(t *testing.T) {
	s := mustNewStore(t)

	if err := s.Health(t.Context()); err != nil {
		t.Fatalf("Health() before Stop error = %v, want nil", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := s.Health(t.Context()); err == nil {
		t.Fatal("Health() after Stop returned nil, want error")
	} else if !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Logf("Health() after Stop returned non-nil error: %v", err)
	}
}

func TestTier2_F7_Health_PubSubPingerError(t *testing.T) {
	ps := newControllablePubSub()
	defer func() { _ = ps.Close() }()

	s := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("health:topic"),
	)

	if err := s.Health(t.Context()); err != nil {
		t.Fatalf("initial Health() error = %v, want nil", err)
	}

	pingErr := errors.New("broker ping timeout")
	ps.SetPingError(pingErr)
	if err := s.Health(t.Context()); err == nil {
		t.Fatal("Health() with failing Pinger returned nil, want error")
	} else if !errors.Is(err, pingErr) {
		t.Fatalf("Health() error = %v, want wrapped %v", err, pingErr)
	}

	// Recovery once Pinger succeeds again.
	ps.SetPingError(nil)
	if err := s.Health(t.Context()); err != nil {
		t.Fatalf("Health() after Pinger recovery error = %v, want nil", err)
	}
}

func TestTier2_F7_InMemoryPubSub_DroppedCountBoundary(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	defer func() { _ = ps.Close() }()

	ch, err := ps.Subscribe(t.Context(), "drop:boundary")
	if err != nil {
		t.Fatalf("Subscribe error = %v", err)
	}

	// Default subscriber channel capacity is 100: first 100 publishes must not drop.
	const bufCap = 100
	for i := range bufCap {
		if err := ps.Publish(t.Context(), "drop:boundary", fmt.Appendf(nil, "msg-%d", i)); err != nil {
			t.Fatalf("Publish(%d) error = %v", i, err)
		}
	}
	if got := ps.DroppedCount(); got != 0 {
		t.Fatalf("DroppedCount() at buffer capacity = %d, want 0", got)
	}

	// Next 7 messages while channel is full must increment DroppedCount by exactly 7.
	const extra = 7
	for i := range extra {
		if err := ps.Publish(t.Context(), "drop:boundary", fmt.Appendf(nil, "drop-%d", i)); err != nil {
			t.Fatalf("Publish(extra %d) error = %v", i, err)
		}
	}
	if got := ps.DroppedCount(); got != extra {
		t.Fatalf("DroppedCount() above capacity = %d, want %d", got, extra)
	}

	// Drain all 100 buffered messages and verify subsequent publish delivers cleanly without increasing drops.
	for i := range bufCap {
		select {
		case msg := <-ch:
			want := fmt.Sprintf("msg-%d", i)
			if string(msg) != want {
				t.Fatalf("drained msg[%d] = %q, want %q", i, msg, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out draining buffered message %d", i)
		}
	}

	if err := ps.Publish(t.Context(), "drop:boundary", []byte("after-drain")); err != nil {
		t.Fatalf("Publish(after-drain) error = %v", err)
	}
	select {
	case msg := <-ch:
		if string(msg) != "after-drain" {
			t.Fatalf("got %q, want after-drain", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for post-drain message")
	}
	if got := ps.DroppedCount(); got != extra {
		t.Fatalf("DroppedCount() after drain = %d, want %d", got, extra)
	}
}

func TestTier2_F7_GetMetrics_EmptyClearedStopped(t *testing.T) {
	s := mustNewStore(t)

	m := s.GetMetrics()
	if m.Items != 0 || m.Bytes != 0 || m.Hits != 0 || m.Misses != 0 || m.Evictions != 0 {
		t.Fatalf("empty store GetMetrics() = %+v, want all zeroes", m)
	}

	if _, ok := s.Get("absent"); ok {
		t.Fatal("Get(absent) hit unexpectedly")
	}
	m = s.GetMetrics()
	if m.Items != 0 || m.Bytes != 0 || m.Misses != 1 {
		t.Fatalf("after miss GetMetrics() = %+v, want Items=0 Bytes=0 Misses=1", m)
	}

	if err := s.Set("k", []byte("abcd"), time.Minute); err != nil {
		t.Fatalf("Set error = %v", err)
	}
	if _, ok := s.Get("k"); !ok {
		t.Fatal("Get(k) missed unexpectedly")
	}
	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete error = %v", err)
	}

	m = s.GetMetrics()
	if m.Items != 0 || m.Bytes != 0 || m.Hits != 1 || m.Evictions != 0 {
		t.Fatalf("after Delete GetMetrics() = %+v, want Items=0 Bytes=0 Hits=1 Evictions=0", m)
	}

	if err := s.Set("k-exp", []byte("x"), time.Minute); err != nil {
		t.Fatalf("Set(k-exp) error = %v", err)
	}
	if err := s.Set("k-exp", []byte("x"), 0); err != nil {
		t.Fatalf("Set(k-exp, 0) error = %v", err)
	}
	m = s.GetMetrics()
	if m.Evictions < 1 {
		t.Fatalf("after expired Set(0) GetMetrics() = %+v, want Evictions>=1", m)
	}

	if err := s.Set("k2", []byte("xyz"), time.Minute); err != nil {
		t.Fatalf("Set(k2) error = %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	m = s.GetMetrics()
	if m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("stopped store GetMetrics() = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
	}
}

// -----------------------------------------------------------------------------
// F8: Bounded Capacity & Pluggable Eviction
// -----------------------------------------------------------------------------

func TestTier2_F8_MaxItems1_Boundary(t *testing.T) {
	policies := []struct {
		name   string
		policy memorystore.EvictionPolicy
	}{
		{name: "S3FIFO", policy: memorystore.EvictionS3FIFO},
		{name: "TinyLFU", policy: memorystore.EvictionTinyLFU},
		{name: "VolatileTTL", policy: memorystore.EvictionVolatileTTL},
	}

	for _, tc := range policies {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingMetrics()
			s := mustNewStore(t,
				memorystore.WithMaxItems(1),
				memorystore.WithEvictionPolicy(tc.policy),
				memorystore.WithMetricsRecorder(rec),
			)

			if err := s.Set("k1", []byte("v1"), time.Minute); err != nil {
				t.Fatalf("Set(k1) error = %v", err)
			}
			if m := s.GetMetrics(); m.Items != 1 {
				t.Fatalf("after 1st key Items = %d, want 1", m.Items)
			}

			// Inserting 2nd key must evict 1st and enforce Items <= 1.
			if err := s.Set("k2", []byte("v2"), time.Minute); err != nil {
				t.Fatalf("Set(k2) error = %v", err)
			}
			if m := s.GetMetrics(); m.Items > 1 {
				t.Fatalf("after 2nd key Items = %d, want <= 1", m.Items)
			}
			if rec.EvictsByReason(memorystore.EvictCapacity) < 1 {
				t.Fatalf("EvictCapacity = %d, want >= 1", rec.EvictsByReason(memorystore.EvictCapacity))
			}

			// Overwriting existing key k2 in-place should keep Items <= 1 and k2 readable.
			if err := s.Set("k2", []byte("v2-new"), time.Minute); err != nil {
				t.Fatalf("overwrite Set(k2) error = %v", err)
			}
			if m := s.GetMetrics(); m.Items > 1 {
				t.Fatalf("after overwrite Items = %d, want <= 1", m.Items)
			}
		})
	}
}

func TestTier2_F8_MaxBytes_ExactBoundaryAndOversizedPayload(t *testing.T) {
	rec := newRecordingMetrics()
	// Exact MaxBytes = 20.
	s := mustNewStore(t,
		memorystore.WithMaxBytes(20),
		memorystore.WithMetricsRecorder(rec),
	)

	// Entry "a" (1) + "123456789" (9) = 10 bytes.
	if err := s.Set("a", []byte("123456789"), time.Minute); err != nil {
		t.Fatalf("Set(a) error = %v", err)
	}
	// Entry "b" (1) + "123456789" (9) = 10 bytes -> total 20 == MaxBytes.
	if err := s.Set("b", []byte("123456789"), time.Minute); err != nil {
		t.Fatalf("Set(b) error = %v", err)
	}
	if m := s.GetMetrics(); m.Items != 2 || m.Bytes != 20 {
		t.Fatalf("at exact MaxBytes boundary metrics = (Items=%d, Bytes=%d), want (2, 20)", m.Items, m.Bytes)
	}

	// Adding 6 more bytes ("c" + "12345") must evict to keep Bytes <= 20.
	if err := s.Set("c", []byte("12345"), time.Minute); err != nil {
		t.Fatalf("Set(c) error = %v", err)
	}
	if m := s.GetMetrics(); m.Bytes > 20 {
		t.Fatalf("Bytes after exceeding MaxBytes = %d, want <= 20", m.Bytes)
	}
	if rec.EvictsByReason(memorystore.EvictCapacity) < 1 {
		t.Fatalf("expected EvictCapacity >= 1, got %d", rec.EvictsByReason(memorystore.EvictCapacity))
	}

	// Single entry larger than MaxBytes must never leave store permanently above MaxBytes.
	if err := s.Set("huge", bytes.Repeat([]byte("Z"), 100), time.Minute); err != nil {
		// Returning an error or evicting are both acceptable, as long as store bounds are preserved.
		t.Logf("Set(oversized > MaxBytes) returned error: %v", err)
	}
	if m := s.GetMetrics(); m.Bytes > 20 {
		t.Fatalf("Bytes after oversized entry = %d, want <= 20", m.Bytes)
	}
}

func TestTier2_F8_ZeroTTL_NeverEvictsLiveEntriesAndEmitsEvictExpired(t *testing.T) {
	rec := newRecordingMetrics()
	s := mustNewStore(t,
		memorystore.WithMaxItems(2),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithMetricsRecorder(rec),
	)

	if err := s.Set("live-1", []byte("one"), time.Minute); err != nil {
		t.Fatalf("Set(live-1) error = %v", err)
	}
	if err := s.Set("live-2", []byte("two"), time.Minute); err != nil {
		t.Fatalf("Set(live-2) error = %v", err)
	}

	// Setting a non-existing key with ttl=0 in a full bounded store must NOT evict live entries.
	if err := s.Set("nonexistent", []byte("dead"), 0); err != nil {
		t.Fatalf("Set(nonexistent, 0) error = %v", err)
	}
	if v, ok := s.Get("live-1"); !ok || string(v) != "one" {
		t.Fatalf("live-1 was evicted by ttl=0 Set on missing key: (%q, %v)", v, ok)
	}
	if v, ok := s.Get("live-2"); !ok || string(v) != "two" {
		t.Fatalf("live-2 was evicted by ttl=0 Set on missing key: (%q, %v)", v, ok)
	}
	if rec.EvictsByReason(memorystore.EvictCapacity) != 0 {
		t.Fatalf("EvictCapacity = %d, want 0", rec.EvictsByReason(memorystore.EvictCapacity))
	}
	if rec.EvictsByReason(memorystore.EvictExpired) != 0 {
		t.Fatalf("EvictExpired on missing key = %d, want 0", rec.EvictsByReason(memorystore.EvictExpired))
	}

	// Setting an existing key with ttl=0 must delete it and emit EvictExpired without evicting live-2.
	if err := s.Set("live-1", []byte("expire-now"), 0); err != nil {
		t.Fatalf("Set(live-1, 0) error = %v", err)
	}
	if _, ok := s.Get("live-1"); ok {
		t.Fatal("live-1 remained present after ttl=0 Set")
	}
	if v, ok := s.Get("live-2"); !ok || string(v) != "two" {
		t.Fatalf("live-2 lost after ttl=0 on live-1: (%q, %v)", v, ok)
	}
	if rec.EvictsByReason(memorystore.EvictExpired) != 1 {
		t.Fatalf("EvictExpired = %d, want 1", rec.EvictsByReason(memorystore.EvictExpired))
	}
	if rec.EvictsByReason(memorystore.EvictCapacity) != 0 {
		t.Fatalf("EvictCapacity = %d, want 0", rec.EvictsByReason(memorystore.EvictCapacity))
	}
}

func TestTier2_F8_NoExpiration_EvictedForCapacityInS3FIFOAndTinyLFU(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy memorystore.EvictionPolicy
	}{
		{name: "S3FIFO", policy: memorystore.EvictionS3FIFO},
		{name: "TinyLFU", policy: memorystore.EvictionTinyLFU},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingMetrics()
			s := mustNewStore(t,
				memorystore.WithMaxItems(4),
				memorystore.WithEvictionPolicy(tc.policy),
				memorystore.WithMetricsRecorder(rec),
			)

			const total = 12
			for i := range total {
				k := fmt.Sprintf("perm-%02d", i)
				if err := s.Set(k, []byte("val"), memorystore.NoExpiration); err != nil {
					t.Fatalf("Set(%s) error = %v", k, err)
				}
			}

			if m := s.GetMetrics(); m.Items > 4 {
				t.Fatalf("Items = %d, want <= 4", m.Items)
			}
			if got := rec.EvictsByReason(memorystore.EvictCapacity); got < int64(total-4) {
				t.Fatalf("EvictCapacity = %d, want >= %d", got, total-4)
			}
			if got := rec.EvictsByReason(memorystore.EvictExpired); got != 0 {
				t.Fatalf("EvictExpired = %d, want 0 for NoExpiration keys", got)
			}
		})
	}
}

func TestTier2_F8_ZipfianPlusScanBurst_HitRatioExceedsVolatileTTL(t *testing.T) {
	const (
		capacity = 128
		nKeys    = 1500
		nZipf    = 12000
		nScan    = 3000
	)

	// Build workload: Zipf(s=0.99) -> one-hit-wonder scan burst -> Zipf(s=0.99).
	zipfPart1 := zipfKeys(777, 0.99, nKeys, nZipf/2)
	zipfPart2 := zipfKeys(778, 0.99, nKeys, nZipf/2)
	workload := make([]string, 0, nZipf+nScan)
	workload = append(workload, zipfPart1...)
	for i := range nScan {
		workload = append(workload, fmt.Sprintf("scan-burst-%d", i))
	}
	workload = append(workload, zipfPart2...)

	runWorkload := func(policy memorystore.EvictionPolicy) float64 {
		s := mustNewStore(t,
			memorystore.WithMaxItems(capacity),
			memorystore.WithEvictionPolicy(policy),
		)
		var hits, total int
		payload := []byte("v")
		for _, k := range workload {
			total++
			if _, ok := s.Get(k); ok {
				hits++
			} else if err := s.Set(k, payload, 5*time.Minute); err != nil {
				t.Fatalf("Set(%s) failed: %v", k, err)
			}
		}
		return float64(hits) / float64(total)
	}

	volatileRatio := runWorkload(memorystore.EvictionVolatileTTL)
	s3Ratio := runWorkload(memorystore.EvictionS3FIFO)
	tinyLFURatio := runWorkload(memorystore.EvictionTinyLFU)

	t.Logf("Hit ratios (cap=%d, Zipf s=0.99 + scan burst): VolatileTTL=%.4f S3FIFO=%.4f TinyLFU=%.4f",
		capacity, volatileRatio, s3Ratio, tinyLFURatio)

	// Verify meaningful relative improvement (>= +15% relative hit ratio over VolatileTTL).
	if s3Ratio <= volatileRatio*1.15 {
		t.Fatalf("S3FIFO hit ratio (%.4f) did not exceed VolatileTTL (%.4f) by >15%%", s3Ratio, volatileRatio)
	}
	if tinyLFURatio <= volatileRatio*1.15 {
		t.Fatalf("TinyLFU hit ratio (%.4f) did not exceed VolatileTTL (%.4f) by >15%%", tinyLFURatio, volatileRatio)
	}
}

// -----------------------------------------------------------------------------
// F9: Stale-While-Revalidate (SWR)
// -----------------------------------------------------------------------------

func TestTier2_F9_SWR_GraceZero_BehavesAsNormalMiss(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	var calls atomic.Int32
	v1, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-zero-grace",
		20*time.Millisecond,
		func(_ context.Context) ([]byte, error) {
			n := calls.Add(1)
			return fmt.Appendf(nil, "v%d", n), nil
		},
		memorystore.WithStaleWhileRevalidate(0),
	)
	if err != nil || string(v1) != "v1" {
		t.Fatalf("initial load = (%q, %v), want (v1, nil)", v1, err)
	}

	eventually(t, 200*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := s.Get("swr-zero-grace")
		return !ok
	}, "key did not expire")

	// With grace == 0, expired entry must miss and run loader synchronously to return v2.
	v2, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-zero-grace",
		time.Minute,
		func(_ context.Context) ([]byte, error) {
			n := calls.Add(1)
			return fmt.Appendf(nil, "v%d", n), nil
		},
		memorystore.WithStaleWhileRevalidate(0),
	)
	if err != nil || string(v2) != "v2" {
		t.Fatalf("post-expiry load with grace=0 = (%q, %v), want (v2, nil)", v2, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("loader calls = %d, want 2", calls.Load())
	}
}

func TestTier2_F9_SWR_PastHardExpiresAt_BlocksAndRunsSynchronousLoader(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	const soft = 20 * time.Millisecond
	const grace = 25 * time.Millisecond

	if _, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-hard",
		soft,
		func(_ context.Context) ([]byte, error) {
			return []byte("v1"), nil
		},
		memorystore.WithStaleWhileRevalidate(grace),
	); err != nil {
		t.Fatalf("initial load error = %v", err)
	}

	// Sleep well past hardExpiresAt (soft + grace = 45ms).
	time.Sleep(70 * time.Millisecond)
	if _, ok := s.Get("swr-hard"); ok {
		t.Fatal("Get() hit past hardExpiresAt")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan []byte, 1)
	errCh := make(chan error, 1)

	go func() {
		v, err := s.GetOrLoadWithOptions(
			t.Context(),
			"swr-hard",
			time.Minute,
			func(_ context.Context) ([]byte, error) {
				close(started)
				<-release
				return []byte("v2-sync"), nil
			},
			memorystore.WithStaleWhileRevalidate(grace),
		)
		done <- v
		errCh <- err
	}()

	<-started
	select {
	case v := <-done:
		t.Fatalf("GetOrLoadWithOptions returned early (%q) instead of blocking past hardExpiresAt", v)
	case <-time.After(15 * time.Millisecond):
	}

	close(release)
	got := <-done
	if err := <-errCh; err != nil || string(got) != "v2-sync" {
		t.Fatalf("post-hardExpiresAt GetOrLoadWithOptions = (%q, %v), want (v2-sync, nil)", got, err)
	}
}

func TestTier2_F9_SWR_ConcurrentStaleRequests_CoalesceBackgroundRefreshToOne(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	const soft = 20 * time.Millisecond
	const grace = 600 * time.Millisecond

	if _, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-coalesce",
		soft,
		func(_ context.Context) ([]byte, error) {
			return []byte("stale-v1"), nil
		},
		memorystore.WithStaleWhileRevalidate(grace),
	); err != nil {
		t.Fatalf("initial load error = %v", err)
	}

	eventually(t, 200*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := s.Get("swr-coalesce")
		return !ok
	}, "key did not enter stale window")

	var refreshCalls atomic.Int32
	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})

	const callers = 16
	vals := make([][]byte, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := range callers {
		idx := i
		wg.Go(func() {
			vals[idx], errs[idx] = s.GetOrLoadWithOptions(
				t.Context(),
				"swr-coalesce",
				time.Minute,
				func(_ context.Context) ([]byte, error) {
					refreshCalls.Add(1)
					startedOnce.Do(func() { close(started) })
					<-release
					return []byte("fresh-v2"), nil
				},
				memorystore.WithStaleWhileRevalidate(grace),
			)
		})
	}

	// All callers must return stale value immediately without waiting for release!
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || string(vals[i]) != "stale-v1" {
			t.Fatalf("caller %d = (%q, %v), want (stale-v1, nil)", i, vals[i], errs[i])
		}
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}

	// Give any stray goroutines a brief moment before releasing; count must remain 1.
	time.Sleep(15 * time.Millisecond)
	if got := refreshCalls.Load(); got != 1 {
		close(release)
		t.Fatalf("background refreshCalls = %d, want 1", got)
	}

	close(release)
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s.Get("swr-coalesce")
		return ok && string(v) == "fresh-v2"
	}, "cache was not updated to fresh-v2 after background refresh")
}

func TestTier2_F9_SWR_CallerContextCancellation_DoesNotAbortBackgroundRefresh(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	const soft = 20 * time.Millisecond
	const grace = 600 * time.Millisecond

	if _, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-detach",
		soft,
		func(_ context.Context) ([]byte, error) {
			return []byte("stale"), nil
		},
		memorystore.WithStaleWhileRevalidate(grace),
	); err != nil {
		t.Fatalf("initial load error = %v", err)
	}

	eventually(t, 200*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := s.Get("swr-detach")
		return !ok
	}, "key did not reach soft expiration")

	callerCtx, cancel := context.WithCancel(t.Context())
	bgStarted := make(chan struct{})
	proceed := make(chan struct{})
	bgCtxErr := make(chan error, 1)

	val, err := s.GetOrLoadWithOptions(
		callerCtx,
		"swr-detach",
		time.Minute,
		func(ctx context.Context) ([]byte, error) {
			close(bgStarted)
			<-proceed
			bgCtxErr <- ctx.Err()
			return []byte("healed"), nil
		},
		memorystore.WithStaleWhileRevalidate(grace),
	)
	if err != nil || string(val) != "stale" {
		t.Fatalf("stale hit = (%q, %v), want (stale, nil)", val, err)
	}

	<-bgStarted
	cancel()
	// Give cancellation a moment to propagate if mistakenly wired to callerCtx.
	time.Sleep(15 * time.Millisecond)
	close(proceed)

	if gotErr := <-bgCtxErr; gotErr != nil {
		t.Fatalf("background refresh ctx.Err() = %v after caller cancel, want nil (detached)", gotErr)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s.Get("swr-detach")
		return ok && string(v) == "healed"
	}, "detached background refresh did not update cache")
}

func TestTier2_F9_SWR_Stop_WaitsForActiveBackgroundRefresh(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock())

	const soft = 20 * time.Millisecond
	const grace = 600 * time.Millisecond

	if _, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-stop-drain",
		soft,
		func(_ context.Context) ([]byte, error) {
			return []byte("v1"), nil
		},
		memorystore.WithStaleWhileRevalidate(grace),
	); err != nil {
		t.Fatalf("initial load error = %v", err)
	}

	eventually(t, 200*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := s.Get("swr-stop-drain")
		return !ok
	}, "key did not enter stale window")

	bgStarted := make(chan struct{})
	releaseBG := make(chan struct{})
	var bgExited atomic.Bool

	val, err := s.GetOrLoadWithOptions(
		t.Context(),
		"swr-stop-drain",
		time.Minute,
		func(_ context.Context) ([]byte, error) {
			close(bgStarted)
			<-releaseBG
			bgExited.Store(true)
			return []byte("v2"), nil
		},
		memorystore.WithStaleWhileRevalidate(grace),
	)
	if err != nil || string(val) != "v1" {
		t.Fatalf("stale hit = (%q, %v), want (v1, nil)", val, err)
	}

	<-bgStarted
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- s.Stop()
	}()

	// Stop() must block until the active SWR background goroutine finishes.
	select {
	case err := <-stopDone:
		close(releaseBG)
		t.Fatalf("Stop() returned early (%v) while SWR background refresh was still active", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseBG)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if !bgExited.Load() {
		t.Fatal("Stop() returned before SWR background refresh exited")
	}
}

func TestTier2_F9_SWR_GetOrLoadJSONWithOptions_FailedRefreshPreservesStale(t *testing.T) {
	type item struct {
		Ver int `json:"ver"`
	}

	s := mustNewStore(t, memorystore.WithPreciseClock())
	const soft = 20 * time.Millisecond
	const grace = 600 * time.Millisecond

	got1, err := s.GetOrLoadJSONWithOptions[item](
		t.Context(),
		"swr-json",
		soft,
		func(_ context.Context) (item, error) {
			return item{Ver: 1}, nil
		},
		[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(grace)},
	)
	if err != nil || got1.Ver != 1 {
		t.Fatalf("initial GetOrLoadJSONWithOptions = (%+v, %v), want {Ver:1}", got1, err)
	}

	eventually(t, 200*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := s.Get("swr-json")
		return !ok
	}, "json key did not enter stale window")

	var failRan atomic.Bool
	got2, err := s.GetOrLoadJSONWithOptions[item](
		t.Context(),
		"swr-json",
		time.Minute,
		func(_ context.Context) (item, error) {
			failRan.Store(true)
			return item{}, errors.New("backend error")
		},
		[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(grace)},
	)
	if err != nil || got2.Ver != 1 {
		t.Fatalf("stale GetOrLoadJSONWithOptions = (%+v, %v), want {Ver:1}", got2, err)
	}
	eventually(t, time.Second, 5*time.Millisecond, failRan.Load, "failing JSON refresh did not execute")

	// Stale value must remain intact for a subsequent call that succeeds.
	got3, err := s.GetOrLoadJSONWithOptions[item](
		t.Context(),
		"swr-json",
		time.Minute,
		func(_ context.Context) (item, error) {
			return item{Ver: 2}, nil
		},
		[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(grace)},
	)
	if err != nil || got3.Ver != 1 {
		t.Fatalf("second stale GetOrLoadJSONWithOptions = (%+v, %v), want preserved {Ver:1}", got3, err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok, err := s.GetJSONAs[item]("swr-json")
		return err == nil && ok && v.Ver == 2
	}, "subsequent JSON refresh did not update cache to Ver:2")
}

// -----------------------------------------------------------------------------
// F10: TTL Jitter Boundaries
// -----------------------------------------------------------------------------

func TestTier2_F10_JitterZero_ExactTTL(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithDefaultTTLJitter(0.0))

	const ttl = 60 * time.Millisecond
	start := time.Now()
	if err := s.Set("j0-def", []byte("v"), ttl); err != nil {
		t.Fatalf("Set error = %v", err)
	}
	if err := s.SetWithOptions("j0-item", []byte("v"), ttl, memorystore.WithTTLJitter(0.0)); err != nil {
		t.Fatalf("SetWithOptions error = %v", err)
	}

	// Well before ttl, both keys must be alive.
	time.Sleep(25 * time.Millisecond)
	if time.Since(start) < ttl-5*time.Millisecond {
		if _, ok := s.Get("j0-def"); !ok {
			t.Fatal("j0-def expired prematurely with jitter=0.0")
		}
		if _, ok := s.Get("j0-item"); !ok {
			t.Fatal("j0-item expired prematurely with jitter=0.0")
		}
	}

	eventually(t, 200*time.Millisecond, 5*time.Millisecond, func() bool {
		_, ok1 := s.Get("j0-def")
		_, ok2 := s.Get("j0-item")
		return !ok1 && !ok2
	}, "jitter=0.0 keys did not expire promptly after TTL")
}

func TestTier2_F10_JitterUpperBoundary_HalfTTL(t *testing.T) {
	s := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithDefaultTTLJitter(0.5))

	const baseTTL = 200 * time.Millisecond
	const count = 40
	keys := make([]string, count)
	start := time.Now()
	for i := range count {
		k := fmt.Sprintf("j05-%02d", i)
		keys[i] = k
		if i%2 == 0 {
			if err := s.Set(k, []byte("v"), baseTTL); err != nil {
				t.Fatalf("Set(%s) error = %v", k, err)
			}
		} else {
			if err := s.SetWithOptions(k, []byte("v"), baseTTL, memorystore.WithTTLJitter(0.5)); err != nil {
				t.Fatalf("SetWithOptions(%s) error = %v", k, err)
			}
		}
	}

	// Lower bound of [0.5*200ms, 1.5*200ms] is 100ms; none should expire before ~90ms.
	expirations := make(map[string]time.Duration, count)
	deadline := time.Now().Add(time.Second)
	for len(expirations) < count && time.Now().Before(deadline) {
		for _, k := range keys {
			if _, seen := expirations[k]; seen {
				continue
			}
			if _, ok := s.Get(k); !ok {
				expirations[k] = time.Since(start)
			}
		}
		if len(expirations) < count {
			time.Sleep(5 * time.Millisecond)
		}
	}

	if len(expirations) != count {
		t.Fatalf("only %d/%d keys expired within 1s", len(expirations), count)
	}

	minExp := time.Hour
	var maxExp time.Duration
	for _, d := range expirations {
		if d < minExp {
			minExp = d
		}
		if d > maxExp {
			maxExp = d
		}
	}

	if minExp < 85*time.Millisecond {
		t.Fatalf("minExp = %v, below expected 0.5*TTL lower bound (100ms)", minExp)
	}
	if maxExp > 360*time.Millisecond {
		t.Fatalf("maxExp = %v, exceeded expected 1.5*TTL upper bound (300ms + tolerance)", maxExp)
	}
	if maxExp-minExp < 50*time.Millisecond {
		t.Fatalf("spread = %v (%v..%v), expected wide spread at jitter=0.5", maxExp-minExp, minExp, maxExp)
	}
}

func TestTier2_F10_NegativeJitter_Rejected(t *testing.T) {
	for _, f := range []float64{-0.0001, -0.1, -1.0} {
		if _, err := memorystore.New(memorystore.WithDefaultTTLJitter(f)); err == nil {
			t.Fatalf("New(WithDefaultTTLJitter(%v)) succeeded, want error", f)
		}
	}

	s := mustNewStore(t)
	for _, f := range []float64{-0.0001, -0.1, -1.0} {
		if err := s.SetWithOptions("k", []byte("v"), time.Minute, memorystore.WithTTLJitter(f)); err == nil {
			t.Fatalf("SetWithOptions(WithTTLJitter(%v)) succeeded, want error", f)
		}
	}
}

func TestTier2_F10_OverHalfAndOverOneJitter_Rejected(t *testing.T) {
	for _, f := range []float64{0.5001, 0.75, 1.0, 2.0} {
		if _, err := memorystore.New(memorystore.WithDefaultTTLJitter(f)); err == nil {
			t.Fatalf("New(WithDefaultTTLJitter(%v)) succeeded, want error", f)
		}
	}

	s := mustNewStore(t)
	for _, f := range []float64{0.5001, 0.75, 1.0, 2.0} {
		if err := s.SetWithOptions("k", []byte("v"), time.Minute, memorystore.WithTTLJitter(f)); err == nil {
			t.Fatalf("SetWithOptions(WithTTLJitter(%v)) succeeded, want error", f)
		}
	}
}

func TestTier2_F10_JitterWithNoExpiration_RemainsNonExpiring(t *testing.T) {
	s := mustNewStore(t,
		memorystore.WithDefaultTTLJitter(0.5),
		memorystore.WithCleanupInterval(5*time.Millisecond),
		memorystore.WithPreciseClock(),
	)

	if err := s.Set("perm-1", []byte("a"), memorystore.NoExpiration); err != nil {
		t.Fatalf("Set(NoExpiration) error = %v", err)
	}
	if err := s.SetWithOptions("perm-2", []byte("b"), 10*time.Millisecond,
		memorystore.WithTTLJitter(0.5),
		memorystore.WithNoExpiration(),
	); err != nil {
		t.Fatalf("SetWithOptions(NoExpiration+jitter) error = %v", err)
	}

	time.Sleep(40 * time.Millisecond)

	if v, ok := s.Get("perm-1"); !ok || string(v) != "a" {
		t.Fatalf("perm-1 lost with jitter+NoExpiration: (%q, %v)", v, ok)
	}
	if v, ok := s.Get("perm-2"); !ok || string(v) != "b" {
		t.Fatalf("perm-2 lost with jitter+WithNoExpiration: (%q, %v)", v, ok)
	}
}

// -----------------------------------------------------------------------------
// F11: Distributed Invalidation, Write-Through & Resilient Backplane
// -----------------------------------------------------------------------------

func TestTier2_F11_WriteThroughMaxBytes_BoundaryAndFallbackToInvalidateOnly(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	defer func() { _ = ps.Close() }()

	const maxWT = 128
	s1 := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("wt:boundary"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithWriteThroughMaxBytes(maxWT),
		memorystore.WithSyncInvalidation(),
	)
	s2 := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("wt:boundary"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithWriteThroughMaxBytes(maxWT),
		memorystore.WithSyncInvalidation(),
	)

	// Pre-seed "big" on s2 with stale value so fallback InvalidateOnly evicts it to a miss.
	// Wait for s1 to consume s2's WriteThrough message so it cannot race with s1.Set("big", bigVal).
	if err := s2.Set("big", []byte("stale-old-value"), time.Minute); err != nil {
		t.Fatalf("s2.Set(big) error = %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s1.Get("big")
		return ok && string(v) == "stale-old-value"
	}, "s1 did not receive pre-seeded s2 WriteThrough for big")

	// Payload within maxWT writes through directly to s2.
	smallVal := []byte("fits-within-limit")
	if err := s1.Set("small", smallVal, time.Minute); err != nil {
		t.Fatalf("s1.Set(small) error = %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := s2.Get("small")
		return ok && bytes.Equal(v, smallVal)
	}, "small payload did not write through to peer s2")

	// Payload strictly > maxWT falls back to InvalidateOnly; peer sees miss rather than stale or new value.
	bigVal := bytes.Repeat([]byte("B"), maxWT+1)
	if err := s1.Set("big", bigVal, time.Minute); err != nil {
		t.Fatalf("s1.Set(big) error = %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s2.Get("big")
		return !ok
	}, "oversized payload did not invalidate stale copy on peer s2")

	if got, ok := s1.Get("big"); !ok || !bytes.Equal(got, bigVal) {
		t.Fatalf("origin s1 lost oversized value: ok=%v len=%d", ok, len(got))
	}
}

func TestTier2_F11_SetMulti_ExceedingWriteThroughMaxBytes_FallsBackCleanly(t *testing.T) {
	ps := memorystore.NewInMemoryPubSub()
	defer func() { _ = ps.Close() }()

	const maxWT = 64
	s1 := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("wt:multi-fallback"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithWriteThroughMaxBytes(maxWT),
		memorystore.WithSyncInvalidation(),
	)
	s2 := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("wt:multi-fallback"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithWriteThroughMaxBytes(maxWT),
		memorystore.WithSyncInvalidation(),
	)

	// Seed stale entries on peer s2.
	if err := s2.SetMulti(map[string][]byte{
		"m1": []byte("stale-1"),
		"m2": []byte("stale-2"),
		"m3": []byte("stale-3"),
	}, time.Minute); err != nil {
		t.Fatalf("s2.SetMulti seed error = %v", err)
	}

	// Batch containing an entry larger than maxWT and total batch > maxWT.
	bigEntry := bytes.Repeat([]byte("X"), maxWT+16)
	if err := s1.SetMulti(map[string][]byte{
		"m1": []byte("new-1"),
		"m2": []byte("new-2"),
		"m3": bigEntry,
	}, time.Minute); err != nil {
		t.Fatalf("s1.SetMulti error = %v", err)
	}

	// Peer s2 must never retain any stale values; m3 must be invalidated (miss).
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v1, ok1 := s2.Get("m1")
		v2, ok2 := s2.Get("m2")
		_, ok3 := s2.Get("m3")
		return (!ok1 || string(v1) == "new-1") && (!ok2 || string(v2) == "new-2") && !ok3
	}, "SetMulti exceeding WriteThroughMaxBytes left stale values on peer s2")
}

func TestTier2_F11_AsyncInvalidation_DropsWithoutBlockingSetUnderStalledPubSub(t *testing.T) {
	ps := newControllablePubSub()
	ps.SetPublishDelay(200 * time.Millisecond)
	defer func() {
		ps.SetPublishDelay(0)
		_ = ps.Close()
	}()

	rec := newRecordingMetrics()
	s := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("async:stall"),
		memorystore.WithAsyncInvalidation(2),
		memorystore.WithMetricsRecorder(rec),
	)

	start := time.Now()
	for i := range 50 {
		if err := s.Set(fmt.Sprintf("k-%d", i), []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set(%d) error = %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("50 Sets took %v under stalled PubSub, want < 50ms (non-blocking)", elapsed)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return rec.InvalidationsDropped() > 0 && s.GetMetrics().InvalidationDrops > 0
	}, "expected bounded async queue drops to increment InvalidationsDropped and StoreMetrics.InvalidationDrops")
}

func TestTier2_F11_Backplane_ReconnectsAfterCloseTopicChannels(t *testing.T) {
	ps := newControllablePubSub()
	defer func() { _ = ps.Close() }()

	const topic = "reconn:topic"
	s1 := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic(topic),
		memorystore.WithSyncInvalidation(),
	)
	s2 := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic(topic),
		memorystore.WithSyncInvalidation(),
	)

	// Confirm initial backplane works.
	if err := s2.Set("k1", []byte("stale-1"), time.Minute); err != nil {
		t.Fatalf("s2.Set(k1) error = %v", err)
	}
	if err := s1.Set("k1", []byte("new-1"), time.Minute); err != nil {
		t.Fatalf("s1.Set(k1) error = %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s2.Get("k1")
		return !ok
	}, "initial invalidation did not propagate")

	beforeSubs := ps.SubscribeCount()

	// Simulate transient broker disconnect by closing all subscriber channels for topic.
	ps.CloseTopicChannels(topic)

	// Both stores must automatically reconnect via backplane retry loop.
	eventually(t, 3*time.Second, 20*time.Millisecond, func() bool {
		return ps.SubscribeCount() >= beforeSubs+2
	}, "backplane did not reconnect both stores after channel close")

	if err := s2.Set("k2", []byte("stale-2"), time.Minute); err != nil {
		t.Fatalf("s2.Set(k2) error = %v", err)
	}
	if err := s1.Set("k2", []byte("new-2"), time.Minute); err != nil {
		t.Fatalf("s1.Set(k2) error = %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		_, ok := s2.Get("k2")
		return !ok
	}, "invalidation did not resume after backplane reconnect")
}

func TestTier2_F11_EmptyInvalidationTopic_IgnoresInvalidationCleanly(t *testing.T) {
	ps := newControllablePubSub()
	defer func() { _ = ps.Close() }()

	s1 := mustNewStore(t, memorystore.WithPubSub(ps), memorystore.WithInvalidationTopic(""))
	s2 := mustNewStore(t, memorystore.WithPubSub(ps))

	if err := s2.Set("shared", []byte("peer-v"), time.Minute); err != nil {
		t.Fatalf("s2.Set error = %v", err)
	}
	if err := s1.Set("shared", []byte("local-v"), time.Minute); err != nil {
		t.Fatalf("s1.Set error = %v", err)
	}
	if err := s1.Delete("shared"); err != nil {
		t.Fatalf("s1.Delete error = %v", err)
	}

	if ps.SubscribeCount() != 0 {
		t.Fatalf("SubscribeCount = %d, want 0 when InvalidationTopic is empty", ps.SubscribeCount())
	}
	if ps.PublishCount() != 0 {
		t.Fatalf("PublishCount = %d, want 0 when InvalidationTopic is empty", ps.PublishCount())
	}
	if v, ok := s2.Get("shared"); !ok || string(v) != "peer-v" {
		t.Fatalf("s2.Get(shared) = (%q, %v), want (peer-v, true)", v, ok)
	}
}

// -----------------------------------------------------------------------------
// F12: Streaming Snapshot Persistence
// -----------------------------------------------------------------------------

func TestTier2_F12_Load_RejectsBadMagicHeader(t *testing.T) {
	s := mustNewStore(t)

	badMagic := []byte("BADMG\x02\x01\x00\x00\x00\x00\x00")
	err := s.Load(bytes.NewReader(badMagic))
	if !errors.Is(err, memorystore.ErrCorruptSnapshot) {
		t.Fatalf("Load(bad magic) error = %v, want ErrCorruptSnapshot", err)
	}

	// Empty reader must also fail cleanly rather than succeeding silently on 0-byte binary stream.
	if err := s.Load(bytes.NewReader(nil)); !errors.Is(err, memorystore.ErrCorruptSnapshot) {
		t.Fatalf("Load(empty reader) error = %v, want ErrCorruptSnapshot", err)
	}
}

func TestTier2_F12_Load_RejectsUnsupportedVersion(t *testing.T) {
	src := mustNewStore(t)
	if err := src.Set("k", []byte("val"), time.Minute); err != nil {
		t.Fatalf("src.Set error = %v", err)
	}
	var buf bytes.Buffer
	if err := src.Save(&buf); err != nil {
		t.Fatalf("src.Save error = %v", err)
	}

	raw := bytes.Clone(buf.Bytes())
	if !bytes.HasPrefix(raw, []byte("MSNAP\x02")) || len(raw) <= 6 {
		t.Fatalf("unexpected snapshot header prefix: %x", raw[:min(len(raw), 16)])
	}
	// Mutate version byte immediately following magic "MSNAP\x02".
	raw[6] = 0xfe

	dst := mustNewStore(t)
	err := dst.Load(bytes.NewReader(raw))
	if !errors.Is(err, memorystore.ErrUnsupportedSnapshotVersion) {
		t.Fatalf("Load(unsupported version) error = %v, want ErrUnsupportedSnapshotVersion", err)
	}
}

func TestTier2_F12_Load_RejectsCorruptCRC32C_TruncatedEOF_AndOversizedLength(t *testing.T) {
	src := mustNewStore(t)
	for i := range 5 {
		if err := src.Set(fmt.Sprintf("snap-%d", i), bytes.Repeat([]byte("A"), 32), time.Minute); err != nil {
			t.Fatalf("src.Set(%d) error = %v", i, err)
		}
	}
	var buf bytes.Buffer
	if err := src.Save(&buf); err != nil {
		t.Fatalf("src.Save error = %v", err)
	}
	valid := buf.Bytes()

	t.Run("corrupt frame CRC32C", func(t *testing.T) {
		corrupt := bytes.Clone(valid)
		corrupt[len(corrupt)/2] ^= 0xff
		dst := mustNewStore(t)
		if err := dst.Load(bytes.NewReader(corrupt)); !errors.Is(err, memorystore.ErrCorruptSnapshot) {
			t.Fatalf("Load(corrupt CRC) error = %v, want ErrCorruptSnapshot", err)
		}
	})

	t.Run("truncated stream missing EOF sentinel", func(t *testing.T) {
		truncated := valid[:len(valid)-4]
		dst := mustNewStore(t)
		if err := dst.Load(bytes.NewReader(truncated)); !errors.Is(err, memorystore.ErrCorruptSnapshot) {
			t.Fatalf("Load(truncated EOF) error = %v, want ErrCorruptSnapshot", err)
		}
	})

	t.Run("oversized key/value length guard", func(t *testing.T) {
		// Preserve stream header + data frame type (0x01), set entryCount=1,
		// and set payloadLen to 0xffffffff so the oversized length guard triggers.
		oversized := bytes.Clone(valid[:min(len(valid), 64)])
		oversized[8] = valid[8]
		binary.LittleEndian.PutUint32(oversized[9:13], 1)
		for i := 13; i < 17; i++ {
			oversized[i] = 0xff
		}
		dst := mustNewStore(t)
		if err := dst.Load(bytes.NewReader(oversized)); !errors.Is(err, memorystore.ErrCorruptSnapshot) {
			t.Fatalf("Load(oversized length) error = %v, want ErrCorruptSnapshot", err)
		}
	})

	t.Run("malformed SnapshotJSON stream", func(t *testing.T) {
		dst := mustNewStore(t)
		if err := dst.Load(strings.NewReader(`{"broken":`), memorystore.WithSnapshotFormat(memorystore.SnapshotJSON)); err == nil {
			t.Fatal("Load(malformed JSON) succeeded, want error")
		}
	})
}

func TestTier2_F12_Load_KeepExistingVsOverwriteConflictPolicy(t *testing.T) {
	src := mustNewStore(t)
	if err := src.Set("shared", []byte("from-snapshot"), time.Minute); err != nil {
		t.Fatalf("src.Set(shared) error = %v", err)
	}
	if err := src.Set("snap-only", []byte("added"), time.Minute); err != nil {
		t.Fatalf("src.Set(snap-only) error = %v", err)
	}
	var buf bytes.Buffer
	if err := src.Save(&buf); err != nil {
		t.Fatalf("src.Save error = %v", err)
	}
	snapBytes := buf.Bytes()

	// KeepExisting preserves fresher local value while loading missing keys.
	keepStore := mustNewStore(t)
	if err := keepStore.Set("shared", []byte("fresher-local"), time.Minute); err != nil {
		t.Fatalf("keepStore.Set error = %v", err)
	}
	if err := keepStore.Load(bytes.NewReader(snapBytes), memorystore.WithSnapshotConflictPolicy(memorystore.SnapshotKeepExisting)); err != nil {
		t.Fatalf("Load(KeepExisting) error = %v", err)
	}
	if v, ok := keepStore.Get("shared"); !ok || string(v) != "fresher-local" {
		t.Fatalf("KeepExisting shared = (%q, %v), want (fresher-local, true)", v, ok)
	}
	if v, ok := keepStore.Get("snap-only"); !ok || string(v) != "added" {
		t.Fatalf("KeepExisting snap-only = (%q, %v), want (added, true)", v, ok)
	}

	// Overwrite (explicit and default) replaces existing keys.
	overwriteStore := mustNewStore(t)
	if err := overwriteStore.Set("shared", []byte("stale-local"), time.Minute); err != nil {
		t.Fatalf("overwriteStore.Set error = %v", err)
	}
	if err := overwriteStore.Load(bytes.NewReader(snapBytes), memorystore.WithSnapshotConflictPolicy(memorystore.SnapshotOverwrite)); err != nil {
		t.Fatalf("Load(Overwrite) error = %v", err)
	}
	if v, ok := overwriteStore.Get("shared"); !ok || string(v) != "from-snapshot" {
		t.Fatalf("Overwrite shared = (%q, %v), want (from-snapshot, true)", v, ok)
	}
}

func TestTier2_F12_Load_EnforcesMaxItemsAndMaxBytesIncrementally(t *testing.T) {
	src := mustNewStore(t)
	payload := bytes.Repeat([]byte("P"), 64)
	for i := range 60 {
		if err := src.Set(fmt.Sprintf("item-%03d", i), payload, time.Minute); err != nil {
			t.Fatalf("src.Set(%d) error = %v", i, err)
		}
	}

	var binBuf, jsonBuf bytes.Buffer
	if err := src.Save(&binBuf); err != nil {
		t.Fatalf("src.Save(binary) error = %v", err)
	}
	if err := src.Save(&jsonBuf, memorystore.WithSnapshotFormat(memorystore.SnapshotJSON)); err != nil {
		t.Fatalf("src.Save(JSON) error = %v", err)
	}

	for _, tc := range []struct {
		name   string
		format memorystore.SnapshotFormat
		data   []byte
	}{
		{name: "binary", format: memorystore.SnapshotBinary, data: binBuf.Bytes()},
		{name: "json", format: memorystore.SnapshotJSON, data: jsonBuf.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := mustNewStore(t,
				memorystore.WithMaxItems(10),
				memorystore.WithMaxBytes(1024),
			)
			if err := dst.Load(bytes.NewReader(tc.data), memorystore.WithSnapshotFormat(tc.format)); err != nil {
				t.Fatalf("Load(%s) error = %v", tc.name, err)
			}
			m := dst.GetMetrics()
			if m.Items > 10 {
				t.Fatalf("Load(%s) Items = %d, want <= 10", tc.name, m.Items)
			}
			if m.Bytes > 1024 {
				t.Fatalf("Load(%s) Bytes = %d, want <= 1024", tc.name, m.Bytes)
			}
			if m.Items == 0 || m.Bytes == 0 {
				t.Fatalf("Load(%s) loaded 0 items/bytes (%+v)", tc.name, m)
			}
		})
	}
}

func TestTier2_F12_SaveLoad_EmptyStore_AndExpiredSkipWithNoExpiration(t *testing.T) {
	for _, format := range []memorystore.SnapshotFormat{memorystore.SnapshotBinary, memorystore.SnapshotJSON} {
		t.Run(fmt.Sprintf("empty-format-%d", format), func(t *testing.T) {
			emptySrc := mustNewStore(t)
			var buf bytes.Buffer
			if err := emptySrc.Save(&buf, memorystore.WithSnapshotFormat(format)); err != nil {
				t.Fatalf("Save(empty) error = %v", err)
			}

			emptyDst := mustNewStore(t)
			if err := emptyDst.Load(&buf, memorystore.WithSnapshotFormat(format)); err != nil {
				t.Fatalf("Load(empty) error = %v", err)
			}
			if m := emptyDst.GetMetrics(); m.Items != 0 || m.Bytes != 0 {
				t.Fatalf("emptyDst metrics = (Items=%d, Bytes=%d), want (0, 0)", m.Items, m.Bytes)
			}
		})
	}

	// Verify expired entries are skipped while NoExpiration and live relative TTLs survive.
	src := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithCleanupInterval(time.Hour))
	if err := src.Set("already-expired", []byte("x"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set(expired) error = %v", err)
	}
	if err := src.Set("perm", []byte("forever"), memorystore.NoExpiration); err != nil {
		t.Fatalf("Set(perm) error = %v", err)
	}
	if err := src.Set("short-live", []byte("soon"), 150*time.Millisecond); err != nil {
		t.Fatalf("Set(short-live) error = %v", err)
	}

	eventually(t, 200*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := src.Get("already-expired")
		return !ok
	}, "already-expired did not expire before Save")

	var buf bytes.Buffer
	if err := src.Save(&buf); err != nil {
		t.Fatalf("Save error = %v", err)
	}

	dst := mustNewStore(t, memorystore.WithPreciseClock(), memorystore.WithCleanupInterval(5*time.Millisecond))
	if err := dst.Load(&buf); err != nil {
		t.Fatalf("Load error = %v", err)
	}

	if _, ok := dst.Get("already-expired"); ok {
		t.Fatal("already-expired was unexpectedly restored")
	}
	if v, ok := dst.Get("perm"); !ok || string(v) != "forever" {
		t.Fatalf("perm = (%q, %v), want (forever, true)", v, ok)
	}
	if v, ok := dst.Get("short-live"); !ok || string(v) != "soon" {
		t.Fatalf("short-live = (%q, %v), want (soon, true)", v, ok)
	}

	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		_, ok := dst.Get("short-live")
		return !ok
	}, "short-live did not expire according to relative TTL")

	if v, ok := dst.Get("perm"); !ok || string(v) != "forever" {
		t.Fatalf("perm lost after short-live expiry: (%q, %v)", v, ok)
	}
}
