package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestManifestValidatesRunPaths covers the filesystem half of Validate: a
// manifest that installs must be one the plugin host can spawn, and the
// check mirrors ResolveCommand so that install and spawn cannot disagree
// about which files must exist.
func TestManifestValidatesRunPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		manifest string
		files    []string
		dirs     []string
		wantErr  string
	}{{
		name: "path command that exists",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "./server", "args": ["-test.run=TestX"]}}`,
		files: []string{"server"},
	}, {
		name: "path command that is missing",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "./server"}}`,
		wantErr: `run.command "./server"`,
	}, {
		name: "path command that is a directory",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "bin/server"}}`,
		dirs:    []string{"bin/server"},
		wantErr: `run.command "bin/server" is a directory`,
	}, {
		name: "absolute command",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "/usr/bin/python3", "args": ["server.py"]}}`,
		wantErr: "must be relative",
	}, {
		name: "bare command is a PATH lookup and stays unchecked",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "definitely-not-a-real-command-xyz",
				"args": ["index.js", "--port", "0"]}}`,
	}, {
		name: "path argument that exists",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "python3", "args": ["server/main.py"]}}`,
		files: []string{filepath.Join("server", "main.py")},
	}, {
		name: "path argument that is missing",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "python3", "args": ["server/main.py"]}}`,
		wantErr: `run.arg "server/main.py"`,
	}, {
		name: "directory argument is a legitimate package path",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "go", "args": ["run", "./server"]}}`,
		dirs: []string{"server"},
	}, {
		name: "flag argument carrying a path is not resolved",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "node",
				"args": ["server.js", "--config=./missing.json"]}}`,
	}, {
		name: "argument that would escape the root",
		manifest: `{"id": "hello", "version": "0.1.0",
			"requires": ["tools.provide"],
			"run": {"command": "python3", "args": ["../../etc/passwd"]}}`,
		wantErr: "escapes the plugin root",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := filepath.Join(root, "hello")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("mkdir plugin: %v", err)
			}
			for _, rel := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(dir, rel), 0o700); err != nil {
					t.Fatalf("mkdir %s: %v", rel, err)
				}
			}
			for _, rel := range tc.files {
				path := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("mkdir %s: %v", filepath.Dir(rel), err)
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
					t.Fatalf("write %s: %v", rel, err)
				}
			}
			manifest, err := ParseManifest(context.Background(), []byte(tc.manifest))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			err = manifest.Validate(dir, NewVocabulary())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.wantErr)
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("Validate = %v, want a Validation error", err)
			}
		})
	}
}

// TestManifestValidateWithoutRootSkipsRunPaths pins the documented escape
// hatch: an empty root still means "no filesystem checks".
func TestManifestValidateWithoutRootSkipsRunPaths(t *testing.T) {
	t.Parallel()
	manifest, err := ParseManifest(context.Background(), []byte(`{
		"id": "hello", "version": "0.1.0",
		"requires": ["tools.provide"],
		"run": {"command": "bin/missing-server"}
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := manifest.Validate("", NewVocabulary()); err != nil {
		t.Fatalf("Validate(rootless) = %v, want nil", err)
	}
}

// TestManifestRequires pins the two halves of the vocabulary: craft's own
// list is closed — a typo in a contribution grant has to be refused rather
// than read as a grant nothing will ever satisfy — and the application's
// grants are accepted on top of it, which is the only way a plugin can ask
// for one of its host's primitives.
func TestManifestRequires(t *testing.T) {
	t.Parallel()
	manifest := func(requires string) Manifest {
		t.Helper()
		parsed, err := ParseManifest(context.Background(), []byte(`{
			"id": "hello", "version": "0.1.0",
			"requires": [`+requires+`]
		}`))
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		return parsed
	}
	craft := NewVocabulary()
	if err := manifest(`"skills.provide"`).Validate("", craft); err != nil {
		t.Fatalf("Validate with a contribution grant = %v, want nil", err)
	}
	err := manifest(`"skills.provid"`).Validate("", craft)
	if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "unknown grant") {
		t.Fatalf("Validate with a typo = %v, want a Validation error naming it", err)
	}
	granted := manifest(`"skills.provide", "notary.sign"`)
	if err := granted.Validate("", craft); !errdefs.IsValidation(err) {
		t.Fatalf("Validate without the grant declared = %v, want Validation", err)
	}
	if err := granted.Validate("", NewVocabulary("notary.sign")); err != nil {
		t.Fatalf("Validate with the grant declared = %v, want nil", err)
	}
}

// TestToolsProvideRequiresARunSection pins the one cross-field rule the
// manifest keeps: tools arrive from a process, so a plugin that requires
// the channel without one is a manifest error rather than a plugin that
// runs and publishes nothing.
func TestToolsProvideRequiresARunSection(t *testing.T) {
	t.Parallel()
	manifest, err := ParseManifest(context.Background(), []byte(`{
		"id": "hello", "version": "0.1.0",
		"requires": ["tools.provide"]
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	err = manifest.Validate("", NewVocabulary())
	if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "run section") {
		t.Fatalf("Validate = %v, want a Validation error naming the run section", err)
	}
}

// TestRemovedManifestFieldsAreNamed covers the fields the wire redesign
// deleted. They are refused with what replaced them: a decoder that only
// said "unknown field" would leave the operator of a still-MCP plugin with
// an installed plugin that publishes nothing and no hint as to why.
func TestRemovedManifestFieldsAreNamed(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"mcp":        `{"id": "old", "version": "0.1.0", "mcp": {"command": "node"}}`,
		"mcpServers": `{"id": "old", "version": "0.1.0", "mcpServers": [{"command": "node"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseManifest(context.Background(), []byte(raw))
			if !errdefs.IsValidation(err) {
				t.Fatalf("ParseManifest = %v, want Validation", err)
			}
			if !strings.Contains(err.Error(), "FLOWCRAFT_PLUGIN_WIRE") {
				t.Fatalf("ParseManifest error = %v, want it to name the wire", err)
			}
		})
	}
}

// TestUnrunnableCommandIsRejectedAtInspectAndInstallAndReportedByScan
// covers the entry points that hand Validate a root: Inspect and Install
// refuse the manifest, and a scanned tree reports it as the entry's
// error instead of dropping the plugin.
func TestUnrunnableCommandIsRejectedAtInspectAndInstallAndReportedByScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "broken", `{
		"id": "broken", "version": "0.1.0",
		"requires": ["tools.provide"],
		"run": {"command": "bin/missing-server"}
	}`)
	store := newStoreOver(t, root)
	ctx := context.Background()

	if _, err := store.Inspect(ctx, filepath.Join(root, "broken")); !errdefs.IsValidation(err) {
		t.Fatalf("Inspect = %v, want Validation", err)
	}
	_, err := store.Install(ctx, filepath.Join(root, "broken"))
	if !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), `run.command "bin/missing-server"`) {
		t.Fatalf("Install = %v, want Validation naming the missing command", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, summary := range list {
		if summary.ID != "broken" {
			continue
		}
		found = true
		if !strings.Contains(summary.Error, "run.command") {
			t.Fatalf("scanned entry error = %q, want it to name the missing command",
				summary.Error)
		}
	}
	if !found {
		t.Fatalf("broken plugin is not listed: %+v", list)
	}
}
