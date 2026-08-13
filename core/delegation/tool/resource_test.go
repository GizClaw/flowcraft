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
	_, err := (tooldelegation.NewSourceFactory(nil)).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without directory = %v, want Validation", err)
	}
}

func TestSourceFactoryRejectsSettings(t *testing.T) {
	_, err := (tooldelegation.NewSourceFactory(&fakeDirectory{})).New(
		context.Background(), resource.Input{
			Settings: []byte(`{"unknown":true}`),
		})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with settings = %v, want Validation", err)
	}
}

func TestSourceFactoryBuildsTools(t *testing.T) {
	value, err := (tooldelegation.NewSourceFactory(&fakeDirectory{})).New(
		context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src, ok := value.(tool.Source)
	if !ok {
		t.Fatalf("New returned %T, want tool.Source", value)
	}
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
	value, err := (tooldelegation.NewSourceFactory(&fakeDirectory{})).New(
		context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reg, err := tool.NewRegistry([]tool.Source{value.(tool.Source)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Len() != 3 {
		t.Fatalf("registry Len = %d, want 3", reg.Len())
	}
}
