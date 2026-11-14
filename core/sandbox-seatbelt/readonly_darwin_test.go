//go:build darwin

package seatbelt

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

func TestBuildProfileRootWritableByDefault(t *testing.T) {
	profile, err := buildProfile(
		[]string{"/srv/root", "/srv/cache"},
		permission.NetPolicy{Mode: permission.NetDefault},
		0,
	)
	if err != nil {
		t.Fatalf("buildProfile: %v", err)
	}
	for _, want := range []string{
		"(deny file-write*)",
		`(allow file-write* (subpath "/srv/root"))`,
		`(allow file-write* (subpath "/srv/cache"))`,
		`(allow file-write* (literal "/dev/null"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %q:\n%s", want, profile)
		}
	}
}

func TestBuildProfileReadOnlyOmitsRoot(t *testing.T) {
	profile, err := buildProfile(
		[]string{"/srv/cache"},
		permission.NetPolicy{Mode: permission.NetDefault},
		0,
	)
	if err != nil {
		t.Fatalf("buildProfile: %v", err)
	}
	if !strings.Contains(profile, "(deny file-write*)") {
		t.Errorf("profile must deny file writes:\n%s", profile)
	}
	if strings.Contains(profile, `(allow file-write* (subpath "/srv/root"))`) {
		t.Errorf("read-only profile must not allow the runner root:\n%s", profile)
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/srv/cache"))`) {
		t.Errorf("read-only profile must keep explicit writable paths:\n%s", profile)
	}
}

func TestNewReadOnlyRootOption(t *testing.T) {
	root := t.TempDir()

	runner, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if runner.readOnlyRoot {
		t.Fatal("default runner must keep the root writable")
	}

	ro, err := New(root, WithReadOnlyRoot())
	if err != nil {
		t.Fatalf("New(WithReadOnlyRoot): %v", err)
	}
	if !ro.readOnlyRoot {
		t.Fatal("WithReadOnlyRoot must make the root read-only")
	}
	if len(ro.extraWritable) != 0 {
		t.Fatalf("unexpected extra writable paths: %v", ro.extraWritable)
	}
}

// TestNewRejectsWritableRootWithReadOnlyRoot pins the documented
// contract: an explicit writable path that resolves to the runner root
// conflicts with readonly_root and must fail construction instead of
// being silently dropped.
func TestNewRejectsWritableRootWithReadOnlyRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := New(root, WithReadOnlyRoot(), WithWritablePaths(root)); err == nil {
		t.Fatal("New must reject a writable path that is the read-only runner root")
	}
	// Without readonly_root the root is already writable by default,
	// so listing it is redundant but harmless.
	runner, err := New(root, WithWritablePaths(root))
	if err != nil {
		t.Fatalf("New(WithWritablePaths(root)): %v", err)
	}
	if len(runner.extraWritable) != 0 {
		t.Fatalf("root must not be duplicated in extraWritable: %v", runner.extraWritable)
	}
}

// TestWritablePathsForPerCallPermission pins the replacement rule: a
// caller-supplied permission is the whole writable set for that call, so the
// runner root *and* the host's explicit writable paths are dropped.
//
// Keeping the exceptions would reintroduce exactly the drift the shared policy
// exists to prevent: fsx would deny a write to cache/ while the kernel mounts
// still allowed it. A host that wants its exceptions on a narrowed call puts
// them in the policy it hands to both interpreters.
func TestWritablePathsForPerCallPermission(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	runner, err := New(root, WithWritablePaths(cache))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if writes := runner.writablePathsFor(sandbox.ExecOptions{}); !slices.Contains(writes, runner.rootDir) {
		t.Fatalf("a call without a permission must keep the root writable: %v", writes)
	}

	writes := runner.writablePathsFor(sandbox.ExecOptions{Permission: &permission.Policy{}})
	if len(writes) != 0 {
		t.Fatalf("a supplied policy is the whole writable set, got %v", writes)
	}

	narrow := &permission.Policy{Writes: permission.Writes{Paths: []string{cache}}}
	writes = runner.writablePathsFor(sandbox.ExecOptions{Permission: narrow})
	// The profile names the executor's real path: a rule written against a
	// symlinked spelling (macOS /var -> /private/var) would match nothing.
	realCache, err := filepath.EvalSymlinks(cache)
	if err != nil {
		t.Fatalf("EvalSymlinks(cache): %v", err)
	}
	if len(writes) != 1 || writes[0] != realCache {
		t.Fatalf("writable = %v, want exactly %q", writes, realCache)
	}
}
