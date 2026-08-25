//go:build darwin

package seatbelt

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

func TestBuildProfileRootWritableByDefault(t *testing.T) {
	profile, err := buildProfile(
		[]string{"/srv/root", "/srv/cache"},
		corenet.NetPolicy{Mode: corenet.NetDefault},
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
		corenet.NetPolicy{Mode: corenet.NetDefault},
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

// TestWritablePathsForPerCallReadOnly is the direct assertion that a
// per-call WriteReadOnly keeps the construction-time writable root out
// of the writable set while explicit writable paths stay writable.
func TestWritablePathsForPerCallReadOnly(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	runner, err := New(root, WithWritablePaths(cache))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if writes := runner.writablePathsFor(sandbox.WriteWorkspace); !slices.Contains(writes, runner.rootDir) {
		t.Fatalf("workspace call must keep the root writable: %v", writes)
	}

	writes := runner.writablePathsFor(sandbox.WriteReadOnly)
	if slices.Contains(writes, runner.rootDir) {
		t.Fatalf("per-call read-only must drop the root from the writable set: %v", writes)
	}
	if len(runner.extraWritable) != 1 || !slices.Contains(writes, runner.extraWritable[0]) {
		t.Fatalf("explicit writable paths must stay writable: %v", writes)
	}
}

func TestRegisterReadOnlyRootSettings(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name     string
		settings string
		wantRO   bool
	}{
		{name: "default", settings: `{"root": "` + root + `"}`},
		{name: "readonly_root false", settings: `{"root": "` + root + `", "readonly_root": false}`},
		{name: "readonly_root true", settings: `{"root": "` + root + `", "readonly_root": true}`, wantRO: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewFactory().New(context.Background(), resource.Input{
				Settings: json.RawMessage(tc.settings),
			})
			if err != nil {
				t.Fatalf("NewFactory().New: %v", err)
			}
			runner, ok := got.(*Runner)
			if !ok {
				t.Fatalf("factory returned %T, want *Runner", got)
			}
			if runner.readOnlyRoot != tc.wantRO {
				t.Fatalf("readOnlyRoot = %v, want %v", runner.readOnlyRoot, tc.wantRO)
			}
		})
	}
}
