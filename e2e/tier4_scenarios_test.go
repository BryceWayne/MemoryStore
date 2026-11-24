package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

func TestTier4_Scenario_MultiPodWriteThroughCluster(t *testing.T) {
	t.Parallel()

	ps := newControllablePubSub()
	t.Cleanup(func() { _ = ps.Close() })

	const topic = "prod-cluster-v2"
	rec := [3]*recordingMetrics{
		newRecordingMetrics(),
		newRecordingMetrics(),
		newRecordingMetrics(),
	}

	makePod := func(idx int) *memorystore.MemoryStore {
		return mustNewStore(t,
			memorystore.WithPubSub(ps),
			memorystore.WithInvalidationTopic(topic),
			memorystore.WithInvalidationMode(memorystore.InvalidationModeWriteThrough),
			memorystore.WithWriteThroughMaxBytes(512),
			memorystore.WithAsyncInvalidation(256),
			memorystore.WithPubSubTimeout(500*time.Millisecond),
			memorystore.WithMetricsRecorder(rec[idx]),
			memorystore.WithPreciseClock(),
		)
	}

	pod1 := makePod(0)
	pod2 := makePod(1)
	pod3 := makePod(2)

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return ps.SubscribeCount() >= 3
	}, "all 3 pods should subscribe to invalidation topic")

	ctx := context.Background()
	for i, pod := range []*memorystore.MemoryStore{pod1, pod2, pod3} {
		if err := pod.Health(ctx); err != nil {
			t.Fatalf("pod%d initial Health() failed: %v", i+1, err)
		}
	}

	// 1. Small-key WriteThrough propagation with remaining TTL.
	if err := pod1.Set("session:u1", []byte("alice"), 160*time.Millisecond); err != nil {
		t.Fatalf("pod1.Set(session:u1) failed: %v", err)
	}
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		v2, ok2 := pod2.Get("session:u1")
		v3, ok3 := pod3.Get("session:u1")
		return ok2 && ok3 && string(v2) == "alice" && string(v3) == "alice"
	}, "small key should propagate via WriteThrough to pod2 and pod3")

	// Remaining TTL must expire across all pods.
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		_, ok1 := pod1.Get("session:u1")
		_, ok2 := pod2.Get("session:u1")
		_, ok3 := pod3.Get("session:u1")
		return !ok1 && !ok2 && !ok3
	}, "propagated small key should honor remaining TTL across all pods")

	// 2. Large-key fallback to InvalidateOnly (> 512 bytes) + echo suppression on origin.
	if err := pod2.Set("blob:1", []byte("stale-pod2"), time.Minute); err != nil {
		t.Fatalf("pod2.Set(blob:1) failed: %v", err)
	}
	if err := pod3.Set("blob:1", []byte("stale-pod3"), time.Minute); err != nil {
		t.Fatalf("pod3.Set(blob:1) failed: %v", err)
	}
	// Wait deterministically for pod2/pod3 async WriteThrough deliveries to settle before pod1 writes the large blob.
	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return rec[0].InvalidationsReceived() >= 2 &&
			rec[1].InvalidationsReceived() >= 2 &&
			rec[2].InvalidationsReceived() >= 2
	}, "pod2 and pod3 WriteThrough deliveries should settle before largeBlob write")

	largeBlob := bytes.Repeat([]byte("B"), 2048)
	if err := pod1.Set("blob:1", largeBlob, time.Minute); err != nil {
		t.Fatalf("pod1.Set(blob:1) failed: %v", err)
	}
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		_, ok2 := pod2.Get("blob:1")
		_, ok3 := pod3.Get("blob:1")
		return !ok2 && !ok3
	}, "oversized key should fall back to InvalidateOnly and evict stale copies on peers")

	// Echo suppression: pod1 retains its own largeBlob and did not self-delete.
	if got, ok := pod1.Get("blob:1"); !ok || !bytes.Equal(got, largeBlob) {
		t.Fatalf("echo suppression failed on pod1: ok=%v len=%d", ok, len(got))
	}

	// 3. Delete cross-pod invalidation.
	if err := pod1.Set("user:42", []byte("bob"), time.Minute); err != nil {
		t.Fatalf("pod1.Set(user:42) failed: %v", err)
	}
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		v2, ok2 := pod2.Get("user:42")
		v3, ok3 := pod3.Get("user:42")
		return ok2 && ok3 && string(v2) == "bob" && string(v3) == "bob"
	}, "user:42 should propagate to pod2 and pod3")

	if err := pod2.Delete("user:42"); err != nil {
		t.Fatalf("pod2.Delete(user:42) failed: %v", err)
	}
	eventually(t, time.Second, 10*time.Millisecond, func() bool {
		_, ok1 := pod1.Get("user:42")
		_, ok2 := pod2.Get("user:42")
		_, ok3 := pod3.Get("user:42")
		return !ok1 && !ok2 && !ok3
	}, "Delete on pod2 should invalidate user:42 across all pods")

	// 4. Transient broker disconnect + health check + reconnect recovery.
	ps.SetPingError(errors.New("broker ping timeout"))
	for i, pod := range []*memorystore.MemoryStore{pod1, pod2, pod3} {
		if err := pod.Health(ctx); err == nil {
			t.Fatalf("pod%d.Health() should fail while Ping returns error", i+1)
		}
	}
	ps.SetPingError(nil)
	for i, pod := range []*memorystore.MemoryStore{pod1, pod2, pod3} {
		if err := pod.Health(ctx); err != nil {
			t.Fatalf("pod%d.Health() should recover after Ping clears: %v", i+1, err)
		}
	}

	subsBefore := ps.SubscribeCount()
	ps.CloseTopicChannels(topic)
	eventually(t, 3*time.Second, 20*time.Millisecond, func() bool {
		return ps.SubscribeCount() >= subsBefore+3
	}, "all 3 pods should resubscribe after transient topic channel close")

	eventually(t, 3*time.Second, 30*time.Millisecond, func() bool {
		if err := pod3.Set("post-reconnect", []byte("recovered"), time.Minute); err != nil {
			return false
		}
		v1, ok1 := pod1.Get("post-reconnect")
		v2, ok2 := pod2.Get("post-reconnect")
		return ok1 && ok2 && string(v1) == "recovered" && string(v2) == "recovered"
	}, "WriteThrough should work across cluster after backplane reconnect")

	if rec[0].InvalidationsSent() == 0 || rec[1].InvalidationsSent() == 0 || rec[2].InvalidationsSent() == 0 {
		t.Fatalf("expected invalidations recorded on all pods: got [%d, %d, %d]",
			rec[0].InvalidationsSent(), rec[1].InvalidationsSent(), rec[2].InvalidationsSent())
	}
}

func TestTier4_Scenario_RollingWarmRestart(t *testing.T) {
	t.Parallel()

	podA := mustNewStore(t,
		memorystore.WithMaxItems(500),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithPreciseClock(),
	)

	if err := podA.SetWithOptions("cfg:flags", []byte(`{"v2":true}`), 10*time.Millisecond, memorystore.WithNoExpiration()); err != nil {
		t.Fatalf("podA.SetWithOptions(cfg:flags) failed: %v", err)
	}
	if err := podA.SetWithOptions("sess:alice", []byte("tok-alice"), 500*time.Millisecond, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("podA.SetWithOptions(sess:alice) failed: %v", err)
	}
	if err := podA.Set("order:100", []byte("pending"), 250*time.Millisecond); err != nil {
		t.Fatalf("podA.Set(order:100) failed: %v", err)
	}
	if err := podA.Set("ephemeral:dead", []byte("stale"), 5*time.Millisecond); err != nil {
		t.Fatalf("podA.Set(ephemeral:dead) failed: %v", err)
	}

	// Ensure ephemeral:dead has expired before taking snapshot.
	eventually(t, 100*time.Millisecond, 2*time.Millisecond, func() bool {
		_, ok := podA.Get("ephemeral:dead")
		return !ok
	}, "ephemeral:dead should expire before Save")

	// Save Binary+Gzip snapshot and JSON snapshot before Pod A shuts down.
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if err := podA.Save(gzw); err != nil {
		t.Fatalf("podA.Save(binary+gzip) failed: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzw.Close() failed: %v", err)
	}

	var jsonBuf bytes.Buffer
	if err := podA.Save(&jsonBuf, memorystore.WithSnapshotFormat(memorystore.SnapshotJSON)); err != nil {
		t.Fatalf("podA.Save(SnapshotJSON) failed: %v", err)
	}

	if err := podA.Stop(); err != nil {
		t.Fatalf("podA.Stop() failed: %v", err)
	}

	// Warm restart into Pod B (bounded EvictionS3FIFO) from Binary+Gzip.
	podB := mustNewStore(t,
		memorystore.WithMaxItems(500),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithPreciseClock(),
	)
	gzr, err := gzip.NewReader(&gzBuf)
	if err != nil {
		t.Fatalf("gzip.NewReader() failed: %v", err)
	}
	if err := podB.Load(gzr); err != nil {
		_ = gzr.Close()
		t.Fatalf("podB.Load(binary+gzip) failed: %v", err)
	}
	if err := gzr.Close(); err != nil {
		t.Fatalf("gzr.Close() failed: %v", err)
	}

	if _, ok := podB.Get("ephemeral:dead"); ok {
		t.Fatal("podB should not load already-expired key")
	}
	if got, ok := podB.Get("cfg:flags"); !ok || string(got) != `{"v2":true}` {
		t.Fatalf("podB.Get(cfg:flags) = (%q, %v), want permanent value", got, ok)
	}
	if got, ok := podB.Get("sess:alice"); !ok || string(got) != "tok-alice" {
		t.Fatalf("podB.Get(sess:alice) = (%q, %v), want %q", got, ok, "tok-alice")
	}
	if got, ok := podB.Get("order:100"); !ok || string(got) != "pending" {
		t.Fatalf("podB.Get(order:100) = (%q, %v), want %q", got, ok, "pending")
	}

	// Verify order:100 retains finite remaining TTL and expires, while cfg:flags survives.
	eventually(t, 600*time.Millisecond, 10*time.Millisecond, func() bool {
		_, ok := podB.Get("order:100")
		return !ok
	}, "order:100 should expire on podB according to preserved remaining TTL")

	if got, ok := podB.Get("cfg:flags"); !ok || string(got) != `{"v2":true}` {
		t.Fatalf("cfg:flags should survive indefinitely after finite TTLs expire: (%q, %v)", got, ok)
	}

	// Verify JSON snapshot Load variant into Pod C.
	podC := mustNewStore(t,
		memorystore.WithMaxItems(500),
		memorystore.WithEvictionPolicy(memorystore.EvictionS3FIFO),
		memorystore.WithPreciseClock(),
	)
	if err := podC.Load(&jsonBuf, memorystore.WithSnapshotFormat(memorystore.SnapshotJSON)); err != nil {
		t.Fatalf("podC.Load(SnapshotJSON) failed: %v", err)
	}
	if _, ok := podC.Get("ephemeral:dead"); ok {
		t.Fatal("podC should not load already-expired key from JSON snapshot")
	}
	if got, ok := podC.Get("cfg:flags"); !ok || string(got) != `{"v2":true}` {
		t.Fatalf("podC.Get(cfg:flags) = (%q, %v), want permanent value", got, ok)
	}
	if got, ok := podC.Get("sess:alice"); !ok || string(got) != "tok-alice" {
		t.Fatalf("podC.Get(sess:alice) = (%q, %v), want %q", got, ok, "tok-alice")
	}
}

func TestTier4_Scenario_ThunderingHerdSWRService(t *testing.T) {
	t.Parallel()

	type apiResp struct {
		Seq  int    `json:"seq"`
		Body string `json:"body"`
	}

	rec := newRecordingMetrics()
	store := mustNewStore(t,
		memorystore.WithDefaultStaleWhileRevalidate(250*time.Millisecond),
		memorystore.WithMetricsRecorder(rec),
		memorystore.WithPreciseClock(),
	)

	ctx := context.Background()
	const callers = 32
	const softTTL = 50 * time.Millisecond
	const swrGrace = 250 * time.Millisecond

	var upstreamCalls atomic.Int32
	var upstreamFail atomic.Bool
	var upstreamSeq atomic.Int32
	upstreamSeq.Store(1)

	loader := func(_ context.Context) (apiResp, error) {
		upstreamCalls.Add(1)
		time.Sleep(15 * time.Millisecond)
		if upstreamFail.Load() {
			return apiResp{}, errors.New("upstream 503 unavailable")
		}
		seq := int(upstreamSeq.Load())
		return apiResp{Seq: seq, Body: fmt.Sprintf("payload-%d", seq)}, nil
	}

	callStorm := func(wantMinSeq, wantMaxSeq int) []apiResp {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, callers)
		resps := make(chan apiResp, callers)
		start := make(chan struct{})

		for range callers {
			wg.Go(func() {
				<-start
				r, err := store.GetOrLoadJSONWithOptions[apiResp](
					ctx,
					"gateway:catalog",
					softTTL,
					loader,
					[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(swrGrace)},
				)
				errs <- err
				resps <- r
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		close(resps)

		for err := range errs {
			if err != nil {
				t.Fatalf("caller received unexpected error: %v", err)
			}
		}
		out := make([]apiResp, 0, callers)
		for r := range resps {
			if r.Seq < wantMinSeq || r.Seq > wantMaxSeq {
				t.Fatalf("unexpected resp Seq=%d (want [%d..%d])", r.Seq, wantMinSeq, wantMaxSeq)
			}
			out = append(out, r)
		}
		return out
	}

	// Phase 1: Cold request storm -> 0 errors, coalesced to 1 upstream call.
	callStorm(1, 1)
	if upstreamCalls.Load() != 1 {
		t.Fatalf("cold storm upstream calls = %d, want 1", upstreamCalls.Load())
	}

	// Phase 2: Fresh hits -> 0 additional upstream calls.
	callStorm(1, 1)
	if upstreamCalls.Load() != 1 {
		t.Fatalf("fresh phase upstream calls = %d, want 1", upstreamCalls.Load())
	}

	// Phase 3: Stale grace with simulated upstream transient error -> 0 caller errors, stale Seq=1 served.
	eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
		_, ok := store.Get("gateway:catalog")
		return !ok
	}, "catalog entry should enter stale window")

	upstreamFail.Store(true)
	callStorm(1, 1)

	eventually(t, 500*time.Millisecond, 5*time.Millisecond, func() bool {
		return rec.FailedLoads() >= 1
	}, "failed background SWR refresh should be recorded")

	// Phase 4: Upstream recovers -> stale hit served immediately, background refresh heals to Seq=2.
	upstreamFail.Store(false)
	upstreamSeq.Store(2)
	_, err := store.GetOrLoadJSONWithOptions[apiResp](
		ctx,
		"gateway:catalog",
		softTTL,
		loader,
		[]memorystore.LoadOption{memorystore.WithStaleWhileRevalidate(swrGrace)},
	)
	if err != nil {
		t.Fatalf("recovery SWR call failed: %v", err)
	}

	eventually(t, time.Second, 5*time.Millisecond, func() bool {
		dto, ok, err := store.GetJSONAs[apiResp]("gateway:catalog")
		return err == nil && ok && dto.Seq == 2
	}, "background SWR refresh should heal entry to Seq=2")

	// Phase 5: Hard expiration past softTTL + swrGrace -> synchronous coalesced reload to Seq=3.
	upstreamSeq.Store(3)
	time.Sleep(softTTL + swrGrace + 40*time.Millisecond)
	callsBeforeHard := upstreamCalls.Load()
	callStorm(3, 3)
	if upstreamCalls.Load()-callsBeforeHard != 1 {
		t.Fatalf("post-hard-expiry storm upstream calls = %d, want 1", upstreamCalls.Load()-callsBeforeHard)
	}
}

func TestTier4_Scenario_BoundedZipfianSessionCache(t *testing.T) {
	t.Parallel()

	const (
		maxItems    = 128
		maxBytes    = int64(32 * 1024)
		nHotKeys    = 1000
		nSamples    = 3500
		nScanBurst  = 450
		payloadSize = 96
	)

	warmKeys := zipfKeys(101, 0.99, nHotKeys, nSamples)
	probeKeys := zipfKeys(202, 0.99, nHotKeys, nSamples)
	sessionVal := bytes.Repeat([]byte("S"), payloadSize)

	runWorkload := func(policy memorystore.EvictionPolicy) float64 {
		rec := newRecordingMetrics()
		store := mustNewStore(t,
			memorystore.WithMaxItems(maxItems),
			memorystore.WithMaxBytes(maxBytes),
			memorystore.WithEvictionPolicy(policy),
			memorystore.WithCopyOnRead(true),
			memorystore.WithMetricsRecorder(rec),
		)

		ctx := context.Background()
		if err := store.Health(ctx); err != nil {
			t.Fatalf("policy %v Health() failed: %v", policy, err)
		}

		// Warm Zipfian working set.
		for _, k := range warmKeys {
			if got, ok := store.Get(k); ok {
				// Verify CopyOnRead isolation on retrieved session bytes.
				if len(got) > 0 {
					got[0] ^= 0xff
				}
				continue
			}
			if err := store.Set(k, sessionVal, 5*time.Minute); err != nil {
				t.Fatalf("Set(%q) failed: %v", k, err)
			}
		}

		// Inject one-hit-wonder scan burst that exceeds cache capacity.
		for i := range nScanBurst {
			scanKey := fmt.Sprintf("scan-%04d", i)
			if err := store.Set(scanKey, sessionVal, 5*time.Minute); err != nil {
				t.Fatalf("scan Set(%q) failed: %v", scanKey, err)
			}
		}

		// Measure Zipfian probe hits after scan burst.
		var probeHits int
		for _, k := range probeKeys {
			if got, ok := store.Get(k); ok {
				if !bytes.Equal(got, sessionVal) {
					t.Fatalf("corrupted session value for %q", k)
				}
				probeHits++
			}
		}

		// Verify O(1) GetMetrics() consistency with bounds and MetricsRecorder.
		m := store.GetMetrics()
		if m.Items > maxItems {
			t.Fatalf("policy %v Items = %d, exceeds MaxItems = %d", policy, m.Items, maxItems)
		}
		if m.Bytes > maxBytes {
			t.Fatalf("policy %v Bytes = %d, exceeds MaxBytes = %d", policy, m.Bytes, maxBytes)
		}
		if m.Hits != rec.Hits() || m.Misses != rec.Misses() || m.Evictions != rec.Evicts() {
			t.Fatalf("policy %v GetMetrics (%d/%d/%d) != MetricsRecorder (%d/%d/%d)",
				policy, m.Hits, m.Misses, m.Evictions, rec.Hits(), rec.Misses(), rec.Evicts())
		}
		if err := store.Health(ctx); err != nil {
			t.Fatalf("policy %v post-workload Health() failed: %v", policy, err)
		}

		return float64(probeHits) / float64(len(probeKeys))
	}

	volatileRatio := runWorkload(memorystore.EvictionVolatileTTL)
	s3fifoRatio := runWorkload(memorystore.EvictionS3FIFO)
	tinylfuRatio := runWorkload(memorystore.EvictionTinyLFU)

	if s3fifoRatio <= volatileRatio+0.10 {
		t.Fatalf("expected S3FIFO hit ratio (%.3f) to beat VolatileTTL (%.3f) by > 0.10 under scan burst",
			s3fifoRatio, volatileRatio)
	}
	if tinylfuRatio <= volatileRatio+0.10 {
		t.Fatalf("expected TinyLFU hit ratio (%.3f) to beat VolatileTTL (%.3f) by > 0.10 under scan burst",
			tinylfuRatio, volatileRatio)
	}
}

func TestTier4_Scenario_GracefulShutdownUnderTraffic(t *testing.T) {
	t.Parallel()

	ps := newControllablePubSub()
	t.Cleanup(func() { _ = ps.Close() })

	store := mustNewStore(t,
		memorystore.WithMaxItems(500),
		memorystore.WithPubSub(ps),
		memorystore.WithInvalidationTopic("graceful-shutdown"),
		memorystore.WithAsyncInvalidation(128),
		memorystore.WithDefaultStaleWhileRevalidate(200*time.Millisecond),
		memorystore.WithPreciseClock(),
	)

	for i := range 50 {
		key := fmt.Sprintf("seed-%02d", i)
		if err := store.Set(key, []byte("initial"), 25*time.Millisecond); err != nil {
			t.Fatalf("seed Set(%q) failed: %v", key, err)
		}
	}

	ctx := context.Background()
	var unexpectedErrs atomic.Int32
	var wg sync.WaitGroup
	stopWorkers := make(chan struct{})

	recordErr := func(err error) {
		if err != nil && !errors.Is(err, memorystore.ErrStoreStopped) {
			unexpectedErrs.Add(1)
		}
	}

	for id := range 16 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stopWorkers:
					return
				default:
				}
				key := fmt.Sprintf("seed-%02d", (id+i)%50)
				switch i % 6 {
				case 0:
					recordErr(store.Set(key, []byte("updated"), 100*time.Millisecond))
				case 1:
					_, _ = store.Get(key)
				case 2:
					_, err := store.GetOrLoad(ctx, key, 100*time.Millisecond, func(loadCtx context.Context) ([]byte, error) {
						select {
						case <-loadCtx.Done():
							return nil, loadCtx.Err()
						case <-time.After(5 * time.Millisecond):
							return []byte("loaded"), nil
						}
					})
					if err != nil && !errors.Is(err, memorystore.ErrStoreStopped) && !errors.Is(err, context.Canceled) {
						unexpectedErrs.Add(1)
					}
				case 3:
					recordErr(store.Delete(key))
				case 4:
					recordErr(store.Save(io.Discard))
				case 5:
					_ = store.GetMetrics()
				}
			}
		})
	}

	time.Sleep(45 * time.Millisecond)

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- store.Stop()
	}()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(stopWorkers)
		t.Fatal("Stop() hung under concurrent mixed traffic")
	}

	// Let workers observe ErrStoreStopped briefly, then terminate loop.
	time.Sleep(15 * time.Millisecond)
	close(stopWorkers)
	wg.Wait()

	if unexpectedErrs.Load() != 0 {
		t.Fatalf("encountered %d unexpected non-ErrStoreStopped errors during shutdown", unexpectedErrs.Load())
	}
	if !store.IsStopped() {
		t.Fatal("IsStopped() = false after Stop()")
	}
	if err := store.Health(ctx); err == nil {
		t.Fatal("expected Health() to report unhealthy after Stop()")
	}
	if v, ok := store.Get("seed-00"); ok || v != nil {
		t.Fatalf("post-Stop Get() = (%v, %v), want (nil, false)", v, ok)
	}
	if err := store.Set("post", []byte("v"), time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Set() = %v, want ErrStoreStopped", err)
	}
	if err := store.SetWithOptions("post", []byte("v"), time.Minute, memorystore.WithNoExpiration()); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop SetWithOptions() = %v, want ErrStoreStopped", err)
	}
	if err := store.SetMulti(map[string][]byte{"post": []byte("v")}, time.Minute); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop SetMulti() = %v, want ErrStoreStopped", err)
	}
	if err := store.Delete("post"); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Delete() = %v, want ErrStoreStopped", err)
	}
	if _, err := store.GetOrLoad(ctx, "post", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("never"), nil
	}); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop GetOrLoad() = %v, want ErrStoreStopped", err)
	}
	if err := store.Save(io.Discard); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Save() = %v, want ErrStoreStopped", err)
	}
	if err := store.Load(bytes.NewReader(nil)); !errors.Is(err, memorystore.ErrStoreStopped) {
		t.Fatalf("post-Stop Load() = %v, want ErrStoreStopped", err)
	}

	m := store.GetMetrics()
	if m.Items != 0 || m.Bytes != 0 {
		t.Fatalf("post-Stop GetMetrics() Items=%d Bytes=%d, want 0/0", m.Items, m.Bytes)
	}
}

func TestTier4_Scenario_MultiTenantAuthTokenStore(t *testing.T) {
	t.Parallel()

	// Exercise default coarse clock alongside sub-10ms preciseTTL fallback,
	// sliding sessions, jittered refresh tokens, and permanent NoExpiration keys.
	store := mustNewStore(t)

	// 1. Permanent service API keys via NoExpiration.
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		key := fmt.Sprintf("apikey:%s", tenant)
		if err := store.SetWithOptions(key, []byte("svc-secret-"+tenant), 10*time.Millisecond, memorystore.WithNoExpiration()); err != nil {
			t.Fatalf("SetWithOptions(%q) failed: %v", key, err)
		}
	}

	// 2. Sliding user sessions (160ms TTL, refreshed on reads).
	if err := store.SetWithOptions("session:tenant-a:u1", []byte("active-session"), 160*time.Millisecond, memorystore.WithSlidingExpiration()); err != nil {
		t.Fatalf("SetWithOptions(session) failed: %v", err)
	}

	// 3. Jittered refresh tokens (100ms base TTL, +/- 25% => [75ms, 125ms]).
	const refreshTokens = 16
	for i := range refreshTokens {
		key := fmt.Sprintf("refresh:tenant-b:%02d", i)
		if err := store.SetWithOptions(key, []byte("rtok"), 100*time.Millisecond, memorystore.WithTTLJitter(0.25)); err != nil {
			t.Fatalf("SetWithOptions(%q) failed: %v", key, err)
		}
	}

	// 4. Sub-10ms ephemeral one-time nonces (6ms TTL -> automatic preciseTTL fallback).
	for i := range 5 {
		key := fmt.Sprintf("nonce:tenant-a:%d", i)
		if err := store.Set(key, []byte("otp"), 6*time.Millisecond); err != nil {
			t.Fatalf("Set(%q) failed: %v", key, err)
		}
	}

	// Ephemeral sub-10ms nonces must expire promptly.
	eventually(t, 250*time.Millisecond, 2*time.Millisecond, func() bool {
		for i := range 5 {
			if _, ok := store.Get(fmt.Sprintf("nonce:tenant-a:%d", i)); ok {
				return false
			}
		}
		return true
	}, "sub-10ms ephemeral nonces should expire promptly under coarse clock fallback")

	// Keep sliding session alive past its initial 160ms TTL (up to ~220ms) via periodic reads.
	for range 4 {
		time.Sleep(55 * time.Millisecond)
		if got, ok := store.Get("session:tenant-a:u1"); !ok || string(got) != "active-session" {
			t.Fatalf("sliding user session expired prematurely while active: (%q, %v)", got, ok)
		}
	}

	// Eventually all jittered refresh tokens (max 125ms) expire without further touches.
	eventually(t, 300*time.Millisecond, 10*time.Millisecond, func() bool {
		for i := range refreshTokens {
			if _, ok := store.Get(fmt.Sprintf("refresh:tenant-b:%02d", i)); ok {
				return false
			}
		}
		return true
	}, "all jittered refresh tokens should expire past max jittered TTL")

	// Stop touching sliding session for > 160ms TTL; verify it expires cleanly too.
	time.Sleep(220 * time.Millisecond)
	if _, ok := store.Get("session:tenant-a:u1"); ok {
		t.Fatal("idle sliding session should expire")
	}

	// Permanent service API keys must still be present and intact.
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		key := fmt.Sprintf("apikey:%s", tenant)
		want := "svc-secret-" + tenant
		if got, ok := store.Get(key); !ok || string(got) != want {
			t.Fatalf("permanent API key %q = (%q, %v), want %q", key, got, ok, want)
		}
	}
}
