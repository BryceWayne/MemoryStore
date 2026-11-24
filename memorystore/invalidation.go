package memorystore

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	invalidationBackoffInit = 100 * time.Millisecond
	invalidationBackoffMax  = 30 * time.Second
	invalidationDrainBatch  = 64
	dropLogRateLimit        = 5 * time.Second
)

type writeThroughEntry struct {
	Key         string `json:"k"`
	Value       []byte `json:"v"`
	TTLNanos    int64  `json:"t"`
	Sliding     bool   `json:"s,omitzero"`
	TTLCapNanos int64  `json:"c,omitzero"`
	StaleGrace  int64  `json:"g,omitzero"`
}

// invalidationEvent is the compact JSON payload broadcast over
// Config.InvalidationTopic when local mutations occur.
type invalidationEvent struct {
	Origin  string              `json:"o"`
	Mode    InvalidationMode    `json:"m,omitzero"`
	Keys    []string            `json:"k,omitzero"`
	Entries []writeThroughEntry `json:"e,omitzero"`
}

type invalidationAction uint8

const (
	invActionDelete invalidationAction = iota
	invActionDeleteBatch
	invActionSet
	invActionSetBatch
)

type writeThroughItem struct {
	key        string
	value      []byte
	expiresAt  int64
	ttlNanos   int64
	staleGrace int64
	noExp      bool
	sliding    bool
}

type invalidationTask struct {
	action invalidationAction
	key    string
	keys   []string
	item   writeThroughItem
	items  []writeThroughItem
}

func writeThroughItemFromEntry(e *entry) writeThroughItem {
	return writeThroughItem{
		key:        e.key,
		value:      e.value,
		expiresAt:  e.expiresAt.Load(),
		ttlNanos:   e.ttlNanos,
		staleGrace: e.staleGrace,
		noExp:      e.isNoExpiration(),
		sliding:    e.isSliding(),
	}
}

func taskKeyCount(t invalidationTask) int {
	switch t.action {
	case invActionDelete, invActionSet:
		return 1
	case invActionDeleteBatch:
		return len(t.keys)
	case invActionSetBatch:
		return len(t.items)
	default:
		return 0
	}
}

func (m *MemoryStore) startInvalidationBackplane() {
	if m.invalidationTopic == "" || m.ps == nil {
		return
	}

	subCtx, subCancel := m.backplanePubSubContext()
	ch, err := m.ps.Subscribe(subCtx, m.invalidationTopic)
	subCancel()
	if err != nil {
		wrapped := fmt.Errorf("invalidation subscription failed: %w", err)
		m.invalidationErr.Store(&wrapped)
		m.logger.Error("failed to subscribe to invalidation topic",
			"topic", m.invalidationTopic, "error", err)
		ch = nil
	} else if ch == nil {
		nilErr := errors.New("invalidation subscription returned nil channel")
		m.invalidationErr.Store(&nilErr)
	}

	m.wg.Go(func() {
		backoff := invalidationBackoffInit
		for {
			if m.ctx.Err() != nil {
				return
			}
			if ch != nil {
				select {
				case msg, ok := <-ch:
					if !ok {
						if !m.stopped.Load() && m.ctx.Err() == nil {
							closedErr := errors.New("invalidation subscription closed")
							m.invalidationErr.Store(&closedErr)
							m.logger.Warn("invalidation subscription closed; reconnecting",
								"topic", m.invalidationTopic)
						}
						ch = nil
						continue
					}
					m.handleInvalidationMessage(msg)
				case <-m.ctx.Done():
					return
				}
				continue
			}

			jittered := time.Duration(float64(backoff) * (0.8 + rand.Float64()*0.4))
			timer := time.NewTimer(jittered)
			select {
			case <-timer.C:
			case <-m.ctx.Done():
				timer.Stop()
				return
			}

			retryCtx, retryCancel := m.backplanePubSubContext()
			newCh, subErr := m.ps.Subscribe(retryCtx, m.invalidationTopic)
			retryCancel()
			if subErr != nil {
				wrapped := fmt.Errorf("invalidation subscription failed: %w", subErr)
				m.invalidationErr.Store(&wrapped)
				m.logger.Warn("failed to resubscribe to invalidation topic",
					"topic", m.invalidationTopic, "error", subErr, "backoff", backoff)
				backoff = min(backoff*2, invalidationBackoffMax)
				continue
			}
			if newCh == nil {
				nilErr := errors.New("invalidation subscription returned nil channel")
				m.invalidationErr.Store(&nilErr)
				backoff = min(backoff*2, invalidationBackoffMax)
				continue
			}
			ch = newCh
			backoff = invalidationBackoffInit
			m.invalidationErr.Store(nil)
		}
	})
}

func (m *MemoryStore) startInvalidationSender() {
	if !m.asyncInvalidation || m.invalidationTopic == "" || m.ps == nil {
		return
	}
	m.wg.Go(func() {
		batch := make([]invalidationTask, 0, invalidationDrainBatch)
		for {
			select {
			case task := <-m.invalidationQueue:
				batch = append(batch[:0], task)
			drainLoop:
				for len(batch) < invalidationDrainBatch {
					select {
					case nextTask := <-m.invalidationQueue:
						batch = append(batch, nextTask)
					default:
						break drainLoop
					}
				}
				if m.stopped.Load() || m.ctx.Err() != nil {
					return
				}
				m.publishInvalidationBatch(batch)
			case <-m.ctx.Done():
				return
			}
		}
	})
}

func (m *MemoryStore) enqueueInvalidation(task invalidationTask) {
	if m.stopped.Load() {
		return
	}
	select {
	case m.invalidationQueue <- task:
	default:
		m.recordInvalidationDrop(taskKeyCount(task))
	}
}

func (m *MemoryStore) recordInvalidationDrop(keyCount int) {
	if keyCount <= 0 {
		return
	}
	m.invalidationDrops.Add(int64(keyCount))
	if m.dropRecorder != nil {
		m.dropRecorder.RecordInvalidationDrop(keyCount)
	}
	now := m.nowExact()
	last := m.lastDropLogNano.Load()
	if last < 0 || now-last >= int64(dropLogRateLimit) {
		if m.lastDropLogNano.CompareAndSwap(last, now) {
			m.logger.Warn("invalidation queue full; dropping invalidation event",
				"topic", m.invalidationTopic,
				"dropped_keys", keyCount,
				"total_drops", m.invalidationDrops.Load())
		}
	}
}

func (m *MemoryStore) broadcastInvalidation(keys ...string) {
	if len(keys) == 0 || m.IsStopped() {
		return
	}
	if m.asyncInvalidation {
		if len(keys) == 1 {
			m.enqueueInvalidation(invalidationTask{action: invActionDelete, key: keys[0]})
		} else {
			m.enqueueInvalidation(invalidationTask{action: invActionDeleteBatch, keys: keys})
		}
		return
	}
	m.publishInvalidationKeys(keys)
}

func (m *MemoryStore) broadcastSetWriteThrough(e *entry) {
	if m.IsStopped() {
		return
	}
	item := writeThroughItemFromEntry(e)
	if m.asyncInvalidation {
		m.enqueueInvalidation(invalidationTask{action: invActionSet, item: item})
		return
	}
	m.publishWriteThroughItems([]writeThroughItem{item}, nil)
}

func (m *MemoryStore) broadcastSetMultiWriteThrough(wtItems []writeThroughItem, invKeys []string) {
	if m.IsStopped() {
		return
	}
	if m.asyncInvalidation {
		if len(wtItems) > 0 {
			m.enqueueInvalidation(invalidationTask{action: invActionSetBatch, items: wtItems})
		}
		if len(invKeys) > 0 {
			m.enqueueInvalidation(invalidationTask{action: invActionDeleteBatch, keys: invKeys})
		}
		return
	}
	m.publishWriteThroughItems(wtItems, invKeys)
}

type coalescedInvalidation struct {
	isSet bool
	item  writeThroughItem
}

func (m *MemoryStore) publishInvalidationBatch(batch []invalidationTask) {
	if len(batch) == 1 {
		t := batch[0]
		switch t.action {
		case invActionDelete:
			m.publishInvalidationKeys([]string{t.key})
		case invActionDeleteBatch:
			m.publishInvalidationKeys(t.keys)
		case invActionSet:
			m.publishWriteThroughItems([]writeThroughItem{t.item}, nil)
		case invActionSetBatch:
			m.publishWriteThroughItems(t.items, nil)
		}
		return
	}

	latest := make(map[string]coalescedInvalidation, len(batch))
	order := make([]string, 0, len(batch))

	setDelete := func(k string) {
		if _, exists := latest[k]; !exists {
			order = append(order, k)
		}
		latest[k] = coalescedInvalidation{isSet: false}
	}
	setWrite := func(it writeThroughItem) {
		if _, exists := latest[it.key]; !exists {
			order = append(order, it.key)
		}
		latest[it.key] = coalescedInvalidation{isSet: true, item: it}
	}

	for _, t := range batch {
		switch t.action {
		case invActionDelete:
			setDelete(t.key)
		case invActionDeleteBatch:
			for _, k := range t.keys {
				setDelete(k)
			}
		case invActionSet:
			setWrite(t.item)
		case invActionSetBatch:
			for _, it := range t.items {
				setWrite(it)
			}
		}
	}

	var invKeys []string
	var wtItems []writeThroughItem
	for _, k := range order {
		c := latest[k]
		if c.isSet {
			wtItems = append(wtItems, c.item)
		} else {
			invKeys = append(invKeys, k)
		}
	}

	if len(wtItems) == 0 {
		m.publishInvalidationKeys(invKeys)
		return
	}
	m.publishWriteThroughItems(wtItems, invKeys)
}

func (m *MemoryStore) publishWriteThroughItems(wtItems []writeThroughItem, invKeys []string) {
	now := m.nowExact()
	wtEntries := make([]writeThroughEntry, 0, len(wtItems))
	fallbackKeys := invKeys

	for _, it := range wtItems {
		var ttlNanos int64
		if it.noExp {
			ttlNanos = int64(NoExpiration)
		} else {
			remaining := it.expiresAt - now
			if remaining <= 0 {
				fallbackKeys = append(fallbackKeys, it.key)
				continue
			}
			ttlNanos = remaining
		}
		wtEntries = append(wtEntries, writeThroughEntry{
			Key:         it.key,
			Value:       it.value,
			TTLNanos:    ttlNanos,
			Sliding:     it.sliding,
			TTLCapNanos: it.ttlNanos,
			StaleGrace:  it.staleGrace,
		})
	}

	if len(wtEntries) == 0 {
		if len(fallbackKeys) > 0 {
			m.publishInvalidationKeys(fallbackKeys)
		}
		return
	}

	if m.writeThroughMaxBytes <= 0 {
		for _, we := range wtEntries {
			fallbackKeys = append(fallbackKeys, we.Key)
		}
		m.publishInvalidationKeys(fallbackKeys)
		return
	}

	payload, err := json.Marshal(&invalidationEvent{
		Origin:  m.instanceID,
		Mode:    InvalidationModeWriteThrough,
		Keys:    fallbackKeys,
		Entries: wtEntries,
	})
	if err != nil {
		m.logger.Warn("failed to marshal write-through invalidation event", "error", err)
		return
	}
	if len(payload) <= m.writeThroughMaxBytes {
		m.publishPayload(payload, len(fallbackKeys)+len(wtEntries))
		return
	}

	// Payload exceeded writeThroughMaxBytes: pack entries into chunks <= limit
	// and fall back to InvalidateOnly for any individual oversized entry.
	chunk := make([]writeThroughEntry, 0, len(wtEntries))
	for _, we := range wtEntries {
		candidate := append(chunk, we)
		candPayload, merr := json.Marshal(&invalidationEvent{
			Origin:  m.instanceID,
			Mode:    InvalidationModeWriteThrough,
			Entries: candidate,
		})
		if merr != nil {
			m.logger.Warn("failed to marshal write-through chunk", "error", merr)
			fallbackKeys = append(fallbackKeys, we.Key)
			continue
		}
		if len(candPayload) <= m.writeThroughMaxBytes {
			chunk = candidate
			continue
		}

		// Flush existing chunk first if non-empty.
		if len(chunk) > 0 {
			if flushPayload, ferr := json.Marshal(&invalidationEvent{
				Origin:  m.instanceID,
				Mode:    InvalidationModeWriteThrough,
				Entries: chunk,
			}); ferr == nil {
				m.publishPayload(flushPayload, len(chunk))
			}
			chunk = chunk[:0]
			// Retry current entry alone.
			singlePayload, serr := json.Marshal(&invalidationEvent{
				Origin:  m.instanceID,
				Mode:    InvalidationModeWriteThrough,
				Entries: []writeThroughEntry{we},
			})
			if serr == nil && len(singlePayload) <= m.writeThroughMaxBytes {
				chunk = append(chunk, we)
				continue
			}
		}
		// Single entry itself exceeds writeThroughMaxBytes: fall back to InvalidateOnly.
		fallbackKeys = append(fallbackKeys, we.Key)
	}
	if len(chunk) > 0 {
		if flushPayload, ferr := json.Marshal(&invalidationEvent{
			Origin:  m.instanceID,
			Mode:    InvalidationModeWriteThrough,
			Entries: chunk,
		}); ferr == nil {
			m.publishPayload(flushPayload, len(chunk))
		}
	}
	if len(fallbackKeys) > 0 {
		m.publishInvalidationKeys(fallbackKeys)
	}
}

func (m *MemoryStore) publishInvalidationKeys(keys []string) {
	if len(keys) == 0 || m.IsStopped() {
		return
	}
	payload, err := json.Marshal(&invalidationEvent{
		Origin: m.instanceID,
		Keys:   keys,
	})
	if err != nil {
		m.logger.Warn("failed to marshal invalidation event", "error", err)
		return
	}
	m.publishPayload(payload, len(keys))
}

func (m *MemoryStore) publishPayload(payload []byte, keyCount int) {
	if m.IsStopped() {
		return
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if m.asyncInvalidation {
		ctx, cancel = m.backplanePubSubContext()
	} else {
		ctx, cancel = m.defaultPubSubContext()
	}
	defer cancel()

	if err := m.ps.Publish(ctx, m.invalidationTopic, payload); err != nil {
		m.logger.Warn("failed to publish invalidation event",
			"topic", m.invalidationTopic, "error", err)
		return
	}
	if m.recorder != nil {
		m.recorder.RecordInvalidation(true, keyCount)
	}
}

func (m *MemoryStore) handleInvalidationMessage(payload []byte) {
	if m.stopped.Load() {
		return
	}
	var evt invalidationEvent
	if err := json.Unmarshal(payload, &evt); err != nil {
		m.logger.Warn("ignored malformed invalidation event", "error", err)
		return
	}
	// Suppress our own broadcasts so local writes do not self-evict.
	if evt.Origin == m.instanceID {
		return
	}
	applied := 0
	if evt.Mode == InvalidationModeWriteThrough && len(evt.Entries) > 0 {
		for _, we := range evt.Entries {
			switch {
			case we.TTLNanos == int64(NoExpiration):
				_ = m.setLocal(we.Key, we.Value, NoExpiration, setOptions{
					hasJitter:     true,
					jitter:        0,
					noExpiration:  true,
					staleGrace:    max(time.Duration(we.StaleGrace), 0),
					hasStaleGrace: true,
				})
			case we.TTLNanos > 0:
				_ = m.setLocal(we.Key, we.Value, time.Duration(we.TTLNanos), setOptions{
					hasJitter:     true,
					jitter:        0,
					sliding:       we.Sliding,
					ttlCapNanos:   we.TTLCapNanos,
					staleGrace:    max(time.Duration(we.StaleGrace), 0),
					hasStaleGrace: true,
				})
			default:
				_ = m.deleteLocal(we.Key)
			}
			applied++
		}
	}
	for _, key := range evt.Keys {
		_ = m.deleteLocal(key)
		applied++
	}
	if m.recorder != nil && applied > 0 {
		m.recorder.RecordInvalidation(false, applied)
	}
}
