package craft

import (
	"runtime/debug"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func buildModule(path, version string) *debug.Module {
	return &debug.Module{Path: path, Version: version}
}

// TestModuleVersionResolvesWhatTheBuildRecorded pins where the host
// version comes from for a released module: the build info of the binary
// that linked it — the version a consumer pinned is exactly what a
// min_host_version floor asks about — and nothing when the build info
// does not describe a released module.
func TestModuleVersionResolvesWhatTheBuildRecorded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{{
		name: "dependency pinned to a release",
		info: &debug.BuildInfo{Deps: []*debug.Module{buildModule(modulePath, "v0.3.1")}},
		want: "0.3.1",
	}, {
		name: "dependency pinned to a commit",
		info: &debug.BuildInfo{Deps: []*debug.Module{
			buildModule(modulePath, "v0.3.2-0.20260101120000-abcdef123456"),
		}},
		want: "0.3.2-0.20260101120000-abcdef123456",
	}, {
		// Before the first tag every commit pin is this shape. It
		// resolves — the commit is what the build is — and the floors
		// are compared against it (see the commit-pin cases in
		// TestHostFloor).
		name: "dependency pinned to a commit before the first tag",
		info: &debug.BuildInfo{Deps: []*debug.Module{
			buildModule(modulePath, "v0.0.0-20260101120000-abcdef123456"),
		}},
		want: "0.0.0-20260101120000-abcdef123456",
	}, {
		name: "dependency replaced by a local path",
		info: &debug.BuildInfo{Deps: []*debug.Module{{
			Path:    modulePath,
			Version: "v0.3.1",
			Replace: buildModule("../craft", ""),
		}}},
		want: "",
	}, {
		name: "dependency without a version",
		info: &debug.BuildInfo{Deps: []*debug.Module{buildModule(modulePath, "(devel)")}},
		want: "",
	}, {
		name: "another module only",
		info: &debug.BuildInfo{Deps: []*debug.Module{buildModule("example.com/other", "v1.2.3")}},
		want: "",
	}, {
		name: "this module is the main one",
		info: &debug.BuildInfo{Main: *buildModule(modulePath, "v0.3.1")},
		want: "",
	}, {
		name: "no build info",
		info: nil,
		want: "",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := moduleVersion(tc.info); got != tc.want {
				t.Fatalf("moduleVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNormalizeVersionKeepsTheSpellingsTheValidatorAccepts pins the
// boundary between a version this module accepts and a development
// build: everything go spells as a module version loses its v prefix,
// and anything this module would not validate resolves to nothing
// rather than to a floor enforced against a version nobody meant.
func TestNormalizeVersionKeepsTheSpellingsTheValidatorAccepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{"v0.3.1", "0.3.1"},
		{"0.3.1", "0.3.1"},
		{"v1.0.0-rc.1", "1.0.0-rc.1"},
		{"(devel)", ""},
		{"", ""},
		{"vnext", ""},
		{"v1.0.0-", ""},
	}
	for _, tc := range cases {
		if got := normalizeVersion(tc.raw); got != tc.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestHostFloor covers the comparison [Version] decides: a development
// build skips the floor, a resolved one enforces it, and the floor's
// spelling is checked either way.
func TestHostFloor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		hostVersion string
		min         string
		wantErr     bool
	}{{
		name:        "development build skips the floor",
		hostVersion: devVersion,
		min:         "9.9.9",
	}, {
		name:        "the floor itself passes",
		hostVersion: "0.2.0",
		min:         "0.2.0",
	}, {
		name:        "a newer host passes",
		hostVersion: "0.3.0",
		min:         "0.2.0",
	}, {
		name:        "an older host is refused",
		hostVersion: "0.1.9",
		min:         "0.2.0",
		wantErr:     true,
	}, {
		name:        "a prerelease of the floor is refused",
		hostVersion: "0.2.0-rc.1",
		min:         "0.2.0",
		wantErr:     true,
	}, {
		name:        "a prerelease above the floor passes",
		hostVersion: "0.3.0-rc.1",
		min:         "0.2.0",
	}, {
		name:        "the spelling is checked in development too",
		hostVersion: devVersion,
		min:         "not a version",
		wantErr:     true,
	}, {
		name:        "an empty prerelease is not a version",
		hostVersion: "0.3.0",
		min:         "0.2.0-",
		wantErr:     true,
	}, {
		// A commit pin is a version rather than a development build:
		// the floor is not skipped for it, because nothing has said
		// that commit is the release the floor names.
		name:        "a commit pin below the floor is refused",
		hostVersion: "0.0.0-20260101120000-abcdef123456",
		min:         "0.2.0",
		wantErr:     true,
	}, {
		name:        "a commit pin above the floor passes",
		hostVersion: "0.3.2-0.20260101120000-abcdef123456",
		min:         "0.2.0",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := hostFloor(tc.hostVersion, tc.min)
			if tc.wantErr {
				if !errdefs.IsValidation(err) {
					t.Fatalf("hostFloor(%q, %q) = %v, want a validation error",
						tc.hostVersion, tc.min, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("hostFloor(%q, %q) = %v, want nil", tc.hostVersion, tc.min, err)
			}
		})
	}
}
