//go:build linux

package bwrap

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/permission"
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

// TestFilesystemFlagsForPerCallPermission pins the replacement rule: a
// caller-supplied permission is the whole writable set, so the runner root and
// the host's explicit writable paths are both dropped. Keeping the exceptions
// would let the kernel allow a write fsx denies — the drift the shared policy
// exists to prevent.
func TestFilesystemFlagsForPerCallPermission(t *testing.T) {
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

	workspace := runner.filesystemFlagsFor(sandbox.ExecOptions{})
	if !containsSeq(workspace, "--bind", runner.rootDir, runner.rootDir) {
		t.Fatalf("a call without a permission must bind the root read-write: %v", workspace)
	}

	readOnly := runner.filesystemFlagsFor(sandbox.ExecOptions{Permission: &permission.Policy{}})
	if !containsSeq(readOnly, "--ro-bind", runner.rootDir, runner.rootDir) {
		t.Fatalf("an empty policy must ro-bind the root: %v", readOnly)
	}
	if containsSeq(readOnly, "--bind", cache, cache) {
		t.Fatalf("a supplied policy is the whole writable set; %q must not be bound: %v", cache, readOnly)
	}

	narrow := &permission.Policy{Files: permission.Files{Entries: []permission.Entry{
		{Path: cache, Access: permission.Write},
	}}}
	allowed := runner.filesystemFlagsFor(sandbox.ExecOptions{Permission: narrow})
	if !containsSeq(allowed, "--bind", cache, cache) {
		t.Fatalf("the policy entry must be bound writable: %v", allowed)
	}
	if !containsSeq(allowed, "--ro-bind", runner.rootDir, runner.rootDir) {
		t.Fatalf("the root stays read-only when the policy does not cover it: %v", allowed)
	}
}

// TestChildEnvIsAllowListOnly is the env-policy assertion: with an
// allow-list, the child environment carries exactly the allow-listed
// host vars plus the injected ones, and a host var outside the list
// never reaches the sandbox.
func TestChildEnvIsAllowListOnly(t *testing.T) {
	hostEnv := []string{
		"PATH=/usr/bin:/bin",
		"OPENAI_API_KEY=sk-host-secret",
		"LANG=en_US.UTF-8",
	}
	got := childEnv(sandbox.EnvPolicy{
		Allow:  []string{"PATH", "LANG"},
		Inject: map[string]string{"LANG": "C", "APP_MODE": "sandbox"},
	}, hostEnv)

	want := []string{"APP_MODE=sandbox", "LANG=C", "PATH=/usr/bin:/bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("child env = %v, want %v", got, want)
	}
}

// TestChildEnvNilAllowPassesHostThrough pins the zero-value policy: a
// nil Allow list means "inherit the host environment", so the child
// sees every host var plus injected overrides.
func TestChildEnvNilAllowPassesHostThrough(t *testing.T) {
	hostEnv := []string{"PATH=/usr/bin:/bin", "HOME=/root", "LANG=en_US.UTF-8"}
	got := childEnv(sandbox.EnvPolicy{
		Inject: map[string]string{"LANG": "C"},
	}, hostEnv)

	want := []string{"HOME=/root", "LANG=C", "PATH=/usr/bin:/bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("child env = %v, want %v", got, want)
	}
}

// TestChildEnvEmptyAllowListDropsEverything covers the closed list: an
// empty (non-nil) Allow selects no host var, so only Inject survives.
func TestChildEnvEmptyAllowListDropsEverything(t *testing.T) {
	hostEnv := []string{"PATH=/usr/bin:/bin", "OPENAI_API_KEY=sk-host-secret"}
	if got := childEnv(sandbox.EnvPolicy{Allow: []string{}}, hostEnv); len(got) != 0 {
		t.Fatalf("child env = %v, want empty", got)
	}
}

// TestBuildFlagsCarriesNoEnvVars is the argv-exposure assertion: no
// env var may travel as a bwrap argument, where /proc/<pid>/cmdline
// would expose it to every local process for the lifetime of the exec.
func TestBuildFlagsCarriesNoEnvVars(t *testing.T) {
	flags, err := buildFlags(sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{
			Allow:  []string{"PATH"},
			Inject: map[string]string{"OPENAI_API_KEY": "sk-host-secret"},
		},
	})
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	for _, flag := range flags {
		if flag == "--setenv" || flag == "--clearenv" || flag == "--unsetenv" {
			t.Fatalf("env must not travel as bwrap flags: %v", flags)
		}
		if strings.Contains(flag, "sk-host-secret") {
			t.Fatalf("secret leaked into bwrap argv: %v", flags)
		}
	}
}
