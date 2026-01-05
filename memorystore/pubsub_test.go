// memorystore/pubsub_test.go
package memorystore

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestMemoryStore_Subscribe tests basic subscription functionality
func TestMemoryStore_Subscribe(t *testing.T) {
	tests := []struct {
		name        string
		pattern     string
		wantErr     bool
		publishKey  string
		publishMsg  []byte
		shouldMatch bool
	}{
		{
			name:        "exact match subscription",
			pattern:     "user:123",
			wantErr:     false,
			publishKey:  "user:123",
			publishMsg:  []byte("test message"),
			shouldMatch: true,
		},
		{
			name:        "wildcard subscription",
			pattern:     "user:*",
			wantErr:     false,
			publishKey:  "user:123",
			publishMsg:  []byte("test message"),
			shouldMatch: true,
		},
		{
			name:        "non-matching subscription",
			pattern:     "user:123",
			wantErr:     false,
			publishKey:  "user:456",
			publishMsg:  []byte("test message"),
			shouldMatch: false,
		},
		{
			name:        "empty pattern",
			pattern:     "",
			wantErr:     true,
			publishKey:  "",
			publishMsg:  nil,
			shouldMatch: false,
		},
		{
			name:        "multiple wildcards",
			pattern:     "user:*:status",
			wantErr:     false,
			publishKey:  "user:123:status",
			publishMsg:  []byte("active"),
			shouldMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := NewMemoryStore()
			defer func() {
				_ = ms.Stop()
			}()

			// Create subscription
			ch, err := ms.Subscribe(tt.pattern)
			if (err != nil) != tt.wantErr {
				t.Errorf("Subscribe() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			// Test publishing
			var receivedMsg []byte
			var wg sync.WaitGroup
			wg.Add(1)

			go func() {
				defer wg.Done()
				select {
				case msg := <-ch:
					receivedMsg = msg
				case <-time.After(100 * time.Millisecond):
					// Timeout if no message received
				}
			}()

			err = ms.Publish(tt.publishKey, tt.publishMsg)
			if err != nil {
				t.Errorf("Publish() error = %v", err)
			}

			wg.Wait()

			if tt.shouldMatch {
				if receivedMsg == nil {
					t.Error("Expected to receive message but got none")
				} else if string(receivedMsg) != string(tt.publishMsg) {
					t.Errorf("Got message %s, want %s", string(receivedMsg), string(tt.publishMsg))
				}
			} else {
				if receivedMsg != nil {
					t.Errorf("Got unexpected message %s", string(receivedMsg))
				}
			}
		})
	}
}

// TestMemoryStore_Unsubscribe tests unsubscription functionality
func TestMemoryStore_Unsubscribe(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	pattern := "test:*"
	ch, err := ms.Subscribe(pattern)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	// Unsubscribe
	err = ms.Unsubscribe(pattern)
	if err != nil {
		t.Errorf("Unsubscribe() error = %v", err)
	}

	// Verify channel is closed
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Channel should be closed after unsubscribe")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Channel should be closed immediately")
	}

	// Verify subscriber count is 0
	if count := ms.SubscriberCount(pattern); count != 0 {
		t.Errorf("SubscriberCount() = %v, want 0", count)
	}
}

// TestMemoryStore_MultipleSubscribers tests multiple subscribers to the same pattern
func TestMemoryStore_MultipleSubscribers(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	pattern := "test:*"
	subscribers := 5
	message := []byte("test message")
	timeout := time.After(2 * time.Second) // Add timeout

	var channels []<-chan []byte
	var wg sync.WaitGroup

	// Create subscribers
	for i := 0; i < subscribers; i++ {
		ch, err := ms.Subscribe(pattern)
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		channels = append(channels, ch)
	}

	// Verify subscriber count
	if count := ms.SubscriberCount(pattern); count != subscribers {
		t.Errorf("SubscriberCount() = %v, want %v", count, subscribers)
	}

	// Test message delivery to all subscribers
	wg.Add(subscribers)
	receivedCount := 0
	var mu sync.Mutex

	for i := 0; i < subscribers; i++ {
		go func(ch <-chan []byte) {
			defer wg.Done()
			select {
			case msg := <-ch:
				if string(msg) == string(message) {
					mu.Lock()
					receivedCount++
					mu.Unlock()
				}
			case <-timeout:
				// Timeout - don't block forever
				t.Error("Timeout waiting for message")
			}
		}(channels[i])
	}

	// Publish message
	err := ms.Publish("test:123", message)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("Test timed out")
	}

	if receivedCount != subscribers {
		t.Errorf("Message received by %v subscribers, want %v", receivedCount, subscribers)
	}
}

func TestMemoryStore_PubSub_Concurrent(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	const publishers = 5
	const subscribers = 5
	const messagesPerPublisher = 20

	var wg sync.WaitGroup
	received := make(map[string]int)
	var mu sync.Mutex

	// Channel to signal when all messages have been published
	allPublished := make(chan struct{})

	// Create subscribers
	var subWg sync.WaitGroup
	for i := 0; i < subscribers; i++ {
		ch, err := ms.Subscribe("test:*")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		subWg.Add(1)
		go func() {
			defer subWg.Done()
			for {
				select {
				case msg, ok := <-ch:
					if !ok {
						return
					}
					mu.Lock()
					received[string(msg)]++
					mu.Unlock()
				case <-allPublished:
					// Give a short time to process any remaining messages
					time.Sleep(100 * time.Millisecond)
					return
				}
			}
		}()
	}

	// Create publishers
	wg.Add(publishers)
	for i := 0; i < publishers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < messagesPerPublisher; j++ {
				msg := []byte(fmt.Sprintf("msg-%d-%d", id, j))
				if err := ms.Publish("test:123", msg); err != nil {
					t.Errorf("Publish() error = %v", err)
				}
				time.Sleep(time.Millisecond) // Small delay to prevent message flood
			}
		}(i)
	}

	// Wait for all publishers to finish
	wg.Wait()
	close(allPublished)

	// Wait for subscribers to finish processing
	done := make(chan struct{})
	go func() {
		subWg.Wait()
		close(done)
	}()

	// Wait with timeout
	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for subscribers to finish")
	}

	// Verify results
	mu.Lock()
	totalMessages := len(received)
	messageCount := 0
	for _, count := range received {
		messageCount += count
	}
	mu.Unlock()

	expectedTotal := publishers * messagesPerPublisher * subscribers
	if messageCount != expectedTotal {
		t.Errorf("Expected %d total message receipts, got %d", expectedTotal, messageCount)
	}
	if totalMessages == 0 {
		t.Error("No messages were received")
	}
}

// BenchmarkMemoryStore_PubSub benchmarks publish/subscribe operations
func BenchmarkMemoryStore_PubSub(b *testing.B) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	ch, err := ms.Subscribe("bench:*")
	if err != nil {
		b.Fatalf("Subscribe() error = %v", err)
	}

	// Start consumer
	go func() {
		for range ch {
			// Consume messages
		}
	}()

	message := []byte("benchmark message")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := ms.Publish("bench:test", message); err != nil {
			b.Fatalf("Publish() error = %v", err)
		}
	}
}
