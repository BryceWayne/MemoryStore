package memorystore

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal(msg)
	}
}

func TestWriteThrough_UniformCluster_ZeroStaleWindowOptOut(t *testing.T) {
	broker := NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	newNode := func(name string) *MemoryStore {
		ms, err := New(
			WithPreciseClock(),
			WithPubSub(broker),
			WithInvalidationTopic("wt:swr:uniform"),
			WithInvalidationMode(InvalidationModeWriteThrough),
			WithSyncInvalidation(),
			WithDefaultStaleWhileRevalidate(300*time.Millisecond),
		)
		if err != nil {
			t.Fatalf("New(%s) error = %v", name, err)
		}
		return ms
	}

	nodeA := newNode("nodeA")
	defer func() { _ = nodeA.Stop() }()
	nodeB := newNode("nodeB")
	defer func() { _ = nodeB.Stop() }()

	t.Run("SetWithOptions positive TTL WithStaleWindow(0)", func(t *testing.T) {
		if err := nodeA.SetWithOptions("strict-set", []byte("old-secret"), 25*time.Millisecond, WithStaleWindow(0)); err != nil {
			t.Fatalf("nodeA.SetWithOptions() error = %v", err)
		}
		waitForCondition(t, time.Second, func() bool {
			v, ok := nodeB.Get("strict-set")
			return ok && string(v) == "old-secret"
		}, "nodeB did not receive strict-set via WriteThrough")

		if e := nodeB.lookupEntry("strict-set"); e == nil || e.staleGrace != 0 {
			t.Fatalf("nodeB strict-set staleGrace = %v, want 0", e)
		}

		time.Sleep(40 * time.Millisecond)

		var loads atomic.Int32
		got, err := nodeB.GetOrLoad(t.Context(), "strict-set", time.Minute, func(_ context.Context) ([]byte, error) {
			loads.Add(1)
			return []byte("fresh-secret"), nil
		})
		if err != nil {
			t.Fatalf("nodeB.GetOrLoad() error = %v", err)
		}
		if string(got) != "fresh-secret" || loads.Load() != 1 {
			t.Fatalf("nodeB.GetOrLoad() = (%q, loads=%d), want (\"fresh-secret\", loads=1)", string(got), loads.Load())
		}
	})

	t.Run("GetOrLoadWithOptions WithStaleWhileRevalidate(0)", func(t *testing.T) {
		val, err := nodeA.GetOrLoadWithOptions(t.Context(), "strict-load", 25*time.Millisecond, func(_ context.Context) ([]byte, error) {
			return []byte("old-loaded"), nil
		}, WithStaleWhileRevalidate(0))
		if err != nil || string(val) != "old-loaded" {
			t.Fatalf("nodeA.GetOrLoadWithOptions() = (%q, %v)", string(val), err)
		}
		waitForCondition(t, time.Second, func() bool {
			v, ok := nodeB.Get("strict-load")
			return ok && string(v) == "old-loaded"
		}, "nodeB did not receive strict-load via WriteThrough")

		if e := nodeB.lookupEntry("strict-load"); e == nil || e.staleGrace != 0 {
			t.Fatalf("nodeB strict-load staleGrace = %v, want 0", e)
		}

		time.Sleep(40 * time.Millisecond)

		var loads atomic.Int32
		got, err := nodeB.GetOrLoad(t.Context(), "strict-load", time.Minute, func(_ context.Context) ([]byte, error) {
			loads.Add(1)
			return []byte("fresh-loaded"), nil
		})
		if err != nil {
			t.Fatalf("nodeB.GetOrLoad() error = %v", err)
		}
		if string(got) != "fresh-loaded" || loads.Load() != 1 {
			t.Fatalf("nodeB.GetOrLoad() = (%q, loads=%d), want (\"fresh-loaded\", loads=1)", string(got), loads.Load())
		}
	})

	t.Run("GetOrLoadJSONWithOptions WithStaleWhileRevalidate(0)", func(t *testing.T) {
		type doc struct {
			Version int `json:"v"`
		}
		initDoc, err := nodeA.GetOrLoadJSONWithOptions(t.Context(), "strict-json", 25*time.Millisecond, func(_ context.Context) (doc, error) {
			return doc{Version: 1}, nil
		}, []LoadOption{WithStaleWhileRevalidate(0)})
		if err != nil || initDoc.Version != 1 {
			t.Fatalf("nodeA.GetOrLoadJSONWithOptions() = (%+v, %v)", initDoc, err)
		}
		waitForCondition(t, time.Second, func() bool {
			_, ok := nodeB.Get("strict-json")
			return ok
		}, "nodeB did not receive strict-json via WriteThrough")

		if e := nodeB.lookupEntry("strict-json"); e == nil || e.staleGrace != 0 {
			t.Fatalf("nodeB strict-json staleGrace = %v, want 0", e)
		}

		time.Sleep(40 * time.Millisecond)

		var loads atomic.Int32
		got, err := nodeB.GetOrLoadJSON[doc](t.Context(), "strict-json", time.Minute, func(_ context.Context) (doc, error) {
			loads.Add(1)
			return doc{Version: 2}, nil
		})
		if err != nil {
			t.Fatalf("nodeB.GetOrLoadJSON() error = %v", err)
		}
		if got.Version != 2 || loads.Load() != 1 {
			t.Fatalf("nodeB.GetOrLoadJSON() = (%+v, loads=%d), want (Version=2, loads=1)", got, loads.Load())
		}
	})

	t.Run("NoExpiration and overwrite with WithStaleWindow(0)", func(t *testing.T) {
		if err := nodeA.SetWithOptions("perm-key", []byte("perm-v1"), 20*time.Millisecond, WithNoExpiration(), WithStaleWindow(0)); err != nil {
			t.Fatalf("nodeA.SetWithOptions(NoExpiration) error = %v", err)
		}
		waitForCondition(t, time.Second, func() bool {
			v, ok := nodeB.Get("perm-key")
			return ok && string(v) == "perm-v1"
		}, "nodeB did not receive perm-key via WriteThrough")

		if e := nodeB.lookupEntry("perm-key"); e == nil || !e.isNoExpiration() || e.staleGrace != 0 {
			t.Fatalf("nodeB perm-key metadata mismatch: %+v", e)
		}

		time.Sleep(35 * time.Millisecond)
		if v, ok := nodeB.Get("perm-key"); !ok || string(v) != "perm-v1" {
			t.Fatalf("nodeB perm-key expired unexpectedly: (%q, %v)", string(v), ok)
		}

		if err := nodeA.SetWithOptions("perm-key", []byte("expiring-v2"), 25*time.Millisecond, WithStaleWindow(0)); err != nil {
			t.Fatalf("nodeA.SetWithOptions(overwrite) error = %v", err)
		}
		waitForCondition(t, time.Second, func() bool {
			v, ok := nodeB.Get("perm-key")
			return ok && string(v) == "expiring-v2"
		}, "nodeB did not receive perm-key overwrite via WriteThrough")

		if e := nodeB.lookupEntry("perm-key"); e == nil || e.isNoExpiration() || e.staleGrace != 0 {
			t.Fatalf("nodeB perm-key overwrite metadata mismatch: %+v", e)
		}

		time.Sleep(40 * time.Millisecond)

		var loads atomic.Int32
		got, err := nodeB.GetOrLoad(t.Context(), "perm-key", time.Minute, func(_ context.Context) ([]byte, error) {
			loads.Add(1)
			return []byte("reloaded-v3"), nil
		})
		if err != nil {
			t.Fatalf("nodeB.GetOrLoad() error = %v", err)
		}
		if string(got) != "reloaded-v3" || loads.Load() != 1 {
			t.Fatalf("nodeB.GetOrLoad() = (%q, loads=%d), want (\"reloaded-v3\", loads=1)", string(got), loads.Load())
		}
	})

	t.Run("default SWR still applies when not opted out", func(t *testing.T) {
		if err := nodeA.Set("default-swr", []byte("stale-v1"), 25*time.Millisecond); err != nil {
			t.Fatalf("nodeA.Set() error = %v", err)
		}
		waitForCondition(t, time.Second, func() bool {
			v, ok := nodeB.Get("default-swr")
			return ok && string(v) == "stale-v1"
		}, "nodeB did not receive default-swr via WriteThrough")

		if e := nodeB.lookupEntry("default-swr"); e == nil || e.staleGrace != int64(300*time.Millisecond) {
			t.Fatalf("nodeB default-swr staleGrace = %v, want 300ms", e)
		}

		time.Sleep(40 * time.Millisecond)

		var loads atomic.Int32
		got, err := nodeB.GetOrLoad(t.Context(), "default-swr", time.Minute, func(_ context.Context) ([]byte, error) {
			loads.Add(1)
			return []byte("fresh-v2"), nil
		})
		if err != nil {
			t.Fatalf("nodeB.GetOrLoad() error = %v", err)
		}
		if string(got) != "stale-v1" {
			t.Fatalf("nodeB.GetOrLoad() = %q, want stale %q", string(got), "stale-v1")
		}
		waitForCondition(t, time.Second, func() bool {
			v, ok := nodeB.Get("default-swr")
			return ok && string(v) == "fresh-v2" && loads.Load() == 1
		}, "nodeB background SWR refresh did not complete")
	})
}

func TestWriteThrough_HeterogeneousCluster_StaleGraceAndSetMulti(t *testing.T) {
	broker := NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	nodeA, err := New(
		WithPreciseClock(),
		WithPubSub(broker),
		WithInvalidationTopic("wt:swr:hetero"),
		WithInvalidationMode(InvalidationModeWriteThrough),
		WithSyncInvalidation(),
	)
	if err != nil {
		t.Fatalf("New(nodeA) error = %v", err)
	}
	defer func() { _ = nodeA.Stop() }()

	nodeB, err := New(
		WithPreciseClock(),
		WithPubSub(broker),
		WithInvalidationTopic("wt:swr:hetero"),
		WithInvalidationMode(InvalidationModeWriteThrough),
		WithSyncInvalidation(),
		WithDefaultStaleWhileRevalidate(300*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New(nodeB) error = %v", err)
	}
	defer func() { _ = nodeB.Stop() }()

	// 1. nodeA (default=0) Set -> nodeB (default=300ms) must preserve staleGrace=0.
	if err := nodeA.Set("a-default", []byte("v-a"), 25*time.Millisecond); err != nil {
		t.Fatalf("nodeA.Set() error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		v, ok := nodeB.Get("a-default")
		return ok && string(v) == "v-a"
	}, "nodeB did not receive a-default")
	if e := nodeB.lookupEntry("a-default"); e == nil || e.staleGrace != 0 {
		t.Fatalf("nodeB a-default staleGrace = %v, want 0", e)
	}
	time.Sleep(40 * time.Millisecond)
	var loadsA atomic.Int32
	gotA, err := nodeB.GetOrLoad(t.Context(), "a-default", time.Minute, func(_ context.Context) ([]byte, error) {
		loadsA.Add(1)
		return []byte("v-a-reloaded"), nil
	})
	if err != nil || string(gotA) != "v-a-reloaded" || loadsA.Load() != 1 {
		t.Fatalf("nodeB.GetOrLoad(a-default) = (%q, loads=%d, err=%v), want reloaded", string(gotA), loadsA.Load(), err)
	}

	// 2. nodeA positive WithStaleWindow(w > 0) replicates w accurately to nodeB.
	if err := nodeA.SetWithOptions("a-pos", []byte("v-apos"), 25*time.Millisecond, WithStaleWindow(250*time.Millisecond)); err != nil {
		t.Fatalf("nodeA.SetWithOptions(250ms) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		v, ok := nodeB.Get("a-pos")
		return ok && string(v) == "v-apos"
	}, "nodeB did not receive a-pos")
	if e := nodeB.lookupEntry("a-pos"); e == nil || e.staleGrace != int64(250*time.Millisecond) {
		t.Fatalf("nodeB a-pos staleGrace = %v, want 250ms", e)
	}
	time.Sleep(40 * time.Millisecond)
	gotPos, err := nodeB.GetOrLoad(t.Context(), "a-pos", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("v-apos-fresh"), nil
	})
	if err != nil || string(gotPos) != "v-apos" {
		t.Fatalf("nodeB.GetOrLoad(a-pos) = (%q, %v), want stale %q", string(gotPos), err, "v-apos")
	}
	waitForCondition(t, time.Second, func() bool {
		v, ok := nodeB.Get("a-pos")
		return ok && string(v) == "v-apos-fresh"
	}, "nodeB a-pos background refresh did not finish")

	// 3. nodeB (default=300ms) Set -> nodeA (default=0) preserves 300ms staleGrace.
	if err := nodeB.Set("b-default", []byte("v-b"), 25*time.Millisecond); err != nil {
		t.Fatalf("nodeB.Set() error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		v, ok := nodeA.Get("b-default")
		return ok && string(v) == "v-b"
	}, "nodeA did not receive b-default")
	if e := nodeA.lookupEntry("b-default"); e == nil || e.staleGrace != int64(300*time.Millisecond) {
		t.Fatalf("nodeA b-default staleGrace = %v, want 300ms", e)
	}
	time.Sleep(40 * time.Millisecond)
	gotB, err := nodeA.GetOrLoad(t.Context(), "b-default", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("v-b-fresh"), nil
	})
	if err != nil || string(gotB) != "v-b" {
		t.Fatalf("nodeA.GetOrLoad(b-default) = (%q, %v), want stale %q", string(gotB), err, "v-b")
	}
	waitForCondition(t, time.Second, func() bool {
		v, ok := nodeA.Get("b-default")
		return ok && string(v) == "v-b-fresh"
	}, "nodeA b-default background refresh did not finish")

	// 4. SetMulti (both small <= 4 and large > 4 batches) preserves writer staleGrace on peer.
	batchA := make(map[string][]byte, 6)
	for i := range 6 {
		batchA[fmt.Sprintf("sma-%d", i)] = []byte("batch-a")
	}
	if err := nodeA.SetMulti(batchA, 25*time.Millisecond); err != nil {
		t.Fatalf("nodeA.SetMulti() error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		for k := range batchA {
			if _, ok := nodeB.Get(k); !ok {
				return false
			}
		}
		return true
	}, "nodeB did not receive nodeA SetMulti batch")
	for k := range batchA {
		if e := nodeB.lookupEntry(k); e == nil || e.staleGrace != 0 {
			t.Fatalf("nodeB %s staleGrace = %v, want 0", k, e)
		}
	}
	time.Sleep(40 * time.Millisecond)
	var batchLoads atomic.Int32
	gotBatch, err := nodeB.GetOrLoad(t.Context(), "sma-0", time.Minute, func(_ context.Context) ([]byte, error) {
		batchLoads.Add(1)
		return []byte("batch-a-reloaded"), nil
	})
	if err != nil || string(gotBatch) != "batch-a-reloaded" || batchLoads.Load() != 1 {
		t.Fatalf("nodeB.GetOrLoad(sma-0) = (%q, loads=%d, err=%v), want reloaded", string(gotBatch), batchLoads.Load(), err)
	}

	// Small SetMulti from nodeB (default=300ms) -> nodeA (default=0) preserves 300ms staleGrace.
	batchB := map[string][]byte{"smb-0": []byte("b0"), "smb-1": []byte("b1")}
	if err := nodeB.SetMulti(batchB, time.Minute); err != nil {
		t.Fatalf("nodeB.SetMulti() error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		v0, ok0 := nodeA.Get("smb-0")
		v1, ok1 := nodeA.Get("smb-1")
		return ok0 && ok1 && string(v0) == "b0" && string(v1) == "b1"
	}, "nodeA did not receive nodeB small SetMulti batch")
	for k := range batchB {
		if e := nodeA.lookupEntry(k); e == nil || e.staleGrace != int64(300*time.Millisecond) {
			t.Fatalf("nodeA %s staleGrace = %v, want 300ms", k, e)
		}
	}

	// SetMulti with NoExpiration from nodeB -> nodeA preserves NoExpiration and staleGrace=0.
	batchPerm := map[string][]byte{"smperm-0": []byte("p0"), "smperm-1": []byte("p1")}
	if err := nodeB.SetMulti(batchPerm, NoExpiration); err != nil {
		t.Fatalf("nodeB.SetMulti(NoExpiration) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		_, ok0 := nodeA.Get("smperm-0")
		_, ok1 := nodeA.Get("smperm-1")
		return ok0 && ok1
	}, "nodeA did not receive nodeB NoExpiration SetMulti batch")
	for k := range batchPerm {
		if e := nodeA.lookupEntry(k); e == nil || !e.isNoExpiration() || e.staleGrace != 0 {
			t.Fatalf("nodeA %s metadata mismatch: %+v", k, e)
		}
	}
}

func TestWriteThrough_AsyncInvalidation_ZeroStaleWindowPreserved(t *testing.T) {
	broker := NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	newAsyncNode := func(name string) *MemoryStore {
		ms, err := New(
			WithPreciseClock(),
			WithPubSub(broker),
			WithInvalidationTopic("wt:swr:async"),
			WithInvalidationMode(InvalidationModeWriteThrough),
			WithAsyncInvalidation(64),
			WithDefaultStaleWhileRevalidate(300*time.Millisecond),
		)
		if err != nil {
			t.Fatalf("New(%s) error = %v", name, err)
		}
		return ms
	}

	nodeA := newAsyncNode("nodeA")
	defer func() { _ = nodeA.Stop() }()
	nodeB := newAsyncNode("nodeB")
	defer func() { _ = nodeB.Stop() }()

	if err := nodeA.SetWithOptions("async-zero", []byte("old-z"), 25*time.Millisecond, WithStaleWindow(0)); err != nil {
		t.Fatalf("SetWithOptions(async-zero) error = %v", err)
	}
	if err := nodeA.SetWithOptions("async-pos", []byte("old-p"), 25*time.Millisecond, WithStaleWindow(250*time.Millisecond)); err != nil {
		t.Fatalf("SetWithOptions(async-pos) error = %v", err)
	}

	waitForCondition(t, time.Second, func() bool {
		_, okZ := nodeB.Get("async-zero")
		_, okP := nodeB.Get("async-pos")
		return okZ && okP
	}, "nodeB did not receive async write-through entries")

	if e := nodeB.lookupEntry("async-zero"); e == nil || e.staleGrace != 0 {
		t.Fatalf("nodeB async-zero staleGrace = %v, want 0", e)
	}
	if e := nodeB.lookupEntry("async-pos"); e == nil || e.staleGrace != int64(250*time.Millisecond) {
		t.Fatalf("nodeB async-pos staleGrace = %v, want 250ms", e)
	}

	time.Sleep(40 * time.Millisecond)

	var zeroLoads atomic.Int32
	gotZ, err := nodeB.GetOrLoad(t.Context(), "async-zero", time.Minute, func(_ context.Context) ([]byte, error) {
		zeroLoads.Add(1)
		return []byte("fresh-z"), nil
	})
	if err != nil || string(gotZ) != "fresh-z" || zeroLoads.Load() != 1 {
		t.Fatalf("async-zero GetOrLoad = (%q, loads=%d, err=%v), want reloaded", string(gotZ), zeroLoads.Load(), err)
	}

	gotP, err := nodeB.GetOrLoad(t.Context(), "async-pos", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("fresh-p"), nil
	})
	if err != nil || string(gotP) != "old-p" {
		t.Fatalf("async-pos GetOrLoad = (%q, err=%v), want stale %q", string(gotP), err, "old-p")
	}
}

func TestWriteThrough_ZeroStaleWindow_WireOmitzero(t *testing.T) {
	ps := NewInMemoryPubSub()
	defer func() { _ = ps.Close() }()

	ch, err := ps.Subscribe(t.Context(), "wt:wire")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	ms, err := New(
		WithPreciseClock(),
		WithPubSub(ps),
		WithInvalidationTopic("wt:wire"),
		WithInvalidationMode(InvalidationModeWriteThrough),
		WithSyncInvalidation(),
		WithDefaultStaleWhileRevalidate(300*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	if err := ms.SetWithOptions("wire-zero", []byte("v"), time.Minute, WithStaleWindow(0)); err != nil {
		t.Fatalf("SetWithOptions() error = %v", err)
	}

	select {
	case payload := <-ch:
		if bytes.Contains(payload, []byte(`"g":`)) {
			t.Fatalf("expected StaleGrace=0 to omit \"g\" field on wire, got %s", string(payload))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for WriteThrough payload")
	}
}

func TestWriteThrough_SetMultiChunkingAndOversizedFallback(t *testing.T) {
	broker := NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	n1, err := New(
		WithPubSub(broker),
		WithInvalidationTopic("wt:chunk:test"),
		WithInvalidationMode(InvalidationModeWriteThrough),
		WithWriteThroughMaxBytes(256),
		WithSyncInvalidation(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n1.Stop() }()

	n2, err := New(
		WithPubSub(broker),
		WithInvalidationTopic("wt:chunk:test"),
		WithInvalidationMode(InvalidationModeWriteThrough),
		WithWriteThroughMaxBytes(256),
		WithSyncInvalidation(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n2.Stop() }()

	if err := n2.Set("oversize-raw", []byte("stale"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := n2.Set("oversize-encoded", []byte("stale"), time.Minute); err != nil {
		t.Fatal(err)
	}

	batch := map[string][]byte{
		"s1":               bytes.Repeat([]byte("a"), 40),
		"s2":               bytes.Repeat([]byte("b"), 40),
		"s3":               bytes.Repeat([]byte("c"), 40),
		"s4":               bytes.Repeat([]byte("d"), 40),
		"s5":               bytes.Repeat([]byte("e"), 40),
		"oversize-encoded": bytes.Repeat([]byte("x"), 220), // <= 256 raw, > 256 JSON+base64
		"oversize-raw":     bytes.Repeat([]byte("y"), 300), // > 256 raw
	}
	if err := n1.SetMulti(batch, time.Minute); err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		_, ok1 := n2.Get("s1")
		_, ok5 := n2.Get("s5")
		_, over1 := n2.Get("oversize-raw")
		_, over2 := n2.Get("oversize-encoded")
		return ok1 && ok5 && !over1 && !over2
	}, "peer did not receive chunked write-through updates")

	for _, k := range []string{"s1", "s2", "s3", "s4", "s5"} {
		if _, ok := n2.Get(k); !ok {
			t.Fatalf("peer missing chunked write-through key %q", k)
		}
	}
	if _, ok := n2.Get("oversize-raw"); ok {
		t.Fatal("oversize-raw should have been invalidated on peer")
	}
	if _, ok := n2.Get("oversize-encoded"); ok {
		t.Fatal("oversize-encoded should have fallen back to InvalidateOnly")
	}

	// SetMulti with duration=0 must broadcast invalidation for deleted batch keys.
	if err := n1.SetMulti(map[string][]byte{"s1": []byte("x"), "s2": []byte("y")}, 0); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		_, ok1 := n2.Get("s1")
		_, ok2 := n2.Get("s2")
		return !ok1 && !ok2
	}, "s1/s2 should have been invalidated by SetMulti(..., 0)")

	// Expired-in-queue writeThroughItem falls back to InvalidateOnly.
	if err := n2.Set("exp-wt", []byte("stale"), time.Minute); err != nil {
		t.Fatal(err)
	}
	n1.publishWriteThroughItems([]writeThroughItem{{
		key:       "exp-wt",
		value:     []byte("expired"),
		expiresAt: n1.nowExact() - int64(time.Second),
	}}, nil)
	waitForCondition(t, 2*time.Second, func() bool {
		_, ok := n2.Get("exp-wt")
		return !ok
	}, "expired-in-queue write-through item should invalidate peer")
}
