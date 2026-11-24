// Package main demonstrates basic MemoryStore operations, encoding/json/v2
// serialization, batch operations, and in-memory Pub/Sub.
package main

import (
	"log"
	"log/slog"
	"os"
	"sync"
	"time"
	"uuid"

	"github.com/BryceWayne/MemoryStore/v2/memorystore"
)

// Person represents a sample JSON-serializable struct.
type Person struct {
	Name    string    `json:"name"`
	Age     int       `json:"age"`
	UID     string    `json:"uid"`
	Created time.Time `json:"created,omitzero"`
}

func demonstrateBasicOperations(ms *memorystore.MemoryStore) {
	log.Println("=== Demonstrating Basic Operations ===")

	person := Person{
		Name:    "Alice Smith",
		Age:     30,
		UID:     uuid.New().String(),
		Created: time.Now(),
	}

	if err := ms.SetJSON(person.UID, &person, 2*time.Second); err != nil {
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

func demonstratePubSub(ms *memorystore.MemoryStore) {
	log.Println("\n=== Demonstrating PubSub System ===")

	var wg sync.WaitGroup
	topics := []string{"updates", "alerts"}
	chans := make(map[string]<-chan []byte, len(topics))

	for _, topic := range topics {
		ch, err := ms.Subscribe(topic)
		if err != nil {
			log.Printf("Failed to subscribe to %s: %v", topic, err)
			continue
		}
		chans[topic] = ch
		log.Printf("Subscribed to topic: %s", topic)
	}

	for topic, ch := range chans {
		wg.Go(func() {
			for {
				select {
				case msg, ok := <-ch:
					if !ok {
						return
					}
					log.Printf("[%s] Received: %s", topic, string(msg))
				case <-time.After(500 * time.Millisecond):
					return
				}
			}
		})
	}

	if err := ms.Publish("updates", []byte("System update available")); err != nil {
		log.Printf("Error publishing to updates: %v", err)
	}
	if err := ms.Publish("alerts", []byte("High CPU usage")); err != nil {
		log.Printf("Error publishing to alerts: %v", err)
	}

	wg.Wait()

	for _, topic := range topics {
		if err := ms.Unsubscribe(topic); err != nil {
			log.Printf("Error unsubscribing from %s: %v", topic, err)
		}
	}
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ms, err := memorystore.New(
		memorystore.WithLogger(logger),
		memorystore.WithPubSubTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatalf("Failed to create memorystore: %v", err)
	}
	defer func() {
		if err := ms.Stop(); err != nil {
			log.Printf("Error stopping store: %v", err)
		}
	}()

	demonstrateBasicOperations(ms)
	demonstratePubSub(ms)
}
