package memorystore

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
)

// ==================== Issue #41: New, Options & Validation ====================

func TestNew_Validation(t *testing.T) {
	t.Run("nil options skipped cleanly", func(t *testing.T) {
		ms, err := New(nil, WithMaxItems(10), nil)
		if err != nil {
			t.Fatalf("New() unexpected error = %v", err)
		}
		defer func() { _ = ms.Stop() }()
	})

	t.Run("option returning error propagates", func(t *testing.T) {
		sentinel := errors.New("option failed")
		_, err := New(func(*Config) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("New() error = %v, want %v", err, sentinel)
		}
	})

	tests := []struct {
		name    string
		opts    []Option
		wantErr error
	}{
		{
			name: "negative MaxItems",
			opts: []Option{WithMaxItems(-1)},
		},
		{
			name: "negative MaxBytes",
			opts: []Option{WithMaxBytes(-1)},
		},
		{
			name: "negative CleanupInterval",
			opts: []Option{WithCleanupInterval(-time.Millisecond)},
		},
		{
			name: "negative PubSubTimeout",
			opts: []Option{WithPubSubTimeout(-time.Second)},
		},
		{
			name: "negative DefaultTTLJitter",
			opts: []Option{WithDefaultTTLJitter(-0.01)},
		},
		{
			name: "DefaultTTLJitter above 0.5",
			opts: []Option{WithDefaultTTLJitter(0.51)},
		},
		{
			name: "NaN DefaultTTLJitter",
			opts: []Option{WithDefaultTTLJitter(math.NaN())},
		},
		{
			name: "InvalidationTopic without PubSub",
			opts: []Option{WithInvalidationTopic("cache:inv")},
		},
		{
			name: "unknown EvictionPolicy",
			opts: []Option{WithEvictionPolicy(EvictionPolicy(99))},
		},
		{
			name: "S3FIFO without bounds",
			opts: []Option{WithEvictionPolicy(EvictionS3FIFO)},
		},
		{
			name: "TinyLFU without bounds",
			opts: []Option{WithEvictionPolicy(EvictionTinyLFU)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := New(tt.opts...)
			if err == nil {
				_ = ms.Stop()
				t.Fatalf("New() succeeded, expected error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("New() error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	t.Run("bounded eviction policies succeed and default to S3FIFO", func(t *testing.T) {
		msDefault, err := New(WithMaxItems(10))
		if err != nil {
			t.Fatalf("New(WithMaxItems) error = %v", err)
		}
		if msDefault.evictionPolicy != EvictionS3FIFO {
			t.Errorf("bounded default evictionPolicy = %v, want %v", msDefault.evictionPolicy, EvictionS3FIFO)
		}
		_ = msDefault.Stop()

		msS3, err := New(WithMaxItems(10), WithEvictionPolicy(EvictionS3FIFO))
		if err != nil {
			t.Fatalf("New(S3FIFO) error = %v", err)
		}
		_ = msS3.Stop()

		msTiny, err := New(WithMaxBytes(1024), WithEvictionPolicy(EvictionTinyLFU))
		if err != nil {
			t.Fatalf("New(TinyLFU) error = %v", err)
		}
		_ = msTiny.Stop()

		msVol, err := New(WithMaxItems(10), WithEvictionPolicy(EvictionVolatileTTL))
		if err != nil {
			t.Fatalf("New(VolatileTTL) error = %v", err)
		}
		if msVol.evictionPolicy != EvictionVolatileTTL {
			t.Errorf("explicit VolatileTTL got %v", msVol.evictionPolicy)
		}
		_ = msVol.Stop()
	})

	t.Run("valid boundary options", func(t *testing.T) {
		ps := NewInMemoryPubSub()
		defer func() { _ = ps.Close() }()

		ms, err := New(
			WithMaxItems(0),
			WithMaxBytes(0),
			WithCleanupInterval(0),
			WithPubSubTimeout(0),
			WithDefaultTTLJitter(0.5),
			WithEvictionPolicy(EvictionVolatileTTL),
			WithPubSub(ps),
			WithInvalidationTopic("inv:valid"),
		)
		if err != nil {
			t.Fatalf("New() valid options failed: %v", err)
		}
		if err := ms.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	})
}

func TestEvictionPolicy_String(t *testing.T) {
	tests := []struct {
		p    EvictionPolicy
		want string
	}{
		{EvictionVolatileTTL, "volatile-ttl"},
		{EvictionS3FIFO, "s3-fifo"},
		{EvictionTinyLFU, "w-tinylfu"},
		{EvictionPolicy(42), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("EvictionPolicy(%d).String() = %q, want %q", tt.p, got, tt.want)
		}
	}
}

// ==================== Issue #41: NoExpiration & SetWithOptions ====================

func TestMemoryStore_NoExpiration(t *testing.T) {
	ms, err := New(WithCleanupInterval(15 * time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	if err := ms.Set("perm", []byte("forever"), NoExpiration); err != nil {
		t.Fatalf("Set(NoExpiration) error = %v", err)
	}
	if err := ms.SetWithOptions("perm-opt", []byte("forever-opt"), 1*time.Millisecond, WithNoExpiration()); err != nil {
		t.Fatalf("SetWithOptions(WithNoExpiration) error = %v", err)
	}
	if err := ms.Set("zero", []byte("gone"), 0); err != nil {
		t.Fatalf("Set(0) error = %v", err)
	}
	if err := ms.Set("neg", []byte("gone"), -5*time.Second); err != nil {
		t.Fatalf("Set(-5s) error = %v", err)
	}
	if err := ms.SetMulti(map[string][]byte{"batch-perm": []byte("bp")}, NoExpiration); err != nil {
		t.Fatalf("SetMulti(NoExpiration) error = %v", err)
	}

	if _, ok := ms.Get("zero"); ok {
		t.Error("ttl=0 entry should expire immediately")
	}
	if _, ok := ms.Get("neg"); ok {
		t.Error("ttl=-5s entry should expire immediately")
	}

	time.Sleep(45 * time.Millisecond)
	ms.cleanupExpiredItems()

	for _, k := range []string{"perm", "perm-opt", "batch-perm"} {
		if v, ok := ms.Get(k); !ok || len(v) == 0 {
			t.Errorf("Get(%q) after cleanup = (%q, %v), want present", k, v, ok)
		}
	}
	gotMulti := ms.GetMulti([]string{"perm", "perm-opt", "batch-perm", "zero"})
	if len(gotMulti) != 3 {
		t.Errorf("GetMulti() len = %d, want 3", len(gotMulti))
	}
}

func TestMemoryStore_NoExpirationEligibleForCapacityEviction(t *testing.T) {
	var evictedReasons []EvictReason
	var mu sync.Mutex
	ms, err := New(
		WithMaxItems(2),
		WithOnEvict(func(_ string, _ []byte, reason EvictReason) {
			mu.Lock()
			evictedReasons = append(evictedReasons, reason)
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	for i := range 20 {
		if err := ms.Set(fmt.Sprintf("perm-%d", i), []byte("val"), NoExpiration); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	if got := ms.GetMetrics().Items; got > 2 {
		t.Fatalf("Items = %d, want <= 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(evictedReasons) < 18 {
		t.Fatalf("expected >= 18 capacity evictions, got %d", len(evictedReasons))
	}
	for _, r := range evictedReasons {
		if r != EvictCapacity {
			t.Errorf("unexpected eviction reason for NoExpiration item: %v, want EvictCapacity", r)
		}
	}
}

func TestMemoryStore_SetWithOptions_JitterAndSliding(t *testing.T) {
	t.Run("jitter validation", func(t *testing.T) {
		ms, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		for _, bad := range []float64{-0.1, 0.51, math.NaN()} {
			if err := ms.SetWithOptions("bad", []byte("v"), time.Minute, WithTTLJitter(bad)); err == nil {
				t.Errorf("SetWithOptions(jitter=%v) succeeded, want error", bad)
			}
			if _, ok := ms.Get("bad"); ok {
				t.Errorf("key should not be inserted on invalid jitter %v", bad)
			}
		}
	})

	t.Run("jitter spread and override", func(t *testing.T) {
		ms, err := New(WithDefaultTTLJitter(0.4))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		ttl := 10 * time.Second
		minExp := int64(float64(ttl) * 0.6)
		maxExp := int64(float64(ttl) * 1.4)

		seen := make(map[int64]struct{})
		for i := range 30 {
			k := fmt.Sprintf("jit-%d", i)
			if err := ms.SetWithOptions(k, []byte("v"), ttl, WithTTLJitter(0.4)); err != nil {
				t.Fatalf("SetWithOptions() error = %v", err)
			}
			e := ms.lookupEntry(k)
			if e == nil {
				t.Fatalf("lookupEntry(%q) = nil", k)
			}
			exp := e.expiresAt.Load()
			// Effective lifetime from baseTime must lie within [minExp, now + maxExp].
			now := ms.nowNano()
			if exp < minExp || exp > now+maxExp {
				t.Fatalf("expiresAt=%d out of expected jitter bounds [%d, %d]", exp, minExp, now+maxExp)
			}
			seen[exp] = struct{}{}
		}
		if len(seen) < 5 {
			t.Errorf("expected jittered expirations to vary across samples, got %d distinct values", len(seen))
		}

		// Per-item WithTTLJitter(0) must override store DefaultTTLJitter(0.4).
		for i := range 10 {
			k := fmt.Sprintf("zero-jit-%d", i)
			before := ms.nowCoarse()
			if err := ms.SetWithOptions(k, []byte("v"), ttl, WithTTLJitter(0)); err != nil {
				t.Fatalf("SetWithOptions(0) error = %v", err)
			}
			after := ms.nowNano()
			e := ms.lookupEntry(k)
			if e == nil {
				t.Fatalf("lookupEntry(%q) = nil", k)
			}
			exp := e.expiresAt.Load()
			if exp < before+int64(ttl) || exp > after+int64(ttl) {
				t.Fatalf("WithTTLJitter(0) did not override default jitter: exp=%d outside [%d, %d]",
					exp, before+int64(ttl), after+int64(ttl))
			}
		}

		// SetMulti honors DefaultTTLJitter when configured.
		if err := ms.SetMulti(map[string][]byte{"sm1": []byte("a"), "sm2": []byte("b")}, ttl); err != nil {
			t.Fatalf("SetMulti() error = %v", err)
		}
		if v, ok := ms.Get("sm1"); !ok || string(v) != "a" {
			t.Errorf("Get(sm1) = (%q, %v), want (a, true)", v, ok)
		}
	})

	t.Run("sliding expiration via Get and GetMulti", func(t *testing.T) {
		ms, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		const ttl = 60 * time.Millisecond
		if err := ms.SetWithOptions("slide-get", []byte("alive"), ttl, nil, WithSlidingExpiration()); err != nil {
			t.Fatalf("SetWithOptions() error = %v", err)
		}
		if err := ms.SetWithOptions("slide-idle", []byte("dies"), ttl, WithSlidingExpiration()); err != nil {
			t.Fatalf("SetWithOptions() error = %v", err)
		}

		// Refresh slide-get repeatedly across 120ms (2x TTL).
		for range 5 {
			time.Sleep(25 * time.Millisecond)
			if v, ok := ms.Get("slide-get"); !ok || string(v) != "alive" {
				t.Fatalf("sliding key expired despite Get refreshes: ok=%v", ok)
			}
		}
		// slide-idle was never accessed so it must be expired.
		if _, ok := ms.Get("slide-idle"); ok {
			t.Error("unaccessed sliding key should have expired")
		}

		// Large batch GetMulti (>4 keys) also refreshes sliding keys.
		keys := make([]string, 6)
		for i := range keys {
			k := fmt.Sprintf("slide-multi-%d", i)
			keys[i] = k
			if err := ms.SetWithOptions(k, []byte("m"), ttl, WithSlidingExpiration()); err != nil {
				t.Fatalf("SetWithOptions(%s) error = %v", k, err)
			}
		}
		for range 4 {
			time.Sleep(25 * time.Millisecond)
			got := ms.GetMulti(keys)
			if len(got) != len(keys) {
				t.Fatalf("GetMulti() returned %d keys, want %d", len(got), len(keys))
			}
		}
	})
}

// ==================== Issue #41: CopyOnWrite & CopyOnRead ====================

func TestMemoryStore_CopyOnWriteAndCopyOnRead(t *testing.T) {
	t.Run("CopyOnWrite isolates Set, SetWithOptions, SetMulti", func(t *testing.T) {
		ms, err := New(WithCopyOnWrite(true))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		buf := []byte("original")
		if err := ms.Set("k-set", buf, time.Minute); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		if err := ms.SetWithOptions("k-opt", buf, time.Minute, WithSlidingExpiration()); err != nil {
			t.Fatalf("SetWithOptions() error = %v", err)
		}
		batchSmall := map[string][]byte{"bs1": buf}
		if err := ms.SetMulti(batchSmall, time.Minute); err != nil {
			t.Fatalf("SetMulti(small) error = %v", err)
		}
		batchLarge := map[string][]byte{
			"bl1": buf, "bl2": buf, "bl3": buf, "bl4": buf, "bl5": buf,
		}
		if err := ms.SetMulti(batchLarge, time.Minute); err != nil {
			t.Fatalf("SetMulti(large) error = %v", err)
		}

		// Mutate caller buffer in place.
		copy(buf, "MUTATED!")

		for _, k := range []string{"k-set", "k-opt", "bs1", "bl1", "bl5"} {
			got, ok := ms.Get(k)
			if !ok || string(got) != "original" {
				t.Errorf("Get(%q) = (%q, %v), want (original, true)", k, got, ok)
			}
		}
	})

	t.Run("CopyOnRead isolates Get, GetMulti, GetOrLoad", func(t *testing.T) {
		ms, err := New(WithCopyOnWrite(true), WithCopyOnRead(true))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		_ = ms.Set("k1", []byte("secret-1"), time.Minute)
		_ = ms.Set("k2", []byte("secret-2"), time.Minute)
		_ = ms.Set("k3", []byte("secret-3"), time.Minute)
		_ = ms.Set("k4", []byte("secret-4"), time.Minute)
		_ = ms.Set("k5", []byte("secret-5"), time.Minute)

		v1, ok := ms.Get("k1")
		if !ok {
			t.Fatal("Get(k1) miss")
		}
		v1[0] = 'X'
		if v1Again, _ := ms.Get("k1"); string(v1Again) != "secret-1" {
			t.Errorf("Get mutation leaked into cache: got %q", v1Again)
		}

		smallRes := ms.GetMulti([]string{"k1", "k2"})
		smallRes["k1"][0] = 'Y'
		if v1Again, _ := ms.Get("k1"); string(v1Again) != "secret-1" {
			t.Errorf("GetMulti(small) mutation leaked into cache: got %q", v1Again)
		}

		largeRes := ms.GetMulti([]string{"k1", "k2", "k3", "k4", "k5"})
		largeRes["k5"][0] = 'Z'
		if v5Again, _ := ms.Get("k5"); string(v5Again) != "secret-5" {
			t.Errorf("GetMulti(large) mutation leaked into cache: got %q", v5Again)
		}

		// Coalesced GetOrLoad callers must not alias each other's slices or cache bytes.
		started := make(chan struct{})
		release := make(chan struct{})
		const workers = 4
		slices := make([][]byte, workers)
		var wg sync.WaitGroup

		wg.Go(func() {
			var loadErr error
			slices[0], loadErr = ms.GetOrLoad(t.Context(), "sf-copy", time.Minute, func(_ context.Context) ([]byte, error) {
				close(started)
				<-release
				return []byte("shared-val"), nil
			})
			if loadErr != nil {
				t.Errorf("leader GetOrLoad error = %v", loadErr)
			}
		})
		<-started
		for i := 1; i < workers; i++ {
			idx := i
			wg.Go(func() {
				var loadErr error
				slices[idx], loadErr = ms.GetOrLoad(t.Context(), "sf-copy", time.Minute, func(_ context.Context) ([]byte, error) {
					return []byte("unexpected"), nil
				})
				if loadErr != nil {
					t.Errorf("waiter %d GetOrLoad error = %v", idx, loadErr)
				}
			})
		}
		time.Sleep(15 * time.Millisecond)
		close(release)
		wg.Wait()

		for i := range workers {
			slices[i][0] = byte('A' + i)
		}
		if cached, ok := ms.Get("sf-copy"); !ok || string(cached) != "shared-val" {
			t.Errorf("GetOrLoad mutation leaked into cache: got (%q, %v)", cached, ok)
		}
	})
}

func TestMemoryStore_SetWithOptions_PostStop(t *testing.T) {
	ms, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := ms.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := ms.SetWithOptions("k", []byte("v"), time.Minute, WithSlidingExpiration()); !errors.Is(err, ErrStoreStopped) {
		t.Fatalf("SetWithOptions() after Stop = %v, want ErrStoreStopped", err)
	}
}

// ==================== Issue #45: MetricsRecorder, DroppedCount & Health ====================

type evictCall struct {
	reason EvictReason
	bytes  int64
}

type loadCall struct {
	duration time.Duration
	shared   bool
	err      error
}

type invCall struct {
	sent     bool
	keyCount int
}

type spyRecorder struct {
	mu          sync.Mutex
	hits        int64
	misses      int64
	evicts      []evictCall
	loads       []loadCall
	invalidates []invCall
}

func (s *spyRecorder) RecordHit() {
	s.mu.Lock()
	s.hits++
	s.mu.Unlock()
}

func (s *spyRecorder) RecordMiss() {
	s.mu.Lock()
	s.misses++
	s.mu.Unlock()
}

func (s *spyRecorder) RecordEvict(reason EvictReason, bytes int64) {
	s.mu.Lock()
	s.evicts = append(s.evicts, evictCall{reason: reason, bytes: bytes})
	s.mu.Unlock()
}

func (s *spyRecorder) RecordLoad(duration time.Duration, shared bool, err error) {
	s.mu.Lock()
	s.loads = append(s.loads, loadCall{duration: duration, shared: shared, err: err})
	s.mu.Unlock()
}

func (s *spyRecorder) RecordInvalidation(sent bool, keyCount int) {
	s.mu.Lock()
	s.invalidates = append(s.invalidates, invCall{sent: sent, keyCount: keyCount})
	s.mu.Unlock()
}

func TestMetricsRecorder_TypedNilSafe(t *testing.T) {
	var typedNil *spyRecorder
	ms, err := New(WithMetricsRecorder(typedNil))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	if err := ms.Set("k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if _, ok := ms.Get("k"); !ok {
		t.Fatal("Get(k) miss")
	}
	_, _ = ms.Get("missing")
	_ = ms.Delete("k")
}

func TestMetricsRecorder_HitsMissesEvictsLoadsInvalidations(t *testing.T) {
	rec := &spyRecorder{}
	broker := NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	recPeer := &spyRecorder{}
	ms, err := New(
		WithMetricsRecorder(rec),
		WithPubSub(broker),
		WithInvalidationTopic("obs:inv"),
		WithMaxItems(3),
	)
	if err != nil {
		t.Fatalf("New(ms) error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	peer, err := New(
		WithMetricsRecorder(recPeer),
		WithPubSub(broker),
		WithInvalidationTopic("obs:inv"),
	)
	if err != nil {
		t.Fatalf("New(peer) error = %v", err)
	}
	defer func() { _ = peer.Stop() }()

	// Populate peer so remote invalidation actually deletes an existing key,
	// then wait for peer's initial invalidation broadcast to settle and reset recorders.
	_ = peer.Set("k1", []byte("stale"), time.Minute)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		invs := len(rec.invalidates)
		rec.mu.Unlock()
		if invs >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.mu.Lock()
	rec.invalidates = nil
	rec.evicts = nil
	rec.mu.Unlock()
	recPeer.mu.Lock()
	recPeer.invalidates = nil
	recPeer.evicts = nil
	recPeer.mu.Unlock()

	// Set k1 on ms -> broadcasts invalidation(sent=true, 1) on ms and (sent=false, 1) + EvictDeleted on peer.
	_ = ms.Set("k1", []byte("v1"), time.Minute)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		recPeer.mu.Lock()
		invs := len(recPeer.invalidates)
		recPeer.mu.Unlock()
		if invs >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec.mu.Lock()
	if len(rec.invalidates) != 1 || !rec.invalidates[0].sent || rec.invalidates[0].keyCount != 1 {
		t.Errorf("ms invalidates = %+v, want [{sent:true, keyCount:1}]", rec.invalidates)
	}
	rec.mu.Unlock()

	recPeer.mu.Lock()
	if len(recPeer.invalidates) == 0 || recPeer.invalidates[0].sent || recPeer.invalidates[0].keyCount != 1 {
		t.Errorf("peer invalidates = %+v, want [{sent:false, keyCount:1}]", recPeer.invalidates)
	}
	if len(recPeer.evicts) != 1 || recPeer.evicts[0].reason != EvictDeleted || recPeer.evicts[0].bytes != int64(len("k1")+len("stale")) {
		t.Errorf("peer evicts = %+v, want [{EvictDeleted, 7}]", recPeer.evicts)
	}
	recPeer.mu.Unlock()

	// Get hit and miss + GetMulti small and large.
	_, _ = ms.Get("k1")
	_, _ = ms.Get("missing")
	_ = ms.GetMulti([]string{"k1", "missing"}) // +1 hit, +1 miss
	_ = ms.Set("k2", []byte("v2"), time.Minute)
	_ = ms.GetMulti([]string{"k1", "k2", "m1", "m2", "m3"}) // +2 hits, +3 misses

	rec.mu.Lock()
	if rec.hits != 4 || rec.misses != 5 {
		t.Errorf("hits/misses = (%d, %d), want (4, 5)", rec.hits, rec.misses)
	}
	rec.mu.Unlock()

	// Delete missing emits 0 evicts; delete existing emits EvictDeleted with exact bytes.
	if err := ms.Delete("no-such-key"); err != nil {
		t.Fatalf("Delete(missing) error = %v", err)
	}
	if err := ms.Delete("k2"); err != nil {
		t.Fatalf("Delete(k2) error = %v", err)
	}
	rec.mu.Lock()
	if len(rec.evicts) != 1 || rec.evicts[0].reason != EvictDeleted || rec.evicts[0].bytes != int64(len("k2")+len("v2")) {
		t.Errorf("evicts after Delete = %+v, want 1 EvictDeleted(4)", rec.evicts)
	}
	rec.evicts = nil
	rec.mu.Unlock()

	// Expired cleanup emits EvictExpired.
	_ = ms.Set("short", []byte("1234"), 10*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	ms.cleanupExpiredItems()
	rec.mu.Lock()
	if len(rec.evicts) != 1 || rec.evicts[0].reason != EvictExpired || rec.evicts[0].bytes != int64(len("short")+len("1234")) {
		t.Errorf("evicts after cleanupExpiredItems = %+v, want 1 EvictExpired(9)", rec.evicts)
	}
	rec.evicts = nil
	rec.mu.Unlock()

	// Capacity eviction emits EvictCapacity.
	for i := range 6 {
		_ = ms.Set(fmt.Sprintf("cap-%d", i), []byte("val"), time.Minute)
	}
	rec.mu.Lock()
	var capCount int
	for _, ev := range rec.evicts {
		if ev.reason == EvictCapacity {
			capCount++
		}
	}
	rec.mu.Unlock()
	if capCount == 0 {
		t.Error("expected EvictCapacity events when exceeding MaxItems=3")
	}
}

func TestMetricsRecorder_GetOrLoad_NoDoubleMissAndSharedFlag(t *testing.T) {
	rec := &spyRecorder{}
	ms, err := New(WithMetricsRecorder(rec))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = ms.Stop() }()

	// Single cold miss on GetOrLoad must record exactly 1 miss and 1 non-shared load.
	val, err := ms.GetOrLoad(t.Context(), "single", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("v"), nil
	})
	if err != nil || string(val) != "v" {
		t.Fatalf("GetOrLoad() = (%q, %v)", val, err)
	}
	// Warm hit must record 1 hit and 0 additional loads.
	_, err = ms.GetOrLoad(t.Context(), "single", time.Minute, func(_ context.Context) ([]byte, error) {
		return []byte("nope"), nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad(warm) error = %v", err)
	}

	rec.mu.Lock()
	if rec.misses != 1 || rec.hits != 1 || len(rec.loads) != 1 || rec.loads[0].shared || rec.loads[0].err != nil {
		t.Fatalf("single GetOrLoad metrics mismatch: hits=%d misses=%d loads=%+v", rec.hits, rec.misses, rec.loads)
	}
	if m := ms.GetMetrics(); m.Misses != 1 || m.Hits != 1 {
		t.Fatalf("StoreMetrics mismatch: %+v, want Hits=1 Misses=1", m)
	}
	rec.loads = nil
	rec.misses = 0
	rec.hits = 0
	rec.mu.Unlock()

	// Single cold miss on GetOrLoadJSON must record exactly 1 miss and 1 non-shared load.
	type doc struct {
		ID int `json:"id"`
	}
	gotDoc, err := ms.GetOrLoadJSON[doc](t.Context(), "json-single", time.Minute, func(_ context.Context) (doc, error) {
		return doc{ID: 7}, nil
	})
	if err != nil || gotDoc.ID != 7 {
		t.Fatalf("GetOrLoadJSON() = (%+v, %v)", gotDoc, err)
	}
	rec.mu.Lock()
	if rec.misses != 1 || len(rec.loads) != 1 || rec.loads[0].shared || rec.loads[0].err != nil {
		t.Fatalf("single GetOrLoadJSON metrics mismatch: misses=%d loads=%+v", rec.misses, rec.loads)
	}
	rec.loads = nil
	rec.misses = 0
	rec.mu.Unlock()

	// Concurrent coalesced GetOrLoad callers: 1 leader (shared=false) + (N-1) followers (shared=true).
	const workers = 5
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = ms.GetOrLoad(t.Context(), "coalesced", time.Minute, func(_ context.Context) ([]byte, error) {
			close(started)
			<-release
			return []byte("ok"), nil
		})
	})
	<-started
	for range workers - 1 {
		wg.Go(func() {
			_, _ = ms.GetOrLoad(t.Context(), "coalesced", time.Minute, func(_ context.Context) ([]byte, error) {
				return []byte("unexpected"), nil
			})
		})
	}
	time.Sleep(15 * time.Millisecond)
	close(release)
	wg.Wait()

	rec.mu.Lock()
	if rec.misses != workers {
		t.Errorf("coalesced misses = %d, want %d (must not double-count)", rec.misses, workers)
	}
	if len(rec.loads) != workers {
		t.Fatalf("coalesced loads = %d, want %d", len(rec.loads), workers)
	}
	var leaderCount, sharedCount int
	for _, lc := range rec.loads {
		if lc.shared {
			sharedCount++
		} else {
			leaderCount++
		}
	}
	if leaderCount != 1 || sharedCount != workers-1 {
		t.Errorf("leader/shared counts = (%d, %d), want (1, %d)", leaderCount, sharedCount, workers-1)
	}
	rec.mu.Unlock()

	// Loader error is passed to RecordLoad.
	boom := errors.New("load failed")
	_, err = ms.GetOrLoad(t.Context(), "err-load", time.Minute, func(_ context.Context) ([]byte, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("GetOrLoad() error = %v, want %v", err, boom)
	}
	rec.mu.Lock()
	lastLoad := rec.loads[len(rec.loads)-1]
	rec.mu.Unlock()
	if !errors.Is(lastLoad.err, boom) || lastLoad.shared {
		t.Errorf("last RecordLoad = %+v, want err=boom shared=false", lastLoad)
	}

	// Verify GetJSON / GetJSONWithOptions / GetJSONAs record 1 hit/miss without double-counting.
	rec.mu.Lock()
	rec.hits, rec.misses = 0, 0
	rec.mu.Unlock()
	var d doc
	_, _ = ms.GetJSON("json-single", &d)
	_, _ = ms.GetJSONWithOptions("missing-json", &d, json.RejectUnknownMembers(true))
	_, _, _ = ms.GetJSONAs[doc]("json-single")
	rec.mu.Lock()
	if rec.hits != 2 || rec.misses != 1 {
		t.Errorf("JSON reads hits/misses = (%d, %d), want (2, 1)", rec.hits, rec.misses)
	}
	rec.mu.Unlock()
}

func TestInMemoryPubSub_DroppedCountAndPing(t *testing.T) {
	ps := NewInMemoryPubSub()
	defer func() { _ = ps.Close() }()

	if err := ps.Ping(t.Context()); err != nil {
		t.Fatalf("Ping() on open broker = %v, want nil", err)
	}
	if err := ps.Ping(nil); err == nil { //nolint:staticcheck // intentionally testing nil context guard
		t.Fatal("Ping(nil) = nil, want error")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ps.Ping(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ping(canceled) = %v, want context.Canceled", err)
	}

	ch, err := ps.Subscribe(t.Context(), "slow:topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	const total = defaultChannelBuffer + 50
	var wg sync.WaitGroup
	for g := range 5 {
		wg.Go(func() {
			for i := range total / 5 {
				_ = ps.Publish(t.Context(), "slow:topic", []byte(fmt.Sprintf("%d-%d", g, i)))
			}
		})
	}
	wg.Wait()

	if got := ps.DroppedCount(); got != 50 {
		t.Fatalf("DroppedCount() = %d, want 50", got)
	}

	// Drain channel so test ends cleanly.
	for range defaultChannelBuffer {
		<-ch
	}
	if err := ps.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := ps.Ping(t.Context()); !errors.Is(err, ErrStoreStopped) {
		t.Fatalf("Ping() after Close = %v, want ErrStoreStopped", err)
	}
}

type failingBackplanePubSub struct {
	mu           sync.Mutex
	subErr       error
	pingErr      error
	ch           chan []byte
	closedByTest bool
}

func (f *failingBackplanePubSub) Subscribe(_ context.Context, _ string) (<-chan []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subErr != nil {
		return nil, f.subErr
	}
	f.ch = make(chan []byte, 1)
	return f.ch, nil
}

func (f *failingBackplanePubSub) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (f *failingBackplanePubSub) Unsubscribe(_ context.Context, _ string) error       { return nil }
func (f *failingBackplanePubSub) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ch != nil && !f.closedByTest {
		close(f.ch)
		f.ch = nil
	}
	return nil
}
func (f *failingBackplanePubSub) Ping(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

func TestMemoryStore_Health(t *testing.T) {
	t.Run("healthy store and stopped store", func(t *testing.T) {
		ms, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if err := ms.Health(t.Context()); err != nil {
			t.Fatalf("Health() on fresh store = %v, want nil", err)
		}
		if err := ms.Health(nil); err == nil { //nolint:staticcheck // intentionally testing nil context guard
			t.Fatal("Health(nil) = nil, want error")
		}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := ms.Health(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("Health(canceled) = %v, want context.Canceled", err)
		}
		_ = ms.Stop()
		if err := ms.Health(t.Context()); !errors.Is(err, ErrStoreStopped) {
			t.Fatalf("Health() after Stop = %v, want ErrStoreStopped", err)
		}
	})

	t.Run("invalidation subscribe failure", func(t *testing.T) {
		subSentinel := errors.New("subscribe broken")
		ps := &failingBackplanePubSub{subErr: subSentinel}
		ms, err := New(WithPubSub(ps), WithInvalidationTopic("inv:fail"))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		if err := ms.Health(t.Context()); !errors.Is(err, subSentinel) {
			t.Fatalf("Health() = %v, want wrapped %v", err, subSentinel)
		}
	})

	t.Run("invalidation channel closed unexpectedly", func(t *testing.T) {
		ps := &failingBackplanePubSub{}
		ms, err := New(WithPubSub(ps), WithInvalidationTopic("inv:close"))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		if err := ms.Health(t.Context()); err != nil {
			t.Fatalf("initial Health() = %v, want nil", err)
		}

		ps.mu.Lock()
		ps.closedByTest = true
		close(ps.ch)
		ps.mu.Unlock()

		deadline := time.Now().Add(time.Second)
		var healthErr error
		for time.Now().Before(deadline) {
			healthErr = ms.Health(t.Context())
			if healthErr != nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if healthErr == nil {
			t.Fatal("expected Health() to fail after invalidation channel closed")
		}
	})

	t.Run("Pinger error propagated", func(t *testing.T) {
		pingSentinel := errors.New("broker unreachable")
		ps := &failingBackplanePubSub{pingErr: pingSentinel}
		ms, err := New(WithPubSub(ps))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer func() { _ = ms.Stop() }()

		if err := ms.Health(t.Context()); !errors.Is(err, pingSentinel) {
			t.Fatalf("Health() = %v, want %v", err, pingSentinel)
		}
	})

	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/v2/memorystore") {
			t.Errorf("detected leaked goroutine in memorystore:\n%s", buf.String())
		}
	}
}
