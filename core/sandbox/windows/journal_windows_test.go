package windows

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/internal/journaltest"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// TestJournalContract is the cross-backend contract, run against the
// windows backend: real processes, real files, the host's
// ReadDirectoryChangesW.
func TestJournalContract(t *testing.T) {
	if !journal.Available() {
		t.Skipf("no file-watch source on %s", runtime.GOOS)
	}
	journaltest.Run(t, buildJournalHarness)
}

func buildJournalHarness(t *testing.T, enabled bool, opts sandbox.JournalOptions) journaltest.Harness {
	t.Helper()
	root := t.TempDir()

	var runnerOpts []Option
	if enabled {
		runnerOpts = append(runnerOpts, WithFileJournal(opts))
	}
	runner, err := New(root, runnerOpts...)
	if err != nil {
		t.Fatalf("windows New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	if got, want := runner.Capabilities().Journal.Enabled, enabled; got != want {
		t.Fatalf("Journal.Enabled = %v, want %v", got, want)
	}

	shell := journaltest.Cmd{}
	return journaltest.Harness{
		Runner: runner,
		Root:   root,
		Shell:  shell,
		Exec: func(t *testing.T, script string) {
			t.Helper()
			argv := shell.Argv(script)
			result, err := sandbox.Exec(context.Background(), runner, argv[0],
				argv[1:], sandbox.ExecOptions{WorkDir: root})
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
// the watch set: a path granted with WithWritablePaths is not under the
// root, so its events carry absolute paths. Without write confinement
// there is no write boundary to enforce, but the journal still has to
// cover the paths a deployment declared writable.
func TestJournalCoversWritablePathsOutsideTheRoot(t *testing.T) {
	if !journal.Available() {
		t.Skipf("no file-watch source on %s", runtime.GOOS)
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
	// out-of-root workdir), so the write reaches the writable path from
	// there: one ".." step, the two temp directories being siblings.
	//
	// It is spelled relatively because a cmd script cannot carry a
	// quoted absolute path: argv is joined into one command line with
	// the C runtime's quoting and cmd, reading that line its own way,
	// takes the backslash of an escaped quote literally — `echo
	// x>"C:\..."` arrives as `echo x>\"C:\..."` and fails with "The
	// filename, directory name, or volume label syntax is incorrect".
	// A relative target needs no quoting at all, whatever the host's
	// temp directory happens to be called.
	rel, err := filepath.Rel(root, filepath.Join(outside, "artifact.md"))
	if err != nil {
		t.Fatalf("Rel(%q, %q): %v", root, outside, err)
	}
	script := "echo x>" + rel
	result, err := sandbox.Exec(context.Background(), runner, "cmd",
		[]string{"/c", script}, sandbox.ExecOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exec: exit %d: %s", result.ExitCode, result.Stderr)
	}

	// The journal resolves the writable path before watching it (the
	// runner resolves its root the same way), so the event carries the
	// resolved path — and the platform's separators, not the source's.
	resolved, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", outside, err)
	}
	wanted := filepath.ToSlash(filepath.Join(resolved, "artifact.md"))
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

// TestOpenJournalWithoutAJournalIsNotAvailable keeps the degraded path
// honest: a runner built without a journal reports no journal and
// refuses to open one, instead of handing out an empty stream.
func TestOpenJournalWithoutAJournalIsNotAvailable(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = runner.Close() }()

	if caps := runner.Capabilities().Journal; caps.Enabled {
		t.Fatal("Capabilities claims a journal that was never attached")
	}
	if _, err := sandbox.OpenJournal(context.Background(), runner); !errdefs.IsNotAvailable(err) {
		t.Fatalf("OpenJournal = %v, want NotAvailable", err)
	}
}
