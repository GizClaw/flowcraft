package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestManifestValidatesMCPPaths covers the stdio half of Validate's
// filesystem section: a manifest that installs must be one the plugin
// host can spawn, and the check mirrors ResolveCommand so that install
// and spawn cannot disagree about which files must exist.
func TestManifestValidatesMCPPaths(t *testing.T) {
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
			"permissions": ["mcp:provide"],
			"mcp": {"command": "./server", "args": ["-test.run=TestX"]}}`,
		files: []string{"server"},
	}, {
		name: "path command that is missing",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "./server"}}`,
		wantErr: `mcp server 0 command "./server"`,
	}, {
		name: "path command that is a directory",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "bin/server"}}`,
		dirs:    []string{"bin/server"},
		wantErr: `mcp server 0 command "bin/server" is a directory`,
	}, {
		name: "absolute command",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "/usr/bin/python3", "args": ["server.py"]}}`,
		wantErr: "must be relative",
	}, {
		name: "bare command is a PATH lookup and stays unchecked",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "definitely-not-a-real-command-xyz",
				"args": ["index.js", "--port", "0"]}}`,
	}, {
		name: "path argument that exists",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "python3", "args": ["server/main.py"]}}`,
		files: []string{filepath.Join("server", "main.py")},
	}, {
		name: "path argument that is missing",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "python3", "args": ["server/main.py"]}}`,
		wantErr: `mcp server 0 arg "server/main.py"`,
	}, {
		name: "directory argument is a legitimate package path",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "go", "args": ["run", "./server"]}}`,
		dirs: []string{"server"},
	}, {
		name: "flag argument carrying a path is not resolved",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "node", "args": ["server.js", "--config=./missing.json"]}}`,
	}, {
		name: "legacy mcpServers are checked the same way",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcpServers": [{"command": "bin/missing-server"}]}`,
		wantErr: `mcp server 0 command "bin/missing-server"`,
	}, {
		name: "argument that would escape the root",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"command": "python3", "args": ["../../etc/passwd"]}}`,
		wantErr: "escapes the plugin root",
	}, {
		name: "http transport is not a filesystem question",
		manifest: `{"id": "hello", "version": "0.1.0",
			"permissions": ["mcp:provide"],
			"mcp": {"transport": "http", "url": "https://mcp.example.com/mcp"}}`,
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
			err = manifest.Validate(dir)
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

// TestManifestValidateWithoutRootSkipsMCPPaths pins the documented
// escape hatch: an empty root still means "no filesystem checks".
func TestManifestValidateWithoutRootSkipsMCPPaths(t *testing.T) {
	t.Parallel()
	manifest, err := ParseManifest(context.Background(), []byte(`{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcp": {"command": "bin/missing-server"}
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := manifest.Validate(""); err != nil {
		t.Fatalf("Validate(rootless) = %v, want nil", err)
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
		"permissions": ["mcp:provide"],
		"mcp": {"command": "bin/missing-server"}
	}`)
	store := newStoreOver(t, root)
	ctx := context.Background()

	if _, err := store.Inspect(ctx, filepath.Join(root, "broken")); !errdefs.IsValidation(err) {
		t.Fatalf("Inspect = %v, want Validation", err)
	}
	_, err := store.Install(ctx, filepath.Join(root, "broken"))
	if !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), `mcp server 0 command "bin/missing-server"`) {
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
		if !strings.Contains(summary.Error, "mcp server 0 command") {
			t.Fatalf("scanned entry error = %q, want it to name the missing command",
				summary.Error)
		}
	}
	if !found {
		t.Fatalf("broken plugin is not listed: %+v", list)
	}
}
