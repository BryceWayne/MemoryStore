// Package main provides a demonstration of the memorystore package functionality.
// It shows various use cases including storing/retrieving data, handling expiration,
// and proper error handling.
package main

import (
	"log"
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
	demonstrateStoreLifecycle()

	log.Println("\nAll demonstrations completed successfully")
}
