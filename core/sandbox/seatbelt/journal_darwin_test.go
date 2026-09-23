//go:build darwin

package seatbelt

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/internal/journaltest"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// TestJournalContract runs the cross-backend journal contract against a
// real Seatbelt sandbox.
//
// It is the test that validates the whole host-side design: the profile
// grants write access to the root and to the explicit writable paths at
// their own absolute host paths, so a write made inside the sandbox is a
// write to the same host path, and the host-side watcher sees it without
// any cooperation from the sandboxed process.
func TestJournalContract(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skipf("sandbox-exec is not installed: %v", err)
	}
	if !journal.Available() {
		t.Skip("no file-watch source on this platform")
	}
	journaltest.Run(t, buildJournalHarness)
}

func buildJournalHarness(t *testing.T, enabled bool, opts sandbox.JournalOptions) journaltest.Harness {
	t.Helper()
	root := t.TempDir()

	var runnerOpts []RunnerOption
	if enabled {
		runnerOpts = append(runnerOpts, WithFileJournal(opts))
	}
	runner, err := New(root, runnerOpts...)
	if err != nil {
		t.Fatalf("seatbelt New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	if got, want := runner.Capabilities().Journal.Enabled, enabled; got != want {
		t.Fatalf("Journal.Enabled = %v, want %v", got, want)
	}

	return journaltest.Harness{
		Runner: runner,
		Root:   root,
		Exec: func(t *testing.T, script string) {
			t.Helper()
			result, err := sandbox.Exec(context.Background(), runner, "/bin/sh",
				[]string{"-c", script}, sandbox.ExecOptions{WorkDir: root})
			if err != nil {
				t.Fatalf("exec %q: %v", script, err)
			}
			if result.ExitCode != 0 {
				t.Fatalf("exec %q: exit %d: %s", script, result.ExitCode, result.Stderr)
			}
		},
	}
}

// TestJournalCoversWritablePathsOutsideTheRoot covers the other half of
// the watch set: a path granted with writable_paths is not under the
// root, so its events carry absolute paths.
func TestJournalCoversWritablePathsOutsideTheRoot(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skipf("sandbox-exec is not installed: %v", err)
	}
	if !journal.Available() {
		t.Skip("no file-watch source on this platform")
	}

	root := t.TempDir()
	outside := t.TempDir()
	runner, err := New(root, WithWritablePaths(outside), WithFileJournal(sandbox.JournalOptions{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = runner.Close() }()

	reader, err := sandbox.OpenJournal(context.Background(), runner)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	// The workdir stays inside the root (the runner rejects an
	// out-of-root workdir); the writable path is reachable by its own
	// absolute path, which is where the journal has to see the write.
	script := "printf x > " + outside + "/artifact.md"
	result, err := sandbox.Exec(context.Background(), runner, "/bin/sh",
		[]string{"-c", script}, sandbox.ExecOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exec: exit %d: %s", result.ExitCode, result.Stderr)
	}

	// The journal resolves the writable path before watching it (the
	// runner does the same), which on macOS turns /var into
	// /private/var: the event carries the resolved path.
	resolved, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", outside, err)
	}
	wanted := filepath.Join(resolved, "artifact.md")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		batch, err := reader.Read(context.Background(), 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range batch.Events {
			if ev.Path == wanted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no event carried the absolute writable path %s", wanted)
}

func TestOpenJournalWithoutAJournalIsNotAvailable(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skipf("sandbox-exec is not installed: %v", err)
	}
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = runner.Close() }()

	if _, err := sandbox.OpenJournal(context.Background(), runner); !errdefs.IsNotAvailable(err) {
		t.Fatalf("OpenJournal = %v, want NotAvailable", err)
	}
}
