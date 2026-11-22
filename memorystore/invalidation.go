package memorystore

import (
	json "encoding/json/v2"
)

// invalidationEvent is the compact JSON payload broadcast over
// Config.InvalidationTopic when local mutations occur.
type invalidationEvent struct {
	Origin string   `json:"o"`
	Keys   []string `json:"k"`
}

func (m *MemoryStore) startInvalidationBackplane() {
	if m.invalidationTopic == "" || m.ps == nil {
		return
	}

	ctx, cancel := m.defaultPubSubContext()
	ch, err := m.ps.Subscribe(ctx, m.invalidationTopic)
	cancel()
	if err != nil {
		m.logger.Error("failed to subscribe to invalidation topic",
			"topic", m.invalidationTopic, "error", err)
		return
	}

	m.wg.Go(func() {
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				m.handleInvalidationMessage(msg)
			case <-m.ctx.Done():
				return
			}
		}
	})
}

func (m *MemoryStore) broadcastInvalidation(keys ...string) {
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

	ctx, cancel := m.defaultPubSubContext()
	defer cancel()
	if err := m.ps.Publish(ctx, m.invalidationTopic, payload); err != nil {
		m.logger.Warn("failed to publish invalidation event",
			"topic", m.invalidationTopic, "error", err)
	}
}

func (m *MemoryStore) handleInvalidationMessage(payload []byte) {
	var evt invalidationEvent
	if err := json.Unmarshal(payload, &evt); err != nil {
		m.logger.Warn("ignored malformed invalidation event", "error", err)
		return
	}
	// Suppress our own broadcasts so local writes do not self-evict.
	if evt.Origin == m.instanceID {
		return
	}
	for _, key := range evt.Keys {
		m.deleteLocal(key)
	}
}
