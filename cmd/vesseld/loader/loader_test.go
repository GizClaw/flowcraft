package loader

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

const sampleDaemon = `apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-default
spec:
  control:
    socket: /tmp/v.sock
`

const sampleVesselAgent = `apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: support
spec:
  agents: [helper]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: helper
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: openai
`

func TestLoad_FlatDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "daemon.yaml", sampleDaemon)
	writeFile(t, dir, "vessel.yaml", sampleVesselAgent)
	writeFile(t, dir, "README.md", "ignored") // non-resource extension

	objs, err := Load([]string{dir}, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("loaded %d objects, want 3", len(objs))
	}
}

func TestLoad_RecursiveOptIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "daemon.yaml", sampleDaemon)
	sub := filepath.Join(dir, "vessels", "support")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "vessel.yaml", sampleVesselAgent)

	// Without -R the sub directory is skipped.
	objs, err := Load([]string{dir}, Options{Recursive: false})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("non-recursive loaded %d, want 1", len(objs))
	}

	// With -R both files are loaded.
	objs, err = Load([]string{dir}, Options{Recursive: true})
	if err != nil {
		t.Fatalf("Load -R: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("recursive loaded %d, want 3", len(objs))
	}
}

func TestLoad_RejectsBadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "broken.yaml", "this is not yaml: ::")
	_, err := Load([]string{dir}, Options{})
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_MultipleInputs(t *testing.T) {
	t.Parallel()
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeFile(t, dir1, "daemon.yaml", sampleDaemon)
	writeFile(t, dir2, "vessel.yaml", sampleVesselAgent)

	objs, err := Load([]string{dir1, dir2}, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("loaded %d, want 3", len(objs))
	}
}
