package memorystore

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

func TestSnapshot_OptionsAndLifecycle(t *testing.T) {
	if got := SnapshotBinary.String(); got != "binary" {
		t.Errorf("SnapshotBinary.String() = %q", got)
	}
	if got := SnapshotJSON.String(); got != "json" {
		t.Errorf("SnapshotJSON.String() = %q", got)
	}
	if got := SnapshotFormat(99).String(); got != "unknown" {
		t.Errorf("SnapshotFormat(99).String() = %q", got)
	}
	if got := SnapshotOverwrite.String(); got != "overwrite" {
		t.Errorf("SnapshotOverwrite.String() = %q", got)
	}
	if got := SnapshotKeepExisting.String(); got != "keep-existing" {
		t.Errorf("SnapshotKeepExisting.String() = %q", got)
	}
	if got := SnapshotConflictPolicy(99).String(); got != "unknown" {
		t.Errorf("SnapshotConflictPolicy(99).String() = %q", got)
	}

	ms, err := New(WithPreciseClock())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := ms.Save(nil); err == nil {
		t.Fatal("Save(nil) succeeded, want error")
	}
	if err := ms.Load(nil); err == nil {
		t.Fatal("Load(nil) succeeded, want error")
	}
	if err := ms.Save(io.Discard, WithSnapshotFormat(SnapshotFormat(99))); err == nil {
		t.Fatal("Save(unknown format) succeeded, want error")
	}
	if err := ms.Load(bytes.NewReader(nil), WithSnapshotConflictPolicy(SnapshotConflictPolicy(99))); err == nil {
		t.Fatal("Load(unknown conflict policy) succeeded, want error")
	}

	if err := ms.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := ms.Save(io.Discard); !errors.Is(err, ErrStoreStopped) {
		t.Fatalf("Save after Stop = %v, want ErrStoreStopped", err)
	}
	if err := ms.Load(bytes.NewReader(nil)); !errors.Is(err, ErrStoreStopped) {
		t.Fatalf("Load after Stop = %v, want ErrStoreStopped", err)
	}
}

func TestSnapshot_RoundtripBinaryAndJSON_Gzip_TTL_SWR_Sliding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format SnapshotFormat
		useGZ  bool
	}{
		{"Binary", SnapshotBinary, false},
		{"BinaryGzip", SnapshotBinary, true},
		{"JSON", SnapshotJSON, false},
		{"JSONGzip", SnapshotJSON, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, err := New(WithPreciseClock())
			if err != nil {
				t.Fatalf("New(src) error = %v", err)
			}
			defer func() { _ = src.Stop() }()

			binVal := []byte{0x00, 0x01, 0xff, 0xfe, 'a', 'b'}
			if err := src.Set("perm", []byte("forever"), NoExpiration); err != nil {
				t.Fatal(err)
			}
			if err := src.Set("empty", []byte{}, time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := src.Set("bin", binVal, 150*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if err := src.SetWithOptions("sliding-swr", []byte("sval"), 300*time.Millisecond,
				WithSlidingExpiration(), WithStaleWindow(400*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if err := src.Set("soon-expired", []byte("gone"), 25*time.Millisecond); err != nil {
				t.Fatal(err)
			}

			// Multi-frame > 256 entries test.
			for i := range 310 {
				if err := src.Set(fmt.Sprintf("bulk-%03d", i), []byte("payload"), time.Minute); err != nil {
					t.Fatal(err)
				}
			}

			// Let "soon-expired" expire and "bin" consume ~50ms of its 150ms TTL.
			time.Sleep(55 * time.Millisecond)

			var wire bytes.Buffer
			if tc.useGZ {
				gw := gzip.NewWriter(&wire)
				if err := src.Save(gw, WithSnapshotFormat(tc.format)); err != nil {
					t.Fatalf("Save(gzip) error = %v", err)
				}
				if err := gw.Close(); err != nil {
					t.Fatalf("gzip Close error = %v", err)
				}
			} else {
				if err := src.Save(&wire, WithSnapshotFormat(tc.format)); err != nil {
					t.Fatalf("Save error = %v", err)
				}
			}

			dst, err := New(WithPreciseClock())
			if err != nil {
				t.Fatalf("New(dst) error = %v", err)
			}
			defer func() { _ = dst.Stop() }()

			var r io.Reader = &wire
			if tc.useGZ {
				gr, gerr := gzip.NewReader(&wire)
				if gerr != nil {
					t.Fatalf("gzip NewReader error = %v", gerr)
				}
				defer func() { _ = gr.Close() }()
				r = gr
			}
			if err := dst.Load(r, WithSnapshotFormat(tc.format)); err != nil {
				t.Fatalf("Load error = %v", err)
			}

			// Expired entry must NOT be restored.
			if _, ok := dst.Get("soon-expired"); ok {
				t.Fatal("expired entry was restored from snapshot")
			}

			// NoExpiration entry preserved.
			if v, ok := dst.Get("perm"); !ok || string(v) != "forever" {
				t.Fatalf("perm = (%q, %v), want (forever, true)", string(v), ok)
			}
			if e := dst.lookupEntry("perm"); e == nil || !e.isNoExpiration() {
				t.Fatalf("perm entry missing NoExpiration flag: %+v", e)
			}

			// Empty slice preserved as non-nil slice.
			if v, ok := dst.Get("empty"); !ok || v == nil || len(v) != 0 {
				t.Fatalf("empty = (%#v, %v), want non-nil empty slice", v, ok)
			}

			// Binary payload & remaining relative TTL preserved (should expire within ~110ms, NOT 150ms).
			if v, ok := dst.Get("bin"); !ok || !bytes.Equal(v, binVal) {
				t.Fatalf("bin = (%v, %v), want %v", v, ok, binVal)
			}
			if e := dst.lookupEntry("sliding-swr"); e == nil || !e.isSliding() || e.staleGrace != int64(400*time.Millisecond) {
				t.Fatalf("sliding-swr metadata not preserved: %+v", e)
			}
			for i := range 310 {
				if v, ok := dst.Get(fmt.Sprintf("bulk-%03d", i)); !ok || string(v) != "payload" {
					t.Fatalf("bulk-%03d missing: (%q, %v)", i, string(v), ok)
				}
			}

			// Wait 125ms: "bin" had ~95ms remaining on Save, so it must expire now.
			time.Sleep(125 * time.Millisecond)
			if _, ok := dst.Get("bin"); ok {
				t.Fatal("relative remaining TTL was not preserved (bin stayed alive past remaining TTL)")
			}
			if _, ok := dst.Get("perm"); !ok {
				t.Fatal("perm should remain alive")
			}
		})
	}
}

func TestSnapshot_ConflictPoliciesAndNoInvalidations(t *testing.T) {
	broker := NewInMemoryPubSub()
	defer func() { _ = broker.Close() }()

	recSrc := &spyRecorder{}
	src, err := New(WithPreciseClock())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Stop() }()

	_ = src.Set("shared", []byte("from-snap"), time.Minute)
	_ = src.Set("only-snap", []byte("snap-val"), time.Minute)
	_ = src.Set("expired-in-dst", []byte("revived"), time.Minute)

	var snapBuf bytes.Buffer
	if err := src.Save(&snapBuf); err != nil {
		t.Fatal(err)
	}

	peer, err := New(WithPubSub(broker), WithInvalidationTopic("snap:inv"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Stop() }()

	dst, err := New(
		WithPreciseClock(),
		WithPubSub(broker),
		WithInvalidationTopic("snap:inv"),
		WithMetricsRecorder(recSrc),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Stop() }()

	_ = dst.Set("shared", []byte("live-dst"), time.Minute)
	_ = dst.Set("expired-in-dst", []byte("old"), 20*time.Millisecond)
	time.Sleep(35 * time.Millisecond)
	// Set "only-snap" on peer (dst does not have it yet) and wait for its invalidation to settle.
	_ = peer.Set("only-snap", []byte("peer-copy"), time.Minute)
	time.Sleep(25 * time.Millisecond)

	// Reset invalidation recordings before Load.
	recSrc.mu.Lock()
	recSrc.invalidates = nil
	recSrc.mu.Unlock()

	// 1. KeepExisting preserves live "shared", replaces expired "expired-in-dst", inserts "only-snap".
	if err := dst.Load(bytes.NewReader(snapBuf.Bytes()), WithSnapshotConflictPolicy(SnapshotKeepExisting)); err != nil {
		t.Fatalf("Load(KeepExisting) error = %v", err)
	}
	if v, ok := dst.Get("shared"); !ok || string(v) != "live-dst" {
		t.Fatalf("KeepExisting shared = (%q, %v), want live-dst", string(v), ok)
	}
	if v, ok := dst.Get("only-snap"); !ok || string(v) != "snap-val" {
		t.Fatalf("KeepExisting only-snap = (%q, %v), want snap-val", string(v), ok)
	}
	if v, ok := dst.Get("expired-in-dst"); !ok || string(v) != "revived" {
		t.Fatalf("KeepExisting expired-in-dst = (%q, %v), want revived", string(v), ok)
	}

	// Verify Load emitted zero invalidations.
	time.Sleep(30 * time.Millisecond)
	recSrc.mu.Lock()
	invs := len(recSrc.invalidates)
	recSrc.mu.Unlock()
	if invs != 0 {
		t.Fatalf("Load emitted %d invalidation broadcasts, want 0", invs)
	}
	if v, ok := peer.Get("only-snap"); !ok || string(v) != "peer-copy" {
		t.Fatalf("peer copy was unexpectedly invalidated during dst.Load: (%q, %v)", string(v), ok)
	}

	// 2. Overwrite replaces live "shared".
	if err := dst.Load(bytes.NewReader(snapBuf.Bytes()), WithSnapshotConflictPolicy(SnapshotOverwrite)); err != nil {
		t.Fatalf("Load(Overwrite) error = %v", err)
	}
	if v, ok := dst.Get("shared"); !ok || string(v) != "from-snap" {
		t.Fatalf("Overwrite shared = (%q, %v), want from-snap", string(v), ok)
	}
}

func TestSnapshot_CorruptionAndVersionRejection(t *testing.T) {
	src, err := New(WithPreciseClock())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Stop() }()
	_ = src.Set("k1", []byte("v1"), time.Minute)
	_ = src.Set("k2", []byte("v2"), time.Minute)

	var binSnap bytes.Buffer
	if err := src.Save(&binSnap, WithSnapshotFormat(SnapshotBinary)); err != nil {
		t.Fatal(err)
	}
	validBin := binSnap.Bytes()

	var jsonSnap bytes.Buffer
	if err := src.Save(&jsonSnap, WithSnapshotFormat(SnapshotJSON)); err != nil {
		t.Fatal(err)
	}
	validJSON := jsonSnap.Bytes()

	checkReject := func(name string, format SnapshotFormat, data []byte, wantErr error, wantEmpty bool) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			dst, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dst.Stop() }()
			err = dst.Load(bytes.NewReader(data), WithSnapshotFormat(format))
			if !errors.Is(err, wantErr) {
				t.Fatalf("Load(%s) error = %v, want errors.Is(..., %v)", name, err, wantErr)
			}
			if wantEmpty {
				if m := dst.GetMetrics(); m.Items != 0 {
					t.Fatalf("corrupt single-frame load committed %d items, want 0", m.Items)
				}
			}
		})
	}

	// Binary corruption / version checks.
	checkReject("bin_empty", SnapshotBinary, nil, ErrCorruptSnapshot, true)

	badMagic := bytes.Clone(validBin)
	badMagic[0] = 'X'
	checkReject("bin_bad_magic", SnapshotBinary, badMagic, ErrCorruptSnapshot, true)

	badVer := bytes.Clone(validBin)
	badVer[6] = 2
	checkReject("bin_unsupported_version", SnapshotBinary, badVer, ErrUnsupportedSnapshotVersion, true)

	badFlags := bytes.Clone(validBin)
	badFlags[7] = 0x80
	checkReject("bin_unsupported_flags", SnapshotBinary, badFlags, ErrUnsupportedSnapshotVersion, true)

	noCRCFlag := bytes.Clone(validBin)
	noCRCFlag[7] = 0x00
	checkReject("bin_missing_crc_flag", SnapshotBinary, noCRCFlag, ErrCorruptSnapshot, true)

	badCRC := bytes.Clone(validBin)
	badCRC[20] ^= 0xFF
	checkReject("bin_corrupt_crc", SnapshotBinary, badCRC, ErrCorruptSnapshot, true)

	// Truncated before EOF sentinel (frame 1 was valid; EOF sentinel missing).
	checkReject("bin_truncated_eof", SnapshotBinary, validBin[:len(validBin)-10], ErrCorruptSnapshot, false)

	// Trailing garbage after valid EOF sentinel.
	withTrailing := append(bytes.Clone(validBin), 0x01, 0x02)
	checkReject("bin_trailing_bytes", SnapshotBinary, withTrailing, ErrCorruptSnapshot, false)

	// Malicious truncated stream claiming huge payloadLen (must fail cleanly without OOM).
	var oomAttempt bytes.Buffer
	oomAttempt.Write(validBin[:8])
	var fh [9]byte
	fh[0] = snapshotFrameData
	binary.LittleEndian.PutUint32(fh[1:5], 1)
	binary.LittleEndian.PutUint32(fh[5:9], 16*1024*1024)
	oomAttempt.Write(fh[:])
	oomAttempt.Write([]byte("short"))
	checkReject("bin_oversized_truncated_no_oom", SnapshotBinary, oomAttempt.Bytes(), ErrCorruptSnapshot, true)

	// JSON corruption / version checks.
	checkReject("json_empty", SnapshotJSON, nil, ErrCorruptSnapshot, true)
	checkReject("json_bad_header", SnapshotJSON, []byte(`{"type":"header","magic":"NOPE","version":1}`), ErrCorruptSnapshot, true)
	checkReject("json_unsupported_version", SnapshotJSON, []byte(`{"type":"header","magic":"MSNAP","version":9}`), ErrUnsupportedSnapshotVersion, true)
	checkReject("json_premature_eof", SnapshotJSON, bytes.Split(validJSON, []byte("\n"))[0], ErrCorruptSnapshot, true)
	checkReject("json_bad_base64", SnapshotJSON, []byte("{\"type\":\"header\",\"magic\":\"MSNAP\",\"version\":1}\n{\"type\":\"entry\",\"key\":\"k\",\"value\":\"***\",\"ttl_ns\":1000}\n{\"type\":\"eof\",\"count\":1}\n"), ErrCorruptSnapshot, true)
	checkReject("json_negative_ttl", SnapshotJSON, []byte("{\"type\":\"header\",\"magic\":\"MSNAP\",\"version\":1}\n{\"type\":\"entry\",\"key\":\"k\",\"value\":\"YQ==\",\"ttl_ns\":-5}\n{\"type\":\"eof\",\"count\":1}\n"), ErrCorruptSnapshot, true)
	checkReject("json_eof_count_mismatch", SnapshotJSON, []byte("{\"type\":\"header\",\"magic\":\"MSNAP\",\"version\":1}\n{\"type\":\"eof\",\"count\":42}\n"), ErrCorruptSnapshot, true)
	checkReject("json_trailing_garbage", SnapshotJSON, append(bytes.Clone(validJSON), []byte(`{"extra":true}`)...), ErrCorruptSnapshot, false)
}

func TestSnapshot_IncrementalBoundsAndEvictionHydration(t *testing.T) {
	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Stop() }()

	const total = 2000
	val := bytes.Repeat([]byte("a"), 128)
	for i := range total {
		if err := src.Set(fmt.Sprintf("k-%04d", i), val, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	var snap bytes.Buffer
	if err := src.Save(&snap); err != nil {
		t.Fatal(err)
	}

	for _, pol := range []EvictionPolicy{EvictionVolatileTTL, EvictionS3FIFO, EvictionTinyLFU} {
		t.Run(pol.String(), func(t *testing.T) {
			dst, err := New(
				WithMaxItems(300),
				WithMaxBytes(40*1024),
				WithEvictionPolicy(pol),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dst.Stop() }()

			if err := dst.Load(bytes.NewReader(snap.Bytes())); err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			m := dst.GetMetrics()
			if m.Items > 300 || m.Bytes > 40*1024 {
				t.Fatalf("bounds exceeded after Load: Items=%d (<=300) Bytes=%d (<=%d)", m.Items, m.Bytes, 40*1024)
			}
			if m.Evictions == 0 {
				t.Fatal("expected incremental capacity evictions during bounded Load")
			}

			// Verify subsequent Get + Set works cleanly with hydrated eviction structures.
			for i := range 2000 {
				dst.Get(fmt.Sprintf("k-%04d", i))
			}
			if err := dst.Set("post-load", val, time.Minute); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshot_100KStreamingO1Memory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100K streaming snapshot memory test in -short mode")
	}

	ms, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Stop() }()

	const count = 100_000
	val := bytes.Repeat([]byte("z"), 256)
	for i := range count {
		if err := ms.Set(fmt.Sprintf("snap-key-%06d", i), val, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Measure heap bytes allocated during Save of 100K x 256B (~27 MB dataset).
	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	if err := ms.Save(io.Discard, WithSnapshotFormat(SnapshotBinary)); err != nil {
		t.Fatalf("Save(100K) error = %v", err)
	}
	runtime.ReadMemStats(&mAfter)
	allocBytes := mAfter.TotalAlloc - mBefore.TotalAlloc
	if allocBytes > 512*1024 {
		t.Fatalf("Save(100K) allocated %d bytes (> 512 KiB); expected O(1) streaming buffers", allocBytes)
	}

	// 2. Stream 100K entries via io.Pipe into a bounded store (MaxItems=1000)
	// and verify incremental enforcement keeps retained items <= 1000.
	pr, pw := io.Pipe()
	saveErrCh := make(chan error, 1)
	go func() {
		err := ms.Save(pw, WithSnapshotFormat(SnapshotBinary))
		_ = pw.CloseWithError(err)
		saveErrCh <- err
	}()

	boundedDst, err := New(WithMaxItems(1000))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = boundedDst.Stop() }()

	if err := boundedDst.Load(pr); err != nil {
		t.Fatalf("Load(100K pipe) error = %v", err)
	}
	if serr := <-saveErrCh; serr != nil {
		t.Fatalf("Save(pipe) error = %v", serr)
	}

	metrics := boundedDst.GetMetrics()
	if metrics.Items > 1000 {
		t.Fatalf("bounded Load retained %d items, want <= 1000", metrics.Items)
	}

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

type chunkedReader struct {
	r     io.Reader
	chunk int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(p) > c.chunk {
		p = p[:c.chunk]
	}
	return c.r.Read(p)
}

func TestSnapshot_KeepExistingAndOverwrite_CoarseVsPreciseTTL(t *testing.T) {
	for _, format := range []SnapshotFormat{SnapshotBinary, SnapshotJSON} {
		t.Run(format.String(), func(t *testing.T) {
			// 1. Expired <10ms preciseTTL existing replaced by incoming NoExpiration and >=10ms
			// under SnapshotKeepExisting on default coarse-clock stores (no WithPreciseClock).
			t.Run("ExpiredPreciseTTL_ReplacedByNoExpAndCoarse", func(t *testing.T) {
				src, err := New()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = src.Stop() }()

				if err := src.Set("k", []byte("from-snap"), NoExpiration); err != nil {
					t.Fatal(err)
				}
				if err := src.Set("k10", []byte("from-snap-10ms"), 500*time.Millisecond); err != nil {
					t.Fatal(err)
				}

				var snap bytes.Buffer
				if err := src.Save(&snap, WithSnapshotFormat(format)); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				snapBytes := snap.Bytes()

				for trial := range 50 {
					dst, err := New()
					if err != nil {
						t.Fatal(err)
					}
					if err := dst.Set("k", []byte("ephemeral"), 500*time.Microsecond); err != nil {
						_ = dst.Stop()
						t.Fatal(err)
					}
					if err := dst.Set("k10", []byte("ephemeral10"), 500*time.Microsecond); err != nil {
						_ = dst.Stop()
						t.Fatal(err)
					}
					ek := dst.lookupEntry("k")
					ek10 := dst.lookupEntry("k10")
					if ek == nil || !ek.isPreciseTTL() || ek10 == nil || !ek10.isPreciseTTL() {
						_ = dst.Stop()
						t.Fatalf("trial %d: expected preciseTTL entries", trial)
					}
					deadline := time.Now().Add(200 * time.Millisecond)
					for dst.nowExact() < ek.hardExpiresAt() || dst.nowExact() < ek10.hardExpiresAt() {
						if time.Now().After(deadline) {
							_ = dst.Stop()
							t.Fatalf("trial %d: timed out waiting for preciseTTL expiry", trial)
						}
						runtime.Gosched()
					}

					if err := dst.Load(bytes.NewReader(snapBytes), WithSnapshotFormat(format), WithSnapshotConflictPolicy(SnapshotKeepExisting)); err != nil {
						_ = dst.Stop()
						t.Fatalf("trial %d: Load(KeepExisting) error = %v", trial, err)
					}

					if v, ok := dst.Get("k"); !ok || string(v) != "from-snap" {
						_ = dst.Stop()
						t.Fatalf("trial %d: Get(k) = (%q, %v), want (from-snap, true)", trial, string(v), ok)
					}
					if v, ok := dst.Get("k10"); !ok || string(v) != "from-snap-10ms" {
						_ = dst.Stop()
						t.Fatalf("trial %d: Get(k10) = (%q, %v), want (from-snap-10ms, true)", trial, string(v), ok)
					}
					if err := dst.Stop(); err != nil {
						t.Fatal(err)
					}
				}
			})

			// 2. Live <10ms preciseTTL existing preserved against incoming NoExpiration and >=10ms.
			t.Run("LivePreciseTTL_Preserved", func(t *testing.T) {
				src, err := New()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = src.Stop() }()
				_ = src.Set("live-fast-noexp", []byte("snap-noexp"), NoExpiration)
				_ = src.Set("live-fast-coarse", []byte("snap-coarse"), 500*time.Millisecond)

				var snap bytes.Buffer
				if err := src.Save(&snap, WithSnapshotFormat(format)); err != nil {
					t.Fatal(err)
				}

				dst, err := New()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = dst.Stop() }()

				_ = dst.Set("live-fast-noexp", []byte("keep-fast-1"), 9*time.Millisecond)
				_ = dst.Set("live-fast-coarse", []byte("keep-fast-2"), 9*time.Millisecond)
				e1 := dst.lookupEntry("live-fast-noexp")
				e2 := dst.lookupEntry("live-fast-coarse")
				if e1 == nil || !e1.isPreciseTTL() || e2 == nil || !e2.isPreciseTTL() {
					t.Fatal("expected preciseTTL entries for 9ms TTL")
				}

				if err := dst.Load(bytes.NewReader(snap.Bytes()), WithSnapshotFormat(format), WithSnapshotConflictPolicy(SnapshotKeepExisting)); err != nil {
					t.Fatalf("Load(KeepExisting) error = %v", err)
				}
				if v, ok := dst.Get("live-fast-noexp"); !ok || string(v) != "keep-fast-1" {
					t.Fatalf("Get(live-fast-noexp) = (%q, %v), want (keep-fast-1, true)", string(v), ok)
				}
				if v, ok := dst.Get("live-fast-coarse"); !ok || string(v) != "keep-fast-2" {
					t.Fatalf("Get(live-fast-coarse) = (%q, %v), want (keep-fast-2, true)", string(v), ok)
				}
			})

			// 3. Live >=10ms coarse existing (and SWR grace window) preserved against incoming <10ms
			// preciseTTL under KeepExisting; expired preciseTTL SWR replaced.
			t.Run("CoarseLiveAndSWR_VsIncomingPreciseTTL", func(t *testing.T) {
				src, err := New()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = src.Stop() }()
				_ = src.Set("coarse-live", []byte("snap-fast"), 5*time.Millisecond)
				_ = src.Set("swr-coarse", []byte("snap-fast"), 5*time.Millisecond)
				_ = src.Set("swr-fast-exp", []byte("revived"), NoExpiration)

				var snap bytes.Buffer
				if err := src.Save(&snap, WithSnapshotFormat(format)); err != nil {
					t.Fatal(err)
				}

				dst, err := New()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = dst.Stop() }()

				_ = dst.Set("coarse-live", []byte("keep-coarse"), 500*time.Millisecond)
				_ = dst.SetWithOptions("swr-coarse", []byte("stale-coarse"), 15*time.Millisecond, WithStaleWindow(500*time.Millisecond))
				_ = dst.SetWithOptions("swr-fast-exp", []byte("old-fast"), 500*time.Microsecond, WithStaleWindow(500*time.Microsecond))

				eCoarse := dst.lookupEntry("coarse-live")
				eSWR := dst.lookupEntry("swr-coarse")
				eFast := dst.lookupEntry("swr-fast-exp")
				if eCoarse == nil || eCoarse.isPreciseTTL() || eSWR == nil || eSWR.isPreciseTTL() || eFast == nil || !eFast.isPreciseTTL() {
					t.Fatal("unexpected preciseTTL flags on test entries")
				}

				// Wait until swr-coarse enters its SWR stale window and swr-fast-exp passes hardExpiresAt.
				deadline := time.Now().Add(300 * time.Millisecond)
				for dst.entryNow(eSWR) < eSWR.expiresAt.Load() || dst.nowExact() < eFast.hardExpiresAt() {
					if time.Now().After(deadline) {
						t.Fatal("timed out waiting for swr-coarse soft expiry and swr-fast-exp hard expiry")
					}
					time.Sleep(2 * time.Millisecond)
				}
				if _, ok := dst.Get("swr-coarse"); ok {
					t.Fatal("expected swr-coarse Get() to miss inside stale window")
				}

				if err := dst.Load(bytes.NewReader(snap.Bytes()), WithSnapshotFormat(format), WithSnapshotConflictPolicy(SnapshotKeepExisting)); err != nil {
					t.Fatalf("Load(KeepExisting) error = %v", err)
				}

				if v, ok := dst.Get("coarse-live"); !ok || string(v) != "keep-coarse" {
					t.Fatalf("coarse-live = (%q, %v), want (keep-coarse, true)", string(v), ok)
				}
				if got := dst.lookupEntry("swr-coarse"); got == nil || string(got.value) != "stale-coarse" || got.staleGrace != int64(500*time.Millisecond) {
					t.Fatalf("swr-coarse was overwritten by KeepExisting: %+v", got)
				}
				gotSWR, err := dst.GetOrLoad(context.Background(), "swr-coarse", 15*time.Millisecond, func(context.Context) ([]byte, error) {
					return []byte("refreshed"), nil
				})
				if err != nil || string(gotSWR) != "stale-coarse" {
					t.Fatalf("GetOrLoad(swr-coarse) = (%q, %v), want (stale-coarse, nil)", string(gotSWR), err)
				}
				if v, ok := dst.Get("swr-fast-exp"); !ok || string(v) != "revived" {
					t.Fatalf("swr-fast-exp = (%q, %v), want (revived, true)", string(v), ok)
				}
			})

			// 4. Bounded SnapshotOverwrite & expired-KeepExisting accounting across EvictionS3FIFO and EvictionTinyLFU.
			for _, pol := range []EvictionPolicy{EvictionS3FIFO, EvictionTinyLFU} {
				t.Run("BoundedAccounting_"+pol.String(), func(t *testing.T) {
					src, err := New()
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = src.Stop() }()

					newVal := bytes.Repeat([]byte("n"), 31) // payloadSize("k", 31B) = 32B
					_ = src.Set("k", newVal, NoExpiration)

					var snap bytes.Buffer
					if err := src.Save(&snap, WithSnapshotFormat(format)); err != nil {
						t.Fatal(err)
					}
					snapBytes := snap.Bytes()

					dst, err := New(WithMaxItems(2), WithMaxBytes(64), WithEvictionPolicy(pol))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = dst.Stop() }()

					// Seed expired 500us "k" (1+11=12B) + live "anchor" (6+6=12B).
					_ = dst.Set("k", bytes.Repeat([]byte("o"), 11), 500*time.Microsecond)
					_ = dst.Set("anchor", []byte("anchr1"), NoExpiration)

					ek := dst.lookupEntry("k")
					deadline := time.Now().Add(200 * time.Millisecond)
					for dst.nowExact() < ek.hardExpiresAt() {
						if time.Now().After(deadline) {
							t.Fatal("timed out waiting for bounded preciseTTL expiry")
						}
						runtime.Gosched()
					}

					if err := dst.Load(bytes.NewReader(snapBytes), WithSnapshotFormat(format), WithSnapshotConflictPolicy(SnapshotKeepExisting)); err != nil {
						t.Fatalf("Load(KeepExisting) error = %v", err)
					}
					m := dst.GetMetrics()
					if m.Items != 2 || m.Bytes != 44 {
						t.Fatalf("after KeepExisting metrics = %+v, want Items=2 Bytes=44", m)
					}
					if e := dst.lookupEntry("k"); e == nil || !e.isNoExpiration() {
						t.Fatalf("k missing NoExpiration after KeepExisting replace: %+v", e)
					}
					if v, ok := dst.Get("k"); !ok || !bytes.Equal(v, newVal) {
						t.Fatalf("Get(k) mismatch after KeepExisting replace: ok=%v len=%d", ok, len(v))
					}
					if v, ok := dst.Get("anchor"); !ok || string(v) != "anchr1" {
						t.Fatalf("Get(anchor) = (%q, %v), want (anchr1, true)", string(v), ok)
					}

					// Overwrite live "k" (32B) with 12B value under SnapshotOverwrite.
					var snap2 bytes.Buffer
					src2, err := New()
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = src2.Stop() }()
					_ = src2.Set("k", bytes.Repeat([]byte("x"), 11), NoExpiration)
					if err := src2.Save(&snap2, WithSnapshotFormat(format)); err != nil {
						t.Fatal(err)
					}

					if err := dst.Load(bytes.NewReader(snap2.Bytes()), WithSnapshotFormat(format), WithSnapshotConflictPolicy(SnapshotOverwrite)); err != nil {
						t.Fatalf("Load(Overwrite) error = %v", err)
					}
					m2 := dst.GetMetrics()
					if m2.Items != 2 || m2.Bytes != 24 {
						t.Fatalf("after Overwrite metrics = %+v, want Items=2 Bytes=24", m2)
					}
					if v, ok := dst.Get("k"); !ok || string(v) != strings.Repeat("x", 11) {
						t.Fatalf("Get(k) after Overwrite = (%q, %v)", string(v), ok)
					}
					if err := dst.Set("anchor", []byte("anchr2"), NoExpiration); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestSnapshot_MaxKeyAndValueBoundaries(t *testing.T) {
	maxVal := bytes.Repeat([]byte{0x5a, 0xa5, 0x00, 0xff}, maxSnapshotValueBytes/4)

	keyCases := []struct {
		name string
		key  string
	}{
		{"ASCII64KiB", strings.Repeat("a", maxSnapshotKeyBytes)},
		{"NulEscaped64KiB", strings.Repeat("\x00", maxSnapshotKeyBytes)},
		{"QuoteEscaped64KiB", strings.Repeat(`"`, maxSnapshotKeyBytes)},
	}

	for _, format := range []SnapshotFormat{SnapshotBinary, SnapshotJSON} {
		t.Run("Roundtrip_"+format.String(), func(t *testing.T) {
			for _, kc := range keyCases {
				t.Run(kc.name, func(t *testing.T) {
					src, err := New(WithPreciseClock())
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = src.Stop() }()

					if err := src.SetWithOptions(kc.key, maxVal, time.Minute, WithSlidingExpiration(), WithStaleWindow(2*time.Minute)); err != nil {
						t.Fatal(err)
					}
					if err := src.Set("trailing", []byte("ok"), NoExpiration); err != nil {
						t.Fatal(err)
					}

					var snap bytes.Buffer
					if err := src.Save(&snap, WithSnapshotFormat(format)); err != nil {
						t.Fatalf("Save(%s, %s) error = %v", format, kc.name, err)
					}

					dst, err := New(WithPreciseClock())
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = dst.Stop() }()

					r := io.Reader(bytes.NewReader(snap.Bytes()))
					if kc.name == "NulEscaped64KiB" {
						r = &chunkedReader{r: r, chunk: 32 * 1024}
					}
					if err := dst.Load(r, WithSnapshotFormat(format)); err != nil {
						t.Fatalf("Load(%s, %s) error = %v", format, kc.name, err)
					}

					if m := dst.GetMetrics(); m.Items != 2 {
						t.Fatalf("Items = %d, want 2", m.Items)
					}
					if got, ok := dst.Get(kc.key); !ok || !bytes.Equal(got, maxVal) {
						t.Fatalf("Get(maxKey) ok=%v len=%d, want len=%d", ok, len(got), len(maxVal))
					}
					if got, ok := dst.Get("trailing"); !ok || string(got) != "ok" {
						t.Fatalf("Get(trailing) = (%q, %v), want (ok, true)", string(got), ok)
					}
				})
			}
		})

		t.Run("SaveRejectsOversize_"+format.String(), func(t *testing.T) {
			sKey, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sKey.Stop() }()
			_ = sKey.Set(strings.Repeat("k", maxSnapshotKeyBytes+1), []byte("v"), NoExpiration)
			if err := sKey.Save(io.Discard, WithSnapshotFormat(format)); err == nil {
				t.Fatalf("Save(%s) accepted 64 KiB + 1 key, want error", format)
			}

			sVal, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sVal.Stop() }()
			_ = sVal.Set("k", make([]byte, maxSnapshotValueBytes+1), NoExpiration)
			if err := sVal.Save(io.Discard, WithSnapshotFormat(format)); err == nil {
				t.Fatalf("Save(%s) accepted 16 MiB + 1 value, want error", format)
			}
		})
	}

	t.Run("LoadRejectsOversize_Binary", func(t *testing.T) {
		writeBinRecord := func(keyLen, valLen uint32) []byte {
			var buf bytes.Buffer
			buf.Write(snapshotMagic[:])
			buf.WriteByte(snapshotBinaryVersion)
			buf.WriteByte(snapshotFlagHasCRC)

			noExpTTL := int64(NoExpiration)
			payload := make([]byte, snapshotMinRecordBytes+int(keyLen)+int(valLen))
			payload[0] = snapRecNoExpiration
			binary.LittleEndian.PutUint64(payload[1:9], uint64(noExpTTL))
			binary.LittleEndian.PutUint32(payload[9:13], keyLen)
			binary.LittleEndian.PutUint32(payload[13:17], valLen)

			var fhdr [snapshotFrameHeaderSize]byte
			fhdr[0] = snapshotFrameData
			binary.LittleEndian.PutUint32(fhdr[1:5], 1)
			binary.LittleEndian.PutUint32(fhdr[5:9], uint32(len(payload)))
			crc := crc32.Update(crc32.Checksum(fhdr[:], castagnoliTable), castagnoliTable, payload)

			buf.Write(fhdr[:])
			buf.Write(payload)
			_ = binary.Write(&buf, binary.LittleEndian, crc)
			return buf.Bytes()
		}

		dst, err := New()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = dst.Stop() }()

		if err := dst.Load(bytes.NewReader(writeBinRecord(maxSnapshotKeyBytes+1, 0)), WithSnapshotFormat(SnapshotBinary)); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("Load(binary, key=64KiB+1) = %v, want ErrCorruptSnapshot", err)
		}
		if err := dst.Load(bytes.NewReader(writeBinRecord(1, maxSnapshotValueBytes+1)), WithSnapshotFormat(SnapshotBinary)); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("Load(binary, val=16MiB+1) = %v, want ErrCorruptSnapshot", err)
		}

		// Frame payloadLen > maxSnapshotFrameBytes.
		var frameHdrOnly bytes.Buffer
		frameHdrOnly.Write(snapshotMagic[:])
		frameHdrOnly.WriteByte(snapshotBinaryVersion)
		frameHdrOnly.WriteByte(snapshotFlagHasCRC)
		var fhdr [snapshotFrameHeaderSize]byte
		fhdr[0] = snapshotFrameData
		binary.LittleEndian.PutUint32(fhdr[1:5], 1)
		binary.LittleEndian.PutUint32(fhdr[5:9], maxSnapshotFrameBytes+1)
		frameHdrOnly.Write(fhdr[:])
		if err := dst.Load(&frameHdrOnly, WithSnapshotFormat(SnapshotBinary)); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("Load(binary, frame>max) = %v, want ErrCorruptSnapshot", err)
		}
	})

	t.Run("LoadRejectsOversize_JSON", func(t *testing.T) {
		dst, err := New()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = dst.Stop() }()

		header := "{\"type\":\"header\",\"magic\":\"MSNAP\",\"version\":1}\n"
		eof := "{\"type\":\"eof\",\"count\":1}\n"

		// 1. Key = 64 KiB + 1.
		overKey := header +
			`{"type":"entry","key":"` + strings.Repeat("k", maxSnapshotKeyBytes+1) + `","value":"YQ==","ttl_ns":-1}` + "\n" +
			eof
		if err := dst.Load(strings.NewReader(overKey), WithSnapshotFormat(SnapshotJSON)); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("Load(json, key=64KiB+1) = %v, want ErrCorruptSnapshot", err)
		}

		// 2. Value = 16 MiB + 1 (same EncodedLen as 16 MiB, caught by post-decode len check).
		b64Plus1 := base64.StdEncoding.EncodeToString(make([]byte, maxSnapshotValueBytes+1))
		overVal1 := header +
			`{"type":"entry","key":"k","value":"` + b64Plus1 + `","ttl_ns":-1}` + "\n" +
			eof
		if err := dst.Load(strings.NewReader(overVal1), WithSnapshotFormat(SnapshotJSON)); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("Load(json, val=16MiB+1) = %v, want ErrCorruptSnapshot", err)
		}

		// 3. Value = 16 MiB + 3 (caught by pre-decode EncodedLen guard).
		b64Plus3 := base64.StdEncoding.EncodeToString(make([]byte, maxSnapshotValueBytes+3))
		overVal3 := header +
			`{"type":"entry","key":"k","value":"` + b64Plus3 + `","ttl_ns":-1}` + "\n" +
			eof
		if err := dst.Load(strings.NewReader(overVal3), WithSnapshotFormat(SnapshotJSON)); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("Load(json, val=16MiB+3) = %v, want ErrCorruptSnapshot", err)
		}
	})
}

type repeatByteReader byte

func (r repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestSnapshot_LoadRejectsMalformedOversizedJSONRecord(t *testing.T) {
	ms, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Stop() }()

	header := "{\"type\":\"header\",\"magic\":\"MSNAP\",\"version\":1}\n"
	prefix := `{"type":"entry","key":"k","value":"`
	r := io.MultiReader(
		strings.NewReader(header),
		strings.NewReader(prefix),
		io.LimitReader(repeatByteReader('A'), 24*1024*1024),
	)
	err = ms.Load(r, WithSnapshotFormat(SnapshotJSON))
	if !errors.Is(err, ErrCorruptSnapshot) {
		t.Fatalf("Load(oversized JSON stream) = %v, want ErrCorruptSnapshot", err)
	}
}
