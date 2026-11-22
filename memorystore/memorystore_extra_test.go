package memorystore

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// mockPubSub implements PubSubClient (and optionally SubscriberCounter via a wrapper)
// to verify that any custom Pub/Sub backend plugs cleanly into MemoryStore.
type mockPubSub struct {
	mu          sync.Mutex
	subscribed  []string
	published   map[string][][]byte
	unsubbed    []string
	closed      atomic.Bool
	closeErr    error
	lastCtxDead atomic.Bool
}

func newMockPubSub() *mockPubSub {
	return &mockPubSub{
		published: make(map[string][][]byte),
	}
}

func (m *mockPubSub) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	if _, ok := ctx.Deadline(); ok {
		m.lastCtxDead.Store(true)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subscribed = append(m.subscribed, topic)
	ch := make(chan []byte, 1)
	return ch, nil
}

func (m *mockPubSub) Publish(ctx context.Context, topic string, message []byte) error {
	if _, ok := ctx.Deadline(); ok {
		m.lastCtxDead.Store(true)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published[topic] = append(m.published[topic], message)
	return nil
}

func (m *mockPubSub) Unsubscribe(ctx context.Context, topic string) error {
	if _, ok := ctx.Deadline(); ok {
		m.lastCtxDead.Store(true)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unsubbed = append(m.unsubbed, topic)
	return nil
}

func (m *mockPubSub) Close() error {
	m.closed.Store(true)
	return m.closeErr
}

func TestMemoryStore_SubscriberCount(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	topic := "test-topic"
	ch1, err := ms.Subscribe(topic)
	if err != nil {
		t.Fatalf("Subscribe 1 failed: %v", err)
	}
	defer func() {
		_ = ms.Unsubscribe(topic)
	}()

	if count := ms.SubscriberCount(topic); count != 1 {
		t.Errorf("SubscriberCount should be 1, got %d", count)
	}

	ch2, err := ms.Subscribe(topic)
	if err != nil {
		t.Fatalf("Subscribe 2 failed: %v", err)
	}

	if count := ms.SubscriberCount(topic); count != 2 {
		t.Errorf("SubscriberCount should be 2, got %d", count)
	}

	if err := ms.Unsubscribe(topic); err != nil {
		t.Fatalf("Unsubscribe failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if count := ms.SubscriberCount(topic); count != 0 {
		t.Errorf("SubscriberCount should be 0, got %d", count)
	}

	go func() {
		for range ch1 {
		}
		for range ch2 {
		}
	}()
}

func TestMemoryStore_CustomPubSubAndConfig(t *testing.T) {
	mock := newMockPubSub()
	ms := NewMemoryStoreWithConfig(Config{
		PubSub:          mock,
		PubSubTimeout:   2 * time.Second,
		CleanupInterval: 50 * time.Millisecond,
		Logger:          slog.New(slog.DiscardHandler),
	})

	if _, err := ms.Subscribe("orders"); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if err := ms.Publish("orders", []byte("created")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := ms.Unsubscribe("orders"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}

	// Custom mock does not implement SubscriberCounter -> returns 0.
	if got := ms.SubscriberCount("orders"); got != 0 {
		t.Errorf("SubscriberCount() = %d, want 0 for non-SubscriberCounter", got)
	}

	// Verify PubSubTimeout created a deadline on the context.
	if !mock.lastCtxDead.Load() {
		t.Error("expected PubSubTimeout to set a deadline on context")
	}

	// Verify background cleanup runs on configured CleanupInterval.
	if err := ms.Set("ephemeral", []byte("bye"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if metrics := ms.GetMetrics(); metrics.Evictions == 0 {
		t.Errorf("expected background cleanup to evict expired item, got evictions=%d", metrics.Evictions)
	}

	closeSentinel := errors.New("close failed")
	mock.closeErr = closeSentinel
	if err := ms.Stop(); !errors.Is(err, closeSentinel) {
		t.Errorf("Stop() error = %v, want %v", err, closeSentinel)
	}
	if !mock.closed.Load() {
		t.Error("expected Stop() to call Close() on custom PubSubClient")
	}
}

func TestMemoryStore_PubSub_Stopped(t *testing.T) {
	ms := NewMemoryStore()
	_ = ms.Stop()

	if _, err := ms.Subscribe("topic"); !errors.Is(err, ErrStoreStopped) {
		t.Errorf("Subscribe after Stop should return ErrStoreStopped, got %v", err)
	}
	if err := ms.Publish("topic", []byte("msg")); !errors.Is(err, ErrStoreStopped) {
		t.Errorf("Publish after Stop should return ErrStoreStopped, got %v", err)
	}
	if err := ms.Unsubscribe("topic"); !errors.Is(err, ErrStoreStopped) {
		t.Errorf("Unsubscribe after Stop should return ErrStoreStopped, got %v", err)
	}
}

func TestMemoryStore_PubSub_CanceledContext(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := ms.SubscribeContext(ctx, "topic"); !errors.Is(err, context.Canceled) {
		t.Errorf("SubscribeContext(canceled) = %v, want context.Canceled", err)
	}
	if err := ms.PublishContext(ctx, "topic", []byte("msg")); !errors.Is(err, context.Canceled) {
		t.Errorf("PublishContext(canceled) = %v, want context.Canceled", err)
	}
	if err := ms.UnsubscribeContext(ctx, "topic"); !errors.Is(err, context.Canceled) {
		t.Errorf("UnsubscribeContext(canceled) = %v, want context.Canceled", err)
	}
}

func TestMemoryStore_SetJSON_Error(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	badValue := make(chan int)
	err := ms.SetJSON("key", badValue, time.Minute)
	if err == nil {
		t.Error("SetJSON should return error for unserializable value")
	}
}

func TestShardCacheLineSize(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		if got := unsafe.Sizeof(shard{}); got != 64 {
			t.Errorf("sizeof(shard) = %d bytes, want 64 (single cache line)", got)
		}
	}
}
