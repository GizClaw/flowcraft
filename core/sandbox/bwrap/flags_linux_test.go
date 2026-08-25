//go:build linux

package bwrap

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

func containsSeq(flags []string, seq ...string) bool {
	return indexSeq(flags, seq...) >= 0
}

func indexSeq(flags []string, seq ...string) int {
	for i := 0; i+len(seq) <= len(flags); i++ {
		if slices.Equal(flags[i:i+len(seq)], seq) {
			return i
		}
	}
	return -1
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
	// The first --ro-bind is the host root; find the workspace bind.
	roBind := indexSeq(flags, "--ro-bind", "/tmp/ws", "/tmp/ws")
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

// TestNewRejectsWritableRootWithReadOnlyRoot pins the documented
// contract: an explicit writable path that resolves to the runner root
// conflicts with readonly_root and must fail construction instead of
// being silently dropped.
func TestNewRejectsWritableRootWithReadOnlyRoot(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bwrap-fake")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	root := t.TempDir()

	if _, err := New(root, WithBinary(binary), WithReadOnlyRoot(), WithWritablePaths(root)); err == nil {
		t.Fatal("New must reject a writable path that is the read-only runner root")
	}
	// Without readonly_root the root is already writable by default,
	// so listing it is redundant but harmless.
	runner, err := New(root, WithBinary(binary), WithWritablePaths(root))
	if err != nil {
		t.Fatalf("New(WithWritablePaths(root)): %v", err)
	}
	if len(runner.writablePaths) != 0 {
		t.Fatalf("root must not be duplicated in writablePaths: %v", runner.writablePaths)
	}
}

// TestFilesystemFlagsForPerCallReadOnly is the direct assertion that a
// per-call WriteReadOnly keeps the construction-time writable root
// read-only while explicit writable paths stay writable.
func TestFilesystemFlagsForPerCallReadOnly(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bwrap-fake")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	root := t.TempDir()
	cache := t.TempDir()
	runner, err := New(root, WithBinary(binary), WithWritablePaths(cache))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workspace := runner.filesystemFlagsFor(sandbox.WriteWorkspace)
	if !containsSeq(workspace, "--bind", runner.rootDir, runner.rootDir) {
		t.Fatalf("workspace call must bind the root read-write: %v", workspace)
	}

	readOnly := runner.filesystemFlagsFor(sandbox.WriteReadOnly)
	if !containsSeq(readOnly, "--ro-bind", runner.rootDir, runner.rootDir) {
		t.Fatalf("per-call read-only must ro-bind the root: %v", readOnly)
	}
	if containsSeq(readOnly, "--bind", runner.rootDir, runner.rootDir) {
		t.Fatalf("per-call read-only must not bind the root read-write: %v", readOnly)
	}
	if len(runner.writablePaths) != 1 ||
		!containsSeq(readOnly, "--bind", runner.writablePaths[0], runner.writablePaths[0]) {
		t.Fatalf("explicit writable paths must stay writable: %v", readOnly)
	}
}
