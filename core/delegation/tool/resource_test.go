package delegation_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/delegation"
	tooldelegation "github.com/GizClaw/flowcraft/core/delegation/tool"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestSourceFactoryRequiresDirectory(t *testing.T) {
	_, err := (tooldelegation.NewSourceFactory()).New(context.Background(), resource.NewTestInput(nil))
	// The typed handle reports a missing required dependency as NotFound; the
	// deployment plan normally rejects it before the factory runs.
	if !errdefs.IsNotFound(err) {
		t.Fatalf("New without directory = %v, want NotFound", err)
	}
}

func TestSourceFactoryRejectsSettings(t *testing.T) {
	_, err := (tooldelegation.NewSourceFactory()).New(
		context.Background(), resource.NewInput([]byte(`{"unknown":true}`),
			map[string]any{delegation.DirectoryDep: &fakeDirectory{}}, nil, nil))
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with settings = %v, want Validation", err)
	}
}

func TestSourceFactoryBuildsTools(t *testing.T) {
	value, err := (tooldelegation.NewSourceFactory()).New(
		context.Background(), resource.NewTestInput(map[string]any{delegation.DirectoryDep: &fakeDirectory{}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src := value
	tools := src.Tools()
	if len(tools) != 3 {
		t.Fatalf("Tools() = %d, want delegate + delegation_status + delegation_targets", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Definition().Name] = true
	}
	if !names[delegation.ToolName] ||
		!names[tooldelegation.StatusToolName] ||
		!names[tooldelegation.TargetsToolName] {
		t.Fatalf("tool names = %v, want %q, %q and %q",
			names, delegation.ToolName,
			tooldelegation.StatusToolName, tooldelegation.TargetsToolName)
	}
}

func TestSourceWorksInToolRegistry(t *testing.T) {
	value, err := (tooldelegation.NewSourceFactory()).New(
		context.Background(), resource.NewTestInput(map[string]any{delegation.DirectoryDep: &fakeDirectory{}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reg, err := tool.NewRegistry([]tool.Source{value})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Len() != 3 {
		t.Fatalf("registry Len = %d, want 3", reg.Len())
	}
}
