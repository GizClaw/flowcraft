//go:build linux

package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newSource starts a real inotify source and closes it with the test.
func newSource(t *testing.T) *linuxSource {
	t.Helper()
	src, err := openSource()
	if err != nil {
		t.Fatalf("openSource: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src.(*linuxSource)
}

func TestSourceReportsWriteLifecycle(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	handle, err := src.Add(dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	events := collect(t, src, 3, 2*time.Second)
	for _, ev := range events {
		if ev.Handle != handle {
			t.Fatalf("handle = %d, want %d", ev.Handle, handle)
		}
		if ev.Name != "a.md" {
			t.Fatalf("name = %q, want a.md", ev.Name)
		}
	}
	got := describeRaw(events)
	if !containsAll(got, "create a.md", "modify a.md", "close_write a.md") {
		t.Fatalf("events = %v, want the create/modify/close_write lifecycle", got)
	}
}

func TestSourceIgnoresReadsAndAttributeChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatal(err)
	}
	// The watch is installed after the write above on purpose: only the
	// chmod and the read below must reach the queue.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if events := settle(t, src, 100*time.Millisecond); len(events) != 0 {
		t.Fatalf("events = %v, want none: reads and chmods are not writes", describeRaw(events))
	}
}

func TestSourceMaskExcludesNoise(t *testing.T) {
	noise := uint32(unix.IN_ATTRIB | unix.IN_ACCESS | unix.IN_OPEN | unix.IN_CLOSE_NOWRITE)
	if inotifyMask&noise != 0 {
		t.Fatalf("inotifyMask includes %#x: attribute changes and reads are not writes", inotifyMask&noise)
	}
}

func TestSourceDirectoryCreateCarriesIsDir(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	events := collect(t, src, 1, 2*time.Second)
	if events[0].Op != rawCreate || !events[0].IsDir || events[0].Name != "sub" {
		t.Fatalf("event = %+v, want a directory create", events[0])
	}
}

func TestSourceRenameCarriesAPairingCookie(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	collect(t, src, 3, 2*time.Second)
	if err := os.Rename(filepath.Join(dir, "a.md"), filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}

	events := collect(t, src, 2, 2*time.Second)
	var from, to *rawEvent
	for i := range events {
		switch events[i].Op {
		case rawMovedFrom:
			from = &events[i]
		case rawMovedTo:
			to = &events[i]
		}
	}
	if from == nil || to == nil {
		t.Fatalf("events = %v, want a paired move", describeRaw(events))
	}
	if from.Name != "a.md" || to.Name != "b.md" {
		t.Fatalf("move = %s -> %s, want a.md -> b.md", from.Name, to.Name)
	}
	if from.Cookie == 0 || from.Cookie != to.Cookie {
		t.Fatalf("cookies = %d/%d, want the same non-zero cookie", from.Cookie, to.Cookie)
	}
}

func TestSourceRemovalDropsTheWatchQuietly(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	handle, err := src.Add(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Remove(handle); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The IN_IGNORED of our own removal is swallowed, and the watch is
	// gone, so nothing is reported at all.
	if events := settle(t, src, 100*time.Millisecond); len(events) != 0 {
		t.Fatalf("events = %v, want none after Remove", describeRaw(events))
	}
	if err := src.Remove(handle); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestSourceReportsMoveSelfOnTheMovedDirectory(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	parentHandle, err := src.Add(parent)
	if err != nil {
		t.Fatal(err)
	}
	subHandle, err := src.Add(filepath.Join(parent, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(parent, "sub"), filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}

	events := collect(t, src, 3, 2*time.Second)
	var sawFrom, sawTo, sawSelf bool
	for _, ev := range events {
		switch {
		case ev.Op == rawMovedFrom && ev.Handle == parentHandle && ev.Name == "sub":
			sawFrom = true
		case ev.Op == rawMovedTo && ev.Handle == parentHandle && ev.Name == "moved":
			sawTo = true
		case ev.Op == rawMoveSelf && ev.Handle == subHandle:
			sawSelf = true
		}
	}
	if !sawFrom || !sawTo || !sawSelf {
		t.Fatalf("events = %v, want the parent's pair plus move_self on the moved directory", describeRaw(events))
	}
}

func TestSourceIsNotRecursive(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// inotify watches one directory: the engine's walk and its seeding
	// of new directories are what make the journal cover a tree.
	if events := settle(t, src, 100*time.Millisecond); len(events) != 0 {
		t.Fatalf("events = %v, want none without a watch on the subdirectory", describeRaw(events))
	}
}

func TestSourceCloseIsSafeAgainstAnInFlightPoll(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Poll(10 * time.Millisecond); err != nil {
		t.Fatalf("Poll before Close: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := src.Poll(200 * time.Millisecond)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)

	// Close waits for the in-flight poll instead of yanking the
	// descriptor out from under it: that is what makes descriptor reuse
	// impossible, and a poll racing with Close either finishes normally
	// or sees the closed source. What bounds the wait is the engine's
	// sweep interval, which is the poll timeout it passes.
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil && err != errSourceClosed {
			t.Fatalf("in-flight Poll = %v, want nil or errSourceClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the in-flight poll never returned")
	}
	if _, err := src.Poll(time.Millisecond); err != errSourceClosed {
		t.Fatalf("Poll after Close = %v, want errSourceClosed", err)
	}
	if _, err := unix.Read(src.fd, make([]byte, 16)); err != unix.EBADF {
		t.Fatalf("read on the closed descriptor = %v, want EBADF", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestDefaultBudgetIsSane(t *testing.T) {
	budget := defaultBudget()
	if budget < 4096 || budget > 65536 {
		t.Fatalf("defaultBudget = %d, want it inside [4096, 65536]", budget)
	}
}

// TestLinuxSourceDecodesTheRecordsTheKernelEmitsRarely drives the two
// record classes that need no filesystem to happen through the same
// decode path Poll uses: a queue overflow and the IN_IGNORED of a
// watch the kernel dropped.
func TestLinuxSourceDecodesTheRecordsTheKernelEmitsRarely(t *testing.T) {
	src := &linuxSource{
		byWd:    map[int32]Handle{7: 3},
		wdOf:    map[Handle]int32{3: 7},
		pending: map[int32]bool{},
	}

	// An overflow record carries no watch descriptor.
	events := src.decodeLocked(-1, unix.IN_Q_OVERFLOW, 0, "", nil)
	if len(events) != 1 || events[0].Op != rawOverflow {
		t.Fatalf("overflow decoded to %v, want one rawOverflow", describeRaw(events))
	}

	// A watch the kernel dropped without being asked is reported...
	events = src.decodeLocked(7, unix.IN_IGNORED, 0, "", nil)
	if len(events) != 1 || events[0].Op != rawIgnored || events[0].Handle != 3 {
		t.Fatalf("ignored decoded to %v, want rawIgnored for handle 3", describeRaw(events))
	}

	// ... while one the engine asked for is consumed and its
	// expectation cleared.
	src.pending[7] = true
	events = src.decodeLocked(7, unix.IN_IGNORED, 0, "", nil)
	if len(events) != 0 {
		t.Fatalf("engine-requested removal decoded to %v, want nothing", describeRaw(events))
	}
	if src.pending[7] {
		t.Fatal("the engine-requested removal left its expectation behind")
	}
}

// TestLinuxSourceDroppedWatchLeavesNoPendingIgnored is the regression
// test for descriptor reuse: once the kernel has dropped a watch on its
// own, the engine's Remove has nothing left to remove and must not
// record an expectation for a second IN_IGNORED. That expectation would
// outlive the watch and later swallow the IN_IGNORED of a watch
// reusing the same descriptor — a silently unwatched subtree.
func TestLinuxSourceDroppedWatchLeavesNoPendingIgnored(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	h, err := src.Add(dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	wd := src.wdOf[h]

	// The kernel drops the watch itself when the directory goes away:
	// one IN_DELETE_SELF and one IN_IGNORED reach the engine.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	events := collect(t, src, 2, 2*time.Second)
	sawIgnored := false
	for _, ev := range events {
		if ev.Op == rawIgnored {
			sawIgnored = true
		}
	}
	if !sawIgnored {
		t.Fatalf("events = %v, want the kernel-dropped watch reported", describeRaw(events))
	}

	if err := src.Remove(h); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if src.pending[wd] {
		t.Fatal("Remove recorded a pending IN_IGNORED the kernel had already sent; " +
			"a watch reusing the descriptor would lose its own IN_IGNORED silently")
	}
}
