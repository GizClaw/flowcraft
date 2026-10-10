package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTour runs the whole example against a temporary data directory:
// definition, plugin scan, start, two keyed runtimes with their own
// notes, a tool round trip through the plugin and the host primitives, a
// manual reload, a plugin disable/enable cycle and shutdown.
//
// It is hermetic but not free: the plugin's MCP server is a Go program
// the manifest starts with `go run`, so the first run compiles it. No
// network access and no credentials are involved.
func TestTour(t *testing.T) {
	if testing.Short() {
		t.Skip("the tour compiles and runs the plugin's MCP server")
	}
	dataDir := t.TempDir()
	out := runTour(t, dataDir)

	if got := readNotes(t, filepath.Join(dataDir, "notes-default.txt")); got != "buy milk\n" {
		t.Errorf("default notes = %q, want the one note the tour added", got)
	}
	if got := readNotes(t, filepath.Join(dataDir, "notes-alpha.txt")); got != "call the builder\n" {
		t.Errorf("alpha notes = %q, want the note added before the reload", got)
	}
	// The reloaded alpha generation reads a second file that nothing
	// wrote to, so it must still not exist.
	round2 := filepath.Join(dataDir, "notes-alpha-round2.txt")
	if _, err := os.Stat(round2); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want it to not exist", round2, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "alpha.layer.json")); err != nil {
		t.Errorf("stat alpha.layer.json: %v", err)
	}

	text := out
	for _, want := range []string{
		"craft anvil 0.1.0 (Anvil)",
		"hello 0.1.0  enabled=true  permissions=mcp:provide, skills:provide, events:emit",
		"plugin tools published: hello__greet, hello__ping_host",
		"event  craft.started",
		`notes_add {"text":"buy milk"} -> added note 1 to ` +
			filepath.Join(dataDir, "notes-default.txt"),
		"notes_add  \"Append one line to the default runtime's notes file.\"",
		`hello__greet {"name":"anvil"} -> Hello, anvil!`,
		"event  plugin.hello.ping",
		"needs a grant for [secret_delete, secret_get, secret_set]",
		"no notes yet in " + filepath.Join(dataDir, "notes-alpha.txt"),
		"event  craft.reload.completed  {\"key\":\"alpha\",\"reason\":\"manual\"}",
		"no notes yet in " + round2,
		"disabled: the plugin's tools are withdrawn from every open runtime",
		"tools: notes_add, notes_list",
		"tools: hello__greet, hello__ping_host, notes_add, notes_list",
		`hello__greet {"name":"the restarted plugin"} -> Hello, the restarted plugin!`,
		"event  craft.reload.completed  {\"key\":\"default\",\"reason\":\"plugin\"}",
		"event  craft.runtime.closed  {\"key\":\"alpha\"}",
		"craft closed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("tour output does not contain %q", want)
		}
	}
	// The keyed runtimes must not share state.
	if strings.Contains(text, "buy milk; call the builder") {
		t.Error("the two runtimes shared one notes file")
	}
}

// TestTourIsRepeatable runs the tour twice against the same data
// directory: the second run has to reset the files it owns (notes files
// and the layer it writes) and produce the same story.
func TestTourIsRepeatable(t *testing.T) {
	if testing.Short() {
		t.Skip("the tour compiles and runs the plugin's MCP server")
	}
	dataDir := t.TempDir()
	runTour(t, dataDir)
	second := runTour(t, dataDir)
	if !strings.Contains(second, "scratch files removed") {
		t.Error("the second run did not report resetting its scratch files")
	}
	if strings.Contains(second, "0 scratch files removed") {
		t.Error("the second run found nothing to reset")
	}
	if got := readNotes(t, filepath.Join(dataDir, "notes-default.txt")); got != "buy milk\n" {
		t.Errorf("default notes after the second run = %q", got)
	}
}

// runTour assembles the example in dir and returns its narration.
func runTour(t *testing.T, dir string) string {
	t.Helper()
	out := &bytes.Buffer{}
	app, err := newApp(options{
		definition:    "craft.yaml",
		dataDir:       dir,
		out:           out,
		pluginTimeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := app.tour(ctx); err != nil {
		t.Fatalf("tour: %v\n--- output ---\n%s", err, out.String())
	}
	return out.String()
}

func readNotes(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
