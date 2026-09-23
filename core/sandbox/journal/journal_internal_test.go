package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// fakeSource is an in-memory [Source]. It records subscriptions so a
// test can inject raw events for a specific directory, and it never
// touches the kernel — which is what makes the folding, retention and
// gap rules testable on platforms that have no watch source at all.
type fakeSource struct {
	mu     sync.Mutex
	next   Handle
	dirs   map[Handle]string
	byPath map[string]Handle
	events []rawEvent
	addErr error
	fail   error
	closed bool
	// gate holds Poll inside the source after it has taken the queued
	// events, which is how a test makes the Close/poll race
	// deterministic. entered signals that the batch is in hand.
	gate    chan struct{}
	entered chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		dirs:   make(map[Handle]string),
		byPath: make(map[string]Handle),
	}
}

func (s *fakeSource) Add(dir string) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addErr != nil {
		return 0, s.addErr
	}
	if h, ok := s.byPath[dir]; ok {
		return h, nil
	}
	s.next++
	h := s.next
	s.dirs[h] = dir
	s.byPath[dir] = h
	return h, nil
}

func (s *fakeSource) Remove(h Handle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir, ok := s.dirs[h]; ok {
		delete(s.dirs, h)
		delete(s.byPath, dir)
	}
	return nil
}

func (s *fakeSource) Poll(timeout time.Duration) ([]rawEvent, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errSourceClosed
	}
	if s.fail != nil {
		err := s.fail
		s.mu.Unlock()
		return nil, err
	}
	events := s.events
	s.events = nil
	entered, gate := s.entered, s.gate
	s.mu.Unlock()
	if gate != nil && len(events) > 0 {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
	}
	if len(events) == 0 {
		// Mimic the idle wakeup that drives the fold's deadline sweep,
		// short enough to keep tests quick.
		time.Sleep(min(timeout, time.Millisecond))
	}
	return events, nil
}

// setGate makes the next non-empty poll hold until release is called.
// The handshake is reported on the returned channel.
func (s *fakeSource) setGate() (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered = make(chan struct{}, 1)
	s.gate = make(chan struct{})
	entered, gate := s.entered, s.gate
	return entered, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.gate == gate {
			close(gate)
		}
	}
}

func (s *fakeSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeSource) inject(events ...rawEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
}

// handle waits for the engine to subscribe dir, which is how a test
// learns the handle a new directory got.
func (s *fakeSource) handle(t *testing.T, dir string) Handle {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		h, ok := s.byPath[dir]
		s.mu.Unlock()
		if ok {
			return h
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("source was never asked to watch %s", dir)
	return 0
}

func (s *fakeSource) watched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.byPath))
	for dir := range s.byPath {
		out = append(out, dir)
	}
	return out
}

// repath mirrors what the kernel does implicitly on a directory move:
// the watches follow the inode, so the source keeps the same handles
// under new paths. The engine is what renames its own bookkeeping.
func (s *fakeSource) repath(old, next string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for dir, h := range s.byPath {
		switch {
		case dir == old:
			delete(s.byPath, dir)
			s.byPath[next] = h
		case strings.HasPrefix(dir, old+separator):
			renamed := next + dir[len(old):]
			delete(s.byPath, dir)
			s.byPath[renamed] = h
		}
	}
}

// harness wires an engine to a fake source and a real temporary
// directory: the filesystem operations are real (so identity and size
// come from a real stat), the kernel events are injected.
type harness struct {
	t      *testing.T
	src    *fakeSource
	j      *Journal
	reader *Reader
	// root is the resolved temporary directory the engine watches.
	root string
}

func newHarness(t *testing.T, opts sandbox.JournalOptions, extraRoots ...string) *harness {
	t.Helper()
	return newHarnessWith(t, newFakeSource(), opts, extraRoots...)
}

func newHarnessWith(t *testing.T, src *fakeSource, opts sandbox.JournalOptions, extraRoots ...string) *harness {
	t.Helper()
	return newHarnessWithTTL(t, src, 20*time.Millisecond, opts, extraRoots...)
}

// newHarnessWithTTL is [newHarnessWith] with an explicit fold window.
// The tests that fold two descriptions of one appearance are about
// which of them wins, and the window is what decides.
func newHarnessWithTTL(t *testing.T, src *fakeSource, ttl time.Duration, opts sandbox.JournalOptions, extraRoots ...string) *harness {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	j, err := newWithSource(Config{
		Root:       root,
		ExtraRoots: extraRoots,
		Options:    opts,
		// The seams are set here rather than after construction: the
		// watch goroutine reads them, and a data race in the test
		// harness would be a lie about the engine.
		sweep: time.Millisecond,
		ttl:   ttl,
	}, src)
	if err != nil {
		t.Fatalf("newWithSource: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	reader, err := j.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return &harness{t: t, src: src, j: j, reader: reader, root: root}
}

func (h *harness) abs(rel string) string { return filepath.Join(h.root, filepath.FromSlash(rel)) }

func (h *harness) read(afterSeq int64, max int) sandbox.JournalBatch {
	h.t.Helper()
	batch, err := h.reader.Read(context.Background(), afterSeq, max)
	if err != nil {
		h.t.Fatalf("Read: %v", err)
	}
	return batch
}

// readOnce opens a throwaway reader: the journal's own handle is busy
// holding a cursor in the harness.
func readOnce(t *testing.T, j *Journal, afterSeq int64, max int) sandbox.JournalBatch {
	t.Helper()
	reader, err := j.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = reader.Close() }()
	batch, err := reader.Read(context.Background(), afterSeq, max)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return batch
}

// createFile performs a real create+write and injects the kernel events
// that go with it.
func (h *harness) createFile(rel string) {
	h.t.Helper()
	abs := h.abs(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	dir := h.src.handle(h.t, filepath.Dir(abs))
	name := filepath.Base(abs)
	h.src.inject(
		rawEvent{Op: rawCreate, Handle: dir, Name: name},
		rawEvent{Op: rawModify, Handle: dir, Name: name},
		rawEvent{Op: rawCloseWrite, Handle: dir, Name: name},
	)
}

// removeFile performs a real removal and injects the delete event.
func (h *harness) removeFile(rel string) {
	h.t.Helper()
	abs := h.abs(rel)
	if err := os.Remove(abs); err != nil {
		h.t.Fatal(err)
	}
	dir := h.src.handle(h.t, filepath.Dir(abs))
	h.src.inject(rawEvent{Op: rawDelete, Handle: dir, Name: filepath.Base(abs)})
}

// renameFile performs a real rename and injects the paired move events.
func (h *harness) renameFile(oldRel, newRel string) {
	h.t.Helper()
	oldAbs, newAbs := h.abs(oldRel), h.abs(newRel)
	if err := os.Rename(oldAbs, newAbs); err != nil {
		h.t.Fatal(err)
	}
	from := h.src.handle(h.t, filepath.Dir(oldAbs))
	to := h.src.handle(h.t, filepath.Dir(newAbs))
	h.src.inject(
		rawEvent{Op: rawMovedFrom, Handle: from, Name: filepath.Base(oldAbs), Cookie: 5},
		rawEvent{Op: rawMovedTo, Handle: to, Name: filepath.Base(newAbs), Cookie: 5},
	)
}

// createDir performs a real mkdir and injects the directory create.
func (h *harness) createDir(rel string) {
	h.t.Helper()
	abs := h.abs(rel)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		h.t.Fatal(err)
	}
	parent := h.src.handle(h.t, filepath.Dir(abs))
	h.src.inject(rawEvent{Op: rawCreate, Handle: parent, Name: filepath.Base(abs), IsDir: true})
	h.src.handle(h.t, abs)
}

// renameDir moves a watched directory inside the tree and injects the
// paired move events, which is what inotify reports: the parent sees
// the rename, and the watches inside follow the inode.
func (h *harness) renameDir(oldRel, newRel string) {
	h.t.Helper()
	oldAbs, newAbs := h.abs(oldRel), h.abs(newRel)
	if err := os.Rename(oldAbs, newAbs); err != nil {
		h.t.Fatal(err)
	}
	parent := h.src.handle(h.t, filepath.Dir(oldAbs))
	h.src.inject(
		rawEvent{Op: rawMovedFrom, Handle: parent, Name: filepath.Base(oldAbs), Cookie: 3, IsDir: true},
		rawEvent{Op: rawMovedTo, Handle: parent, Name: filepath.Base(newAbs), Cookie: 3, IsDir: true},
	)
	h.src.repath(oldAbs, newAbs)
}

// waitEvents polls the journal from seq 0 until it has at least want
// events, then returns them.
func (h *harness) waitEvents(want int) []sandbox.WriteEvent {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last []sandbox.WriteEvent
	for time.Now().Before(deadline) {
		last = h.read(0, 512).Events
		if len(last) >= want {
			return last
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("journal reported %d events, want %d: %v", len(last), want, eventPaths(last))
	return nil
}

// waitSettled waits until the journal has reported want events, then
// gives the engine a moment to report anything it should not.
func (h *harness) waitSettled(want int) []sandbox.WriteEvent {
	h.t.Helper()
	h.waitEvents(want)
	time.Sleep(50 * time.Millisecond)
	return h.read(0, 512).Events
}

// waitCursor polls the journal until its high watermark reaches want.
// With a retention window too small to hold everything, the cursor is
// the only way to know that every write has been folded.
func (h *harness) waitCursor(want int64) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.read(0, 512).NextSeq >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("journal cursor never reached %d", want)
}

func eventPaths(events []sandbox.WriteEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Op.String() + " " + ev.Path
		if ev.OldPath != "" {
			out[i] += " <- " + ev.OldPath
		}
	}
	return out
}

func wantEventPaths(t *testing.T, events []sandbox.WriteEvent, want ...string) {
	t.Helper()
	got := eventPaths(events)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestJournalReportsOneCreateForCreateAndWrite(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createFile("a.md")

	events := h.waitSettled(1)
	wantEventPaths(t, events, "create a.md")

	if got := events[0].Seq; got != 1 {
		t.Fatalf("Seq = %d, want 1", got)
	}
	if events[0].Size != 1 {
		t.Fatalf("Size = %d, want 1 (stat'ed at emission)", events[0].Size)
	}
	if events[0].Dev == 0 || events[0].Ino == 0 {
		t.Fatalf("identity = %d/%d, want a device and inode", events[0].Dev, events[0].Ino)
	}
	if events[0].Session != "" {
		t.Fatalf("Session = %q, want empty: inotify cannot name the writer", events[0].Session)
	}
	if events[0].At.IsZero() {
		t.Fatal("At is zero")
	}
}

func TestJournalCollapsesCreateThenRename(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createFile("a.md")
	h.waitEvents(1)
	h.renameFile("a.md", "b.md")

	events := h.waitSettled(2)
	wantEventPaths(t, events, "create a.md", "rename b.md <- a.md")
	if events[1].OldPath != "a.md" {
		t.Fatalf("OldPath = %q, want a.md", events[1].OldPath)
	}
}

func TestJournalRemovalRetiresAPath(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createFile("a.md")
	h.waitEvents(1)
	h.removeFile("a.md")

	events := h.waitSettled(2)
	wantEventPaths(t, events, "create a.md", "remove a.md")
	if events[1].Size != -1 {
		t.Fatalf("Size = %d, want -1 for a path that is gone", events[1].Size)
	}
}

func TestJournalTemporaryFileRoundTripIsSilent(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	// write tmp, then delete it before closing: nothing was produced.
	abs := h.abs("tmp.md")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := h.src.handle(t, h.root)
	h.src.inject(
		rawEvent{Op: rawCreate, Handle: dir, Name: "tmp.md"},
		rawEvent{Op: rawModify, Handle: dir, Name: "tmp.md"},
	)
	h.removeFile("tmp.md")
	if got := h.waitSettled(0); len(got) != 0 {
		t.Fatalf("events = %v, want none for a temporary file", eventPaths(got))
	}
}

func TestJournalRetentionGap(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{Retention: 2})
	for _, name := range []string{"a.md", "b.md", "c.md", "d.md", "e.md"} {
		h.createFile(name)
	}
	h.waitCursor(5)

	batch := h.read(0, 16)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapRetention {
		t.Fatalf("gap = %+v, want a retention gap", batch.Gap)
	}
	if batch.Gap.FirstMissing != 1 {
		t.Fatalf("FirstMissing = %d, want 1", batch.Gap.FirstMissing)
	}
	if len(batch.Events) != 2 {
		t.Fatalf("events = %v, want the two retained", eventPaths(batch.Events))
	}
	if batch.NextSeq != 5 {
		t.Fatalf("NextSeq = %d, want 5", batch.NextSeq)
	}
}

func TestJournalQueueOverflowIsReported(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createFile("a.md")
	h.waitEvents(1)
	h.src.inject(rawEvent{Op: rawOverflow})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		batch := h.read(1, 16)
		if batch.Gap != nil {
			if batch.Gap.Reason != sandbox.JournalGapOverflow {
				t.Fatalf("reason = %v, want overflow", batch.Gap.Reason)
			}
			if batch.Gap.FirstMissing != 2 {
				t.Fatalf("FirstMissing = %d, want 2 (the boundary)", batch.Gap.FirstMissing)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the overflow was never reported")
}

func TestJournalLostWatchIsReportedAndRetired(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createDir("sub")
	h.waitEvents(1)

	h.src.inject(rawEvent{Op: rawIgnored, Handle: h.src.handle(t, h.abs("sub"))})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		batch := h.read(0, 16)
		if batch.Gap != nil {
			if batch.Gap.Reason != sandbox.JournalGapWatchLost {
				t.Fatalf("reason = %v, want watch_lost", batch.Gap.Reason)
			}
			// The subtree is retired as well as declared unwatched:
			wantEventPaths(t, batch.Events, "create sub", "remove sub")
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the lost watch was never reported")
}

func TestJournalCapacityGapFromTheWalk(t *testing.T) {
	src := newFakeSource()
	src.addErr = errors.New("no watches left")
	h := newHarnessWith(t, src, sandbox.JournalOptions{})

	batch := h.read(0, 16)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapCapacity {
		t.Fatalf("gap = %+v, want a capacity gap", batch.Gap)
	}
}

func TestJournalWatchBudgetIsEnforced(t *testing.T) {
	// Two nested directories under the root, but room for one watch.
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	for _, dir := range []string{"a", "a/b"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := newFakeSource()
	j, err := newWithSource(Config{Root: root, Options: sandbox.JournalOptions{MaxWatchSet: 2}}, src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()

	if got := len(src.watched()); got != 2 {
		t.Fatalf("watched %d directories, want the budget of 2", got)
	}
	batch := readOnce(t, j, 0, 16)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapCapacity {
		t.Fatalf("gap = %+v, want a capacity gap for the unwatched directory", batch.Gap)
	}
}

func TestJournalExcludeSkipsSubtrees(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	for _, dir := range []string{"keep", "skip", "skip/deep"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := newFakeSource()
	j, err := newWithSource(Config{
		Root:    root,
		Options: sandbox.JournalOptions{Exclude: []string{"skip"}},
	}, src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()

	for _, dir := range src.watched() {
		if dir == filepath.Join(root, "skip") || strings.HasPrefix(dir, filepath.Join(root, "skip")+separator) {
			t.Fatalf("excluded directory was watched: %s", dir)
		}
	}
	if len(src.watched()) != 2 {
		t.Fatalf("watched = %v, want the root and keep", src.watched())
	}
}

func TestJournalExcludeOutsideTheRootIsRejected(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../elsewhere", "/etc", "."} {
		if _, err := newWithSource(Config{
			Root:    root,
			Options: sandbox.JournalOptions{Exclude: []string{bad}},
		}, newFakeSource()); err == nil {
			t.Fatalf("exclude %q was accepted", bad)
		} else if !errdefs.IsValidation(err) {
			t.Fatalf("exclude %q: error %v, want a validation error", bad, err)
		}
	}
}

// TestJournalSeededAppearanceIsNotReportedTwice covers the second half
// of the race the seed exists for. The readdir pass closes the window
// between "the directory appeared" and "its watch was installed" — but
// the kernel's own description of that same appearance is usually still
// queued when the seed is folded, and the two have to collapse into one
// event. A seed that is reported before those events are read reports
// the file twice.
func TestJournalSeededAppearanceIsNotReportedTwice(t *testing.T) {
	src := newFakeSource()
	// A window wide enough that the queued events are folded long before
	// the seed's deadline, whatever else the machine is doing.
	h := newHarnessWithTTL(t, src, 300*time.Millisecond, sandbox.JournalOptions{})

	dir := h.abs("out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "artifact.md")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The directory appears: the engine registers the watch and the
	// readdir pass finds the file that was written in between.
	root := src.handle(t, h.root)
	src.inject(rawEvent{Op: rawCreate, Handle: root, Name: "out", IsDir: true})
	handle := src.handle(t, dir)

	// The kernel's queued description of the same appearance.
	src.inject(
		rawEvent{Op: rawCreate, Handle: handle, Name: "artifact.md"},
		rawEvent{Op: rawModify, Handle: handle, Name: "artifact.md"},
		rawEvent{Op: rawCloseWrite, Handle: handle, Name: "artifact.md"},
	)

	// The completion edge publishes the seeded entry promptly, and the
	// window passing afterwards adds nothing.
	events := h.waitEvents(2)
	wantEventPaths(t, events, "create out", "create out/artifact.md")
	time.Sleep(400 * time.Millisecond)
	if got := h.read(0, 64).Events; len(got) != 2 {
		t.Fatalf("events = %v, want the appearance reported exactly once", eventPaths(got))
	}
}

func TestJournalDirectoryCreateIsWatchedAndSeeded(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	// The race the seed exists for: the file is written between the
	// mkdir and the inotify watch being installed, so the only kernel
	// event is the directory create.
	abs := h.abs("out")
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs, "artifact.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := h.src.handle(t, h.root)
	h.src.inject(rawEvent{Op: rawCreate, Handle: parent, Name: "out", IsDir: true})
	h.src.handle(t, abs)

	events := h.waitEvents(2)
	wantEventPaths(t, events, "create out", "create out/artifact.md")
	if !events[0].IsDir {
		t.Fatal("the directory create is not marked as a directory")
	}
}

func TestJournalDirectoryMoveRewritesPaths(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createDir("a")
	h.waitEvents(1)

	// Move the directory inside the tree: the watch follows the inode,
	// so only the engine's paths change.
	h.renameDir("a", "b")
	events := h.waitEvents(2)
	wantEventPaths(t, events, "create a", "rename b <- a")

	// Events inside the moved directory are reported at the new path.
	h.createFile("b/f.md")
	events = h.waitEvents(3)
	wantEventPaths(t, events, "create a", "rename b <- a", "create b/f.md")
}

func TestJournalExtraRootsCarryAbsolutePaths(t *testing.T) {
	outside := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(outside); err == nil {
		outside = resolved
	}
	h := newHarness(t, sandbox.JournalOptions{}, outside)

	h.createFile("a.md")
	h.waitEvents(1)

	abs := filepath.Join(outside, "outside.md")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	handle := h.src.handle(t, outside)
	h.src.inject(rawEvent{Op: rawCreate, Handle: handle, Name: "outside.md"})
	h.src.inject(rawEvent{Op: rawCloseWrite, Handle: handle, Name: "outside.md"})

	events := h.waitEvents(2)
	wantEventPaths(t, events, "create a.md", "create "+filepath.ToSlash(abs))
}

func TestJournalOpsFilterNeverProducesAGap(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{Ops: []sandbox.FileOp{sandbox.FileOpCreate}})
	// A write to an existing file is folded and then filtered: the
	// reader sees nothing, and above all no gap.
	abs := h.abs("a.md")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := h.src.handle(t, h.root)
	h.src.inject(
		rawEvent{Op: rawModify, Handle: dir, Name: "a.md"},
		rawEvent{Op: rawCloseWrite, Handle: dir, Name: "a.md"},
	)
	h.createFile("b.md")

	events := h.waitEvents(1)
	wantEventPaths(t, events, "create b.md")
	if batch := h.read(0, 16); batch.Gap != nil {
		t.Fatalf("gap = %+v, want none: a filtered event is not a missing event", batch.Gap)
	}
}

func TestJournalReadersHaveIndependentCursors(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createFile("a.md")
	h.createFile("b.md")
	h.waitEvents(2)

	first := h.read(0, 16)
	second := h.read(0, 1)
	if len(first.Events) != 2 {
		t.Fatalf("first reader saw %d events, want 2", len(first.Events))
	}
	if len(second.Events) != 1 || second.NextSeq != 1 {
		t.Fatalf("second reader = %+v, want one event and cursor 1", second)
	}
	// Replaying from an earlier cursor is allowed: the window is a
	// log, not a queue.
	again := h.read(0, 16)
	if len(again.Events) != 2 {
		t.Fatalf("replay = %v, want both events again", eventPaths(again.Events))
	}
}

func TestJournalCloseFreezesButKeepsResidue(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	h.createFile("a.md")
	h.waitEvents(1)

	reader, err := h.j.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.j.Close(); err != nil {
		t.Fatal(err)
	}
	batch, err := reader.Read(context.Background(), 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Closed {
		t.Fatal("Closed = false after the runner's journal was closed")
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events = %v, want the residue to stay readable", eventPaths(batch.Events))
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.j.Open(); err == nil {
		t.Fatal("Open succeeded after Close")
	} else if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Open error = %v, want NotAvailable", err)
	}
}

func TestJournalClosedReaderRefusesReads(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	reader, err := h.j.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(context.Background(), 0, 1); err == nil {
		t.Fatal("Read on a closed reader succeeded")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestJournalReadValidation(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	if _, err := h.reader.Read(context.Background(), -1, 1); !errdefs.IsValidation(err) {
		t.Fatalf("negative afterSeq: %v, want Validation", err)
	}
	if _, err := h.reader.Read(context.Background(), 0, 0); !errdefs.IsValidation(err) {
		t.Fatalf("max 0: %v, want Validation", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.reader.Read(ctx, 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: %v, want context.Canceled", err)
	}
}

func TestJournalPendingWriteIsReportedAtTheDeadline(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	// A writer that keeps the file open: modify, no close_write.
	abs := h.abs("log.txt")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := h.src.handle(t, h.root)
	h.src.inject(
		rawEvent{Op: rawCreate, Handle: dir, Name: "log.txt"},
		rawEvent{Op: rawModify, Handle: dir, Name: "log.txt"},
	)
	events := h.waitEvents(1)
	wantEventPaths(t, events, "create log.txt")
}

func TestJournalCloseFlushesWhatIsStillFolded(t *testing.T) {
	h := newHarness(t, sandbox.JournalOptions{})
	abs := h.abs("pending.md")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := h.src.handle(t, h.root)
	h.src.inject(rawEvent{Op: rawCreate, Handle: dir, Name: "pending.md"})
	time.Sleep(10 * time.Millisecond) // let the engine fold it

	reader, err := h.j.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.j.Close(); err != nil {
		t.Fatal(err)
	}
	batch, err := reader.Read(context.Background(), 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Closed {
		t.Fatal("Closed = false")
	}
	wantEventPaths(t, batch.Events, "create pending.md")
}

// A Close that lands while the watch goroutine already holds a batch is
// the one race that could drop events without a gap: the source handed
// them over, so nothing downstream would ever report them missing. The
// gate makes that interleaving deterministic.
func TestJournalCloseKeepsABatchThePollAlreadyDelivered(t *testing.T) {
	src := newFakeSource()
	h := newHarnessWith(t, src, sandbox.JournalOptions{})
	entered, release := src.setGate()

	if err := os.WriteFile(h.abs("a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := src.handle(t, h.root)
	src.inject(
		rawEvent{Op: rawCreate, Handle: dir, Name: "a.md"},
		rawEvent{Op: rawCloseWrite, Handle: dir, Name: "a.md"},
	)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the watch goroutine never picked the batch up")
	}

	closed := make(chan error, 1)
	go func() { closed <- h.j.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for !h.j.closing.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !h.j.closing.Load() {
		t.Fatal("Close never reached the shutdown flag")
	}
	release()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}

	batch := h.read(0, 16)
	if !batch.Closed {
		t.Fatal("Closed = false after Close")
	}
	wantEventPaths(t, batch.Events, "create a.md")
}

func TestJournalSourceFailureIsReportedAsABackendGap(t *testing.T) {
	src := newFakeSource()
	h := newHarnessWith(t, src, sandbox.JournalOptions{})

	src.mu.Lock()
	src.fail = errors.New("inotify broke")
	src.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		batch := h.read(0, 16)
		if batch.Gap != nil {
			if batch.Gap.Reason != sandbox.JournalGapBackend {
				t.Fatalf("reason = %v, want backend", batch.Gap.Reason)
			}
			if !batch.Closed {
				t.Fatal("the journal kept running after its source failed")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the source failure was never reported")
}

func TestJournalRejectsUnusableConfiguration(t *testing.T) {
	root := t.TempDir()
	cases := map[string]Config{
		"no root":      {Root: ""},
		"missing root": {Root: filepath.Join(root, "nope")},
		"huge retention": {
			Root:    root,
			Options: sandbox.JournalOptions{Retention: maxRetention + 1},
		},
		"unknown op": {
			Root:    root,
			Options: sandbox.JournalOptions{Ops: []sandbox.FileOp{sandbox.FileOpRemove + 1}},
		},
	}
	for name, cfg := range cases {
		if _, err := newWithSource(cfg, newFakeSource()); err == nil {
			t.Fatalf("%s: accepted", name)
		} else if !errdefs.IsValidation(err) {
			t.Fatalf("%s: %v, want a validation error", name, err)
		}
	}
}

func TestJournalCapabilitiesMatchThePlatform(t *testing.T) {
	caps := Capabilities()
	if Available() {
		if !caps.RenamePairing || !caps.FileIdentity {
			t.Fatalf("capabilities = %+v, want rename pairing and identity on a platform with a source", caps)
		}
		if caps.WatchBudget <= 0 {
			t.Fatalf("WatchBudget = %d, want a positive budget", caps.WatchBudget)
		}
	} else if caps.Enabled || caps.WatchBudget != 0 {
		t.Fatalf("capabilities = %+v, want the zero value without a source", caps)
	}
	if caps.Enabled {
		t.Fatal("Enabled is an instance fact; Capabilities must not claim it")
	}
}
