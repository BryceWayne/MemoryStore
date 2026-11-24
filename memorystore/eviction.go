package memorystore

import (
	"math/bits"
	"sync/atomic"
)

// entryList is an intrusive doubly-linked list of *entry nodes guarded by shard.mu.
// head is the most-recently-inserted/accessed end; tail is the oldest/victim end.
type entryList struct {
	head  *entry
	tail  *entry
	count int64
	bytes int64
}

func (l *entryList) pushHead(e *entry, queue uint8, sz int64) {
	e.evictPrev = nil
	e.evictNext = l.head
	e.evictQueue = queue
	if l.head != nil {
		l.head.evictPrev = e
	} else {
		l.tail = e
	}
	l.head = e
	l.count++
	l.bytes += sz
}

func (l *entryList) pushTail(e *entry, queue uint8, sz int64) {
	e.evictNext = nil
	e.evictPrev = l.tail
	e.evictQueue = queue
	if l.tail != nil {
		l.tail.evictNext = e
	} else {
		l.head = e
	}
	l.tail = e
	l.count++
	l.bytes += sz
}

func (l *entryList) moveToHead(e *entry) {
	if l.head == e {
		return
	}
	if e.evictPrev != nil {
		e.evictPrev.evictNext = e.evictNext
	}
	if e.evictNext != nil {
		e.evictNext.evictPrev = e.evictPrev
	} else {
		l.tail = e.evictPrev
	}
	e.evictPrev = nil
	e.evictNext = l.head
	if l.head != nil {
		l.head.evictPrev = e
	} else {
		l.tail = e
	}
	l.head = e
}

func (l *entryList) remove(e *entry, sz int64) {
	if e.evictPrev != nil {
		e.evictPrev.evictNext = e.evictNext
	} else {
		l.head = e.evictNext
	}
	if e.evictNext != nil {
		e.evictNext.evictPrev = e.evictPrev
	} else {
		l.tail = e.evictPrev
	}
	e.evictPrev = nil
	e.evictNext = nil
	e.evictQueue = evictQueueNone
	l.count--
	l.bytes -= sz
}

func (l *entryList) replace(oldE, newE *entry, oldSz, newSz int64) {
	newE.evictQueue = oldE.evictQueue
	newE.evictPrev = oldE.evictPrev
	newE.evictNext = oldE.evictNext
	if newE.evictPrev != nil {
		newE.evictPrev.evictNext = newE
	} else {
		l.head = newE
	}
	if newE.evictNext != nil {
		newE.evictNext.evictPrev = newE
	} else {
		l.tail = newE
	}
	oldE.evictPrev = nil
	oldE.evictNext = nil
	oldE.evictQueue = evictQueueNone
	l.bytes += newSz - oldSz
}

func (l *entryList) popHead() *entry {
	e := l.head
	if e == nil {
		return nil
	}
	l.remove(e, payloadSize(e.key, e.value))
	return e
}

func (l *entryList) popTail() *entry {
	e := l.tail
	if e == nil {
		return nil
	}
	l.remove(e, payloadSize(e.key, e.value))
	return e
}

func (l *entryList) clear() {
	for cur := l.head; cur != nil; {
		next := cur.evictNext
		cur.evictPrev = nil
		cur.evictNext = nil
		cur.evictQueue = evictQueueNone
		cur = next
	}
	l.head = nil
	l.tail = nil
	l.count = 0
	l.bytes = 0
}

// ghostSlot stores a hash entry and liveness flag in ghostRing so remove()
// vacates slots in place without desynchronizing ring and set.
type ghostSlot struct {
	hash uint64
	live bool
}

// ghostRing tracks recently evicted key hashes for S3-FIFO without retaining
// keys, values, or entry pointers. Guarded by shard.mu.
type ghostRing struct {
	cap  int
	pos  int
	ring []ghostSlot
	set  map[uint64]int
}

func (g *ghostRing) add(h uint64) {
	if g.cap <= 0 {
		return
	}
	if g.ring == nil {
		g.ring = make([]ghostSlot, g.cap)
		g.set = make(map[uint64]int, g.cap)
	}
	if _, exists := g.set[h]; exists {
		return
	}
	if cur := &g.ring[g.pos]; cur.live {
		delete(g.set, cur.hash)
		cur.live = false
	}
	g.ring[g.pos] = ghostSlot{hash: h, live: true}
	g.set[h] = g.pos
	g.pos++
	if g.pos >= g.cap {
		g.pos = 0
	}
}

func (g *ghostRing) remove(h uint64) bool {
	if g.set == nil {
		return false
	}
	idx, ok := g.set[h]
	if !ok {
		return false
	}
	g.ring[idx].live = false
	delete(g.set, h)
	return true
}

func (g *ghostRing) clear() {
	clear(g.ring)
	clear(g.set)
	g.pos = 0
}

// countMinSketch is a 4-row × 4-bit packed Count-Min Sketch with a Doorkeeper
// bitset and SWAR halving reset. Guarded by shard.mu.
type countMinSketch struct {
	rows       [4][]uint64
	doorkeeper []uint64
	mask       uint64
	addCount   int
	sampleCap  int
}

func nextPow2Int(n int) int {
	if n <= 64 {
		return 64
	}
	if n >= 2048 {
		return 2048
	}
	p := 64
	for p < n {
		p <<= 1
	}
	return p
}

func (c *countMinSketch) ensureInit(targetItems int64) {
	if c.mask != 0 {
		return
	}
	cols := nextPow2Int(int(targetItems) * 4)
	wordsPerRow := cols / 16 // 16 × 4-bit counters per uint64
	for r := range c.rows {
		c.rows[r] = make([]uint64, wordsPerRow)
	}
	c.doorkeeper = make([]uint64, max(cols/64, 1))
	c.mask = uint64(cols - 1)
	c.sampleCap = max(cols*10, 256)
}

func cmsIndex(h uint64, row int, mask uint64) uint64 {
	const (
		k0 = 0x9e3779b97f4a7c15
		k1 = 0xbf58476d1ce4e5b9
		k2 = 0x94d049bb133111eb
		k3 = 0xd6e8feb86659fd93
	)
	var x uint64
	switch row {
	case 0:
		x = h ^ (h >> 30)
	case 1:
		x = bits.RotateLeft64(h*k0, 17) ^ k1
	case 2:
		x = bits.RotateLeft64(h*k1, 31) ^ k2
	default:
		x = bits.RotateLeft64(h*k2, 43) ^ k3
	}
	return x & mask
}

func (c *countMinSketch) doorkeeperCheckAndSet(h uint64) bool {
	idx := cmsIndex(h, 0, c.mask)
	word := idx >> 6
	bit := uint64(1) << (idx & 63)
	if c.doorkeeper[word]&bit != 0 {
		return true
	}
	c.doorkeeper[word] |= bit
	return false
}

func (c *countMinSketch) doorkeeperContains(h uint64) bool {
	idx := cmsIndex(h, 0, c.mask)
	return c.doorkeeper[idx>>6]&(uint64(1)<<(idx&63)) != 0
}

func (c *countMinSketch) add(h uint64, targetItems int64) {
	c.ensureInit(targetItems)
	c.addCount++
	if !c.doorkeeperCheckAndSet(h) {
		if c.addCount >= c.sampleCap {
			c.reset()
		}
		return
	}
	for r := range c.rows {
		col := cmsIndex(h, r, c.mask)
		wordIdx := col >> 4
		shift := (col & 15) * 4
		w := c.rows[r][wordIdx]
		nibble := (w >> shift) & 0xF
		if nibble < 15 {
			c.rows[r][wordIdx] = (w & ^(uint64(0xF) << shift)) | ((nibble + 1) << shift)
		}
	}
	if c.addCount >= c.sampleCap {
		c.reset()
	}
}

func (c *countMinSketch) estimate(h uint64) uint8 {
	if c.mask == 0 {
		return 0
	}
	minVal := uint8(15)
	for r := range c.rows {
		col := cmsIndex(h, r, c.mask)
		nibble := uint8((c.rows[r][col>>4] >> ((col & 15) * 4)) & 0xF)
		if nibble < minVal {
			minVal = nibble
		}
	}
	if c.doorkeeperContains(h) && minVal < 15 {
		minVal++
	}
	return minVal
}

func (c *countMinSketch) reset() {
	for r := range c.rows {
		row := c.rows[r]
		for i := range row {
			row[i] = (row[i] >> 1) & 0x7777777777777777
		}
	}
	clear(c.doorkeeper)
	c.addCount = 0
}

func (c *countMinSketch) clear() {
	for r := range c.rows {
		clear(c.rows[r])
	}
	clear(c.doorkeeper)
	c.addCount = 0
}

// shardEvictor holds per-shard eviction policy state.
// Pointer remains non-nil across Stop() so lock-free readers never race on s.evict.
type shardEvictor struct {
	policy           EvictionPolicy
	shardTargetItems int64
	shardTargetBytes int64
	volatileCursor   uint64

	// S3-FIFO: small = Small FIFO, main = Main FIFO, ghost = Ghost hash ring.
	// TinyLFU: small = Window LRU, main = Probation SLRU, protected = Protected SLRU.
	small     entryList
	main      entryList
	protected entryList
	ghost     ghostRing

	// W-TinyLFU lock-free lossy read ring buffer.
	readBuf  [64]atomic.Pointer[entry]
	writeIdx atomic.Uint64
	drainIdx uint64
	cms      countMinSketch
}

const maxGhostCapPerShard = 8192

func newShardEvictor(policy EvictionPolicy, shardTargetItems, shardTargetBytes int64) *shardEvictor {
	ev := &shardEvictor{
		policy:           policy,
		shardTargetItems: shardTargetItems,
		shardTargetBytes: shardTargetBytes,
	}
	if policy == EvictionS3FIFO {
		var capVal int64
		if shardTargetItems > 0 {
			capVal = max(shardTargetItems, 4)
		} else if shardTargetBytes > 0 {
			capVal = max(shardTargetBytes/64, 16)
		} else {
			capVal = 4
		}
		ev.ghost.cap = int(min(capVal, maxGhostCapPerShard))
	}
	return ev
}

func (ev *shardEvictor) cmsTargetItems() int64 {
	if ev.shardTargetItems > 0 {
		return ev.shardTargetItems
	}
	if ev.shardTargetBytes > 0 {
		return max(ev.shardTargetBytes/64, 64)
	}
	return 0
}

// drainLocked processes buffered read accesses for W-TinyLFU under shard.mu.Lock().
func (ev *shardEvictor) drainLocked() {
	if ev.policy != EvictionTinyLFU {
		return
	}
	w := ev.writeIdx.Load()
	d := ev.drainIdx
	if w == d {
		return
	}
	if w-d > 64 {
		d = w - 64
	}
	for d < w {
		slot := d & 63
		d++
		e := ev.readBuf[slot].Swap(nil)
		if e == nil {
			continue
		}
		ev.cms.add(e.hash, ev.cmsTargetItems())
		switch e.evictQueue {
		case evictQueueNone:
			continue
		case evictQueueSmall:
			ev.small.moveToHead(e)
		case evictQueueMain:
			sz := payloadSize(e.key, e.value)
			ev.main.remove(e, sz)
			ev.protected.pushHead(e, evictQueueProtected, sz)
			ev.enforceProtectedLimitLocked()
		case evictQueueProtected:
			ev.protected.moveToHead(e)
		}
	}
	ev.drainIdx = w
}

func (ev *shardEvictor) enforceProtectedLimitLocked() {
	mainCount := ev.main.count + ev.protected.count
	if mainCount <= 1 {
		return
	}
	maxProtCount := max((mainCount*4)/5, 1)
	overCount := ev.protected.count > maxProtCount
	overBytes := ev.shardTargetBytes > 0 && ev.protected.count > 1 && ev.protected.bytes > ((ev.main.bytes+ev.protected.bytes)*4)/5
	for (overCount || overBytes) && ev.protected.count > 1 {
		demoted := ev.protected.popTail()
		if demoted == nil {
			break
		}
		ev.main.pushHead(demoted, evictQueueMain, payloadSize(demoted.key, demoted.value))
		overCount = ev.protected.count > maxProtCount
		overBytes = ev.shardTargetBytes > 0 && ev.protected.count > 1 && ev.protected.bytes > ((ev.main.bytes+ev.protected.bytes)*4)/5
	}
}

func (ev *shardEvictor) onInsertLocked(e *entry, sz int64) {
	switch ev.policy {
	case EvictionS3FIFO:
		e.freq.Store(0)
		if ev.ghost.remove(e.hash) {
			ev.main.pushTail(e, evictQueueMain, sz)
		} else {
			ev.small.pushTail(e, evictQueueSmall, sz)
		}
	case EvictionTinyLFU:
		ev.drainLocked()
		ev.cms.add(e.hash, ev.cmsTargetItems())
		ev.small.pushHead(e, evictQueueSmall, sz)
	}
}

func (ev *shardEvictor) onOverwriteLocked(oldE, newE *entry, oldSz, newSz int64) {
	switch ev.policy {
	case EvictionS3FIFO:
		newE.freq.Store(oldE.freq.Load())
		switch oldE.evictQueue {
		case evictQueueSmall:
			ev.small.replace(oldE, newE, oldSz, newSz)
		case evictQueueMain:
			ev.main.replace(oldE, newE, oldSz, newSz)
		default:
			ev.onInsertLocked(newE, newSz)
		}
	case EvictionTinyLFU:
		ev.drainLocked()
		ev.cms.add(newE.hash, ev.cmsTargetItems())
		switch oldE.evictQueue {
		case evictQueueSmall:
			ev.small.replace(oldE, newE, oldSz, newSz)
			ev.small.moveToHead(newE)
		case evictQueueMain:
			ev.main.replace(oldE, newE, oldSz, newSz)
			ev.main.moveToHead(newE)
		case evictQueueProtected:
			ev.protected.replace(oldE, newE, oldSz, newSz)
			ev.protected.moveToHead(newE)
		default:
			ev.small.pushHead(newE, evictQueueSmall, newSz)
		}
	}
}

func (ev *shardEvictor) onDeleteLocked(e *entry, sz int64) {
	switch e.evictQueue {
	case evictQueueSmall:
		ev.small.remove(e, sz)
	case evictQueueMain:
		ev.main.remove(e, sz)
	case evictQueueProtected:
		ev.protected.remove(e, sz)
	}
}

func (ev *shardEvictor) clearLocked() {
	ev.small.clear()
	ev.main.clear()
	ev.protected.clear()
	ev.ghost.clear()
	for i := range ev.readBuf {
		ev.readBuf[i].Store(nil)
	}
	ev.drainIdx = ev.writeIdx.Load()
	ev.cms.clear()
	ev.volatileCursor = 0
}

// recordReadAccess updates eviction read-tracking metadata completely lock-free
// with 0 B/op and 0 allocs/op.
func (m *MemoryStore) recordReadAccess(s *shard, e *entry) {
	switch m.evictionPolicy {
	case EvictionS3FIFO:
		if f := e.freq.Load(); f < 3 {
			e.freq.CompareAndSwap(f, f+1)
		}
	case EvictionTinyLFU:
		ev := s.evict
		if ev == nil {
			return
		}
		idx := (ev.writeIdx.Add(1) - 1) & 63
		ev.readBuf[idx].Store(e)
		if idx == 63 && s.mu.TryLock() {
			if s.table.Load() != nil && !m.stopped.Load() {
				ev.drainLocked()
			}
			s.mu.Unlock()
		}
	}
}

func isProtectedEntry(key, protectedKey string, protectedBatch map[string][]byte) bool {
	if protectedKey != "" && key == protectedKey {
		return true
	}
	if protectedBatch != nil {
		_, ok := protectedBatch[key]
		return ok
	}
	return false
}

// evictOneLocked removes one victim from s according to m.evictionPolicy while
// skipping protectedKey if other candidates exist. Caller must hold s.mu.Lock().
func (m *MemoryStore) evictOneLocked(s *shard, now int64, protectedKey string) (evictedEntry, bool) {
	return m.evictOneProtectedLocked(s, now, protectedKey, nil)
}

// evictOneProtectedLocked removes one victim from s according to m.evictionPolicy while
// skipping protectedKey and any key in protectedBatch if other candidates exist.
// Caller must hold s.mu.Lock().
func (m *MemoryStore) evictOneProtectedLocked(s *shard, now int64, protectedKey string, protectedBatch map[string][]byte) (evictedEntry, bool) {
	tbl := s.table.Load()
	if tbl == nil || s.count.Load() == 0 {
		return evictedEntry{}, false
	}

	switch m.evictionPolicy {
	case EvictionS3FIFO:
		return m.evictOneS3FIFOLocked(s, tbl, now, protectedKey, protectedBatch)
	case EvictionTinyLFU:
		return m.evictOneTinyLFULocked(s, tbl, now, protectedKey, protectedBatch)
	default:
		return m.evictOneVolatileTTLLocked(s, tbl, now, protectedKey, protectedBatch)
	}
}

func (m *MemoryStore) commitEvictionLocked(s *shard, tbl *bucketTable, victim *entry, reason EvictReason) evictedEntry {
	sz := payloadSize(victim.key, victim.value)
	tbl.deletePtrLocked(victim)
	if s.evict != nil {
		s.evict.onDeleteLocked(victim, sz)
	}
	s.count.Store(s.count.Load() - 1)
	s.bytes.Store(s.bytes.Load() - sz)
	s.evictions.Add(1)
	if m.bounded {
		m.totalItems.Add(-1)
		m.totalBytes.Add(-sz)
	}
	return evictedEntry{key: victim.key, value: victim.value, reason: reason}
}

func (m *MemoryStore) evictOneVolatileTTLLocked(s *shard, tbl *bucketTable, now int64, protectedKey string, protectedBatch map[string][]byte) (evictedEntry, bool) {
	nBuckets := len(tbl.buckets)
	var start int
	if s.evict != nil {
		start = int(s.evict.volatileCursor & tbl.mask)
	}

	var victim *entry
	var victimReason EvictReason
	sampled := 0
	nextCursor := (uint64(start) + 1) & tbl.mask

	for i := range nBuckets {
		bIdx := (start + i) & int(tbl.mask)
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
			if e == nil || isProtectedEntry(e.key, protectedKey, protectedBatch) {
				continue
			}
			if !e.isNoExpiration() && now >= e.hardExpiresAt() {
				victim = e
				victimReason = EvictExpired
				sampled = evictionSampleSize
				break
			}
			exp := e.expiresAt.Load()
			if victim == nil || exp < victim.expiresAt.Load() {
				victim = e
				victimReason = EvictCapacity
			}
			sampled++
			if sampled >= evictionSampleSize {
				break
			}
		}
		if sampled >= evictionSampleSize {
			nextCursor = (uint64(bIdx) + 1) & tbl.mask
			break
		}
	}

	if s.evict != nil {
		s.evict.volatileCursor = nextCursor
	}
	if victim == nil {
		return evictedEntry{}, false
	}
	return m.commitEvictionLocked(s, tbl, victim, victimReason), true
}

func (m *MemoryStore) evictOneS3FIFOLocked(s *shard, tbl *bucketTable, now int64, protectedKey string, protectedBatch map[string][]byte) (evictedEntry, bool) {
	ev := s.evict
	total := ev.small.count + ev.main.count
	if total == 0 {
		return evictedEntry{}, false
	}
	if total == 1 && (protectedKey != "" || protectedBatch != nil) {
		only := ev.small.head
		if only == nil {
			only = ev.main.head
		}
		if only != nil && isProtectedEntry(only.key, protectedKey, protectedBatch) {
			return evictedEntry{}, false
		}
	}

	smallTarget := max(total/10, 1)
	maxSteps := int(total)*4 + 8
	var smallSkipped, mainSkipped int64

	for range maxSteps {
		smallAvail := ev.small.count > 0 && smallSkipped < ev.small.count
		mainAvail := ev.main.count > 0 && mainSkipped < ev.main.count
		if !smallAvail && !mainAvail {
			return evictedEntry{}, false
		}
		useSmall := smallAvail && (!mainAvail || (ev.small.count > smallTarget) || (m.maxBytes > 0 && ev.small.count > 1 && ev.small.bytes > (ev.small.bytes+ev.main.bytes)/10))
		if useSmall {
			cand := ev.small.popHead()
			if cand == nil {
				break
			}
			if isProtectedEntry(cand.key, protectedKey, protectedBatch) {
				ev.small.pushTail(cand, evictQueueSmall, payloadSize(cand.key, cand.value))
				smallSkipped++
				continue
			}
			if !cand.isNoExpiration() && now >= cand.hardExpiresAt() {
				tbl.deletePtrLocked(cand)
				sz := payloadSize(cand.key, cand.value)
				s.count.Store(s.count.Load() - 1)
				s.bytes.Store(s.bytes.Load() - sz)
				s.evictions.Add(1)
				if m.bounded {
					m.totalItems.Add(-1)
					m.totalBytes.Add(-sz)
				}
				return evictedEntry{key: cand.key, value: cand.value, reason: EvictExpired}, true
			}
			if f := cand.freq.Load(); f > 0 {
				cand.freq.Store(0)
				ev.main.pushTail(cand, evictQueueMain, payloadSize(cand.key, cand.value))
				mainSkipped = 0
				continue
			}
			ev.ghost.add(cand.hash)
			tbl.deletePtrLocked(cand)
			sz := payloadSize(cand.key, cand.value)
			s.count.Store(s.count.Load() - 1)
			s.bytes.Store(s.bytes.Load() - sz)
			s.evictions.Add(1)
			if m.bounded {
				m.totalItems.Add(-1)
				m.totalBytes.Add(-sz)
			}
			return evictedEntry{key: cand.key, value: cand.value, reason: EvictCapacity}, true
		}

		if !mainAvail {
			continue
		}
		cand := ev.main.popHead()
		if cand == nil {
			break
		}
		if isProtectedEntry(cand.key, protectedKey, protectedBatch) {
			ev.main.pushTail(cand, evictQueueMain, payloadSize(cand.key, cand.value))
			mainSkipped++
			continue
		}
		if !cand.isNoExpiration() && now >= cand.hardExpiresAt() {
			tbl.deletePtrLocked(cand)
			sz := payloadSize(cand.key, cand.value)
			s.count.Store(s.count.Load() - 1)
			s.bytes.Store(s.bytes.Load() - sz)
			s.evictions.Add(1)
			if m.bounded {
				m.totalItems.Add(-1)
				m.totalBytes.Add(-sz)
			}
			return evictedEntry{key: cand.key, value: cand.value, reason: EvictExpired}, true
		}
		if f := cand.freq.Load(); f > 0 {
			cand.freq.CompareAndSwap(f, f-1)
			ev.main.pushTail(cand, evictQueueMain, payloadSize(cand.key, cand.value))
			mainSkipped = 0
			continue
		}
		tbl.deletePtrLocked(cand)
		sz := payloadSize(cand.key, cand.value)
		s.count.Store(s.count.Load() - 1)
		s.bytes.Store(s.bytes.Load() - sz)
		s.evictions.Add(1)
		if m.bounded {
			m.totalItems.Add(-1)
			m.totalBytes.Add(-sz)
		}
		return evictedEntry{key: cand.key, value: cand.value, reason: EvictCapacity}, true
	}

	return m.evictOneVolatileTTLLocked(s, tbl, now, protectedKey, protectedBatch)
}

func (m *MemoryStore) evictOneTinyLFULocked(s *shard, tbl *bucketTable, now int64, protectedKey string, protectedBatch map[string][]byte) (evictedEntry, bool) {
	ev := s.evict
	ev.drainLocked()

	total := ev.small.count + ev.main.count + ev.protected.count
	if total == 0 {
		return evictedEntry{}, false
	}
	if total == 1 && (protectedKey != "" || protectedBatch != nil) {
		only := ev.small.head
		if only == nil {
			only = ev.main.head
		}
		if only == nil {
			only = ev.protected.head
		}
		if only != nil && isProtectedEntry(only.key, protectedKey, protectedBatch) {
			return evictedEntry{}, false
		}
	}

	// Cold-start seeding: move excess window entries into probation (main) so
	// TinyLFU can compare window candidates against main victims.
	if ev.main.count+ev.protected.count == 0 && ev.small.count > 2 {
		for ev.small.count > 2 {
			moved := ev.small.popTail()
			if moved == nil {
				break
			}
			ev.main.pushHead(moved, evictQueueMain, payloadSize(moved.key, moved.value))
		}
	}

	windowTarget := max(total/100, 1)
	if ev.small.count > windowTarget && (ev.main.count+ev.protected.count) > 0 {
		cand := pickTailExcluding(&ev.small, protectedKey, protectedBatch)
		if cand != nil {
			if !cand.isNoExpiration() && now >= cand.hardExpiresAt() {
				return m.commitEvictionLocked(s, tbl, cand, EvictExpired), true
			}
			victim := pickTailExcluding(&ev.main, protectedKey, protectedBatch)
			if victim == nil {
				victim = pickTailExcluding(&ev.protected, protectedKey, protectedBatch)
			}
			if victim != nil {
				if !victim.isNoExpiration() && now >= victim.hardExpiresAt() {
					candSz := payloadSize(cand.key, cand.value)
					ev.small.remove(cand, candSz)
					ev.main.pushHead(cand, evictQueueMain, candSz)
					return m.commitEvictionLocked(s, tbl, victim, EvictExpired), true
				}
				if ev.cms.estimate(cand.hash) > ev.cms.estimate(victim.hash) {
					candSz := payloadSize(cand.key, cand.value)
					ev.small.remove(cand, candSz)
					ev.main.pushHead(cand, evictQueueMain, candSz)
					return m.commitEvictionLocked(s, tbl, victim, EvictCapacity), true
				}
				return m.commitEvictionLocked(s, tbl, cand, EvictCapacity), true
			}
		}
	}

	// Evict from probation -> protected -> window (preferring expired entries).
	for _, list := range []*entryList{&ev.main, &ev.protected, &ev.small} {
		victim := pickTailExcluding(list, protectedKey, protectedBatch)
		if victim == nil {
			continue
		}
		reason := EvictCapacity
		if !victim.isNoExpiration() && now >= victim.hardExpiresAt() {
			reason = EvictExpired
		}
		return m.commitEvictionLocked(s, tbl, victim, reason), true
	}
	return evictedEntry{}, false
}

func pickTailExcluding(l *entryList, protectedKey string, protectedBatch map[string][]byte) *entry {
	cur := l.tail
	for cur != nil {
		if !isProtectedEntry(cur.key, protectedKey, protectedBatch) {
			return cur
		}
		cur = cur.evictPrev
	}
	return nil
}
