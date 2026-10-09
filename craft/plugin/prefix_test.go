package plugin

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestResolveCommand covers the documented manifest shapes: a bare
// command is looked up through PATH while its path-bearing arguments
// stay inside the plugin root, and a command with a separator is
// resolved against the root itself.
func TestResolveCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := []struct {
		name        string
		command     string
		args        []string
		wantCommand string
		wantArgs    []string
	}{{
		name:        "bare command with a plugin-relative script",
		command:     "python3",
		args:        []string{"server/main.py"},
		wantCommand: "python3",
		wantArgs:    []string{filepath.Join(root, "server", "main.py")},
	}, {
		name:        "bare command with flags and a plugin-relative script",
		command:     "uvx",
		args:        []string{"--from", "mcp[cli]", "python", "./server/main.py"},
		wantCommand: "uvx",
		wantArgs: []string{
			"--from", "mcp[cli]", "python",
			filepath.Join(root, "server", "main.py"),
		},
	}, {
		name:        "relative command",
		command:     "./server",
		args:        []string{"-test.run=TestX"},
		wantCommand: filepath.Join(root, "server"),
		wantArgs:    []string{"-test.run=TestX"},
	}, {
		name:        "bare command and bare arguments are untouched",
		command:     "node",
		args:        []string{"index.js", "--port", "0"},
		wantCommand: "node",
		wantArgs:    []string{"index.js", "--port", "0"},
	}, {
		name:        "paths are cleaned",
		command:     "./bin/../server",
		args:        nil,
		wantCommand: filepath.Join(root, "server"),
		wantArgs:    []string{},
	}, {
		name:        "argument that would escape the root",
		command:     "python3",
		args:        []string{"../../etc/passwd"},
		wantCommand: "",
		wantArgs:    nil,
	}, {
		name:        "absolute command",
		command:     "/usr/bin/python3",
		args:        []string{"server/main.py"},
		wantCommand: "",
		wantArgs:    nil,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command, args, err := ResolveCommand(root, tc.command, tc.args)
			if tc.wantCommand == "" {
				if err == nil {
					t.Fatalf("ResolveCommand(%q, %v) = %q, %v; want an error",
						tc.command, tc.args, command, args)
				}
				if !errdefs.IsForbidden(err) && !errdefs.IsValidation(err) {
					t.Fatalf("ResolveCommand error = %v, want Forbidden or Validation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveCommand: %v", err)
			}
			if command != tc.wantCommand {
				t.Fatalf("command = %q, want %q", command, tc.wantCommand)
			}
			if len(args) != len(tc.wantArgs) ||
				(len(args) > 0 && !reflect.DeepEqual(args, tc.wantArgs)) {
				t.Fatalf("args = %v, want %v", args, tc.wantArgs)
			}
		})
	}
}
