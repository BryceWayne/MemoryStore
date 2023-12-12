package memorystore

import (
	"context"
	"sync"
	"time"
)

type item struct {
	value     []byte // Raw data as a byte slice
	expiresAt time.Time
}

type MemoryStore struct {
	mu         sync.RWMutex
	store      map[string]item
	ctx        context.Context
	cancelFunc context.CancelFunc
}

func NewMemoryStore() *MemoryStore {
	ctx, cancel := context.WithCancel(context.Background())
	return &MemoryStore{
		store:      make(map[string]item),
		ctx:        ctx,
		cancelFunc: cancel,
	}
}

// Add a method to stop the background worker
func (m *MemoryStore) Stop() {
	m.cancelFunc()
}

func (m *MemoryStore) startCleanupWorker() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute) // Adjust the interval as needed
		for {
			select {
			case <-ticker.C:
				m.cleanupExpiredItems()
			case <-m.ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
}

func (m *MemoryStore) cleanupExpiredItems() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, item := range m.store {
		if time.Now().After(item.expiresAt) {
			delete(m.store, key)
		}
	}
}

func (m *MemoryStore) Set(key string, value []byte, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.store[key] = item{
		value:     value,
		expiresAt: time.Now().Add(duration),
	}
}

func (m *MemoryStore) Get(key string) ([]byte, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	it, exists := m.store[key]
	if !exists || time.Now().After(it.expiresAt) {
		return nil, false, nil
	}

	return it.value, true, nil
}

func (m *MemoryStore) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, key)
}
