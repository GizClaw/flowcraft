package seatbelt_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sandboxconfig "github.com/GizClaw/flowcraft/sdk/sandbox/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
	"github.com/GizClaw/flowcraft/sdkx/sandbox/seatbelt"
)

func TestRegisterRejectsUnknownSettings(t *testing.T) {
	root := t.TempDir()
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, root),
	})
	if err := seatbelt.Register(builder); err != nil {
		t.Fatalf("Register: %v", err)
	}
	doc := mustParse(t, `
version: v1
sandboxes:
  x: {backend: seatbelt, workspace: project, settings: {bogus: true}}
`)
	_, err := builder.Build(context.Background(), doc)
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("Build error = %v, want validation mentioning bogus", err)
	}
}

func TestRegisterRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, root),
	})
	if err := seatbelt.Register(builder); err != nil {
		t.Fatalf("Register: %v", err)
	}
	doc := mustParse(t, `
version: v1
sandboxes:
  x:
    backend: seatbelt
    workspace: project
    settings:
      writable_paths: [escape]
`)
	_, err := builder.Build(context.Background(), doc)
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Build error = %v, want symlink escape validation", err)
	}
}

func buildWorkspaces(t *testing.T, root string) *workspaceconfig.Registry {
	t.Helper()
	doc, err := workspaceconfig.Parse([]byte(
		"version: v1\nworkspaces:\n  project:\n    driver: local\n    settings:\n      root: " + root + "\n"))
	if err != nil {
		t.Fatalf("workspace Parse: %v", err)
	}
	registry, err := workspaceconfig.NewBuilder(workspaceconfig.Deps{}).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("workspace Build: %v", err)
	}
	return registry
}

func mustParse(t *testing.T, input string) sandboxconfig.Document {
	t.Helper()
	doc, err := sandboxconfig.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}
