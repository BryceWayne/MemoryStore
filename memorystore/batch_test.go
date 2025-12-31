package memorystore

import (
	"testing"
	"time"
)

func TestBatchOperations(t *testing.T) {
	ms := NewMemoryStore()
	defer ms.Stop()

	// Test SetMulti
	items := map[string][]byte{
		"key1": []byte("value1"),
		"key2": []byte("value2"),
		"key3": []byte("value3"),
	}

	if err := ms.SetMulti(items, time.Minute); err != nil {
		t.Fatalf("SetMulti failed: %v", err)
	}

	// Verify items are set
	for k, v := range items {
		val, exists := ms.Get(k)
		if !exists {
			t.Errorf("Key %s not found", k)
		}
		if string(val) != string(v) {
			t.Errorf("Value mismatch for key %s: expected %s, got %s", k, v, val)
		}
	}

	// Test GetMulti
	keys := []string{"key1", "key2", "key3", "nonexistent"}
	results := ms.GetMulti(keys)

	if len(results) != 3 {
		t.Errorf("Expected 3 items, got %d", len(results))
	}

	for k, v := range items {
		if val, ok := results[k]; !ok {
			t.Errorf("Key %s missing from results", k)
		} else if string(val) != string(v) {
			t.Errorf("Value mismatch for key %s: expected %s, got %s", k, v, val)
		}
	}

	if _, ok := results["nonexistent"]; ok {
		t.Error("Non-existent key returned in results")
	}

	// Verify metrics update
	metrics := ms.GetMetrics()
	// Hits: 3 from individual Get calls in loop + 3 from GetMulti call
	// Misses: 1 from GetMulti call (nonexistent)
	if metrics.Hits != 6 {
		t.Errorf("Expected 6 hits, got %d", metrics.Hits)
	}
	if metrics.Misses != 1 {
		t.Errorf("Expected 1 miss, got %d", metrics.Misses)
	}
}
