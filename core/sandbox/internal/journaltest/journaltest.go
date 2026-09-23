// Package journaltest holds the file-journal contract suite that every
// backend with a journal runs against itself.
//
// It lives in an internal package because it is a test fixture, not
// API: the suite drives a real runner (a local process, a bubblewrap
// namespace) with real shell commands, and asserts the promises
// core/sandbox makes — net state changes, replayable cursors, honest
// gaps — on whatever the backend actually reports.
package journaltest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Harness is one backend, ready to be driven.
type Harness struct {
	// Runner is the runner under test. It has a journal attached when
	// the builder was asked for one.
	Runner sandbox.Runner
	// Root is the directory the journal watches. Scripts run with it as
	// their working directory, so a relative path in a script is a path
	// inside the watched tree.
	Root string
	// Exec runs a shell script in the sandbox and fails the test if it
	// does not exit cleanly. It must run the command *inside* the
	// sandbox: the point of the suite is to check what a real sandboxed
	// write looks like from the host side.
	Exec func(t *testing.T, script string)
}

// Builder builds a runner for one subtest. enabled asks for a journal;
// opts are the journal options, ignored when enabled is false. Each
// call must return a runner over a fresh directory.
type Builder func(t *testing.T, enabled bool, opts sandbox.JournalOptions) Harness

// Run exercises the contract. Backends call it from their own test file
// so failures point at the backend that broke.
func Run(t *testing.T, build Builder) {
	t.Helper()

	t.Run("writes are net state changes", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{})
		j := open(t, h)

		h.Exec(t, "printf x > a.md")
		h.Exec(t, "mv a.md b.md")
		// Noise the journal must never report: a read and a chmod.
		h.Exec(t, "cat b.md > /dev/null; chmod 600 b.md")

		events := waitEvents(t, j, 2, 3*time.Second)
		events = settle(t, j, events, 400*time.Millisecond)
		wantEvents(t, events, "create a.md", "rename b.md <- a.md")
		for _, ev := range events {
			if strings.HasPrefix(ev.Path, "/") {
				t.Fatalf("path %q is absolute; root-relative is the contract", ev.Path)
			}
			if ev.Session != "" {
				t.Fatalf("event %v is attributed to %q; the backend cannot name the writer", ev.Op, ev.Session)
			}
		}
	})

	t.Run("a temporary file that never closes is silent", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{})
		j := open(t, h)

		// Open, unlink, close: the entry is gone before the writer is
		// done with it, so nothing was produced. This is the same
		// shape as a lock file or an install-in-progress temp file.
		h.Exec(t, "exec 3>tmp.lock; rm -f tmp.lock; exec 3>&-")

		events := settle(t, j, nil, 400*time.Millisecond)
		wantEvents(t, events)
	})

	t.Run("a write to an existing file is a write", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{})
		j := open(t, h)

		// The appearance is one event; the content change after it is
		// another. A write that follows a reported create must not be
		// swallowed by the create's de-duplication window — the bytes
		// changed after the consumer was told about the file.
		h.Exec(t, "printf x > f.md")
		events := waitEvents(t, j, 1, 3*time.Second)
		wantEvents(t, events, "create f.md")

		h.Exec(t, "printf yy >> f.md")
		events = waitEvents(t, j, 2, 3*time.Second)
		events = settle(t, j, events, 400*time.Millisecond)
		wantEvents(t, events, "create f.md", "write f.md")
	})

	t.Run("directories and nested writes", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{})
		j := open(t, h)

		h.Exec(t, "mkdir -p out/deep && printf x > out/deep/f.md")

		events := waitEvents(t, j, 3, 3*time.Second)
		events = settle(t, j, events, 400*time.Millisecond)
		wantEvents(t, events,
			"create out",
			"create out/deep",
			"create out/deep/f.md",
		)
		if !events[0].IsDir || !events[1].IsDir || events[2].IsDir {
			t.Fatalf("IsDir = %v/%v/%v, want directory creates then a file",
				events[0].IsDir, events[1].IsDir, events[2].IsDir)
		}
	})

	t.Run("filtered ops are not gaps", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{Ops: []sandbox.FileOp{sandbox.FileOpCreate}})
		j := open(t, h)

		h.Exec(t, "printf x > existing.md")
		waitEvents(t, j, 1, 3*time.Second)
		h.Exec(t, "printf yy >> existing.md")

		events := settle(t, j, nil, 400*time.Millisecond)
		wantEvents(t, events, "create existing.md")
		batch, err := j.Read(context.Background(), 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		if batch.Gap != nil {
			t.Fatalf("gap = %+v; a filtered change is not a missing one", batch.Gap)
		}
	})

	t.Run("a reader that falls behind is told", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{Retention: 2})
		j := open(t, h)

		h.Exec(t, "printf x > a.md; printf x > b.md; printf x > c.md; printf x > d.md; printf x > e.md")

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			batch, err := j.Read(context.Background(), 0, 64)
			if err != nil {
				t.Fatal(err)
			}
			if batch.Gap == nil {
				continue
			}
			if batch.Gap.Reason != sandbox.JournalGapRetention {
				t.Fatalf("reason = %v, want retention", batch.Gap.Reason)
			}
			if batch.Gap.FirstMissing != 1 {
				t.Fatalf("FirstMissing = %d, want 1", batch.Gap.FirstMissing)
			}
			if len(batch.Events) != 2 {
				t.Fatalf("events = %v, want the two retained", describe(batch.Events))
			}
			return
		}
		t.Fatal("the retention gap was never reported")
	})

	t.Run("replay is per reader", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{})
		j := open(t, h)

		h.Exec(t, "printf x > a.md")
		waitEvents(t, j, 1, 3*time.Second)

		second, err := sandbox.OpenJournal(context.Background(), h.Runner)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = second.Close() }()

		first, err := j.Read(context.Background(), 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		again, err := second.Read(context.Background(), 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Events) != 1 || len(again.Events) != 1 {
			t.Fatalf("first = %v, second = %v; every reader replays from its own cursor",
				describe(first.Events), describe(again.Events))
		}
		// The first reader's cursor advanced without touching the
		// second one's view.
		if first.NextSeq != again.NextSeq {
			t.Fatalf("cursors diverge: %d vs %d", first.NextSeq, again.NextSeq)
		}
	})

	t.Run("closing the runner keeps the residue readable", func(t *testing.T) {
		h := build(t, true, sandbox.JournalOptions{})
		j := open(t, h)

		h.Exec(t, "printf x > a.md")
		waitEvents(t, j, 1, 3*time.Second)

		if err := h.Runner.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		batch, err := j.Read(context.Background(), 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		if !batch.Closed {
			t.Fatal("Closed = false; a closed runner's journal is frozen")
		}
		if len(batch.Events) != 1 {
			t.Fatalf("events = %v, want the residue", describe(batch.Events))
		}
		if _, err := sandbox.OpenJournal(context.Background(), h.Runner); !errdefs.IsNotAvailable(err) {
			t.Fatalf("OpenJournal after Close = %v, want NotAvailable", err)
		}
	})

	t.Run("a runner without a journal says so", func(t *testing.T) {
		h := build(t, false, sandbox.JournalOptions{})

		caps := h.Runner.Capabilities().Journal
		if caps.Enabled {
			t.Fatal("Capabilities reports a journal that was never attached")
		}
		if _, err := sandbox.OpenJournal(context.Background(), h.Runner); !errdefs.IsNotAvailable(err) {
			t.Fatalf("OpenJournal = %v, want NotAvailable", err)
		}
		// ... and the runner still runs commands.
		h.Exec(t, "printf x > a.md")
	})
}

// open attaches a reader and closes it with the test.
func open(t *testing.T, h Harness) sandbox.FileJournal {
	t.Helper()
	j, err := sandbox.OpenJournal(context.Background(), h.Runner)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// waitEvents reads from the first seq until at least want events have
// been reported.
func waitEvents(t *testing.T, j sandbox.FileJournal, want int, timeout time.Duration) []sandbox.WriteEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []sandbox.WriteEvent
	for time.Now().Before(deadline) {
		batch, err := j.Read(context.Background(), 0, 512)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		last = batch.Events
		if len(last) >= want {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("journal reported %v, want %d events", describe(last), want)
	return nil
}

// settle keeps reading for d and returns everything the journal has
// seen, which is how "and nothing else" gets asserted.
func settle(t *testing.T, j sandbox.FileJournal, seen []sandbox.WriteEvent, d time.Duration) []sandbox.WriteEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	batch, err := j.Read(context.Background(), 0, 512)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Events) < len(seen) {
		t.Fatalf("events went backwards: %v after %v", describe(batch.Events), describe(seen))
	}
	return batch.Events
}

func describe(events []sandbox.WriteEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Op.String() + " " + ev.Path
		if ev.OldPath != "" {
			out[i] += " <- " + ev.OldPath
		}
	}
	return out
}

func wantEvents(t *testing.T, events []sandbox.WriteEvent, want ...string) {
	t.Helper()
	got := describe(events)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
