package memorystore

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"time"
)

// SnapshotFormat selects the serialization format used by Save and Load.
type SnapshotFormat uint8

const (
	// SnapshotBinary is the default compact framed binary format with per-frame
	// CRC32C checksums and an explicit EOF sentinel frame.
	SnapshotBinary SnapshotFormat = iota
	// SnapshotJSON streams newline-delimited JSON records using encoding/json/jsontext.
	SnapshotJSON
)

// String returns a human-readable name for the SnapshotFormat.
func (f SnapshotFormat) String() string {
	switch f {
	case SnapshotBinary:
		return "binary"
	case SnapshotJSON:
		return "json"
	default:
		return "unknown"
	}
}

// SnapshotConflictPolicy controls how Load handles keys that already exist in
// the target MemoryStore.
type SnapshotConflictPolicy uint8

const (
	// SnapshotOverwrite replaces any existing entry with the snapshot entry (default).
	SnapshotOverwrite SnapshotConflictPolicy = iota
	// SnapshotKeepExisting preserves existing live entries (including entries
	// within an active Stale-While-Revalidate grace window where now < hardExpiresAt())
	// and skips conflicting entries from the snapshot. Hard-expired existing
	// entries are still replaced.
	SnapshotKeepExisting
)

// String returns a human-readable name for the SnapshotConflictPolicy.
func (p SnapshotConflictPolicy) String() string {
	switch p {
	case SnapshotOverwrite:
		return "overwrite"
	case SnapshotKeepExisting:
		return "keep-existing"
	default:
		return "unknown"
	}
}

type snapshotOptions struct {
	format         SnapshotFormat
	conflictPolicy SnapshotConflictPolicy
}

// SnapshotOption configures MemoryStore.Save and MemoryStore.Load.
type SnapshotOption func(*snapshotOptions)

// WithSnapshotFormat selects the snapshot serialization format
// (SnapshotBinary or SnapshotJSON).
func WithSnapshotFormat(f SnapshotFormat) SnapshotOption {
	return func(o *snapshotOptions) {
		o.format = f
	}
}

// WithSnapshotConflictPolicy selects how Load handles existing live keys
// (SnapshotOverwrite or SnapshotKeepExisting).
func WithSnapshotConflictPolicy(p SnapshotConflictPolicy) SnapshotOption {
	return func(o *snapshotOptions) {
		o.conflictPolicy = p
	}
}

func resolveSnapshotOptions(opts []SnapshotOption) (snapshotOptions, error) {
	var cfg snapshotOptions
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&cfg)
	}
	switch cfg.format {
	case SnapshotBinary, SnapshotJSON:
	default:
		return cfg, fmt.Errorf("unknown snapshot format %d", cfg.format)
	}
	switch cfg.conflictPolicy {
	case SnapshotOverwrite, SnapshotKeepExisting:
	default:
		return cfg, fmt.Errorf("unknown snapshot conflict policy %d", cfg.conflictPolicy)
	}
	return cfg, nil
}

var snapshotMagic = [6]byte{'M', 'S', 'N', 'A', 'P', 0x02}

const (
	snapshotBinaryVersion uint8 = 1
	snapshotFlagHasCRC    uint8 = 1 << 0

	snapshotFrameData uint8 = 0x01
	snapshotFrameEOF  uint8 = 0x02

	snapRecNoExpiration  uint8 = 1 << 0
	snapRecSliding       uint8 = 1 << 1
	snapRecHasStaleGrace uint8 = 1 << 2
	snapRecValidMask     uint8 = snapRecNoExpiration | snapRecSliding | snapRecHasStaleGrace

	snapshotStreamHeaderSize = 8  // 6B magic + 1B version + 1B flags
	snapshotFrameHeaderSize  = 9  // 1B type + 4B entryCount + 4B payloadLen
	snapshotCRCSize          = 4  // 4B CRC32C (Castagnoli)
	snapshotEOFPayloadSize   = 12 // 8B totalEntries + 4B streamDigest
	snapshotMinRecordBytes   = 17 // 1B recFlags + 8B remainingTTL + 4B keyLen + 4B valLen

	snapshotMaxFrameEntries  = 256
	snapshotTargetFrameBytes = 64 * 1024
	maxSnapshotKeyBytes      = 64 * 1024
	maxSnapshotValueBytes    = 16 * 1024 * 1024
	maxSnapshotFrameBytes    = maxSnapshotKeyBytes + maxSnapshotValueBytes + 64
	snapshotReadChunkLimit   = 64 * 1024
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

type snapDecodedEntry struct {
	key          []byte
	value        []byte
	remainingTTL int64
	ttlNanos     int64
	staleGrace   int64
	noExpiration bool
	sliding      bool
}

// Save streams all unexpired cache entries to w without acquiring shard locks
// or buffering the entire dataset in memory. Relative remaining TTLs and
// NoExpiration (-1) are preserved; already-expired entries are skipped.
//
// Callers may wrap w with compress/gzip (or any io.Writer filter) for
// compressed snapshots. Returns ErrStoreStopped if the store has been stopped.
func (m *MemoryStore) Save(w io.Writer, opts ...SnapshotOption) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}
	cfg, err := resolveSnapshotOptions(opts)
	if err != nil {
		return err
	}
	if w == nil {
		return errors.New("snapshot writer must not be nil")
	}

	switch cfg.format {
	case SnapshotBinary:
		return m.saveBinary(w)
	case SnapshotJSON:
		return m.saveJSON(w)
	default:
		return fmt.Errorf("unknown snapshot format %d", cfg.format)
	}
}

// Load streams cache entries from r into the store, verifying frame checksums
// and record integrity before committing entries to shards. Relative remaining
// TTLs and NoExpiration (-1) are restored relative to the current monotonic
// clock, and MaxItems/MaxBytes bounds are enforced incrementally per entry.
//
// Load is local node hydration and does NOT broadcast distributed invalidation
// events over Config.InvalidationTopic. Returns ErrStoreStopped if the store
// has been stopped, ErrCorruptSnapshot on framing/checksum/syntax errors, or
// ErrUnsupportedSnapshotVersion on incompatible snapshot versions.
func (m *MemoryStore) Load(r io.Reader, opts ...SnapshotOption) error {
	if m.stopped.Load() {
		return ErrStoreStopped
	}
	cfg, err := resolveSnapshotOptions(opts)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("snapshot reader must not be nil")
	}

	switch cfg.format {
	case SnapshotBinary:
		return m.loadBinary(r, cfg.conflictPolicy)
	case SnapshotJSON:
		return m.loadJSON(r, cfg.conflictPolicy)
	default:
		return fmt.Errorf("unknown snapshot format %d", cfg.format)
	}
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (m *MemoryStore) saveBinary(w io.Writer) error {
	var hdr [snapshotStreamHeaderSize]byte
	copy(hdr[0:6], snapshotMagic[:])
	hdr[6] = snapshotBinaryVersion
	hdr[7] = snapshotFlagHasCRC
	if err := writeFull(w, hdr[:]); err != nil {
		return err
	}

	frameBuf := make([]byte, snapshotFrameHeaderSize, snapshotTargetFrameBytes+4096)
	var frameEntries uint32
	var totalEntries uint64
	var streamDigest uint32

	flushFrame := func() error {
		if frameEntries == 0 {
			return nil
		}
		payloadLen := len(frameBuf) - snapshotFrameHeaderSize
		frameBuf[0] = snapshotFrameData
		binary.LittleEndian.PutUint32(frameBuf[1:5], frameEntries)
		binary.LittleEndian.PutUint32(frameBuf[5:9], uint32(payloadLen))

		frameCRC := crc32.Checksum(frameBuf, castagnoliTable)
		streamDigest = crc32.Update(streamDigest, castagnoliTable, frameBuf[snapshotFrameHeaderSize:])
		frameBuf = binary.LittleEndian.AppendUint32(frameBuf, frameCRC)

		if err := writeFull(w, frameBuf); err != nil {
			return err
		}
		frameBuf = frameBuf[:snapshotFrameHeaderSize]
		frameEntries = 0
		return nil
	}

	for i := range m.activeShards() {
		if m.stopped.Load() {
			return ErrStoreStopped
		}
		s := &m.shards[i]
		tbl := s.table.Load()
		if tbl == nil || m.stopped.Load() {
			return ErrStoreStopped
		}
		now := m.nowExact()

		for bIdx := range tbl.buckets {
			b := &tbl.buckets[bIdx]
			ctrl := b.ctrl.Load()
			if ctrl&slotCtrlMask == 0 {
				continue
			}
			for j := range bucketSlots {
				if (ctrl>>(j*8))&0xFF == 0 {
					continue
				}
				e := b.slots[j].Load()
				if e == nil {
					continue
				}

				var recFlags uint8
				var remainingTTL int64
				if e.isNoExpiration() {
					recFlags |= snapRecNoExpiration
					remainingTTL = int64(NoExpiration)
				} else {
					softExp := e.expiresAt.Load()
					if now >= softExp {
						continue
					}
					remainingTTL = softExp - now
					if e.isSliding() && e.ttlNanos > 0 {
						recFlags |= snapRecSliding
					}
					if e.staleGrace > 0 {
						recFlags |= snapRecHasStaleGrace
					}
				}

				keyLen := len(e.key)
				valLen := len(e.value)
				if keyLen > maxSnapshotKeyBytes {
					return fmt.Errorf("snapshot key length %d exceeds max %d", keyLen, maxSnapshotKeyBytes)
				}
				if valLen > maxSnapshotValueBytes {
					return fmt.Errorf("snapshot value length %d exceeds max %d", valLen, maxSnapshotValueBytes)
				}

				recLen := snapshotMinRecordBytes + keyLen + valLen
				if recFlags&snapRecSliding != 0 {
					recLen += 8
				}
				if recFlags&snapRecHasStaleGrace != 0 {
					recLen += 8
				}

				if frameEntries > 0 && (frameEntries >= snapshotMaxFrameEntries || len(frameBuf)+recLen > snapshotTargetFrameBytes) {
					if err := flushFrame(); err != nil {
						return err
					}
				}

				frameBuf = append(frameBuf, recFlags)
				frameBuf = binary.LittleEndian.AppendUint64(frameBuf, uint64(remainingTTL))
				frameBuf = binary.LittleEndian.AppendUint32(frameBuf, uint32(keyLen))
				frameBuf = binary.LittleEndian.AppendUint32(frameBuf, uint32(valLen))
				if recFlags&snapRecSliding != 0 {
					frameBuf = binary.LittleEndian.AppendUint64(frameBuf, uint64(e.ttlNanos))
				}
				if recFlags&snapRecHasStaleGrace != 0 {
					frameBuf = binary.LittleEndian.AppendUint64(frameBuf, uint64(e.staleGrace))
				}
				frameBuf = append(frameBuf, e.key...)
				frameBuf = append(frameBuf, e.value...)

				frameEntries++
				totalEntries++
			}
		}
	}

	if m.stopped.Load() {
		return ErrStoreStopped
	}
	if err := flushFrame(); err != nil {
		return err
	}

	var eofFrame [snapshotFrameHeaderSize + snapshotEOFPayloadSize + snapshotCRCSize]byte
	eofFrame[0] = snapshotFrameEOF
	binary.LittleEndian.PutUint32(eofFrame[1:5], 0)
	binary.LittleEndian.PutUint32(eofFrame[5:9], snapshotEOFPayloadSize)
	binary.LittleEndian.PutUint64(eofFrame[9:17], totalEntries)
	binary.LittleEndian.PutUint32(eofFrame[17:21], streamDigest)
	eofCRC := crc32.Checksum(eofFrame[:snapshotFrameHeaderSize+snapshotEOFPayloadSize], castagnoliTable)
	binary.LittleEndian.PutUint32(eofFrame[21:25], eofCRC)

	return writeFull(w, eofFrame[:])
}

func readChunked(r io.Reader, buf *[]byte, total int) error {
	dst := (*buf)[:0]
	for len(dst) < total {
		rem := total - len(dst)
		step := min(rem, snapshotReadChunkLimit)
		curLen := len(dst)
		if cap(dst) < curLen+step {
			grown := make([]byte, curLen, curLen+step)
			copy(grown, dst)
			dst = grown
		}
		dst = dst[:curLen+step]
		if _, err := io.ReadFull(r, dst[curLen:curLen+step]); err != nil {
			*buf = dst
			return err
		}
	}
	*buf = dst
	return nil
}

func (m *MemoryStore) loadBinary(r io.Reader, conflictPolicy SnapshotConflictPolicy) error {
	var hdr [snapshotStreamHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("%w: reading stream header: %v", ErrCorruptSnapshot, err)
	}
	if [6]byte(hdr[0:6]) != snapshotMagic {
		return fmt.Errorf("%w: invalid snapshot magic header", ErrCorruptSnapshot)
	}
	if hdr[6] != snapshotBinaryVersion {
		return fmt.Errorf("%w: version %d", ErrUnsupportedSnapshotVersion, hdr[6])
	}
	if hdr[7]&^snapshotFlagHasCRC != 0 {
		return fmt.Errorf("%w: unsupported flags 0x%02x", ErrUnsupportedSnapshotVersion, hdr[7])
	}
	if hdr[7]&snapshotFlagHasCRC == 0 {
		return fmt.Errorf("%w: missing required CRC32C flag", ErrCorruptSnapshot)
	}

	frameBuf := make([]byte, 0, snapshotTargetFrameBytes+4096)
	decoded := make([]snapDecodedEntry, 0, snapshotMaxFrameEntries)
	var fhdr [snapshotFrameHeaderSize]byte
	var totalEntries uint64
	var streamDigest uint32

	for {
		if m.stopped.Load() {
			return ErrStoreStopped
		}

		if _, err := io.ReadFull(r, fhdr[:]); err != nil {
			return fmt.Errorf("%w: reading frame header: %v", ErrCorruptSnapshot, err)
		}

		frameType := fhdr[0]
		entryCount := binary.LittleEndian.Uint32(fhdr[1:5])
		payloadLen := binary.LittleEndian.Uint32(fhdr[5:9])

		switch frameType {
		case snapshotFrameEOF:
			if entryCount != 0 || payloadLen != snapshotEOFPayloadSize {
				return fmt.Errorf("%w: invalid EOF frame metadata (count=%d len=%d)", ErrCorruptSnapshot, entryCount, payloadLen)
			}
			var eofBuf [snapshotEOFPayloadSize + snapshotCRCSize]byte
			if _, err := io.ReadFull(r, eofBuf[:]); err != nil {
				return fmt.Errorf("%w: reading EOF frame: %v", ErrCorruptSnapshot, err)
			}
			gotCRC := binary.LittleEndian.Uint32(eofBuf[snapshotEOFPayloadSize:])
			wantCRC := crc32.Update(crc32.Checksum(fhdr[:], castagnoliTable), castagnoliTable, eofBuf[:snapshotEOFPayloadSize])
			if gotCRC != wantCRC {
				return fmt.Errorf("%w: EOF frame CRC32C mismatch", ErrCorruptSnapshot)
			}
			eofTotal := binary.LittleEndian.Uint64(eofBuf[0:8])
			eofDigest := binary.LittleEndian.Uint32(eofBuf[8:12])
			if eofTotal != totalEntries || eofDigest != streamDigest {
				return fmt.Errorf("%w: EOF sentinel mismatch (got count=%d digest=0x%08x, want count=%d digest=0x%08x)",
					ErrCorruptSnapshot, eofTotal, eofDigest, totalEntries, streamDigest)
			}
			var trailing [1]byte
			n, err := r.Read(trailing[:])
			if n != 0 || !errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: unexpected trailing data after EOF sentinel", ErrCorruptSnapshot)
			}
			return nil

		case snapshotFrameData:
			if entryCount == 0 || entryCount > snapshotMaxFrameEntries {
				return fmt.Errorf("%w: invalid frame entry count %d", ErrCorruptSnapshot, entryCount)
			}
			if payloadLen > maxSnapshotFrameBytes || uint64(payloadLen) < uint64(entryCount)*snapshotMinRecordBytes {
				return fmt.Errorf("%w: invalid frame payload length %d", ErrCorruptSnapshot, payloadLen)
			}

			if err := readChunked(r, &frameBuf, int(payloadLen)+snapshotCRCSize); err != nil {
				return fmt.Errorf("%w: reading data frame: %v", ErrCorruptSnapshot, err)
			}

			payload := frameBuf[:payloadLen]
			gotCRC := binary.LittleEndian.Uint32(frameBuf[payloadLen : int(payloadLen)+snapshotCRCSize])
			wantCRC := crc32.Update(crc32.Checksum(fhdr[:], castagnoliTable), castagnoliTable, payload)
			if gotCRC != wantCRC {
				return fmt.Errorf("%w: data frame CRC32C mismatch", ErrCorruptSnapshot)
			}

			decoded = decoded[:0]
			off := 0
			for range entryCount {
				if off+snapshotMinRecordBytes > len(payload) {
					return fmt.Errorf("%w: truncated record header in frame", ErrCorruptSnapshot)
				}
				recFlags := payload[off]
				off++
				if recFlags&^snapRecValidMask != 0 {
					return fmt.Errorf("%w: invalid record flags 0x%02x", ErrCorruptSnapshot, recFlags)
				}
				noExp := recFlags&snapRecNoExpiration != 0
				sliding := recFlags&snapRecSliding != 0
				hasStale := recFlags&snapRecHasStaleGrace != 0
				if noExp && (sliding || hasStale) {
					return fmt.Errorf("%w: invalid flag combination 0x%02x", ErrCorruptSnapshot, recFlags)
				}

				remainingTTL := int64(binary.LittleEndian.Uint64(payload[off : off+8]))
				off += 8
				if noExp {
					if remainingTTL != int64(NoExpiration) {
						return fmt.Errorf("%w: NoExpiration record has TTL %d", ErrCorruptSnapshot, remainingTTL)
					}
				} else if remainingTTL < 0 {
					return fmt.Errorf("%w: negative record TTL %d", ErrCorruptSnapshot, remainingTTL)
				}

				keyLen := binary.LittleEndian.Uint32(payload[off : off+4])
				off += 4
				valLen := binary.LittleEndian.Uint32(payload[off : off+4])
				off += 4
				if keyLen > maxSnapshotKeyBytes || valLen > maxSnapshotValueBytes {
					return fmt.Errorf("%w: record key/value length exceeds guard (%d/%d)", ErrCorruptSnapshot, keyLen, valLen)
				}

				var ttlNanos int64
				if sliding {
					if off+8 > len(payload) {
						return fmt.Errorf("%w: truncated sliding TTL field", ErrCorruptSnapshot)
					}
					ttlNanos = int64(binary.LittleEndian.Uint64(payload[off : off+8]))
					off += 8
					if ttlNanos <= 0 {
						return fmt.Errorf("%w: invalid sliding TTL capacity %d", ErrCorruptSnapshot, ttlNanos)
					}
				}

				var staleGrace int64
				if hasStale {
					if off+8 > len(payload) {
						return fmt.Errorf("%w: truncated stale grace field", ErrCorruptSnapshot)
					}
					staleGrace = int64(binary.LittleEndian.Uint64(payload[off : off+8]))
					off += 8
					if staleGrace <= 0 {
						return fmt.Errorf("%w: invalid stale grace %d", ErrCorruptSnapshot, staleGrace)
					}
				}

				needBytes := uint64(keyLen) + uint64(valLen)
				if needBytes > uint64(len(payload)-off) {
					return fmt.Errorf("%w: truncated record key/value payload", ErrCorruptSnapshot)
				}
				kBytes := payload[off : off+int(keyLen)]
				off += int(keyLen)
				vBytes := payload[off : off+int(valLen)]
				off += int(valLen)

				decoded = append(decoded, snapDecodedEntry{
					key:          kBytes,
					value:        vBytes,
					remainingTTL: remainingTTL,
					ttlNanos:     ttlNanos,
					staleGrace:   staleGrace,
					noExpiration: noExp,
					sliding:      sliding,
				})
			}
			if off != len(payload) {
				return fmt.Errorf("%w: trailing %d bytes inside data frame", ErrCorruptSnapshot, len(payload)-off)
			}

			streamDigest = crc32.Update(streamDigest, castagnoliTable, payload)
			totalEntries += uint64(entryCount)

			for i := range decoded {
				if err := m.commitSnapshotEntry(&decoded[i], conflictPolicy); err != nil {
					return err
				}
			}

		default:
			return fmt.Errorf("%w: unknown frame type 0x%02x", ErrCorruptSnapshot, frameType)
		}
	}
}

func (m *MemoryStore) commitSnapshotEntry(rec *snapDecodedEntry, conflictPolicy SnapshotConflictPolicy) error {
	if !rec.noExpiration && rec.remainingTTL == 0 {
		return nil
	}

	key := string(rec.key)
	var val []byte
	if len(rec.value) > 0 {
		val = make([]byte, len(rec.value))
		copy(val, rec.value)
	} else {
		val = []byte{}
	}

	h, idx := m.shardHash(key)
	var flags uint8
	var now int64
	var exp int64
	var ttlNanos int64
	var staleGrace int64

	if rec.noExpiration {
		flags = entryFlagNoExpiration
		exp = math.MaxInt64
		now = m.nowCoarse()
	} else {
		ttl := time.Duration(rec.remainingTTL)
		if m.preciseClock || ttl < preciseTTLThreshold {
			flags |= entryFlagPreciseTTL
			now = m.nowExact()
		} else {
			now = m.nowCoarse()
		}
		if rec.sliding {
			flags |= entryFlagSliding
			ttlNanos = rec.ttlNanos
			if ttlNanos <= 0 {
				ttlNanos = rec.remainingTTL
			}
		}
		if rec.staleGrace > 0 {
			staleGrace = rec.staleGrace
		}
		exp = calcExpiresAt(now, ttl)
	}

	newE := &entry{
		key:        key,
		value:      val,
		ttlNanos:   ttlNanos,
		flags:      flags,
		hash:       h,
		staleGrace: staleGrace,
	}
	newE.expiresAt.Store(exp)

	s := &m.shards[idx]
	newSize := payloadSize(key, val)
	var evictedBuf []evictedEntry
	needNotify := m.onEvict != nil || m.recorder != nil

	s.mu.Lock()
	tbl := s.table.Load()
	if tbl == nil || m.stopped.Load() {
		s.mu.Unlock()
		return ErrStoreStopped
	}
	if conflictPolicy == SnapshotKeepExisting {
		if existing := tbl.lookup(h, key); existing != nil && (existing.isNoExpiration() || m.entryNow(existing) < existing.hardExpiresAt()) {
			s.mu.Unlock()
			return nil
		}
	}
	if m.maxBytes > 0 && newSize > m.maxBytes {
		s.rejectOversizedLocked(m, tbl, h, key, val, needNotify, &evictedBuf)
		s.mu.Unlock()
		m.notifyEvicted(evictedBuf)
		return nil
	}

	oldE, existed := s.upsertLocked(m, h, newE, now, &evictedBuf)
	if existed {
		oldSize := payloadSize(key, oldE.value)
		dBytes := newSize - oldSize
		s.bytes.Store(s.bytes.Load() + dBytes)
		if s.evict != nil {
			s.evict.onOverwriteLocked(oldE, newE, oldSize, newSize)
		}
		if m.bounded && dBytes != 0 {
			m.totalBytes.Add(dBytes)
		}
	} else {
		s.count.Store(s.count.Load() + 1)
		s.bytes.Store(s.bytes.Load() + newSize)
		if s.evict != nil {
			s.evict.onInsertLocked(newE, newSize)
		}
		if m.bounded {
			m.totalItems.Add(1)
			m.totalBytes.Add(newSize)
		}
	}

	if m.bounded {
		for m.overBounds() && m.shardOverTarget(s) {
			evEntry, ok := m.evictOneLocked(s, now, key)
			if !ok {
				break
			}
			if needNotify {
				evictedBuf = append(evictedBuf, evEntry)
			}
		}
	}
	s.mu.Unlock()

	m.notifyEvicted(evictedBuf)

	if m.bounded && !m.stopped.Load() && m.overBounds() {
		cursor := uint8(m.evictCursor.Add(1) & uint32(m.shardMask))
		m.enforceBounds(cursor, now, key)
	}
	return nil
}

func (m *MemoryStore) saveJSON(w io.Writer) error {
	enc := jsontext.NewEncoder(w)

	writeHeader := func() error {
		for _, tok := range []jsontext.Token{
			jsontext.BeginObject,
			jsontext.String("type"), jsontext.String("header"),
			jsontext.String("magic"), jsontext.String("MSNAP"),
			jsontext.String("version"), jsontext.Int(int64(snapshotBinaryVersion)),
			jsontext.EndObject,
		} {
			if err := enc.WriteToken(tok); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writeHeader(); err != nil {
		return err
	}

	b64Buf := make([]byte, 0, 1024)
	var totalEntries uint64

	for i := range m.activeShards() {
		if m.stopped.Load() {
			return ErrStoreStopped
		}
		s := &m.shards[i]
		tbl := s.table.Load()
		if tbl == nil || m.stopped.Load() {
			return ErrStoreStopped
		}
		now := m.nowExact()

		for bIdx := range tbl.buckets {
			b := &tbl.buckets[bIdx]
			ctrl := b.ctrl.Load()
			if ctrl&slotCtrlMask == 0 {
				continue
			}
			for j := range bucketSlots {
				if (ctrl>>(j*8))&0xFF == 0 {
					continue
				}
				e := b.slots[j].Load()
				if e == nil {
					continue
				}

				var remainingTTL int64
				noExp := e.isNoExpiration()
				if noExp {
					remainingTTL = int64(NoExpiration)
				} else {
					softExp := e.expiresAt.Load()
					if now >= softExp {
						continue
					}
					remainingTTL = softExp - now
				}

				if len(e.key) > maxSnapshotKeyBytes {
					return fmt.Errorf("snapshot key length %d exceeds max %d", len(e.key), maxSnapshotKeyBytes)
				}
				if len(e.value) > maxSnapshotValueBytes {
					return fmt.Errorf("snapshot value length %d exceeds max %d", len(e.value), maxSnapshotValueBytes)
				}

				if err := enc.WriteToken(jsontext.BeginObject); err != nil {
					return err
				}
				if err := enc.WriteToken(jsontext.String("type")); err != nil {
					return err
				}
				if err := enc.WriteToken(jsontext.String("entry")); err != nil {
					return err
				}
				if err := enc.WriteToken(jsontext.String("key")); err != nil {
					return err
				}
				if err := enc.WriteToken(jsontext.String(e.key)); err != nil {
					return err
				}
				if err := enc.WriteToken(jsontext.String("value")); err != nil {
					return err
				}
				b64Buf = append(b64Buf[:0], '"')
				b64Buf = base64.StdEncoding.AppendEncode(b64Buf, e.value)
				b64Buf = append(b64Buf, '"')
				if err := enc.WriteValue(jsontext.Value(b64Buf)); err != nil {
					return err
				}
				if err := enc.WriteToken(jsontext.String("ttl_ns")); err != nil {
					return err
				}
				if err := enc.WriteToken(jsontext.Int(remainingTTL)); err != nil {
					return err
				}
				if !noExp && e.isSliding() && e.ttlNanos > 0 {
					if err := enc.WriteToken(jsontext.String("sliding")); err != nil {
						return err
					}
					if err := enc.WriteToken(jsontext.Bool(true)); err != nil {
						return err
					}
					if err := enc.WriteToken(jsontext.String("ttl_cap_ns")); err != nil {
						return err
					}
					if err := enc.WriteToken(jsontext.Int(e.ttlNanos)); err != nil {
						return err
					}
				}
				if !noExp && e.staleGrace > 0 {
					if err := enc.WriteToken(jsontext.String("stale_grace_ns")); err != nil {
						return err
					}
					if err := enc.WriteToken(jsontext.Int(e.staleGrace)); err != nil {
						return err
					}
				}
				if err := enc.WriteToken(jsontext.EndObject); err != nil {
					return err
				}
				totalEntries++
			}
		}
	}

	if m.stopped.Load() {
		return ErrStoreStopped
	}
	for _, tok := range []jsontext.Token{
		jsontext.BeginObject,
		jsontext.String("type"), jsontext.String("eof"),
		jsontext.String("count"), jsontext.Uint(totalEntries),
		jsontext.EndObject,
	} {
		if err := enc.WriteToken(tok); err != nil {
			return err
		}
	}
	return nil
}

var errSnapshotRecordTooLarge = errors.New("snapshot JSON record exceeds maximum size")

type limitedPerRecordReader struct {
	r         io.Reader
	limit     int
	remaining int
}

func (l *limitedPerRecordReader) reset() {
	l.remaining = l.limit
}

func (l *limitedPerRecordReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errSnapshotRecordTooLarge
	}
	if len(p) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= n
	return n, err
}

func (m *MemoryStore) loadJSON(r io.Reader, conflictPolicy SnapshotConflictPolicy) error {
	maxRecordBytes := maxSnapshotKeyBytes*6 + base64.StdEncoding.EncodedLen(maxSnapshotValueBytes) + 64*1024
	lr := &limitedPerRecordReader{r: r, limit: maxRecordBytes, remaining: maxRecordBytes}
	dec := jsontext.NewDecoder(lr)

	wrapJSONErr := func(err error) error {
		if errors.Is(err, ErrStoreStopped) || errors.Is(err, ErrCorruptSnapshot) || errors.Is(err, ErrUnsupportedSnapshotVersion) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrCorruptSnapshot, err)
	}

	// 1. Parse required header object.
	tok, err := dec.ReadToken()
	if err != nil {
		return wrapJSONErr(err)
	}
	if tok.Kind() != '{' {
		return fmt.Errorf("%w: expected header JSON object", ErrCorruptSnapshot)
	}
	var objType, magic string
	var version int64
	var hasType, hasMagic, hasVersion bool

	for {
		tok, err = dec.ReadToken()
		if err != nil {
			return wrapJSONErr(err)
		}
		if tok.Kind() == '}' {
			break
		}
		if tok.Kind() != '"' {
			return fmt.Errorf("%w: expected header field name", ErrCorruptSnapshot)
		}
		name := tok.String()
		valTok, verr := dec.ReadToken()
		if verr != nil {
			return wrapJSONErr(verr)
		}
		switch name {
		case "type":
			if valTok.Kind() != '"' {
				return fmt.Errorf("%w: header type must be string", ErrCorruptSnapshot)
			}
			objType = valTok.String()
			hasType = true
		case "magic":
			if valTok.Kind() != '"' {
				return fmt.Errorf("%w: header magic must be string", ErrCorruptSnapshot)
			}
			magic = valTok.String()
			hasMagic = true
		case "version":
			if valTok.Kind() != '0' {
				return fmt.Errorf("%w: header version must be integer", ErrCorruptSnapshot)
			}
			v, ierr := valTok.Int()
			if ierr != nil {
				return wrapJSONErr(ierr)
			}
			version = v
			hasVersion = true
		default:
			return fmt.Errorf("%w: unexpected header field %q", ErrCorruptSnapshot, name)
		}
	}
	if !hasType || objType != "header" || !hasMagic || magic != "MSNAP" || !hasVersion {
		return fmt.Errorf("%w: invalid snapshot JSON header", ErrCorruptSnapshot)
	}
	if version != int64(snapshotBinaryVersion) {
		return fmt.Errorf("%w: version %d", ErrUnsupportedSnapshotVersion, version)
	}

	// 2. Stream entry objects until EOF sentinel object.
	var totalEntries uint64
	valScratch := make([]byte, 0, 1024)
	b64Scratch := make([]byte, 0, 1024)

	for {
		if m.stopped.Load() {
			return ErrStoreStopped
		}
		lr.reset()

		tok, err = dec.ReadToken()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: premature EOF before sentinel", ErrCorruptSnapshot)
			}
			return wrapJSONErr(err)
		}
		if tok.Kind() != '{' {
			return fmt.Errorf("%w: expected JSON object", ErrCorruptSnapshot)
		}

		var typ string
		var key string
		var ttlNS int64
		var sliding bool
		var ttlCapNS int64
		var staleGraceNS int64
		var count uint64
		var hasObjType, hasKey, hasVal, hasTTL, hasSliding, hasTTLCap, hasStaleGrace, hasCount bool
		valScratch = valScratch[:0]

		for {
			tok, err = dec.ReadToken()
			if err != nil {
				return wrapJSONErr(err)
			}
			if tok.Kind() == '}' {
				break
			}
			if tok.Kind() != '"' {
				return fmt.Errorf("%w: expected member name", ErrCorruptSnapshot)
			}
			name := tok.String()
			valTok, verr := dec.ReadToken()
			if verr != nil {
				return wrapJSONErr(verr)
			}
			switch name {
			case "type":
				if valTok.Kind() != '"' {
					return fmt.Errorf("%w: type must be string", ErrCorruptSnapshot)
				}
				typ = valTok.String()
				hasObjType = true
			case "key":
				if valTok.Kind() != '"' {
					return fmt.Errorf("%w: key must be string", ErrCorruptSnapshot)
				}
				key = valTok.String()
				if len(key) > maxSnapshotKeyBytes {
					return fmt.Errorf("%w: key length %d exceeds max %d", ErrCorruptSnapshot, len(key), maxSnapshotKeyBytes)
				}
				hasKey = true
			case "value":
				if valTok.Kind() != '"' {
					return fmt.Errorf("%w: value must be base64 string", ErrCorruptSnapshot)
				}
				b64Str := valTok.String()
				if len(b64Str) > base64.StdEncoding.EncodedLen(maxSnapshotValueBytes) {
					return fmt.Errorf("%w: decoded value exceeds max %d", ErrCorruptSnapshot, maxSnapshotValueBytes)
				}
				b64Scratch = append(b64Scratch[:0], b64Str...)
				var derr error
				valScratch, derr = base64.StdEncoding.AppendDecode(valScratch[:0], b64Scratch)
				if derr != nil {
					return fmt.Errorf("%w: invalid base64 value: %v", ErrCorruptSnapshot, derr)
				}
				if len(valScratch) > maxSnapshotValueBytes {
					return fmt.Errorf("%w: value length %d exceeds max %d", ErrCorruptSnapshot, len(valScratch), maxSnapshotValueBytes)
				}
				hasVal = true
			case "ttl_ns":
				if valTok.Kind() != '0' {
					return fmt.Errorf("%w: ttl_ns must be integer", ErrCorruptSnapshot)
				}
				v, ierr := valTok.Int()
				if ierr != nil {
					return wrapJSONErr(ierr)
				}
				ttlNS = v
				hasTTL = true
			case "sliding":
				if valTok.Kind() != 't' && valTok.Kind() != 'f' {
					return fmt.Errorf("%w: sliding must be bool", ErrCorruptSnapshot)
				}
				sliding = valTok.Bool()
				hasSliding = true
			case "ttl_cap_ns":
				if valTok.Kind() != '0' {
					return fmt.Errorf("%w: ttl_cap_ns must be integer", ErrCorruptSnapshot)
				}
				v, ierr := valTok.Int()
				if ierr != nil {
					return wrapJSONErr(ierr)
				}
				ttlCapNS = v
				hasTTLCap = true
			case "stale_grace_ns":
				if valTok.Kind() != '0' {
					return fmt.Errorf("%w: stale_grace_ns must be integer", ErrCorruptSnapshot)
				}
				v, ierr := valTok.Int()
				if ierr != nil {
					return wrapJSONErr(ierr)
				}
				staleGraceNS = v
				hasStaleGrace = true
			case "count":
				if valTok.Kind() != '0' {
					return fmt.Errorf("%w: count must be unsigned integer", ErrCorruptSnapshot)
				}
				u, uerr := valTok.Uint()
				if uerr != nil {
					return wrapJSONErr(uerr)
				}
				count = u
				hasCount = true
			default:
				return fmt.Errorf("%w: unexpected snapshot field %q", ErrCorruptSnapshot, name)
			}
		}

		if !hasObjType {
			return fmt.Errorf("%w: missing object type", ErrCorruptSnapshot)
		}

		switch typ {
		case "entry":
			if !hasKey || !hasVal || !hasTTL || hasCount {
				return fmt.Errorf("%w: incomplete or malformed entry object", ErrCorruptSnapshot)
			}
			noExp := ttlNS == int64(NoExpiration)
			if !noExp && ttlNS < 0 {
				return fmt.Errorf("%w: negative ttl_ns %d", ErrCorruptSnapshot, ttlNS)
			}
			if noExp && (hasSliding || hasTTLCap || hasStaleGrace) {
				return fmt.Errorf("%w: NoExpiration entry cannot have sliding/stale fields", ErrCorruptSnapshot)
			}
			if sliding && hasTTLCap && ttlCapNS <= 0 {
				return fmt.Errorf("%w: invalid ttl_cap_ns %d", ErrCorruptSnapshot, ttlCapNS)
			}
			if hasStaleGrace && staleGraceNS < 0 {
				return fmt.Errorf("%w: negative stale_grace_ns %d", ErrCorruptSnapshot, staleGraceNS)
			}

			rec := snapDecodedEntry{
				key:          []byte(key),
				value:        valScratch,
				remainingTTL: ttlNS,
				ttlNanos:     ttlCapNS,
				staleGrace:   staleGraceNS,
				noExpiration: noExp,
				sliding:      sliding,
			}
			if err := m.commitSnapshotEntry(&rec, conflictPolicy); err != nil {
				return err
			}
			totalEntries++

		case "eof":
			if !hasCount || hasKey || hasVal || hasTTL || hasSliding || hasTTLCap || hasStaleGrace {
				return fmt.Errorf("%w: malformed EOF object", ErrCorruptSnapshot)
			}
			if count != totalEntries {
				return fmt.Errorf("%w: EOF count mismatch (got %d, want %d)", ErrCorruptSnapshot, count, totalEntries)
			}
			lr.reset()
			if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: unexpected trailing data after EOF sentinel", ErrCorruptSnapshot)
			}
			return nil

		default:
			return fmt.Errorf("%w: unknown object type %q", ErrCorruptSnapshot, typ)
		}
	}
}
