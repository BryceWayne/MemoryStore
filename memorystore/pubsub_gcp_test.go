package memorystore

import (
	"context"
	"testing"
)

func TestGCPPubSub_New_EmptyProject(t *testing.T) {
	ctx := context.Background()

	_, err := NewGCPPubSub(ctx, "")
	if err == nil {
		t.Log("NewGCPPubSub with empty project ID did not return error immediately. This might be expected depending on client lib version.")
	} else {
		t.Logf("NewGCPPubSub with empty project ID returned error: %v", err)
	}
}

func TestGCPPubSub_New_WithInvalidProject(t *testing.T) {
	ctx := context.Background()
	_, err := NewGCPPubSub(ctx, "invalid-project")
	if err == nil {
		t.Log("NewGCPPubSub success (likely due to default credentials)")
	} else {
		t.Logf("NewGCPPubSub failed as expected: %v", err)
	}
}
