package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

func TestTier3_S3FIFO_SaveLoad_IncrementalBounds(t *testing.T) {
	t.Parallel()

	src := mustNewStore(t,
		memorystore.WithMaxItems(200),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithPreciseClock(),
	)

	const totalItems = 100
	val := bytes.Repeat([]byte("s3fifo-payload-"), 4) // 60 bytes each
	for i := range totalItems {
		key := fmt.Sprintf("item-%03d", i)
		if i < 10 {
			if err := src.SetWithOptions(key, val, memorystore.NoExpiration, memorystore.WithNoExpiration()); err != nil {
				t.Fatalf("SetWithOptions(%q) failed: %v", key, err)
			}
		} else {
			if err := src.Set(key, val, 10*time.Minute); err != nil {
				t.Fatalf("Set(%q) failed: %v", key, err)
			}
		}
	}

	// Warm the first 10 items repeatedly so S3-FIFO promotes/retains them.
	for round := range 5 {
		for i := range 10 {
			key := fmt.Sprintf("item-%03d", i)
			if got, ok := src.Get(key); !ok || !bytes.Equal(got, val) {
				t.Fatalf("round %d: Get(%q) = (%v, %v), want value", round, key, ok, len(got))
			}
		}
	}

	var snap bytes.Buffer
	if err := src.Save(&snap); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}
	if snap.Len() == 0 {
		t.Fatal("Save() produced empty snapshot")
	}

	const maxItems = 30
	const maxBytes = int64(2048)
	dst := mustNewStore(t,
		memorystore.WithMaxItems(maxItems),
		memorystore.WithMaxBytes(maxBytes),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithPreciseClock(),
	)

	if err := dst.Load(&snap); err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	m := dst.GetMetrics()
	if m.Items == 0 {
		t.Fatal("dst store has 0 items after Load()")
	}
	if m.Items > maxItems {
		t.Fatalf("dst Items = %d, exceeds MaxItems = %d", m.Items, maxItems)
	}
	if m.Bytes > maxBytes {
		t.Fatalf("dst Bytes = %d, exceeds MaxBytes = %d", m.Bytes, maxBytes)
	}

	// Verify loaded entries have intact values and valid bounds behavior.
	retained := 0
	for i := range totalItems {
		key := fmt.Sprintf("item-%03d", i)
		if got, ok := dst.Get(key); ok {
			if !bytes.Equal(got, val) {
				t.Fatalf("Get(%q) returned corrupted value: %q", key, got)
			}
			retained++
		}
	}
	if retained == 0 {
		t.Fatal("expected retained items in dst after bounded Load()")
	}
}

func TestTier3_TinyLFU_SaveLoad_JSON_KeepExisting(t *testing.T) {
	t.Parallel()

	src := mustNewStore(t,
		memorystore.WithMaxItems(100),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
		memorystore.WithPreciseClock(),
	)

	if err := src.Set("shared-key", []byte("from-snapshot"), time.Minute); err != nil {
		t.Fatalf("src.Set(shared-key) failed: %v", err)
	}
	if err := src.Set("missing-key", []byte("only-in-snapshot"), time.Minute); err != nil {
		t.Fatalf("src.Set(missing-key) failed: %v", err)
	}
	if err := src.Set("perm-key", []byte("permanent-val"), memorystore.NoExpiration); err != nil {
		t.Fatalf("src.Set(perm-key) failed: %v", err)
	}

	var jsonSnap bytes.Buffer
	if err := src.Save(&jsonSnap, memorystore.WithSnapshotFormat(memorystore.SnapshotJSON)); err != nil {
		t.Fatalf("Save(SnapshotJSON) failed: %v", err)
	}
	trimmed := strings.TrimSpace(jsonSnap.String())
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		t.Fatalf("expected JSON snapshot output, got prefix %q", trimmed[:min(len(trimmed), 32)])
	}
	rawJSON := jsonSnap.Bytes()

	dst := mustNewStore(t,
		memorystore.WithMaxItems(100),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
		memorystore.WithPreciseClock(),
	)
	if err := dst.Set("shared-key", []byte("live-newer-value"), time.Minute); err != nil {
		t.Fatalf("dst.Set(shared-key) failed: %v", err)
	}

	if err := dst.Load(
		bytes.NewReader(rawJSON),
		memorystore.WithSnapshotFormat(memorystore.SnapshotJSON),
		memorystore.WithSnapshotConflictPolicy(memorystore.SnapshotKeepExisting),
	); err != nil {
		t.Fatalf("Load(SnapshotJSON, KeepExisting) failed: %v", err)
	}

	if got, ok := dst.Get("shared-key"); !ok || string(got) != "live-newer-value" {
		t.Fatalf("KeepExisting failed: got (%q, %v), want %q", got, ok, "live-newer-value")
	}
	if got, ok := dst.Get("missing-key"); !ok || string(got) != "only-in-snapshot" {
		t.Fatalf("Get(missing-key) = (%q, %v), want %q", got, ok, "only-in-snapshot")
	}
	if got, ok := dst.Get("perm-key"); !ok || string(got) != "permanent-val" {
		t.Fatalf("Get(perm-key) = (%q, %v), want %q", got, ok, "permanent-val")
	}

	// Loading with Overwrite replaces the live key with snapshot contents.
	if err := dst.Load(
		bytes.NewReader(rawJSON),
		memorystore.WithSnapshotFormat(memorystore.SnapshotJSON),
		memorystore.WithSnapshotConflictPolicy(memorystore.SnapshotOverwrite),
	); err != nil {
		t.Fatalf("Load(SnapshotJSON, Overwrite) failed: %v", err)
	}
	if got, ok := dst.Get("shared-key"); !ok || string(got) != "from-snapshot" {
		t.Fatalf("Overwrite failed: got (%q, %v), want %q", got, ok, "from-snapshot")
	}
}

func TestTier3_SWR_Singleflight_MetricsRecorder_Health(t *testing.T) {
	t.Parallel()

	rec := newRecordingMetrics()
	store := mustNewStore(t,
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithPreciseClock(),
	)

	ctx := context.Background()
	if err := store.Health(ctx); err != nil {
		t.Fatalf("initial Health() failed: %v", err)
	}

	var coldLoads atomic.Int32
	const callers = 16
	const softTTL = 40 * time.Millisecond
	const swrGrace = 250 * time.Millisecond

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	vals := make(chan string, callers)
	start := make(chan struct{})

	for range callers {
		wg.Go(func() {
			<-start
			v, err := store.GetOrLoadWithOptions(
				ctx,
				"swr-sf-key",
				softTTL,
				func(_ context.Context) ([]byte, error) {
					coldLoads.Add(1)
					time.Sleep(15 * time.Millisecond)
					return []byte("v1"), nil
				},
				memorystore.WithStaleWhileRevalidate(swrGrace),
			)
			errs <- err
			vals <- string(v)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	close(vals)

	for err := range errs {
		if err != nil {
			t.Fatalf("cold GetOrLoadWithOptions error: %v", err)
		}
	}
	for v := range vals {
		if v != "v1" {
			t.Fatalf("cold GetOrLoadWithOptions = %q, want %q", v, "v1")
		}
	}
	if coldLoads.Load() != 1 {
		t.Fatalf("cold loader executions = %d, want 1", coldLoads.Load())
	}
	if rec.Loads() < 1 {
		t.Fatalf("rec.Loads() = %d, want >= 1", rec.Loads())
	}
	if rec.SharedLoads() < 1 {
		t.Fatalf("rec.SharedLoads() = %d, want >= 1 for concurrent singleflight", rec.SharedLoads())
	}

	// Wait until soft TTL expires so plain Get treats it as a miss while still inside SWR grace.
	eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
		_, ok := store.Get("swr-sf-key")
		return !ok
	}, "soft TTL should expire for plain Get")

	var staleRefreshes atomic.Int32
	staleErrs := make(chan error, callers)
	staleVals := make(chan string, callers)
	startStale := make(chan struct{})

	for range callers {
		wg.Go(func() {
			<-startStale
			v, err := store.GetOrLoadWithOptions(
				ctx,
				"swr-sf-key",
				time.Minute,
				func(_ context.Context) ([]byte, error) {
					staleRefreshes.Add(1)
					time.Sleep(15 * time.Millisecond)
					return []byte("v2"), nil
				},
				memorystore.WithStaleWhileRevalidate(swrGrace),
			)
			staleErrs <- err
			staleVals <- string(v)
		})
	}
	close(startStale)
	wg.Wait()
	close(staleErrs)
	close(staleVals)

	for err := range staleErrs {
		if err != nil {
			t.Fatalf("stale GetOrLoadWithOptions error: %v", err)
		}
	}
	// At least one caller served immediately with stale v1 (and after refresh, v2).
	sawV1 := false
	for v := range staleVals {
		if v != "v1" && v != "v2" {
			t.Fatalf("unexpected stale/refreshed value %q", v)
		}
		if v == "v1" {
			sawV1 = true
		}
	}
	if !sawV1 {
		t.Fatal("expected stale SWR hit to serve v1 immediately")
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := store.Get("swr-sf-key")
		return ok && string(v) == "v2"
	}, "background SWR refresh should populate v2")

	if staleRefreshes.Load() != 1 {
		t.Fatalf("stale background refresh count = %d, want 1", staleRefreshes.Load())
	}
	if err := store.Health(ctx); err != nil {
		t.Fatalf("Health() after SWR failed: %v", err)
	}
}

func TestTier3_SlidingExpiration_NoExpiration_CoarseVsPreciseClock(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts []memorystore.Option
	}{
		{name: "DefaultCoarseClock", opts: nil},
		{name: "PreciseClockOptOut", opts: []memorystore.Option{memorystore.WithPreciseClock()}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := mustNewStore(t, tc.opts...)

			if err := store.SetWithOptions("perm", []byte("forever"), 10*time.Millisecond, memorystore.WithNoExpiration()); err != nil {
				t.Fatalf("SetWithOptions(perm) failed: %v", err)
			}
			if err := store.SetWithOptions("slide", []byte("session"), 180*time.Millisecond, memorystore.WithSlidingExpiration()); err != nil {
				t.Fatalf("SetWithOptions(slide) failed: %v", err)
			}
			if err := store.Set("ephemeral", []byte("short"), 5*time.Millisecond); err != nil {
				t.Fatalf("Set(ephemeral) failed: %v", err)
			}

			// Sub-10ms entry must expire promptly under both coarse (preciseTTL fallback) and precise clocks.
			eventually(t, 100*time.Millisecond, 2*time.Millisecond, func() bool {
				_, ok := store.Get("ephemeral")
				return !ok
			}, "5ms ephemeral entry should expire promptly")

			// Touch sliding entry every 60ms for 240ms (> initial 180ms TTL).
			for range 4 {
				time.Sleep(60 * time.Millisecond)
				got, ok := store.Get("slide")
				if !ok || string(got) != "session" {
					t.Fatalf("sliding entry expired prematurely while being read: (%q, %v)", got, ok)
				}
			}

			// Stop touching sliding entry for > 180ms TTL; verify it expires while perm stays alive.
			time.Sleep(240 * time.Millisecond)
			if _, ok := store.Get("slide"); ok {
				t.Fatal("sliding entry should expire after reads stop")
			}

			if got, ok := store.Get("perm"); !ok || string(got) != "forever" {
				t.Fatalf("NoExpiration entry missing or altered: (%q, %v)", got, ok)
			}
		})
	}
}

func TestTier3_CopyOnWriteRead_WriteThroughInvalidation(t *testing.T) {
	t.Parallel()

	ps := newControllablePubSub()
	t.Cleanup(func() { _ = ps.Close() })

	opts := func() []memorystore.Option {
		return []memorystore.Option{
			memorystore.WithPubSub(ps),
			memorystore.WithInvalidationTopic("wt-copy-isolation"),
			memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
			memorystore.WithSyncInvalidation(),
			memorystore.WithCopyOnWrite(true),
			memorystore.WithCopyOnRead(true),
			memorystore.WithPreciseClock(),
		}
	}

	nodeA := mustNewStore(t, opts()...)
	nodeB := mustNewStore(t, opts()...)

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return ps.SubscribeCount() >= 2
	}, "nodeA and nodeB should subscribe to invalidation topic")

	orig := []byte("hello-world")
	if err := nodeA.Set("isolated-key", orig, time.Minute); err != nil {
		t.Fatalf("nodeA.Set() failed: %v", err)
	}
	// Mutate caller input slice after Set (CopyOnWrite check).
	orig[0] = 'X'

	gotA1, ok := nodeA.Get("isolated-key")
	if !ok || string(gotA1) != "hello-world" {
		t.Fatalf("CopyOnWrite failed on nodeA: got (%q, %v), want %q", gotA1, ok, "hello-world")
	}
	// Mutate slice returned by Get on origin (CopyOnRead check).
	gotA1[1] = 'Y'
	gotA2, ok := nodeA.Get("isolated-key")
	if !ok || string(gotA2) != "hello-world" {
		t.Fatalf("CopyOnRead failed on nodeA: got (%q, %v), want %q", gotA2, ok, "hello-world")
	}

	// Wait for WriteThrough propagation to nodeB.
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		v, ok := nodeB.Get("isolated-key")
		return ok && string(v) == "hello-world"
	}, "nodeB should receive write-through value")

	gotB1, ok := nodeB.Get("isolated-key")
	if !ok || string(gotB1) != "hello-world" {
		t.Fatalf("nodeB.Get() = (%q, %v), want %q", gotB1, ok, "hello-world")
	}
	// Mutate slice returned by Get on peer (CopyOnRead check on peer).
	gotB1[2] = 'Z'
	gotB2, ok := nodeB.Get("isolated-key")
	if !ok || string(gotB2) != "hello-world" {
		t.Fatalf("CopyOnRead failed on nodeB: got (%q, %v), want %q", gotB2, ok, "hello-world")
	}
}

func TestTier3_Jitter_SWR_GetOrLoadJSONWithOptions(t *testing.T) {
	t.Parallel()

	type itemDTO struct {
		Version int    `json:"version"`
		Label   string `json:"label"`
	}

	store := mustNewStore(t,
		memorystore.WithDefaultTTLJitter(0.25),
		memorystore.WithPreciseClock(),
	)

	ctx := context.Background()
	const baseTTL = 40 * time.Millisecond
	const swrGrace = 300 * time.Millisecond

	var loads atomic.Int32
	for i := range 5 {
		key := fmt.Sprintf("json-jitter-%d", i)
		got, err := store.GetOrLoadJSONWithOptions[itemDTO](
			ctx,
			key,
			baseTTL,
			func(_ context.Context) (itemDTO, error) {
				loads.Add(1)
				return itemDTO{Version: 1, Label: key}, nil
			},
			[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(swrGrace)},
		)
		if err != nil {
			t.Fatalf("initial GetOrLoadJSONWithOptions(%q) error: %v", key, err)
		}
		if got.Version != 1 || got.Label != key {
			t.Fatalf("unexpected initial DTO: %+v", got)
		}
	}

	// Wait past maximum jittered soft TTL (40ms * 1.25 = 50ms) so entries are stale but within 300ms grace.
	time.Sleep(65 * time.Millisecond)

	for i := range 5 {
		key := fmt.Sprintf("json-jitter-%d", i)
		if _, ok := store.Get(key); ok {
			t.Fatalf("plain Get(%q) should miss once jittered soft TTL elapsed", key)
		}

		staleDTO, err := store.GetOrLoadJSONWithOptions[itemDTO](
			ctx,
			key,
			time.Minute,
			func(_ context.Context) (itemDTO, error) {
				loads.Add(1)
				return itemDTO{Version: 2, Label: key}, nil
			},
			[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(swrGrace)},
		)
		if err != nil {
			t.Fatalf("stale GetOrLoadJSONWithOptions(%q) error: %v", key, err)
		}
		if staleDTO.Version != 1 && staleDTO.Version != 2 {
			t.Fatalf("unexpected version %d for %q", staleDTO.Version, key)
		}
	}

	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		for i := range 5 {
			key := fmt.Sprintf("json-jitter-%d", i)
			dto, ok, err := store.GetJSONAs[itemDTO](key)
			if err != nil || !ok || dto.Version != 2 {
				return false
			}
		}
		return true
	}, "all jittered JSON keys should refresh to Version 2 via SWR")
}

func TestTier3_WriteThrough_MaxBytesFallback_MetricsRecorder(t *testing.T) {
	t.Parallel()

	ps := newControllablePubSub()
	t.Cleanup(func() { _ = ps.Close() })

	recA := newRecordingMetrics()
	recB := newRecordingMetrics()

	nodeA := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("wt-maxbytes"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithWriteThroughMaxBytes(128),
		memorystore.WithSyncInvalidation(),
		memorystore.WithMetricsRecorder(recA),
		memorystore.WithPreciseClock(),
	)
	nodeB := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("wt-maxbytes"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithWriteThroughMaxBytes(128),
		memorystore.WithSyncInvalidation(),
		memorystore.WithMetricsRecorder(recB),
		memorystore.WithPreciseClock(),
	)

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return ps.SubscribeCount() >= 2
	}, "nodeA and nodeB should subscribe to invalidation topic")

	// Pre-populate oversized key on nodeB to verify fallback invalidation evicts it.
	// Wait for nodeA to consume nodeB's WriteThrough message before nodeA overwrites "oversized".
	if err := nodeB.Set("oversized", []byte("stale-peer-value"), time.Minute); err != nil {
		t.Fatalf("nodeB.Set(oversized) failed: %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v, ok := nodeA.Get("oversized")
		return ok && string(v) == "stale-peer-value"
	}, "nodeA did not receive pre-seeded nodeB WriteThrough for oversized")

	// Small payload (<= 128 bytes) propagates via WriteThrough.
	if err := nodeA.Set("small", []byte("compact-value"), time.Minute); err != nil {
		t.Fatalf("nodeA.Set(small) failed: %v", err)
	}
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		v, ok := nodeB.Get("small")
		return ok && string(v) == "compact-value"
	}, "small key should be populated on nodeB via WriteThrough")

	// Oversized payload (> 128 bytes) falls back to InvalidateOnly on peer.
	largeVal := bytes.Repeat([]byte("L"), 512)
	if err := nodeA.Set("oversized", largeVal, time.Minute); err != nil {
		t.Fatalf("nodeA.Set(oversized) failed: %v", err)
	}
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		_, ok := nodeB.Get("oversized")
		return !ok
	}, "oversized key should fall back to InvalidateOnly and evict stale copy on nodeB")

	if got, ok := nodeA.Get("oversized"); !ok || !bytes.Equal(got, largeVal) {
		t.Fatalf("nodeA lost its own oversized key: ok=%v len=%d", ok, len(got))
	}
	if recA.InvalidationsSent() < 2 {
		t.Fatalf("recA.InvalidationsSent() = %d, want >= 2", recA.InvalidationsSent())
	}
	if recA.InvalidatedKeys() < 2 {
		t.Fatalf("recA.InvalidatedKeys() = %d, want >= 2", recA.InvalidatedKeys())
	}
}

func TestTier3_AsyncInvalidation_BackplaneReconnect_Health(t *testing.T) {
	t.Parallel()

	ps := newControllablePubSub()
	t.Cleanup(func() { _ = ps.Close() })

	const topic = "async-reconn-health"
	nodeA := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic(topic),
		memorystore.WithAsyncInvalidation(64),
		memorystore.WithPubSubTimeout(500*time.Millisecond),
	)
	nodeB := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic(topic),
		memorystore.WithAsyncInvalidation(64),
		memorystore.WithPubSubTimeout(500*time.Millisecond),
	)

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return ps.SubscribeCount() >= 2
	}, "nodeA and nodeB should subscribe to invalidation topic")

	ctx := context.Background()
	if err := nodeA.Health(ctx); err != nil {
		t.Fatalf("initial nodeA.Health() failed: %v", err)
	}
	if err := nodeB.Health(ctx); err != nil {
		t.Fatalf("initial nodeB.Health() failed: %v", err)
	}

	// Simulate Pinger failure and recovery.
	ps.SetPingError(errors.New("backplane unreachable"))
	if err := nodeA.Health(ctx); err == nil {
		t.Fatal("expected nodeA.Health() to fail when Ping returns error")
	}
	ps.SetPingError(nil)
	if err := nodeA.Health(ctx); err != nil {
		t.Fatalf("expected nodeA.Health() to recover after Ping clears: %v", err)
	}

	// Slow publish must not block local Set when async invalidation is enabled.
	ps.SetPublishDelay(250 * time.Millisecond)
	start := time.Now()
	if err := nodeA.Set("fast-local", []byte("v1"), time.Minute); err != nil {
		t.Fatalf("nodeA.Set(fast-local) failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("async Set took %v despite slow PubSub publish", elapsed)
	}
	ps.SetPublishDelay(0)

	// Seed key on nodeB, then break subscription channels to trigger backplane reconnect.
	if err := nodeB.Set("reconn-key", []byte("stale"), time.Minute); err != nil {
		t.Fatalf("nodeB.Set(reconn-key) failed: %v", err)
	}

	subsBefore := ps.SubscribeCount()
	ps.CloseTopicChannels(topic)

	// Wait for backplanes to re-subscribe.
	eventually(t, 3*time.Second, 20*time.Millisecond, func() bool {
		return ps.SubscribeCount() > subsBefore
	}, "backplane should reconnect after closed topic channel")

	// Wait briefly until invalidation after reconnect removes stale key on nodeB.
	eventually(t, 3*time.Second, 25*time.Millisecond, func() bool {
		if err := nodeA.Set("reconn-key", []byte("fresh"), time.Minute); err != nil {
			return false
		}
		_, ok := nodeB.Get("reconn-key")
		return !ok
	}, "invalidation should work after backplane reconnect")

	if err := nodeB.Health(ctx); err != nil {
		t.Fatalf("nodeB.Health() after reconnect failed: %v", err)
	}
}

func TestTier3_S3FIFO_DeleteZeroTTL_OnEvict(t *testing.T) {
	t.Parallel()

	rec := newRecordingMetrics()
	var mu sync.Mutex
	evicted := make(map[string]memorystore.EvictReason)
	var capEvictCount atomic.Int32

	store := mustNewStore(t,
		memorystore.WithMaxItems(10),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithOnEvict(func(key string, _ []byte, reason memorystore.EvictReason) {
			mu.Lock()
			evicted[key] = reason
			mu.Unlock()
			if reason == memorystore.EvictCapacity {
				capEvictCount.Add(1)
			}
		}),
	)

	for i := range 10 {
		key := fmt.Sprintf("live-%d", i)
		if err := store.Set(key, []byte("payload"), time.Minute); err != nil {
			t.Fatalf("Set(%q) failed: %v", key, err)
		}
	}

	// Setting an existing key with ttl=0 deletes it as EvictExpired without triggering EvictCapacity.
	if err := store.Set("live-0", []byte("zero"), 0); err != nil {
		t.Fatalf("Set(live-0, ttl=0) failed: %v", err)
	}
	// Setting a non-existent key with ttl=0 must not insert or evict any live key.
	if err := store.Set("absent", []byte("zero"), 0); err != nil {
		t.Fatalf("Set(absent, ttl=0) failed: %v", err)
	}

	if _, ok := store.Get("live-0"); ok {
		t.Fatal("live-0 should be deleted after Set(..., 0)")
	}
	if _, ok := store.Get("absent"); ok {
		t.Fatal("absent should not be present after Set(..., 0)")
	}
	for i := 1; i < 10; i++ {
		key := fmt.Sprintf("live-%d", i)
		if _, ok := store.Get(key); !ok {
			t.Fatalf("live key %q was spuriously evicted on ttl=0 Set", key)
		}
	}

	if capEvictCount.Load() != 0 {
		t.Fatalf("unexpected OnEvict(EvictCapacity) count = %d, want 0", capEvictCount.Load())
	}
	if rec.EvictsByReason(memorystore.EvictCapacity) != 0 {
		t.Fatalf("rec.EvictsByReason(EvictCapacity) = %d, want 0", rec.EvictsByReason(memorystore.EvictCapacity))
	}
	if rec.EvictsByReason(memorystore.EvictExpired) < 1 {
		t.Fatalf("rec.EvictsByReason(EvictExpired) = %d, want >= 1", rec.EvictsByReason(memorystore.EvictExpired))
	}

	mu.Lock()
	reason, seen := evicted["live-0"]
	mu.Unlock()
	if !seen || reason != memorystore.EvictExpired {
		t.Fatalf("OnEvict for live-0 = (%v, %v), want EvictExpired", reason, seen)
	}
}

func TestTier3_GzipSnapshot_WriteThroughPeer_NoBroadcast(t *testing.T) {
	t.Parallel()

	src := mustNewStore(t, memorystore.WithPreciseClock())
	for i := range 20 {
		key := fmt.Sprintf("warm-%02d", i)
		val := fmt.Appendf(nil, "value-%02d", i)
		if i%2 == 0 {
			if err := src.Set(key, val, memorystore.NoExpiration); err != nil {
				t.Fatalf("src.Set(%q) failed: %v", key, err)
			}
		} else {
			if err := src.Set(key, val, 10*time.Minute); err != nil {
				t.Fatalf("src.Set(%q) failed: %v", key, err)
			}
		}
	}

	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if err := src.Save(gzw); err != nil {
		t.Fatalf("src.Save(gzip) failed: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzw.Close() failed: %v", err)
	}

	ps := newControllablePubSub()
	t.Cleanup(func() { _ = ps.Close() })

	nodeA := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("snap-nobcast"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithSyncInvalidation(),
	)
	nodeB := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("snap-nobcast"),
		memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
		memorystore.WithSyncInvalidation(),
	)

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return ps.SubscribeCount() >= 2
	}, "nodeA and nodeB should subscribe to invalidation topic")

	pubBefore := ps.PublishCount()

	gzr, err := gzip.NewReader(&gzBuf)
	if err != nil {
		t.Fatalf("gzip.NewReader() failed: %v", err)
	}
	if err := nodeA.Load(gzr); err != nil {
		_ = gzr.Close()
		t.Fatalf("nodeA.Load(gzip) failed: %v", err)
	}
	if err := gzr.Close(); err != nil {
		t.Fatalf("gzr.Close() failed: %v", err)
	}

	if ps.PublishCount() != pubBefore {
		t.Fatalf("Load() unexpectedly broadcast %d invalidation messages", ps.PublishCount()-pubBefore)
	}

	for i := range 20 {
		key := fmt.Sprintf("warm-%02d", i)
		want := fmt.Sprintf("value-%02d", i)
		if got, ok := nodeA.Get(key); !ok || string(got) != want {
			t.Fatalf("nodeA.Get(%q) = (%q, %v), want %q", key, got, ok, want)
		}
		if _, ok := nodeB.Get(key); ok {
			t.Fatalf("nodeB unexpectedly received key %q from nodeA.Load()", key)
		}
	}

	// Subsequent normal Set still broadcasts to peer.
	if err := nodeA.Set("live-write", []byte("broadcasted"), time.Minute); err != nil {
		t.Fatalf("nodeA.Set(live-write) failed: %v", err)
	}
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		v, ok := nodeB.Get("live-write")
		return ok && string(v) == "broadcasted"
	}, "normal Set after Load should propagate to peer")
}

func TestTier3_GetMulti_SetMulti_CopyOnRead_Sliding(t *testing.T) {
	t.Parallel()

	store := mustNewStore(t,
		memorystore.WithCopyOnRead(true),
		memorystore.WithPreciseClock(),
	)

	const ttl = 240 * time.Millisecond
	if err := store.SetMulti(map[string][]byte{
		"fixed-1": []byte("alpha"),
		"fixed-2": []byte("beta"),
	}, ttl); err != nil {
		t.Fatalf("SetMulti() failed: %v", err)
	}
	if err := store.SetWithOptions("sliding-1", []byte("gamma"), ttl, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions(sliding-1) failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	res := store.GetMulti([]string{"fixed-1", "fixed-2", "sliding-1", "absent"})
	if len(res) != 3 {
		t.Fatalf("GetMulti len = %d, want 3", len(res))
	}
	if string(res["fixed-1"]) != "alpha" || string(res["fixed-2"]) != "beta" || string(res["sliding-1"]) != "gamma" {
		t.Fatalf("GetMulti unexpected values: %v", res)
	}
	// Mutate slice returned by GetMulti to verify CopyOnRead isolation.
	res["sliding-1"][0] = 'X'

	// Wait past the original 240ms TTL (100ms + 160ms = 260ms > 240ms, while < 240ms since last read of sliding-1).
	time.Sleep(160 * time.Millisecond)

	if _, ok := store.Get("fixed-1"); ok {
		t.Fatal("fixed-1 should have expired")
	}
	if _, ok := store.Get("fixed-2"); ok {
		t.Fatal("fixed-2 should have expired")
	}
	if got, ok := store.Get("sliding-1"); !ok || string(got) != "gamma" {
		t.Fatalf("sliding-1 should be alive and isolated: (%q, %v)", got, ok)
	}

	// Stop reading sliding-1 for > 240ms TTL so it expires cleanly.
	time.Sleep(300 * time.Millisecond)
	if _, ok := store.Get("sliding-1"); ok {
		t.Fatal("sliding-1 should expire once reads stop")
	}
}

func TestTier3_ConcurrentStop_SWR_AsyncInvalidation_Save(t *testing.T) {
	t.Parallel()

	ps := newControllablePubSub()
	ps.SetPublishDelay(5 * time.Millisecond)
	t.Cleanup(func() { _ = ps.Close() })

	store := mustNewStore(t,
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("concurrent-stop"),
		memorystore.WithAsyncInvalidation(128),
		memorystore.WithDefaultStaleWhileRevalidate(500*time.Millisecond),
		memorystore.WithPreciseClock(),
	)

	for i := range 20 {
		key := fmt.Sprintf("k-%02d", i)
		if err := store.Set(key, []byte("seed"), 10*time.Millisecond); err != nil {
			t.Fatalf("seed Set(%q) failed: %v", key, err)
		}
	}
	time.Sleep(15 * time.Millisecond) // Enter stale window for seeded keys.

	var workers sync.WaitGroup
	stopSignal := make(chan struct{})

	// SWR background-refresh workers.
	for id := range 4 {
		workers.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stopSignal:
					return
				default:
				}
				key := fmt.Sprintf("k-%02d", (id+i)%20)
				_, err := store.GetOrLoadWithOptions(
					context.Background(),
					key,
					time.Second,
					func(ctx context.Context) ([]byte, error) {
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-time.After(15 * time.Millisecond):
							return []byte("refreshed"), nil
						}
					},
					memorystore.WithStaleWhileRevalidate(500*time.Millisecond),
				)
				if errors.Is(err, memorystore.ErrStoreStopped) {
					return
				}
			}
		})
	}

	// Async invalidation write/delete workers.
	for id := range 4 {
		workers.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stopSignal:
					return
				default:
				}
				key := fmt.Sprintf("w-%02d-%02d", id, i%20)
				if err := store.Set(key, []byte("val"), time.Minute); err != nil {
					if errors.Is(err, memorystore.ErrStoreStopped) {
						return
					}
					t.Errorf("unexpected Set error: %v", err)
				}
				if i%3 == 0 {
					if err := store.Delete(key); err != nil {
						if errors.Is(err, memorystore.ErrStoreStopped) {
							return
						}
						t.Errorf("unexpected Delete error: %v", err)
					}
				}
			}
		})
	}

	// Concurrent snapshot Save workers.
	for range 2 {
		workers.Go(func() {
			for {
				select {
				case <-stopSignal:
					return
				default:
				}
				if err := store.Save(io.Discard); err != nil {
					if errors.Is(err, memorystore.ErrStoreStopped) {
						return
					}
					t.Errorf("unexpected Save error: %v", err)
				}
			}
		})
	}

	time.Sleep(30 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		done <- store.Stop()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(stopSignal)
		t.Fatal("Stop() hung while SWR, async invalidation, and Save were active")
	}
	close(stopSignal)
	workers.Wait()

	if !store.IsStopped() {
		t.Fatal("IsStopped() = false after Stop()")
	}
	if err := store.Stop(); err != nil {
		t.Fatalf("second Stop() returned error: %v", err)
	}
	if err := store.Set("after", []byte("v"), time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Set() err = %v, want ErrStoreStopped", err)
	}
	if err := store.Delete("after"); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Delete() err = %v, want ErrStoreStopped", err)
	}
	if err := store.Save(io.Discard); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Save() err = %v, want ErrStoreStopped", err)
	}
	if err := store.Load(bytes.NewReader(nil)); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Load() err = %v, want ErrStoreStopped", err)
	}
	if v, ok := store.Get("k-00"); ok || v != nil {
		t.Fatalf("post-Stop Get() = (%v, %v), want (nil, false)", v, ok)
	}
}

func TestTier3_TinyLFU_Jitter_MaxBytes_SetMulti_OnEvict(t *testing.T) {
	t.Parallel()

	var capacityEvicts atomic.Int64
	rec := newRecordingMetrics()

	const maxBytes = int64(1024)
	store := mustNewStore(t,
		memorystore.WithMaxBytes(maxBytes),
		memorystore.WithEvictionPolicy(memorystore.EvictionTinyLFU),
		memorystore.WithDefaultTTLJitter(0.2),
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithOnEvict(func(_ string, _ []byte, reason memorystore.EvictReason) {
			if reason == memorystore.EvictCapacity {
				capacityEvicts.Add(1)
			}
		}),
	)

	batch := make(map[string][]byte, 40)
	payload := bytes.Repeat([]byte("j"), 64)
	for i := range 40 {
		batch[fmt.Sprintf("tinylfu-%02d", i)] = payload
	}
	if err := store.SetMulti(batch, time.Minute); err != nil {
		t.Fatalf("SetMulti() failed: %v", err)
	}

	m := store.GetMetrics()
	if m.Bytes > maxBytes {
		t.Fatalf("GetMetrics().Bytes = %d, exceeds MaxBytes = %d", m.Bytes, maxBytes)
	}
	if capacityEvicts.Load() == 0 || rec.EvictsByReason(memorystore.EvictCapacity) == 0 {
		t.Fatalf("expected EvictCapacity notifications (onEvict=%d, recorder=%d)",
			capacityEvicts.Load(), rec.EvictsByReason(memorystore.EvictCapacity))
	}
}
