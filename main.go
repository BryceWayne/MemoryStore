// main.go
// Package main provides a demonstration of the memorystore package functionality.
package main

import (
	"log"
	"os"
	"sync"
	"time"

	"github.com/BryceWayne/MemoryStore/memorystore"
	"github.com/google/uuid"
)

// Person represents a sample data structure.
type Person struct {
	Name    string    `json:"name"`
	Age     int       `json:"age"`
	UID     string    `json:"uid"`
	Created time.Time `json:"created,omitempty"`
}

// demonstrateBasicOperations shows basic Set/Get operations.
func demonstrateBasicOperations(ms *memorystore.MemoryStore) {
	log.Println("=== Demonstrating Basic Operations ===")

	person := Person{
		Name:    "Alice Smith",
		Age:     30,
		UID:     uuid.New().String(),
		Created: time.Now(),
	}

	err := ms.SetJSON(person.UID, person, 2*time.Second)
	if err != nil {
		log.Printf("Failed to store person: %v", err)
		return
	}
	log.Printf("Stored person with UID: %s", person.UID)

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

// demonstratePubSub shows the publish/subscribe functionality.
// Note: We use exact topic names to ensure compatibility with GCP PubSub.
func demonstratePubSub(ms *memorystore.MemoryStore) {
	log.Println("\n=== Demonstrating PubSub System ===")

	var wg sync.WaitGroup
	topics := []string{"updates", "alerts"}
	chans := make(map[string]<-chan []byte)

	// Subscribe
	for _, topic := range topics {
		ch, err := ms.Subscribe(topic)
		if err != nil {
			log.Printf("Failed to subscribe to %s: %v", topic, err)
			continue
		}
		chans[topic] = ch
		log.Printf("Subscribed to topic: %s", topic)
	}

	// Listeners
	for topic, ch := range chans {
		wg.Add(1)
		go func(t string, c <-chan []byte) {
			defer wg.Done()
			for {
				select {
				case msg, ok := <-c:
					if !ok {
						return
					}
					log.Printf("[%s] Received: %s", t, string(msg))
				case <-time.After(3 * time.Second):
					return
				}
			}
		}(topic, ch)
	}

	// Publish
	time.Sleep(500 * time.Millisecond) // Wait for subscriptions
	ms.Publish("updates", []byte("System update available"))
	ms.Publish("alerts", []byte("High CPU usage"))

	wg.Wait()

	// Unsubscribe
	for _, topic := range topics {
		ms.Unsubscribe(topic)
	}
}

func main() {
	// Example of using GCP PubSub if env var is set
	if os.Getenv("GOOGLE_CLOUD_PROJECT") == "" {
		log.Println("Note: Set GOOGLE_CLOUD_PROJECT env var to test GCP PubSub backend")
	}

	ms := memorystore.NewMemoryStore()
	defer ms.Stop()

	demonstrateBasicOperations(ms)
	demonstratePubSub(ms)
}
