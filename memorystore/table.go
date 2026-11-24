package memorystore

import (
	"math"
	"math/bits"
	"sync/atomic"
	"time"
)

const (
	bucketSlots    = 7
	initialBuckets = 2 // 14 slots per shard; 256 * 2 * 64B = 32 KiB per store

	overflowBit   = uint64(1) << 63
	slotCtrlMask  = uint64(0x00ffffffffffffff)
	matchLaneMask = uint64(0x0080808080808080)
	mask7F        = uint64(0x7f7f7f7f7f7f7f7f)
	loBits        = uint64(0x0101010101010101)

	// entries with positive TTL < 10ms automatically bypass the 1ms coarse
	// clock and use exact monotonic timestamps on both Set and Get.
	preciseTTLThreshold = 10 * time.Millisecond
)

const (
	entryFlagNoExpiration uint8 = 1 << iota
	entryFlagSliding
	entryFlagPreciseTTL
)

const (
	evictQueueNone uint8 = iota
	evictQueueSmall
	evictQueueMain
	evictQueueProtected
)

// entry represents a single immutable-header cache entry stored behind
// atomic.Pointer[entry] slots.
//
// Layout is exactly 96 bytes on 64-bit architectures:
//   - Cache line 0 (64B): all fields accessed by hot Get() paths.
//   - Cache line 1 (32B): write, resize, SWR, and eviction-only fields.
type entry struct {
	// Cache line 0 (64B)
	key        string        // 16B (immutable)
	value      []byte        // 24B (immutable)
	expiresAt  atomic.Int64  //  8B (softExpiresAt; math.MaxInt64 when NoExpiration)
	ttlNanos   int64         //  8B (immutable; effective TTL for sliding refresh)
	freq       atomic.Uint32 //  4B (2-bit saturating frequency counter for S3-FIFO)
	flags      uint8         //  1B (NoExpiration, Sliding, PreciseTTL)
	evictQueue uint8         //  1B (eviction queue membership, guarded by shard.mu)
	_          [2]byte       //  2B

	// Cache line 1 (32B)
	hash       uint64 // 8B (immutable 64-bit maphash)
	staleGrace int64  // 8B (SWR grace window; 0 when disabled; hardExpiry = expiresAt + staleGrace)
	evictPrev  *entry // 8B (intrusive DLL prev, guarded by shard.mu)
	evictNext  *entry // 8B (intrusive DLL next, guarded by shard.mu)
}

func (e *entry) isNoExpiration() bool {
	return e.flags&entryFlagNoExpiration != 0
}

func (e *entry) isSliding() bool {
	return e.flags&entryFlagSliding != 0
}

func (e *entry) isPreciseTTL() bool {
	return e.flags&entryFlagPreciseTTL != 0
}

func (e *entry) hardExpiresAt() int64 {
	return calcHardExpiry(e.expiresAt.Load(), e.staleGrace)
}

func calcHardExpiry(softExp, staleGrace int64) int64 {
	if staleGrace <= 0 {
		return softExp
	}
	if softExp > math.MaxInt64-staleGrace {
		return math.MaxInt64
	}
	return softExp + staleGrace
}

// bucket is a 64-byte single-cache-line Swiss-style hash bucket with 7 slots
// and an atomic control word holding 7 h2 tags (bytes 0..6) plus overflowBit (bit 63).
type bucket struct {
	ctrl  atomic.Uint64
	slots [bucketSlots]atomic.Pointer[entry]
}

// bucketTable is an immutable-topology open-addressed Swiss-style hash table
// published via atomic.Pointer[bucketTable]. Writers hold shard.mu; readers
// traverse buckets completely lock-free.
type bucketTable struct {
	buckets []bucket
	mask    uint64
}

func newBucketTable(numBuckets int) *bucketTable {
	return &bucketTable{
		buckets: make([]bucket, numBuckets),
		mask:    uint64(numBuckets - 1),
	}
}

func h2Tag(h uint64) uint8 {
	tag := uint8(h >> 56)
	if tag == 0 {
		return 1
	}
	return tag
}

// lookup probes the bucketTable lock-free using triangular probing and SWAR
// carry-free h2 tag matching.
func (t *bucketTable) lookup(h uint64, key string) *entry {
	h2 := h2Tag(h)
	tagMask := loBits * uint64(h2)
	idx := (h >> 8) & t.mask
	step := uint64(0)
	for {
		b := &t.buckets[idx]
		ctrl := b.ctrl.Load()
		x := (ctrl ^ tagMask) & slotCtrlMask
		matches := (^(((x & mask7F) + mask7F) | x | mask7F)) & matchLaneMask
		for matches != 0 {
			slotIdx := bits.TrailingZeros64(matches) >> 3
			matches &= matches - 1
			if e := b.slots[slotIdx].Load(); e != nil && e.key == key {
				return e
			}
		}
		if ctrl&overflowBit == 0 {
			return nil
		}
		step++
		if step > t.mask {
			return nil
		}
		idx = (idx + step) & t.mask
	}
}

// findEmptySlot probes t for the first empty slot along key's probe chain.
// Caller must hold shard.mu.Lock() and have verified at least one empty slot exists.
func (t *bucketTable) findEmptySlot(h uint64) (emptyB *bucket, emptySlot int, emptyStep uint64, found bool) {
	idx := (h >> 8) & t.mask
	step := uint64(0)
	for {
		b := &t.buckets[idx]
		ctrl := b.ctrl.Load()
		emptyMatches := (^(((ctrl & mask7F) + mask7F) | ctrl | mask7F)) & matchLaneMask
		if emptyMatches != 0 {
			return b, bits.TrailingZeros64(emptyMatches) >> 3, step, true
		}
		step++
		if step > t.mask {
			return nil, 0, 0, false
		}
		idx = (idx + step) & t.mask
	}
}

// insertFreshLocked inserts an entry known not to exist in t.
// Caller must hold shard.mu.Lock().
func (t *bucketTable) insertFreshLocked(h uint64, newE *entry) {
	emptyB, emptySlot, emptyStep, ok := t.findEmptySlot(h)
	if !ok {
		return
	}
	t.publishAtSlotLocked(h, newE, emptyB, emptySlot, emptyStep)
}

func (t *bucketTable) publishAtSlotLocked(h uint64, newE *entry, emptyB *bucket, emptySlot int, emptyStep uint64) {
	h2 := h2Tag(h)
	// Linearizability order:
	// 1. Store entry pointer in slot.
	// 2. Publish h2 tag in ctrl (preserving overflowBit and other lanes).
	// 3. Set overflowBit on any bypassed buckets so concurrent readers probe further.
	emptyB.slots[emptySlot].Store(newE)
	c := emptyB.ctrl.Load()
	c = (c & ^(uint64(0xFF) << (emptySlot * 8))) | (uint64(h2) << (emptySlot * 8))
	emptyB.ctrl.Store(c)

	if emptyStep > 0 {
		pIdx := (h >> 8) & t.mask
		for pStep := range emptyStep {
			pb := &t.buckets[pIdx]
			if pc := pb.ctrl.Load(); pc&overflowBit == 0 {
				pb.ctrl.Store(pc | overflowBit)
			}
			pIdx = (pIdx + pStep + 1) & t.mask
		}
	}
}

// upsertLocked inserts or replaces an entry in shard s. Caller must hold s.mu.Lock().
func (s *shard) upsertLocked(m *MemoryStore, h uint64, newE *entry, now int64, evictedBuf *[]evictedEntry) (oldE *entry, existed bool) {
	t := s.table.Load()
	h2 := h2Tag(h)
	tagMask := loBits * uint64(h2)
	idx := (h >> 8) & t.mask
	step := uint64(0)

	var emptyB *bucket
	var emptySlot int
	var emptyStep uint64
	foundEmpty := false

	for {
		b := &t.buckets[idx]
		ctrl := b.ctrl.Load()

		x := (ctrl ^ tagMask) & slotCtrlMask
		matches := (^(((x & mask7F) + mask7F) | x | mask7F)) & matchLaneMask
		for matches != 0 {
			slotIdx := bits.TrailingZeros64(matches) >> 3
			matches &= matches - 1
			if e := b.slots[slotIdx].Load(); e != nil && e.key == newE.key {
				b.slots[slotIdx].Store(newE)
				return e, true
			}
		}

		if !foundEmpty {
			emptyMatches := (^(((ctrl & mask7F) + mask7F) | ctrl | mask7F)) & matchLaneMask
			if emptyMatches != 0 {
				emptyB = b
				emptySlot = bits.TrailingZeros64(emptyMatches) >> 3
				emptyStep = step
				foundEmpty = true
			}
		}

		if ctrl&overflowBit == 0 {
			break
		}
		step++
		if step > t.mask {
			break
		}
		idx = (idx + step) & t.mask
	}

	if s.count.Load() >= s.resizeThreshold || !foundEmpty {
		s.resizeLocked(m, now, evictedBuf)
		t = s.table.Load()
		emptyB, emptySlot, emptyStep, foundEmpty = t.findEmptySlot(h)
		if !foundEmpty {
			return nil, false
		}
	}

	t.publishAtSlotLocked(h, newE, emptyB, emptySlot, emptyStep)
	return nil, false
}

// deleteLocked removes key from t if present, preserving overflowBit.
// Caller must hold shard.mu.Lock().
func (t *bucketTable) deleteLocked(h uint64, key string) (*entry, bool) {
	h2 := h2Tag(h)
	tagMask := loBits * uint64(h2)
	idx := (h >> 8) & t.mask
	step := uint64(0)
	for {
		b := &t.buckets[idx]
		ctrl := b.ctrl.Load()
		x := (ctrl ^ tagMask) & slotCtrlMask
		matches := (^(((x & mask7F) + mask7F) | x | mask7F)) & matchLaneMask
		for matches != 0 {
			slotIdx := bits.TrailingZeros64(matches) >> 3
			matches &= matches - 1
			if e := b.slots[slotIdx].Load(); e != nil && e.key == key {
				b.slots[slotIdx].Store(nil)
				b.ctrl.Store(ctrl & ^(uint64(0xFF) << (slotIdx * 8)))
				return e, true
			}
		}
		if ctrl&overflowBit == 0 {
			return nil, false
		}
		step++
		if step > t.mask {
			return nil, false
		}
		idx = (idx + step) & t.mask
	}
}

// deletePtrLocked removes the exact entry pointer target from t if still present,
// preserving overflowBit. Caller must hold shard.mu.Lock().
func (t *bucketTable) deletePtrLocked(target *entry) bool {
	h := target.hash
	h2 := h2Tag(h)
	tagMask := loBits * uint64(h2)
	idx := (h >> 8) & t.mask
	step := uint64(0)
	for {
		b := &t.buckets[idx]
		ctrl := b.ctrl.Load()
		x := (ctrl ^ tagMask) & slotCtrlMask
		matches := (^(((x & mask7F) + mask7F) | x | mask7F)) & matchLaneMask
		for matches != 0 {
			slotIdx := bits.TrailingZeros64(matches) >> 3
			matches &= matches - 1
			if b.slots[slotIdx].Load() == target {
				b.slots[slotIdx].Store(nil)
				b.ctrl.Store(ctrl & ^(uint64(0xFF) << (slotIdx * 8)))
				return true
			}
		}
		if ctrl&overflowBit == 0 {
			return false
		}
		step++
		if step > t.mask {
			return false
		}
		idx = (idx + step) & t.mask
	}
}

// resizeLocked prunes expired entries and publishes a new bucketTable via
// atomic s.table.Store(newTable), leaving oldTable immutable for concurrent readers.
// Caller must hold s.mu.Lock().
func (s *shard) resizeLocked(m *MemoryStore, now int64, evictedBuf *[]evictedEntry) {
	oldTable := s.table.Load()
	if oldTable == nil {
		return
	}

	var stackSurvivors [64]*entry
	survivors := stackSurvivors[:0]

	var expiredCount int64
	var freedBytes int64
	needNotify := m.onEvict != nil || m.recorder != nil

	for i := range oldTable.buckets {
		b := &oldTable.buckets[i]
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
			if !e.isNoExpiration() && now >= e.hardExpiresAt() {
				sz := payloadSize(e.key, e.value)
				if s.evict != nil {
					s.evict.onDeleteLocked(e, sz)
				}
				expiredCount++
				freedBytes += sz
				if needNotify && evictedBuf != nil {
					*evictedBuf = append(*evictedBuf, evictedEntry{
						key:    e.key,
						value:  e.value,
						reason: EvictExpired,
					})
				}
				continue
			}
			survivors = append(survivors, e)
		}
	}

	if expiredCount > 0 {
		s.count.Store(s.count.Load() - expiredCount)
		s.bytes.Store(s.bytes.Load() - freedBytes)
		s.evictions.Add(expiredCount)
		if m.bounded {
			m.totalItems.Add(-expiredCount)
			m.totalBytes.Add(-freedBytes)
		}
	}

	newCap := len(oldTable.buckets)
	for int64(len(survivors)) >= int64((newCap*bucketSlots*3)/4) {
		newCap *= 2
	}
	newTable := newBucketTable(newCap)
	for _, e := range survivors {
		newTable.insertFreshLocked(e.hash, e)
	}
	s.resizeThreshold = int64((newCap * bucketSlots * 3) / 4)
	s.table.Store(newTable)
}
