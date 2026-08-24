//go:build windows

package windows

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
)

// TestNewOnWindows runs only on Windows hosts: New must succeed and
// Capabilities must claim exactly the surface P1 implements.
func TestNewOnWindows(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps := runner.Capabilities()
	if !caps.Policy.EnvAllowList {
		t.Error("EnvAllowList should be available (env filtering is implemented)")
	}
	if !caps.Policy.FilesystemBounds {
		t.Error("FilesystemBounds should be claimed after workspace ACLs are applied")
	}
	if !caps.Policy.MemoryCap || !caps.Policy.CPUCap {
		t.Error("resource caps should be claimed with the job-object watcher implemented")
	}
	if !caps.Features.TTY || !caps.Features.Signal || !caps.Features.Events {
		t.Error("session features should be claimed with the ConPTY session implemented")
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestFactoryRoundTripOnWindows(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup(ResourceKind, BackendName)
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": ` + mustJSON(t.TempDir()) + `, "level": "restricted-token"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(*Runner); !ok {
		t.Fatalf("New returned %T, want *Runner", value)
	}
}

func TestFactoryAcceptsElevatedLevel(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup(ResourceKind, BackendName)
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": ` + mustJSON(t.TempDir()) + `, "level": "elevated"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runner, ok := value.(*Runner)
	if !ok {
		t.Fatalf("New returned %T, want *Runner", value)
	}
	if runner.level != LevelElevated {
		t.Fatalf("level = %q, want %q", runner.level, LevelElevated)
	}
	_ = runner.Close()
}

// mustJSON renders a string as a JSON string literal, so Windows
// paths with backslashes stay valid JSON in Settings.
func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
