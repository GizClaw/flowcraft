//go:build linux

package bwrap

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func containsSeq(flags []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(flags); i++ {
		if slices.Equal(flags[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestFilesystemFlagsRootWritableByDefault(t *testing.T) {
	flags := filesystemFlags("/srv/root", []string{"/srv/cache"}, false)
	if !containsSeq(flags, "--bind", "/srv/root", "/srv/root") {
		t.Fatalf("default flags must bind the root read-write: %v", flags)
	}
	if containsSeq(flags, "--ro-bind", "/srv/root", "/srv/root") {
		t.Fatalf("default flags must not ro-bind the root: %v", flags)
	}
	if !containsSeq(flags, "--bind", "/srv/cache", "/srv/cache") {
		t.Fatalf("explicit writable paths must stay writable: %v", flags)
	}
}

func TestFilesystemFlagsReadOnlyRoot(t *testing.T) {
	flags := filesystemFlags("/srv/root", []string{"/srv/cache"}, true)
	if !containsSeq(flags, "--ro-bind", "/srv/root", "/srv/root") {
		t.Fatalf("read-only flags must ro-bind the root: %v", flags)
	}
	if containsSeq(flags, "--bind", "/srv/root", "/srv/root") {
		t.Fatalf("read-only flags must not bind the root read-write: %v", flags)
	}
	if !containsSeq(flags, "--bind", "/srv/cache", "/srv/cache") {
		t.Fatalf("explicit writable paths must stay writable: %v", flags)
	}
}

func TestFilesystemFlagsReadOnlyRootUnderTmp(t *testing.T) {
	flags := filesystemFlags("/tmp/ws", nil, true)
	tmpfs := slices.Index(flags, "--tmpfs")
	roBind := slices.Index(flags, "--ro-bind")
	if tmpfs < 0 || roBind < 0 || roBind < tmpfs {
		t.Fatalf("read-only root bind must follow the /tmp tmpfs: %v", flags)
	}
	if !containsSeq(flags, "--ro-bind", "/tmp/ws", "/tmp/ws") {
		t.Fatalf("root under /tmp must be re-exposed read-only: %v", flags)
	}
}

// TestNewReadOnlyRootOption checks the option wiring into the Runner.
// The binary is a throwaway executable so New does not need bwrap
// installed to reach the option application.
func TestNewReadOnlyRootOption(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bwrap-fake")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	runner, err := New(t.TempDir(), WithBinary(binary))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if runner.readOnlyRoot {
		t.Fatal("default runner must keep the root writable")
	}

	ro, err := New(t.TempDir(), WithBinary(binary), WithReadOnlyRoot())
	if err != nil {
		t.Fatalf("New(WithReadOnlyRoot): %v", err)
	}
	if !ro.readOnlyRoot {
		t.Fatal("WithReadOnlyRoot must mark the runner read-only")
	}
}
