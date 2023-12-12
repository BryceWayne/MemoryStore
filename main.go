package main

import (
	"log"
	"time"

	"github.com/BryceWayne/MemoryStore/memorystore"
)

func main() {
	ms := memorystore.NewMemoryStore()
	defer ms.Stop()

	key1 := "key1"
	expiration := 5 * time.Second // Set expiration time

	// Set value with expiration
	ms.Set(key1, "value1", expiration)
	log.Println("INFO: Value set with expiration of 5 seconds")

	// Attempt to retrieve the value immediately
	if item, exists := ms.Get(key1); exists {
		if value, ok := item.(string); ok {
			log.Printf("INFO: MemoryStore - Retrieved value: %s for key: %s\n", value, key1)
		} else {
			log.Printf("ERROR: MemoryStore - Incorrect type for value of key: %s\n", key1)
		}
	}

	// Wait for the value to expire
	log.Println("INFO: Waiting for value to expire...")
	time.Sleep(expiration + 1*time.Second) // Wait for longer than the expiration time

	// Attempt to retrieve the value after expiration
	if _, exists := ms.Get(key1); !exists {
		log.Printf("INFO: MemoryStore - Value expired for key: %s\n", key1)
	}

	// Cleanup example
	ms.Delete(key1)
	if _, exists := ms.Get(key1); !exists {
		log.Printf("INFO: MemoryStore - Value manually deleted for key: %s\n", key1)
	}

	// Non-existent key example
	if _, exists := ms.Get("key2"); !exists {
		log.Printf("ERROR: MemoryStore - Value does not exist for key: %s\n", "key2")
	}
}
