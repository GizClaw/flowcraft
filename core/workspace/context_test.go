package workspace

import (
	"context"
	"testing"
)

func TestWithWorkspace_RoundTrip(t *testing.T) {
	ws := newTestWorkspace(t)
	ctx := WithWorkspace(context.Background(), ws)

	got, ok := WorkspaceFrom(ctx)
	if !ok {
		t.Fatal("expected workspace in context")
	}
	if got != ws {
		t.Fatal("expected same workspace instance")
	}
}

func TestWorkspaceFrom_Missing(t *testing.T) {
	_, ok := WorkspaceFrom(context.Background())
	if ok {
		t.Fatal("expected no workspace in empty context")
	}
}
