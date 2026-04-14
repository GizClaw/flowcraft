package workspace

import (
	"context"
	"testing"
)

func TestWithWorkspace_RoundTrip(t *testing.T) {
	ws := NewMemWorkspace()
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

func TestMustWorkspaceFrom_OK(t *testing.T) {
	ws := NewMemWorkspace()
	ctx := WithWorkspace(context.Background(), ws)
	got := MustWorkspaceFrom(ctx)
	if got != ws {
		t.Fatal("expected same workspace instance")
	}
}

func TestMustWorkspaceFrom_Panic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustWorkspaceFrom")
		}
	}()
	MustWorkspaceFrom(context.Background())
}
