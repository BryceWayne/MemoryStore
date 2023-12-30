package main

import (
	"log"
	"time"

	"github.com/goccy/go-json"

	"github.com/BryceWayne/MemoryStore/memorystore"
	"github.com/google/uuid"
)

// Person struct to be used as an example data structure.
type Person struct {
	Name string
	Age  int
	UID  string
}

func main() {
	ms := memorystore.NewMemoryStore()
	defer ms.Stop() // Ensure MemoryStore is properly stopped when main exits.

	expiration := 1 * time.Second

	// Serialize data before storing
	person := Person{
		Name: "Alice",
		Age:  30,
		UID:  uuid.New().String(),
	}
	personKey := person.UID
	personBytes, _ := json.Marshal(person) // Handle error appropriately
	err := ms.Set(personKey, personBytes, expiration)
	if err != nil {
		log.Fatalf("ERROR: MemoryStore - Error: %v", err)
	}

	// Retrieve and deserialize data
	if data, exists, _ := ms.Get(personKey); exists {
		var retrievedPerson Person
		if err := json.Unmarshal(data, &retrievedPerson); err != nil {
			log.Printf("Error: %v", err)
		}

		log.Printf("INFO: MemoryStore - Retrieved person: %+v\n", retrievedPerson)
	}

	// Wait for the stored item to expire.
	log.Println("INFO: Waiting for value to expire...")
	time.Sleep(expiration + 1*time.Second)

	// Attempt to retrieve the value after it should have expired.
	if _, exists, _ := ms.Get(personKey); !exists {
		log.Printf("INFO: MemoryStore - Value expired for key: %s\n", personKey)
	}

	// Check retrieval for a key that was never set.
	nonExistentKey := "nonExistentKey"
	if _, exists, _ := ms.Get(nonExistentKey); !exists {
		log.Printf("ERROR: MemoryStore - Value does not exist for key: %s\n", nonExistentKey)
	}
}
