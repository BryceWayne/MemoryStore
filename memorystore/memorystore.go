package memorystore

import (
	"context"
	"sync"
	"time"
)

type item struct {
	value     interface{}
	expiresAt time.Time
}

// MemoryStore is a simple in-memory key-value store
type MemoryStore struct {
	mu         sync.RWMutex
	store      map[string]item
	ctx        context.Context
	cancelFunc context.CancelFunc
}

func NewMemoryStore() *MemoryStore {
	ctx, cancel := context.WithCancel(context.Background())
	store := &MemoryStore{
		store:      make(map[string]item),
		ctx:        ctx,
		cancelFunc: cancel,
	}
	store.startCleanupWorker()
	return store
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

// Set a value in the store
func (m *MemoryStore) Set(key string, value interface{}, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[key] = item{
		value:     value,
		expiresAt: time.Now().Add(duration),
	}
}

// Get a value from the store
func (m *MemoryStore) Get(key string) (interface{}, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, exists := m.store[key]
	if !exists || time.Now().After(item.expiresAt) {
		return nil, false
	}
	return item.value, true
}

// Delete a value from the store
func (m *MemoryStore) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, key)
}
