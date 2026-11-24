package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

// mustNewStore constructs a MemoryStore with opts, fails the test on error,
// and registers Stop() with t.Cleanup.
func mustNewStore(t *testing.T, opts ...memorystore.Option) *memorystore.MemoryStore {
	t.Helper()
	s, err := memorystore.New(opts...)
	if err != nil {
		t.Fatalf("memorystore.New() unexpected error: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Stop()
	})
	return s
}

// eventually polls cond every interval until it returns true or timeout elapses.
func eventually(t *testing.T, timeout, interval time.Duration, cond func() bool, msg string) {
	t.Helper()
	if interval <= 0 {
		interval = time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("condition not met within %v: %s", timeout, msg)
		}
		time.Sleep(interval)
	}
}

// recordingMetrics is a thread-safe memorystore.MetricsRecorder implementation
// for verifying observability callbacks in E2E tests.
type recordingMetrics struct {
	hits                  atomic.Int64
	misses                atomic.Int64
	evicts                atomic.Int64
	evictedBytes          atomic.Int64
	loads                 atomic.Int64
	sharedLoads           atomic.Int64
	failedLoads           atomic.Int64
	invalidationsSent     atomic.Int64
	invalidationsReceived atomic.Int64
	invalidationsDropped  atomic.Int64
	invalidatedKeys       atomic.Int64
	lastLoadNanos         atomic.Int64

	reasonMu       sync.RWMutex
	evictsByReason map[memorystore.EvictReason]int64
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{
		evictsByReason: make(map[memorystore.EvictReason]int64),
	}
}

func (m *recordingMetrics) RecordHit() {
	m.hits.Add(1)
}

func (m *recordingMetrics) RecordMiss() {
	m.misses.Add(1)
}

func (m *recordingMetrics) RecordEvict(reason memorystore.EvictReason, bytes int64) {
	m.evicts.Add(1)
	m.evictedBytes.Add(bytes)
	m.reasonMu.Lock()
	if m.evictsByReason == nil {
		m.evictsByReason = make(map[memorystore.EvictReason]int64)
	}
	m.evictsByReason[reason]++
	m.reasonMu.Unlock()
}

func (m *recordingMetrics) RecordLoad(duration time.Duration, shared bool, err error) {
	m.loads.Add(1)
	m.lastLoadNanos.Store(duration.Nanoseconds())
	if shared {
		m.sharedLoads.Add(1)
	}
	if err != nil {
		m.failedLoads.Add(1)
	}
}

func (m *recordingMetrics) RecordInvalidation(sent bool, keyCount int) {
	if sent {
		m.invalidationsSent.Add(1)
		m.invalidatedKeys.Add(int64(keyCount))
		return
	}
	m.invalidationsReceived.Add(1)
}

func (m *recordingMetrics) RecordInvalidationDrop(keyCount int) {
	m.invalidationsDropped.Add(int64(keyCount))
}

func (m *recordingMetrics) Hits() int64 {
	return m.hits.Load()
}

func (m *recordingMetrics) Misses() int64 {
	return m.misses.Load()
}

func (m *recordingMetrics) Evicts() int64 {
	return m.evicts.Load()
}

func (m *recordingMetrics) EvictsByReason(reason memorystore.EvictReason) int64 {
	m.reasonMu.RLock()
	defer m.reasonMu.RUnlock()
	return m.evictsByReason[reason]
}

func (m *recordingMetrics) EvictedBytes() int64 {
	return m.evictedBytes.Load()
}

func (m *recordingMetrics) Loads() int64 {
	return m.loads.Load()
}

func (m *recordingMetrics) SharedLoads() int64 {
	return m.sharedLoads.Load()
}

func (m *recordingMetrics) FailedLoads() int64 {
	return m.failedLoads.Load()
}

func (m *recordingMetrics) InvalidationsSent() int64 {
	return m.invalidationsSent.Load()
}

func (m *recordingMetrics) InvalidationsReceived() int64 {
	return m.invalidationsReceived.Load()
}

func (m *recordingMetrics) InvalidationsDropped() int64 {
	return m.invalidationsDropped.Load()
}

func (m *recordingMetrics) InvalidatedKeys() int64 {
	return m.invalidatedKeys.Load()
}

type controllableSub struct {
	ch        chan []byte
	closeOnce sync.Once
	closed    bool
}

func (s *controllableSub) close() {
	s.closeOnce.Do(func() {
		s.closed = true
		close(s.ch)
	})
}

// controllablePubSub is a thread-safe PubSubClient and Pinger test double
// supporting deterministic error, latency, and disconnect fault injection.
type controllablePubSub struct {
	mu                sync.Mutex
	subs              map[string][]*controllableSub
	closed            bool
	pingErr           error
	subscribeErr      error
	publishErr        error
	publishDelay      time.Duration
	publishedPayloads [][]byte

	subscribeCount atomic.Int64
	publishCount   atomic.Int64
}

func newControllablePubSub() *controllablePubSub {
	return &controllablePubSub{
		subs: make(map[string][]*controllableSub),
	}
}

func (p *controllablePubSub) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	p.subscribeCount.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, memorystore.ErrStoreStopped
	}
	if p.subscribeErr != nil {
		return nil, p.subscribeErr
	}
	if topic == "" {
		return nil, memorystore.ErrInvalidTopic
	}

	sub := &controllableSub{
		ch: make(chan []byte, 128),
	}
	p.subs[topic] = append(p.subs[topic], sub)
	return sub.ch, nil
}

func (p *controllablePubSub) Publish(ctx context.Context, topic string, message []byte) error {
	p.publishCount.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	delay := p.publishDelay
	p.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return memorystore.ErrStoreStopped
	}
	if p.publishErr != nil {
		return p.publishErr
	}
	if topic == "" {
		return memorystore.ErrInvalidTopic
	}

	payload := bytes.Clone(message)
	p.publishedPayloads = append(p.publishedPayloads, payload)

	for pattern, subs := range p.subs {
		if pattern == topic || (strings.ContainsRune(pattern, '*') && matchesWildcardTopic(topic, pattern)) {
			for _, sub := range subs {
				if sub.closed {
					continue
				}
				select {
				case sub.ch <- bytes.Clone(payload):
				default:
				}
			}
		}
	}
	return nil
}

func (p *controllablePubSub) Unsubscribe(ctx context.Context, topic string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return memorystore.ErrStoreStopped
	}
	for _, sub := range p.subs[topic] {
		sub.close()
	}
	delete(p.subs, topic)
	return nil
}

func (p *controllablePubSub) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	for topic, subs := range p.subs {
		for _, sub := range subs {
			sub.close()
		}
		delete(p.subs, topic)
	}
	return nil
}

func (p *controllablePubSub) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return memorystore.ErrStoreStopped
	}
	return p.pingErr
}

func (p *controllablePubSub) SetPingError(err error) {
	p.mu.Lock()
	p.pingErr = err
	p.mu.Unlock()
}

func (p *controllablePubSub) SetSubscribeError(err error) {
	p.mu.Lock()
	p.subscribeErr = err
	p.mu.Unlock()
}

func (p *controllablePubSub) SetPublishError(err error) {
	p.mu.Lock()
	p.publishErr = err
	p.mu.Unlock()
}

func (p *controllablePubSub) SetPublishDelay(d time.Duration) {
	p.mu.Lock()
	p.publishDelay = d
	p.mu.Unlock()
}

func (p *controllablePubSub) CloseTopicChannels(topic string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, sub := range p.subs[topic] {
		sub.close()
	}
	delete(p.subs, topic)
}

func (p *controllablePubSub) SubscribeCount() int64 {
	return p.subscribeCount.Load()
}

func (p *controllablePubSub) PublishCount() int64 {
	return p.publishCount.Load()
}

func (p *controllablePubSub) PublishedPayloads() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([][]byte, len(p.publishedPayloads))
	for i, b := range p.publishedPayloads {
		out[i] = bytes.Clone(b)
	}
	return out
}

func matchesWildcardTopic(topic, pattern string) bool {
	tParts := strings.Split(topic, ":")
	pParts := strings.Split(pattern, ":")
	if len(tParts) != len(pParts) {
		return false
	}
	for i := range pParts {
		if pParts[i] != "*" && pParts[i] != tParts[i] {
			return false
		}
	}
	return true
}

// zipfKeys generates a deterministic Zipfian-distributed slice of key names
// using math/rand/v2. For s > 1.0 it uses rand.NewZipf; for 0 < s <= 1.0
// (such as s = 0.99) it uses normalized discrete Zipf CDF sampling.
func zipfKeys(seed uint64, s float64, nKeys, nSamples int) []string {
	if nKeys <= 0 || nSamples <= 0 {
		return nil
	}
	keys := make([]string, nSamples)
	if nKeys == 1 {
		for i := range keys {
			keys[i] = "key-0"
		}
		return keys
	}

	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	if s > 1.0 {
		z := rand.NewZipf(rng, s, 1, uint64(nKeys-1))
		for i := range keys {
			keys[i] = fmt.Sprintf("key-%d", z.Uint64())
		}
		return keys
	}

	cdf := make([]float64, nKeys)
	var sum float64
	for i := range nKeys {
		sum += 1.0 / math.Pow(float64(i+1), s)
		cdf[i] = sum
	}
	for i := range nSamples {
		target := rng.Float64() * sum
		idx := sort.SearchFloat64s(cdf, target)
		if idx >= nKeys {
			idx = nKeys - 1
		}
		keys[i] = fmt.Sprintf("key-%d", idx)
	}
	return keys
}
