package memorystore

import (
    "testing"
    "time"
)

// TestMemoryStore_SetGet tests the basic Set and Get operations
func TestMemoryStore_SetGet(t *testing.T) {
    tests := []struct {
        name       string
        key        string
        value      []byte
        expiration time.Duration
        wantExists bool
        wantErr    bool
    }{
        {
            name:       "basic set and get",
            key:        "test1",
            value:      []byte("hello"),
            expiration: time.Minute,
            wantExists: true,
            wantErr:    false,
        },
        {
            name:       "empty key",
            key:        "",
            value:      []byte("value"),
            expiration: time.Minute,
            wantExists: true,
            wantErr:    false,
        },
        {
            name:       "nil value",
            key:        "nil-value",
            value:      nil,
            expiration: time.Minute,
            wantExists: true,
            wantErr:    false,
        },
        {
            name:       "immediate expiration",
            key:        "expire",
            value:      []byte("expired"),
            expiration: 0,
            wantExists: false,
            wantErr:    false,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            ms := NewMemoryStore()
            defer ms.Stop()

            err := ms.Set(tt.key, tt.value, tt.expiration)
            if (err != nil) != tt.wantErr {
                t.Errorf("Set() error = %v, wantErr %v", err, tt.wantErr)
                return
            }

            got, exists := ms.Get(tt.key)
            if exists != tt.wantExists {
                t.Errorf("Get() exists = %v, want %v", exists, tt.wantExists)
            }
            if exists && string(got) != string(tt.value) {
                t.Errorf("Get() got = %v, want %v", string(got), string(tt.value))
            }
        })
    }
}

// TestMemoryStore_SetGetJSON tests JSON serialization and deserialization
func TestMemoryStore_SetGetJSON(t *testing.T) {
    type testStruct struct {
        Name  string `json:"name"`
        Value int    `json:"value"`
    }

    tests := []struct {
        name       string
        key        string
        value      testStruct
        expiration time.Duration
        wantErr    bool
    }{
        {
            name: "valid struct",
            key:  "test1",
            value: testStruct{
                Name:  "test",
                Value: 123,
            },
            expiration: time.Minute,
            wantErr:    false,
        },
        {
            name: "zero value struct",
            key:  "test2",
            value: testStruct{
                Name:  "",
                Value: 0,
            },
            expiration: time.Minute,
            wantErr:    false,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            ms := NewMemoryStore()
            defer ms.Stop()

            err := ms.SetJSON(tt.key, tt.value, tt.expiration)
            if (err != nil) != tt.wantErr {
                t.Errorf("SetJSON() error = %v, wantErr %v", err, tt.wantErr)
                return
            }

            var got testStruct
            exists, err := ms.GetJSON(tt.key, &got)
            if (err != nil) != tt.wantErr {
                t.Errorf("GetJSON() error = %v, wantErr %v", err, tt.wantErr)
                return
            }
            if !exists {
                t.Error("GetJSON() value should exist")
                return
            }
            if got != tt.value {
                t.Errorf("GetJSON() got = %v, want %v", got, tt.value)
            }
        })
    }
}

// TestMemoryStore_Delete tests the Delete operation
func TestMemoryStore_Delete(t *testing.T) {
    ms := NewMemoryStore()
    defer ms.Stop()

    // Set up test data
    testData := map[string][]byte{
        "key1": []byte("value1"),
        "key2": []byte("value2"),
    }

    for k, v := range testData {
        if err := ms.Set(k, v, time.Minute); err != nil {
            t.Fatalf("Setup failed: %v", err)
        }
    }

    // Test deletion
    for k := range testData {
        ms.Delete(k)
        if _, exists := ms.Get(k); exists {
            t.Errorf("Delete() key %s should not exist after deletion", k)
        }
    }
}

// TestMemoryStore_Expiration tests expiration functionality
func TestMemoryStore_Expiration(t *testing.T) {
    ms := NewMemoryStore()
    defer ms.Stop()

    key := "expiring"
    value := []byte("value")
    shortDuration := 100 * time.Millisecond

    if err := ms.Set(key, value, shortDuration); err != nil {
        t.Fatalf("Set() error = %v", err)
    }

    // Verify value exists immediately
    if _, exists := ms.Get(key); !exists {
        t.Error("Value should exist before expiration")
    }

    // Wait for expiration
    time.Sleep(shortDuration + 50*time.Millisecond)

    // Verify value has expired
    if _, exists := ms.Get(key); exists {
        t.Error("Value should not exist after expiration")
    }
}

// TestMemoryStore_Stop tests the store shutdown functionality
func TestMemoryStore_Stop(t *testing.T) {
    ms := NewMemoryStore()

    // Store a value
    if err := ms.Set("key", []byte("value"), time.Minute); err != nil {
        t.Fatalf("Set() error = %v", err)
    }

    // Stop the store
    if err := ms.Stop(); err != nil {
        t.Fatalf("Stop() error = %v", err)
    }

    // Verify store is stopped
    if !ms.IsStopped() {
        t.Error("IsStopped() should return true after Stop()")
    }

    // Verify second stop is safe
    if err := ms.Stop(); err != nil {
        t.Errorf("Second Stop() should not return error, got %v", err)
    }
}

// TestMemoryStore_Concurrent tests concurrent access
func TestMemoryStore_Concurrent(t *testing.T) {
    ms := NewMemoryStore()
    defer ms.Stop()

    const goroutines = 10
    const operationsPerGoroutine = 100
    done := make(chan bool, goroutines)

    for i := 0; i < goroutines; i++ {
        go func(id int) {
            for j := 0; j < operationsPerGoroutine; j++ {
                key := time.Now().String() // Use timestamp to ensure unique keys
                value := []byte("test")

                // Test Set
                if err := ms.Set(key, value, time.Minute); err != nil {
                    t.Errorf("Concurrent Set() error = %v", err)
                }

                // Test Get
                if _, exists := ms.Get(key); !exists {
                    t.Errorf("Concurrent Get() value should exist")
                }

                // Test Delete
                ms.Delete(key)
            }
            done <- true
        }(i)
    }

    // Wait for all goroutines to complete
    for i := 0; i < goroutines; i++ {
        <-done
    }
}

// BenchmarkMemoryStore_SetGet benchmarks Set and Get operations
func BenchmarkMemoryStore_SetGet(b *testing.B) {
    ms := NewMemoryStore()
    defer ms.Stop()

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        key := time.Now().String()
        value := []byte("benchmark value")

        if err := ms.Set(key, value, time.Minute); err != nil {
            b.Fatalf("Set() error = %v", err)
        }

        if _, exists := ms.Get(key); !exists {
            b.Fatal("Get() value should exist")
        }
    }
}
