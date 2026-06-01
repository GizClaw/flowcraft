package main

import (
	"context"
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

//go:embed testdata/examples/*.yaml
var testExamples embed.FS

func TestLoadYAMLExpandsEnv(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Setenv("CLAW_TEST_MODEL", "mock-fast")
	ctx := context.Background()
	if err := ws.Write(ctx, "config/models.yaml", []byte(`
apiVersion: claw.flowcraft.io/v1alpha1
kind: ModelsConfig
metadata:
  name: test
spec:
  chat: fast
  llm:
    fast:
      provider: mock
      model: ${CLAW_TEST_MODEL}
`)); err != nil {
		t.Fatalf("Write models: %v", err)
	}

	cfg, err := Load(ctx, ws, "config")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models.Chat != "fast" {
		t.Fatalf("Models.Chat = %q, want fast", cfg.Models.Chat)
	}
	if cfg.Models.LLM["fast"].Model != "mock-fast" {
		t.Fatalf("model = %q, want mock-fast", cfg.Models.LLM["fast"].Model)
	}
}

func TestWriteExampleCompilesYAMLTemplatesToJSON(t *testing.T) {
	dir := t.TempDir()
	examples, err := fs.Sub(testExamples, "testdata")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	if err := WriteExample(examples, "chat", dir); err != nil {
		t.Fatalf("WriteExample: %v", err)
	}
	for _, name := range []string{"workspace", "models", "memory", "agent"} {
		path := filepath.Join(dir, "config", name+".json")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "models.yaml")); !os.IsNotExist(err) {
		t.Fatalf("models.yaml exists after template compile, err=%v", err)
	}

	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	t.Setenv("CLAW_TEST_MODEL", "mock-fast")
	cfg, err := Load(context.Background(), ws, "config")
	if err != nil {
		t.Fatalf("Load compiled config: %v", err)
	}
	if cfg.Models.LLM["fast"].Model != "mock-fast" {
		t.Fatalf("compiled model = %q, want mock-fast", cfg.Models.LLM["fast"].Model)
	}
}

func TestWriteEmbeddedExamples(t *testing.T) {
	examples, err := listExamples()
	if err != nil {
		t.Fatalf("listExamples: %v", err)
	}
	if len(examples) == 0 {
		t.Fatal("no embedded examples")
	}
	for _, example := range examples {
		t.Run(example, func(t *testing.T) {
			dir := t.TempDir()
			if err := WriteExample(exampleFS, example, dir); err != nil {
				t.Fatalf("WriteExample: %v", err)
			}
			for _, name := range []string{"workspace", "models", "memory", "agent"} {
				path := filepath.Join(dir, "config", name+".json")
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("missing %s: %v", path, err)
				}
			}
		})
	}
}
