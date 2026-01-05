package memorystore

import (
	"testing"
	"time"
)

func TestMetrics(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	// Initial metrics should be zero
	metrics := ms.GetMetrics()
	if metrics.Hits != 0 || metrics.Misses != 0 || metrics.Evictions != 0 || metrics.Items != 0 {
		t.Errorf("Expected initial metrics to be zero, got %+v", metrics)
	}

	// Test Hits
	_ = ms.Set("key1", []byte("value1"), time.Minute)
	ms.Get("key1")
	metrics = ms.GetMetrics()
	if metrics.Hits != 1 {
		t.Errorf("Expected 1 hit, got %d", metrics.Hits)
	}

	// Test Misses
	ms.Get("nonexistent")
	metrics = ms.GetMetrics()
	if metrics.Misses != 1 {
		t.Errorf("Expected 1 miss, got %d", metrics.Misses)
	}

	// Test Items
	_ = ms.Set("key2", []byte("value2"), time.Minute)
	metrics = ms.GetMetrics()
	if metrics.Items != 2 {
		t.Errorf("Expected 2 items, got %d", metrics.Items)
	}

	// Test Evictions
	_ = ms.Set("expired", []byte("expired"), 1*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // Wait for expiration

	// Trigger cleanup
	ms.cleanupExpiredItems()

	metrics = ms.GetMetrics()
	if metrics.Evictions != 1 {
		t.Errorf("Expected 1 eviction, got %d", metrics.Evictions)
	}
}
