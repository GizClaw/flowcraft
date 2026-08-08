package bwrap_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sandboxconfig "github.com/GizClaw/flowcraft/sdk/sandbox/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
	"github.com/GizClaw/flowcraft/sdkx/sandbox/bwrap"
)

func TestRegisterRejectsUnknownSettingsAndEscape(t *testing.T) {
	root := t.TempDir()
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, root),
	})
	if err := bwrap.Register(builder); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for name, entry := range map[string]string{
		"unknown setting": "{backend: bwrap, workspace: project, settings: {bogus: true}}",
		"path escape":     "{backend: bwrap, workspace: project, settings: {writable_paths: [../escape]}}",
	} {
		t.Run(name, func(t *testing.T) {
			doc := mustParse(t, "version: v1\nsandboxes:\n  x: "+entry+"\n")
			_, err := builder.Build(context.Background(), doc)
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want validation", err)
			}
		})
	}
}

func TestRegisterUnsupportedPlatformClassification(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("bwrap support depends on the host binary on Linux")
	}
	root := t.TempDir()
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, root),
	})
	if err := bwrap.Register(builder); err != nil {
		t.Fatalf("Register: %v", err)
	}
	doc := mustParse(t, `
version: v1
sandboxes:
  x: {backend: bwrap, workspace: project}
`)
	_, err := builder.Build(context.Background(), doc)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("Build error = %v, want NotAvailable", err)
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
