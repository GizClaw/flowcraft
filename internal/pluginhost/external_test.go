package pluginhost

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/plugin"
)

func TestExternalManager_Discover_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	m := NewExternalManager(ExternalManagerConfig{PluginDir: dir})

	found, err := m.Discover()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("expected 0, got %d", len(found))
	}
}

func TestExternalManager_Discover_WithExecutables(t *testing.T) {
	dir := t.TempDir()

	// Create an executable file
	execPath := filepath.Join(dir, "test-plugin")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\necho hi"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a non-executable file (should be skipped)
	nonExecPath := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(nonExecPath, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewExternalManager(ExternalManagerConfig{PluginDir: dir})
	found, err := m.Discover()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1 executable plugin, got %d", len(found))
	}
	if found[0].info.ID != "test-plugin" {
		t.Fatalf("expected 'test-plugin', got %q", found[0].info.ID)
	}
}

func TestExternalManager_Discover_TypeNotHardcoded(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "my-plugin")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\necho hi"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewExternalManager(ExternalManagerConfig{PluginDir: dir})
	found, err := m.Discover()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1, got %d", len(found))
	}
	if found[0].info.Type != "" {
		t.Fatalf("expected empty Type (filled by Handshake), got %q", found[0].info.Type)
	}
}

func TestNewExternalPlugin(t *testing.T) {
	info := plugin.PluginInfo{ID: "test", Name: "Test", Version: "1.0.0"}
	ep := NewExternalPlugin("/path/to/plugin", info)
	if ep.Info().ID != "test" {
		t.Fatalf("expected ID 'test', got %q", ep.Info().ID)
	}
	if ep.path != "/path/to/plugin" {
		t.Fatalf("expected path '/path/to/plugin', got %q", ep.path)
	}
}

func TestExternalManager_Discover_NonexistentDir(t *testing.T) {
	m := NewExternalManager(ExternalManagerConfig{PluginDir: "/nonexistent/dir"})
	found, err := m.Discover()
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir, got %v", err)
	}
	if found != nil {
		t.Fatal("expected nil for nonexistent dir")
	}
}

func TestExternalManager_RegisterGet(t *testing.T) {
	m := NewExternalManager(ExternalManagerConfig{})
	ep := &ExternalPlugin{info: plugin.PluginInfo{ID: "ep1"}, path: "/test"}

	m.Register(ep)
	got, ok := m.Get("ep1")
	if !ok || got.info.ID != "ep1" {
		t.Fatal("expected ep1")
	}

	_, ok = m.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}
