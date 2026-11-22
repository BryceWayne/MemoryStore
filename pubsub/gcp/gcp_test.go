package gcp

import (
	"context"
	"errors"
	"testing"
)

func TestNewClient_EmptyProject(t *testing.T) {
	ctx := t.Context()
	_, err := NewClient(ctx, "")
	if err == nil {
		t.Error("expected error for empty project ID")
	}
}

func TestClient_ValidationErrors(t *testing.T) {
	c := &Client{
		projectID:     "test-project",
		subscriptions: make(map[string]*gcpSubscription),
	}

	if _, err := c.Subscribe(t.Context(), ""); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Subscribe(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Publish(t.Context(), "", []byte("msg")); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Publish(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Unsubscribe(t.Context(), "nonexistent"); err != nil {
		t.Errorf("Unsubscribe(nonexistent) = %v, want nil", err)
	}

	// Mark closed and verify operations return ErrClosed.
	c.closed = true
	if _, err := c.Subscribe(t.Context(), "topic"); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Publish(t.Context(), "topic", []byte("msg")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish(closed) = %v, want ErrClosed", err)
	}
	if err := c.Unsubscribe(t.Context(), "topic"); !errors.Is(err, ErrClosed) {
		t.Errorf("Unsubscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close(already closed) = %v, want nil", err)
	}

	// Verify helper path formatting.
	if got, want := c.topicPath("updates"), "projects/test-project/topics/updates"; got != want {
		t.Errorf("topicPath() = %q, want %q", got, want)
	}
	if got, want := c.subscriptionPath("sub-1"), "projects/test-project/subscriptions/sub-1"; got != want {
		t.Errorf("subscriptionPath() = %q, want %q", got, want)
	}
}

// Verify Client satisfies the standard PubSubClient contract structurally.
type pubSubContract interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}

var _ pubSubContract = (*Client)(nil)
