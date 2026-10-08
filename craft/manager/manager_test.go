package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/craft"
)

type testCapability struct{}

func (testCapability) Name() string { return "test" }

func (testCapability) Register(registry *resource.Registry) error {
	return event.Register(registry)
}

const definition = `craft: {id: test, version: 0.1.0}
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
  runtime:
    event_bus: bus
`

func writeDefinition(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "craft.yaml")
	if err := os.WriteFile(path, []byte(definition), 0o600); err != nil {
		t.Fatalf("write craft.yaml: %v", err)
	}
	return path
}

func TestLocateDefinition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	located, err := LocateDefinition("", func(string) (string, bool) {
		return "", false
	}, dir, "")
	if err != nil || located != path {
		t.Fatalf("LocateDefinition = %q, %v; want %q", located, err, path)
	}
	if _, err := LocateDefinition("", nil, t.TempDir(), ""); err == nil {
		t.Fatal("missing definition did not error")
	}
}

func TestManagerLifecycleAndLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	opts := Options{
		DefinitionPath: path,
		Paths:          Paths{DataDir: dir},
		Lock:           true,
		Capabilities:   []craft.Capability{testCapability{}},
	}
	m, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.State() != StateRunning || m.Craft() == nil {
		t.Fatalf("state=%s craft=%v", m.State(), m.Craft())
	}
	second, err := New(opts)
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	if err := second.Start(context.Background()); err == nil {
		t.Fatal("second manager acquired the same lock")
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.State() != StateStopped {
		t.Fatalf("state = %s, want stopped", m.State())
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("restart after release: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type nilRunner struct{ called bool }

func (r *nilRunner) Run(context.Context, *craft.Craft) error {
	r.called = true
	return nil
}

func TestManagerRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	m, err := New(Options{
		DefinitionPath: path,
		Paths:          Paths{DataDir: dir},
		Capabilities:   []craft.Capability{testCapability{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runner := &nilRunner{}
	if err := m.Run(context.Background(), runner); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !runner.called || m.State() != StateStopped {
		t.Fatalf("runner called=%v state=%s", runner.called, m.State())
	}
}
