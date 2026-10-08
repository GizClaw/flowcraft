package manager

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/craft"
)

func writeInstanceDefinition(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "craft.yaml")
	if err := os.WriteFile(path, []byte(definition), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestGroupLifecycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	specs := []Spec{
		{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
		{ID: "beta", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "beta"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	}
	group, err := NewGroup(GroupOptions{Root: root}, specs...)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	ctx := context.Background()
	if err := group.StartAll(ctx); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	infos := group.Instances()
	if len(infos) != 2 || infos[0].ID != "alpha" || infos[1].ID != "beta" {
		t.Fatalf("instances = %+v", infos)
	}
	for _, info := range infos {
		if info.State != StateRunning {
			t.Fatalf("instance %s state = %s", info.ID, info.State)
		}
	}
	if err := group.StopAll(ctx); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if err := group.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type countingRunner struct {
	mu    sync.Mutex
	count int
}

func (r *countingRunner) Run(context.Context, *craft.Craft) error {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	// Return immediately: Run then stops every instance.
	return nil
}

func TestGroupRunAndMaxInstances(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	spec := func(id string) Spec {
		return Spec{ID: InstanceID(id), Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, id),
			Capabilities:   []craft.Capability{testCapability{}},
		}}
	}
	group, err := NewGroup(GroupOptions{Root: root, MaxInstances: 1}, spec("alpha"))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	runner := &countingRunner{}
	if err := group.Run(context.Background(), runner); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.count != 1 {
		t.Fatalf("runner count = %d, want 1", runner.count)
	}
	if _, err := NewGroup(GroupOptions{Root: root, MaxInstances: 1},
		spec("alpha"), spec("beta")); err == nil {
		t.Fatal("MaxInstances was not enforced")
	}
}
