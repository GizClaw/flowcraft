//go:build windows

package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// newSource opens a real ReadDirectoryChangesW source and closes it with
// the test.
func newSource(t *testing.T) *winSource {
	t.Helper()
	src, err := openSource()
	if err != nil {
		t.Fatalf("openSource: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src.(*winSource)
}

// opsNamed lists the operations reported for one name, so a test can
// talk about what happened to a path and not about the order the
// platform chose to say it in.
func opsNamed(events []rawEvent, name string) []string {
	var out []string
	for _, ev := range events {
		if ev.Name == name {
			out = append(out, ev.Op.String())
		}
	}
	return out
}

// TestWindowsSourceReportsWriteLifecycle pins the shape of a plain
// write. Unlike inotify there is no close edge to wait for here: the
// platform reports the entry appearing and its content changing, and
// the engine's fold is what turns those into one event.
func TestWindowsSourceReportsWriteLifecycle(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	handle, err := src.Add(dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	events := settle(t, src, 500*time.Millisecond)
	for _, ev := range events {
		if ev.Handle != handle {
			t.Fatalf("handle = %d, want %d (%v)", ev.Handle, handle, describeRaw(events))
		}
	}
	ops := opsNamed(events, "a.md")
	if !containsAll(ops, "create") {
		t.Fatalf("events = %v, want the appearance of a.md", describeRaw(events))
	}
	for _, op := range ops {
		if op != "create" && op != "modify" {
			t.Fatalf("ops for a.md = %v, want only the appearance and its content change", ops)
		}
	}
}

// TestWindowsSourceSeesContentChange is the write half on its own: a
// file that existed before the watch was installed must be reported as
// a content change, not as an appearance.
//
// The flush is part of the test on purpose. This platform records a
// size or last-write change when the write reaches the filesystem, so a
// writer that never flushes can have its write reported late — a
// property of the platform, stated in the package doc, not something
// the source can fix.
func TestWindowsSourceSeesContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	events := collect(t, src, 1, 3*time.Second)
	if ops := opsNamed(events, "a.md"); !containsAll(ops, "modify") {
		t.Fatalf("events = %v, want a content change for a.md", describeRaw(events))
	}
}

// TestWindowsSourceIgnoresReadsAndAttributes pins the half of the
// filter that is a decision rather than a platform limit: reads and
// attribute changes are not writes, and the kernel is never asked about
// them.
func TestWindowsSourceIgnoresReadsAndAttributes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The watch is installed after the write above on purpose: only the
	// chmod and the read below must reach the queue.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if events := settle(t, src, 300*time.Millisecond); len(events) != 0 {
		t.Fatalf("events = %v, want none: reads and chmods are not writes", describeRaw(events))
	}
}

func TestWindowsSourceMaskExcludesNoise(t *testing.T) {
	noise := uint32(windows.FILE_NOTIFY_CHANGE_ATTRIBUTES |
		windows.FILE_NOTIFY_CHANGE_LAST_ACCESS |
		windows.FILE_NOTIFY_CHANGE_CREATION |
		windows.FILE_NOTIFY_CHANGE_SECURITY)
	if notifyFilter&noise != 0 {
		t.Fatalf("notifyFilter includes %#x: attribute changes, accesses and security descriptors are not writes",
			notifyFilter&noise)
	}
}

// TestWindowsSourceDirectoryCreateCarriesIsDir pins the one thing this
// platform does not say: its records name an entry and stop, so whether
// the name is a directory is answered by asking the filesystem. A
// missing answer here is a subtree the engine never watches.
func TestWindowsSourceDirectoryCreateCarriesIsDir(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	events := collect(t, src, 1, 2*time.Second)
	if ops := opsNamed(events, "sub"); len(ops) == 0 {
		t.Fatalf("events = %v, want the appearance of sub", describeRaw(events))
	}
	for _, ev := range events {
		if ev.Name == "sub" && ev.Op == rawCreate && !ev.IsDir {
			t.Fatalf("event = %+v, want a directory create", ev)
		}
	}
}

// TestWindowsSourceRenameWithinADirectoryIsOneEvent pins the pairing
// the platform does offer: a rename inside one directory arrives as two
// adjacent records in one completion, and the source joins them into
// one move with one cookie.
func TestWindowsSourceRenameWithinADirectoryIsOneEvent(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	handle, err := src.Add(sub)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.Rename(filepath.Join(sub, "x.md"), filepath.Join(sub, "y.md")); err != nil {
		t.Fatal(err)
	}

	events := collect(t, src, 2, 3*time.Second)
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
	if from.Handle != handle || to.Handle != handle {
		t.Fatalf("handles = %d/%d, want both on the watched directory %d", from.Handle, to.Handle, handle)
	}
	if from.Name != "x.md" || to.Name != "y.md" {
		t.Fatalf("move = %s -> %s, want x.md -> y.md", from.Name, to.Name)
	}
	if from.Cookie == 0 || from.Cookie != to.Cookie {
		t.Fatalf("cookies = %d/%d, want the same non-zero cookie", from.Cookie, to.Cookie)
	}
}

// TestWindowsSourceDoesNotGuessAMoveAcrossDirectories is the honest
// half of the same capability. One directory's handle reports the old
// name and the other's reports the new one, and this platform gives no
// cookie to join them with: the two halves are reported as the removal
// and the appearance they also are, never as a rename that would name a
// destination nobody proved.
//
// The test also pins what the second half of that costs: neither
// directory's timestamp update is reported. The entry set of a watched
// directory changing is what that directory's own watch reports; a
// "write" of the directory would be a statement about content that does
// not exist.
func TestWindowsSourceDoesNotGuessAMoveAcrossDirectories(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	rootHandle, err := src.Add(root)
	if err != nil {
		t.Fatalf("Add root: %v", err)
	}
	handleA, err := src.Add(a)
	if err != nil {
		t.Fatalf("Add a: %v", err)
	}
	handleB, err := src.Add(b)
	if err != nil {
		t.Fatalf("Add b: %v", err)
	}
	if err := os.Rename(filepath.Join(a, "x.md"), filepath.Join(b, "x.md")); err != nil {
		t.Fatal(err)
	}

	events := collect(t, src, 2, 3*time.Second)
	// Whatever else arrives, the old directory must say the entry left
	// and the new one must say it arrived.
	var departure, arrival *rawEvent
	for i := range events {
		ev := events[i]
		if ev.Handle == handleA && ev.Name == "x.md" && (ev.Op == rawMovedFrom || ev.Op == rawDelete) {
			departure = &events[i]
		}
		if ev.Handle == handleB && ev.Name == "x.md" && (ev.Op == rawMovedTo || ev.Op == rawCreate) {
			arrival = &events[i]
		}
	}
	if departure == nil || arrival == nil {
		t.Fatalf("events = %v, want a departure from a and an arrival in b", describeRaw(events))
	}
	if arrival.Op == rawMovedTo && arrival.Cookie != 0 && arrival.Cookie == departure.Cookie {
		t.Fatalf("cookies = %d/%d: two records in different directories were paired as one move",
			departure.Cookie, arrival.Cookie)
	}
	for _, ev := range events {
		if ev.Handle == rootHandle {
			t.Fatalf("events = %v: a watched directory's own timestamp change was reported as %s %s",
				describeRaw(events), ev.Op, ev.Name)
		}
	}
}

// TestWindowsSourceReportsALostBufferWithoutLosingTheWatch covers the
// overflow path: a burst that does not fit the buffer is reported as
// what it is — a gap of unknown size — and the watch keeps reporting
// afterwards, which is the difference between a reported loss and a
// silent one.
func TestWindowsSourceReportsALostBufferWithoutLosingTheWatch(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Written without polling first, so the kernel's buffer has to hold
	// the whole burst: 500 files are more change records than 16 KiB
	// can describe.
	const burst = 500
	for i := range burst {
		name := filepath.Join(dir, fmt.Sprintf("burst%05d.tmp", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	names := make(map[string]bool)
	lost := false
	quiet := 0
	for time.Now().Before(deadline) {
		events, err := src.Poll(20 * time.Millisecond)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if len(events) == 0 {
			// A few quiet polls in a row end the burst; the watch itself
			// is only done when a later write is still reported.
			if quiet++; quiet >= 3 {
				break
			}
			continue
		}
		quiet = 0
		for _, ev := range events {
			if ev.Op == rawOverflow {
				lost = true
				continue
			}
			names[ev.Name] = true
		}
	}
	if !lost {
		// Nothing was lost, so every file has to have been named: a
		// missing one here is a silent loss, which is the one thing
		// this package must never produce.
		var missing []string
		for i := range burst {
			name := fmt.Sprintf("burst%05d.tmp", i)
			if !names[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("%d of %d files were reported by nothing and no gap was reported either, starting with %q",
				len(missing), burst, missing[0])
		}
	}

	// The watch is still live after the burst, gap or not.
	if err := os.WriteFile(filepath.Join(dir, "after.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	events := collect(t, src, 1, 3*time.Second)
	if ops := opsNamed(events, "after.tmp"); !containsAll(ops, "create") {
		t.Fatalf("events = %v, want the write after a burst still reported", describeRaw(events))
	}
}

// TestWindowsSourceRootDeletionIsReported is the one removal no parent
// watch can report: the watched root itself. The pending read is how
// the platform says it — as a failure or as an empty completion — and
// either way the source must report the directory gone rather than an
// empty stream.
func TestWindowsSourceRootDeletionIsReported(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src := newSource(t)
	handle, err := src.Add(root)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}

	events := collect(t, src, 1, 5*time.Second)
	for _, ev := range events {
		if ev.Op == rawDeleteSelf && ev.Handle == handle {
			return
		}
	}
	t.Fatalf("events = %v, want the watch root reported gone", describeRaw(events))
}

func TestWindowsSourceRemovalDropsTheWatchQuietly(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	handle, err := src.Add(dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := src.Remove(handle); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if events := settle(t, src, 300*time.Millisecond); len(events) != 0 {
		t.Fatalf("events = %v, want none: the watch was removed", describeRaw(events))
	}
	// Removing an unknown handle is not an error, and neither is asking
	// twice.
	if err := src.Remove(handle); err != nil {
		t.Fatalf("second Remove = %v, want nil", err)
	}
}

func TestWindowsSourceCloseIsSafeAgainstAnInFlightPoll(t *testing.T) {
	dir := t.TempDir()
	src := newSource(t)
	if _, err := src.Add(dir); err != nil {
		t.Fatalf("Add: %v", err)
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

	// Close waits for the poll in flight instead of closing the port
	// under it: what bounds the wait is the timeout that poll was called
	// with, which is the engine's sweep interval.
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
	if err := src.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// TestWindowsSourceCloseDrainsEveryCompletion covers the other half of
// Close: every cancelled read's completion is drained, so no buffer is
// parked waiting for a completion that already came.
func TestWindowsSourceCloseDrainsEveryCompletion(t *testing.T) {
	src, err := openSource()
	if err != nil {
		t.Fatalf("openSource: %v", err)
	}
	win := src.(*winSource)
	for i := range 4 {
		dir := t.TempDir()
		if _, err := win.Add(dir); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := win.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	unreclaimedMu.Lock()
	parked := len(unreclaimed)
	unreclaimedMu.Unlock()
	if parked != 0 {
		t.Fatalf("parked %d watches after Close, want none: a cancelled read's completion always arrives", parked)
	}
}

func TestDefaultBudgetIsSane(t *testing.T) {
	budget := defaultBudget()
	if budget <= 0 || budget > 65536 {
		t.Fatalf("defaultBudget = %d, want a positive budget a runner can afford", budget)
	}
}
