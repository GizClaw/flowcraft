//go:build darwin

package seatbelt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
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
	if !runner.rootWritable {
		t.Fatal("default runner must keep the root writable")
	}

	ro, err := New(root, WithReadOnlyRoot())
	if err != nil {
		t.Fatalf("New(WithReadOnlyRoot): %v", err)
	}
	if ro.rootWritable {
		t.Fatal("WithReadOnlyRoot must make the root read-only")
	}
	if len(ro.extraWritable) != 0 {
		t.Fatalf("unexpected extra writable paths: %v", ro.extraWritable)
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
			if runner.rootWritable != !tc.wantRO {
				t.Fatalf("rootWritable = %v, want %v", runner.rootWritable, !tc.wantRO)
			}
		})
	}
}
