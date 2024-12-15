// main.go
// Package main provides a demonstration of the memorystore package functionality.
// It shows various use cases including storing/retrieving data, handling expiration,
// and proper error handling.
package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/BryceWayne/MemoryStore/memorystore"
	"github.com/google/uuid"
)

// Person represents a sample data structure used to demonstrate
// JSON serialization and deserialization with the MemoryStore.
type Person struct {
	Name    string    `json:"name"`              // Person's full name
	Age     int       `json:"age"`               // Person's age in years
	UID     string    `json:"uid"`               // Unique identifier
	Created time.Time `json:"created,omitempty"` // Time when the record was created
}

// demonstrateBasicOperations shows basic Set/Get operations with raw bytes
// and proper error handling.
func demonstrateBasicOperations(ms *memorystore.MemoryStore) {
	log.Println("=== Demonstrating Basic Operations ===")

	// Create a sample person
	person := Person{
		Name:    "Alice Smith",
		Age:     30,
		UID:     uuid.New().String(),
		Created: time.Now(),
	}

	// Using SetJSON for convenient JSON serialization
	err := ms.SetJSON(person.UID, person, 2*time.Second)
	if err != nil {
		log.Printf("Failed to store person: %v", err)
		return
	}
	log.Printf("Stored person with UID: %s", person.UID)

	// Retrieve the stored person
	var retrievedPerson Person
	exists, err := ms.GetJSON(person.UID, &retrievedPerson)
	if err != nil {
		log.Printf("Error retrieving person: %v", err)
		return
	}
	if exists {
		log.Printf("Retrieved person: %+v", retrievedPerson)
	}
}

// demonstrateExpiration shows how the cache handles expired items.
func demonstrateExpiration(ms *memorystore.MemoryStore) {
	log.Println("\n=== Demonstrating Expiration ===")

	key := "expiring_key"
	value := []byte("This value will expire soon")
	shortDuration := 1 * time.Second

	if err := ms.Set(key, value, shortDuration); err != nil {
		log.Printf("Failed to store expiring value: %v", err)
		return
	}
	log.Printf("Stored value with %v expiration", shortDuration)

	// Wait for the value to expire
	time.Sleep(shortDuration + 100*time.Millisecond)

	if _, exists := ms.Get(key); !exists {
		log.Println("Value has expired as expected")
	}
}

// demonstrateNonExistentKeys shows how the cache handles missing keys.
func demonstrateNonExistentKeys(ms *memorystore.MemoryStore) {
	log.Println("\n=== Demonstrating Non-Existent Keys ===")

	nonExistentKey := uuid.New().String()
	if _, exists := ms.Get(nonExistentKey); !exists {
		log.Printf("As expected, key does not exist: %s", nonExistentKey)
	}

	var person Person
	exists, err := ms.GetJSON(nonExistentKey, &person)
	if !exists && err == nil {
		log.Println("GetJSON correctly handles non-existent keys")
	}
}

// demonstrateStoreLifecycle shows proper initialization and cleanup of the MemoryStore.
func demonstrateStoreLifecycle() {
	log.Println("\n=== Demonstrating Store Lifecycle ===")

	// Create a new store
	ms := memorystore.NewMemoryStore()
	log.Println("Created new MemoryStore")

	// Store a value
	key := "lifecycle_test"
	if err := ms.Set(key, []byte("test"), time.Minute); err != nil {
		log.Printf("Failed to store value: %v", err)
		return
	}

	// Properly stop the store
	if err := ms.Stop(); err != nil {
		log.Printf("Error stopping store: %v", err)
		return
	}
	log.Println("Successfully stopped MemoryStore")

	// Verify store is stopped
	if ms.IsStopped() {
		log.Println("Confirmed store is stopped")
	}
}

// demonstratePubSub shows the publish/subscribe functionality
// with pattern matching and multiple subscribers.
// demonstratePubSub shows the publish/subscribe functionality
// with complex pattern matching and JSON message support.
func demonstratePubSub(ms *memorystore.MemoryStore) {
	log.Println("\n=== Demonstrating PubSub System ===")

	var wg sync.WaitGroup

	// 1. Complex Pattern Matching Examples
	log.Println("Setting up pattern-based subscriptions...")
	patterns := map[string]<-chan []byte{} // Store channels for cleanup

	// Subscribe to various patterns
	subscribePatterns := []string{
		"users:*:status",       // Match all user statuses
		"users:admin:*",        // Match all admin events
		"orders:*.completed",   // Match all completed orders
		"notifications:*:high", // Match high-priority notifications
		"system:*.error",       // Match all system errors
	}

	for _, pattern := range subscribePatterns {
		ch, err := ms.Subscribe(pattern)
		if err != nil {
			log.Printf("Failed to subscribe to %s: %v", pattern, err)
			continue
		}
		patterns[pattern] = ch
		log.Printf("Subscribed to pattern: %s", pattern)
	}

	// 2. JSON Message Integration
	type UserStatus struct {
		UserID   string            `json:"user_id"`
		Status   string            `json:"status"`
		LastSeen time.Time         `json:"last_seen"`
		Metadata map[string]string `json:"metadata"`
	}

	type OrderEvent struct {
		OrderID     string    `json:"order_id"`
		Status      string    `json:"status"`
		CompletedAt time.Time `json:"completed_at"`
		Total       float64   `json:"total"`
	}

	// Set up listeners for each pattern
	for pattern, ch := range patterns {
		wg.Add(1)
		go func(pattern string, ch <-chan []byte) {
			defer wg.Done()
			log.Printf("Listening on pattern: %s", pattern)

			select {
			case msg := <-ch:
				// Try to decode as UserStatus if it's a user event
				if strings.HasPrefix(pattern, "users:") {
					var status UserStatus
					if err := json.Unmarshal(msg, &status); err == nil {
						log.Printf("[%s] User Status Update: %+v", pattern, status)
					} else {
						log.Printf("[%s] Raw message: %s", pattern, string(msg))
					}
				} else if strings.HasPrefix(pattern, "orders:") {
					// Try to decode as OrderEvent
					var order OrderEvent
					if err := json.Unmarshal(msg, &order); err == nil {
						log.Printf("[%s] Order Event: %+v", pattern, order)
					} else {
						log.Printf("[%s] Raw message: %s", pattern, string(msg))
					}
				} else {
					log.Printf("[%s] Message received: %s", pattern, string(msg))
				}
			case <-time.After(2 * time.Second):
				log.Printf("[%s] No message received", pattern)
			}
		}(pattern, ch)
	}

	// Publish various types of messages
	time.Sleep(100 * time.Millisecond) // Ensure subscribers are ready
	log.Println("\nPublishing messages...")

	// Publish JSON user status
	adminStatus := UserStatus{
		UserID:   "admin123",
		Status:   "online",
		LastSeen: time.Now(),
		Metadata: map[string]string{"location": "NYC", "device": "desktop"},
	}
	statusJSON, _ := json.Marshal(adminStatus)
	ms.Publish("users:admin:status", statusJSON)

	// Publish JSON order completion
	order := OrderEvent{
		OrderID:     "ORD-789",
		Status:      "completed",
		CompletedAt: time.Now(),
		Total:       299.99,
	}
	orderJSON, _ := json.Marshal(order)
	ms.Publish("orders:ORD-789.completed", orderJSON)

	// Publish system error
	ms.Publish("system:database.error", []byte("Connection timeout"))

	// Publish high-priority notification
	ms.Publish("notifications:user123:high", []byte("Account security alert"))

	// Wait for message processing
	wg.Wait()

	log.Println("\nDemonstrating pattern matching scenarios...")
	// Show which patterns match different keys
	testCases := []struct {
		channel  string
		patterns []string
	}{
		{
			channel:  "users:admin:login",
			patterns: []string{"users:admin:*"},
		},
		{
			channel:  "orders:xyz-789.completed",
			patterns: []string{"orders:*.completed"},
		},
		{
			channel:  "notifications:admin:high",
			patterns: []string{"notifications:*:high"},
		},
	}

	for _, tc := range testCases {
		log.Printf("Channel '%s' matches patterns: %v", tc.channel, tc.patterns)
	}

	// Cleanup
	log.Println("\nCleaning up subscriptions...")
	for pattern := range patterns {
		if err := ms.Unsubscribe(pattern); err != nil {
			log.Printf("Error unsubscribing from %s: %v", pattern, err)
		} else {
			log.Printf("Unsubscribed from %s", pattern)
		}
	}
}

func main() {
	// Create a new MemoryStore instance
	ms := memorystore.NewMemoryStore()

	// Ensure proper cleanup when main exits
	defer func() {
		if err := ms.Stop(); err != nil {
			log.Printf("Error stopping MemoryStore: %v", err)
		}
	}()

	// Run demonstrations
	demonstrateBasicOperations(ms)
	demonstrateExpiration(ms)
	demonstrateNonExistentKeys(ms)
	demonstratePubSub(ms) // Add this line
	demonstrateStoreLifecycle()

	log.Println("\nAll demonstrations completed successfully")
}
