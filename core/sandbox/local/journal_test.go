package local_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/internal/journaltest"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
)

// buildHarness builds a local runner with a journal attached. A journal
// that cannot start is a test failure here: the suite only runs on
// platforms where one can.
func buildHarness(t *testing.T, enabled bool, opts sandbox.JournalOptions) journaltest.Harness {
	t.Helper()
	root := t.TempDir()
	var runner *local.Runner
	if enabled {
		runner = local.New(root, local.WithFileJournal(opts))
		if !runner.Capabilities().Journal.Enabled {
			t.Fatalf("the journal did not start on %s", runtime.GOOS)
		}
	} else {
		runner = local.New(root)
	}
	t.Cleanup(func() { _ = runner.Close() })

	return journaltest.Harness{
		Runner: runner,
		Root:   root,
		Exec: func(t *testing.T, script string) {
			t.Helper()
			result, err := sandbox.Exec(context.Background(), runner, "sh",
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

// TestJournalContract is the cross-backend contract, run against the
// local backend: real processes, real files, real kernel watch.
func TestJournalContract(t *testing.T) {
	if !journal.Available() {
		t.Skipf("no file-watch source on %s", runtime.GOOS)
	}
	journaltest.Run(t, buildHarness)
}

func TestRunnerWithoutAJournalReportsNoJournal(t *testing.T) {
	runner := local.New(t.TempDir())
	t.Cleanup(func() { _ = runner.Close() })

	caps := runner.Capabilities().Journal
	if caps.Enabled {
		t.Fatal("Capabilities claims a journal that was never attached")
	}
	if caps.WatchBudget != 0 {
		t.Fatalf("WatchBudget = %d, want 0 without a journal", caps.WatchBudget)
	}
	if _, err := sandbox.OpenJournal(context.Background(), runner); !errdefs.IsNotAvailable(err) {
		t.Fatalf("OpenJournal = %v, want NotAvailable", err)
	}
}

func TestUnavailableJournalIsReportedNotHidden(t *testing.T) {
	// With or without a watch source, the degraded path has to be
	// honest: a runner built with a journal request it could not honour
	// reports no journal and refuses to open one.
	root := t.TempDir()
	runner := local.New(root, local.WithFileJournal(sandbox.JournalOptions{}))
	t.Cleanup(func() { _ = runner.Close() })

	enabled := runner.Capabilities().Journal.Enabled
	if !enabled {
		if _, err := sandbox.OpenJournal(context.Background(), runner); err == nil {
			t.Fatal("OpenJournal succeeded although Capabilities reports no journal")
		} else if !errdefs.IsNotAvailable(err) && !errdefs.IsValidation(err) {
			t.Fatalf("OpenJournal = %v, want NotAvailable or Validation", err)
		}
	} else if !journal.Available() {
		t.Fatalf("journal is enabled on %s, but this build has no watch source", runtime.GOOS)
	}
}

// TestJournalCostsNothingWhenNotRequested is the "asserted by test, not
// by convention" half of the cost promise: a runner without a journal
// opens no descriptor and starts no goroutine.
func TestJournalCostsNothingWhenNotRequested(t *testing.T) {
	root := t.TempDir()

	fdsBefore := countFDs(t)
	goroutinesBefore := runtime.NumGoroutine()

	const runners = 8
	built := make([]*local.Runner, 0, runners)
	for i := 0; i < runners; i++ {
		built = append(built, local.New(root))
	}
	defer func() {
		for _, runner := range built {
			_ = runner.Close()
		}
	}()

	if after := countFDs(t); after != fdsBefore {
		t.Fatalf("open descriptors went from %d to %d for %d journal-less runners", fdsBefore, after, runners)
	}
	// The goroutine count is inherently racy (the runtime may retire or
	// start unrelated goroutines), so the check is directional: a
	// per-runner watch goroutine would be eight, not zero.
	deadline := time.Now().Add(time.Second)
	for {
		after := runtime.NumGoroutine()
		if after <= goroutinesBefore+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines went from %d to %d for %d journal-less runners",
				goroutinesBefore, after, runners)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestJournalSettingsBuild(t *testing.T) {
	root := t.TempDir()
	settings := func(body string) []byte {
		t.Helper()
		quoted, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		return []byte(`{"root": ` + string(quoted) + `, "journal": ` + body + `}`)
	}

	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := local.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, ok := reg.Lookup(local.ResourceKind, local.BackendName)
	if !ok {
		t.Fatal("factory not registered")
	}

	for _, tc := range []struct {
		name string
		body string
		want func(error) bool
	}{
		{
			name: "unknown op",
			body: `{"ops": ["chmod"]}`,
			want: errdefs.IsValidation,
		},
		{
			name: "negative retention",
			body: `{"retention": -1}`,
			want: errdefs.IsValidation,
		},
		{
			name: "exclude outside the root",
			body: `{"exclude": ["../elsewhere"]}`,
			want: errdefs.IsValidation,
		},
		{
			name: "unknown field",
			body: `{"retention": 10, "typo": true}`,
			want: errdefs.IsValidation,
		},
		{
			name: "valid settings",
			body: `{"exclude": ["node_modules"], "ops": ["create", "rename"], "retention": 64, "max_watch_set": 128}`,
			// Resolved successfully, then refused (or accepted) purely
			// on whether this platform has a watch source.
			want: func(err error) bool {
				if journal.Available() {
					return err == nil
				}
				return errdefs.IsNotAvailable(err)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := factory.New(ctx, resource.Input{Settings: settings(tc.body)})
			if tc.want(err) {
				return
			}
			if err != nil {
				t.Fatalf("build failed: %v", err)
			}
			runner, ok := value.(*local.Runner)
			if !ok {
				t.Fatalf("New returned %T, want *local.Runner", value)
			}
			t.Cleanup(func() { _ = runner.Close() })
			if !runner.Capabilities().Journal.Enabled {
				t.Fatal("a valid journal setting produced a runner without a journal")
			}
		})
	}
}

// TestJournalSeesWritesMadeByTheRunnerProcess drives the acceptance
// sequence with a real command and checks the reported paths and ops.
func TestJournalSeesWritesMadeByTheRunnerProcess(t *testing.T) {
	if !journal.Available() {
		t.Skipf("no file-watch source on %s", runtime.GOOS)
	}
	h := buildHarness(t, true, sandbox.JournalOptions{})
	j, err := sandbox.OpenJournal(context.Background(), h.Runner)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()

	if err := os.MkdirAll(filepath.Join(h.Root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.Exec(t, "printf hello > src/out.txt")
	h.Exec(t, "ln -s out.txt src/link.txt")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		batch, err := j.Read(context.Background(), 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, ev := range batch.Events {
			paths = append(paths, ev.Path)
		}
		if len(paths) >= 1 && contains(paths, "src/out.txt") && contains(paths, "src/link.txt") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("journal never reported the writes: %v", lastPaths(t, j))
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func lastPaths(t *testing.T, j sandbox.FileJournal) []string {
	t.Helper()
	batch, err := j.Read(context.Background(), 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, ev := range batch.Events {
		paths = append(paths, ev.Op.String()+" "+ev.Path)
	}
	return paths
}

// countFDs counts the process's open descriptors on platforms where
// that is cheap to observe.
func countFDs(t *testing.T) int {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skipf("cannot count descriptors: %v", err)
		}
		return len(entries)
	case "darwin":
		entries, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Skipf("cannot count descriptors: %v", err)
		}
		return len(entries)
	default:
		t.Skip("descriptor counting is not implemented for this platform")
		return 0
	}
}
