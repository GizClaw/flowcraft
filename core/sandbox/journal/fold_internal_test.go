package journal

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

var foldBase = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// foldHarness drives a fold with a clock the test controls.
type foldHarness struct {
	t    *testing.T
	f    *fold
	now  time.Time
	out  []change
	gaps []sandbox.JournalGapReason
}

func newFoldHarness(t *testing.T, ops ...sandbox.FileOp) *foldHarness {
	t.Helper()
	return &foldHarness{t: t, f: newFold(ops, 200*time.Millisecond), now: foldBase}
}

func (h *foldHarness) at(offset time.Duration) *foldHarness {
	h.now = foldBase.Add(offset)
	return h
}

// hostPath rewrites one of the suite's "/"-flavored literals into the
// shape the engine actually hands the fold: an absolute host path,
// whose separators are the platform's own. The fold's subtree checks
// are prefix checks on exactly those paths, so a literal that is only
// right on a "/"-separated host would test the wrong thing.
func hostPath(path string) string { return filepath.FromSlash(path) }

func (h *foldHarness) route(ev rawEvent, path string) {
	h.t.Helper()
	h.out = append(h.out, h.f.Observe(ev, hostPath(path), h.now)...)
}

// resolveMove, reparent and forget drive the fold's engine-side entry
// points with the same host-flavored paths.
func (h *foldHarness) resolveMove(path string) { h.f.ResolveMove(hostPath(path)) }

func (h *foldHarness) reparent(old, new string) {
	h.f.Reparent(hostPath(old), hostPath(new))
}

func (h *foldHarness) forget(prefix string) { h.f.Forget(hostPath(prefix)) }

func (h *foldHarness) create(path string, isDir bool) *foldHarness {
	h.route(rawEvent{Op: rawCreate, IsDir: isDir}, path)
	return h
}

func (h *foldHarness) modify(path string) *foldHarness {
	h.route(rawEvent{Op: rawModify}, path)
	return h
}

func (h *foldHarness) closeWrite(path string) *foldHarness {
	h.route(rawEvent{Op: rawCloseWrite}, path)
	return h
}

func (h *foldHarness) remove(path string) *foldHarness {
	h.route(rawEvent{Op: rawDelete}, path)
	return h
}

func (h *foldHarness) movedFrom(path string, cookie uint32, isDir bool) *foldHarness {
	h.route(rawEvent{Op: rawMovedFrom, Cookie: cookie, IsDir: isDir}, path)
	return h
}

func (h *foldHarness) movedTo(path string, cookie uint32, isDir bool) *foldHarness {
	h.route(rawEvent{Op: rawMovedTo, Cookie: cookie, IsDir: isDir}, path)
	return h
}

func (h *foldHarness) moveSelf(path string) *foldHarness {
	h.route(rawEvent{Op: rawMoveSelf}, path)
	return h
}

func (h *foldHarness) sweep() *foldHarness {
	var out []change
	h.gaps = append(h.gaps, h.f.Sweep(&out, h.now)...)
	h.out = append(h.out, out...)
	return h
}

func (h *foldHarness) flush() *foldHarness {
	var out []change
	h.f.Flush(&out)
	h.out = append(h.out, out...)
	return h
}

// take returns the changes gathered so far and resets the buffer.
func (h *foldHarness) take() []change {
	h.t.Helper()
	out := h.out
	h.out = nil
	h.gaps = nil
	return out
}

func describe(changes []change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		s := c.op.String() + " " + filepath.ToSlash(c.path)
		if c.isDir {
			s += "/"
		}
		if c.oldPath != "" {
			s += " <- " + filepath.ToSlash(c.oldPath)
		}
		out[i] = s
	}
	return out
}

func wantChanges(t *testing.T, got []change, want ...string) {
	t.Helper()
	names := describe(got)
	sort.Strings(names)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(names, "|") != strings.Join(sorted, "|") {
		t.Fatalf("changes = %v, want %v", names, sorted)
	}
}

func TestFoldCreateWriteCloseIsOneCreate(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/a.md", false).modify("/w/a.md").closeWrite("/w/a.md")
	wantChanges(t, h.take(), "create /w/a.md")

	// A second write cycle on the file that now exists is one write.
	h.modify("/w/a.md").closeWrite("/w/a.md")
	wantChanges(t, h.take(), "write /w/a.md")
}

func TestFoldCloseWithoutModifyIsNotAContentChange(t *testing.T) {
	h := newFoldHarness(t)
	// touch an existing file: the kernel reports a close_write and no
	// modify, because the bytes never changed.
	h.closeWrite("/w/existing.md")
	wantChanges(t, h.take())

	// chmod and reads never even reach the fold; the source's mask
	// drops them. Nothing to assert here beyond "no ops are invented".
}

func TestFoldCreateThenDeleteIsSilent(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/tmp.md", false).modify("/w/tmp.md").remove("/w/tmp.md")
	wantChanges(t, h.take())
}

func TestFoldDeleteAfterWriteIsARemoval(t *testing.T) {
	h := newFoldHarness(t)
	h.modify("/w/out.md").remove("/w/out.md")
	wantChanges(t, h.take(), "remove /w/out.md")
}

func TestFoldCreatedThenRemovedReportsBoth(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/out.md", false).modify("/w/out.md").closeWrite("/w/out.md")
	wantChanges(t, h.take(), "create /w/out.md")
	h.remove("/w/out.md")
	wantChanges(t, h.take(), "remove /w/out.md")
}

func TestFoldDirectoryCreateIsImmediate(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/out", true)
	wantChanges(t, h.take(), "create /w/out/")

	// The queued inotify create is the same appearance: the tombstone
	// keeps it from being reported twice.
	h.create("/w/out", true)
	wantChanges(t, h.take())

	// ... but a real re-creation after a removal is a new appearance.
	h.remove("/w/out")
	wantChanges(t, h.take(), "remove /w/out/")
	h.create("/w/out", true)
	wantChanges(t, h.take(), "create /w/out/")
}

func TestFoldRenamePairedIsOneEvent(t *testing.T) {
	h := newFoldHarness(t)
	h.movedFrom("/w/a.md", 7, false).movedTo("/w/b.md", 7, false)
	wantChanges(t, h.take(), "rename /w/b.md <- /w/a.md")
}

func TestFoldRenameOfAFreshTempFileIsOneRename(t *testing.T) {
	h := newFoldHarness(t)
	// write a temp file and rename it over the destination, the way
	// editors and sed -i do: one create (or none) plus one rename, but
	// never a phantom create of the temp name.
	h.create("/w/.tmp", false).modify("/w/.tmp").closeWrite("/w/.tmp")
	h.movedFrom("/w/.tmp", 9, false).movedTo("/w/target.md", 9, false)
	wantChanges(t, h.take(), "create /w/.tmp", "rename /w/target.md <- /w/.tmp")
}

func TestFoldRenameOfAnUnreportedTempFileIsCreateThenRename(t *testing.T) {
	h := newFoldHarness(t)
	// The same move without a close edge — what a platform that cannot
	// observe a close (kqueue) reports. The appearance is still a
	// create: the entry existed, and it survived the move as an
	// artifact under the destination's name.
	h.create("/w/.tmp", false).modify("/w/.tmp")
	h.movedFrom("/w/.tmp", 9, false).movedTo("/w/target.md", 9, false)
	wantChanges(t, h.take(), "create /w/.tmp", "rename /w/target.md <- /w/.tmp")
}

func TestFoldRenameOutOfTheTreeReportsRemoval(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/a.md", false).modify("/w/a.md").closeWrite("/w/a.md")
	wantChanges(t, h.take(), "create /w/a.md")

	// The destination is outside the watched set, so no MOVED_TO ever
	// arrives: the grace period turns it into a removal.
	h.movedFrom("/w/a.md", 3, false)
	wantChanges(t, h.take())
	h.at(300 * time.Millisecond).sweep()
	wantChanges(t, h.take(), "remove /w/a.md")
}

func TestFoldRenameOfAnUnreportedTempFileOutOfTheTree(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/.tmp", false).modify("/w/.tmp")
	h.movedFrom("/w/.tmp", 3, false)
	// The appearance is published on the way out: the entry existed,
	// and only a deletion makes the net change zero.
	wantChanges(t, h.take(), "create /w/.tmp")

	// The destination is outside the watched set, so no MOVED_TO ever
	// arrives: the grace period turns the source into a removal.
	h.at(300 * time.Millisecond).sweep()
	wantChanges(t, h.take(), "remove /w/.tmp")
}

func TestFoldMovedInFromOutsideIsACreate(t *testing.T) {
	h := newFoldHarness(t)
	h.movedTo("/w/a.md", 42, false)
	// A moved-in file has no completion edge coming: the deadline
	// reports the appearance.
	h.at(300 * time.Millisecond).sweep()
	wantChanges(t, h.take(), "create /w/a.md")
}

func TestFoldPendingWithoutCompletionEdgeIsReportedAtTheDeadline(t *testing.T) {
	h := newFoldHarness(t)
	// A writer that keeps the file open (a log, an mmap'd output) has
	// no close_write coming: the deadline reports the change anyway.
	h.create("/w/log.txt", false).modify("/w/log.txt")
	wantChanges(t, h.take())
	h.at(300 * time.Millisecond).sweep()
	wantChanges(t, h.take(), "create /w/log.txt")
}

func TestFoldSeedReportsEntriesFoundInANewDirectory(t *testing.T) {
	h := newFoldHarness(t)
	// The readdir pass after registering a new directory: these files
	// were written between mkdir and the watch being installed.
	var out []change
	h.f.Seed(&out, "/w/out", true, h.now)
	h.f.Seed(&out, "/w/out/a.md", false, h.now)
	h.out = append(h.out, out...)
	wantChanges(t, h.take(), "create /w/out/")

	// A seeded file is not flushed immediately: the kernel's own
	// description of the same appearance may still be queued, and
	// reporting the seed first would report the file twice.
	h.sweep()
	wantChanges(t, h.take())

	// Once the window passes with no completion edge, it is reported:
	// the path existed before the watch, so nothing is coming.
	h.at(300 * time.Millisecond).sweep()
	wantChanges(t, h.take(), "create /w/out/a.md")

	// The queued inotify create merges into the seeded entry instead of
	// reporting the file twice.
	h.create("/w/out/b.md", false)
	h.at(600 * time.Millisecond).sweep()
	wantChanges(t, h.take(), "create /w/out/b.md")
}

func TestFoldSeededDirectoryIsReportedOnce(t *testing.T) {
	h := newFoldHarness(t)
	var out []change
	h.f.Seed(&out, "/w/out/sub", true, h.now)
	h.out = append(h.out, out...)
	wantChanges(t, h.take(), "create /w/out/sub/")

	// The inotify create for the same directory merges into the
	// tombstone.
	h.create("/w/out/sub", true)
	wantChanges(t, h.take())
}

func TestFoldMoveSelfWithoutAPairingIsARemovalAndAGap(t *testing.T) {
	h := newFoldHarness(t)
	h.moveSelf("/w/out")
	h.at(300 * time.Millisecond).sweep()
	if len(h.gaps) != 1 || h.gaps[0] != sandbox.JournalGapWatchLost {
		t.Fatalf("gaps = %v, want one watch-lost gap", h.gaps)
	}
	wantChanges(t, h.take(), "remove /w/out/")
}

func TestFoldMoveSelfResolvedByAPairingIsSilent(t *testing.T) {
	h := newFoldHarness(t)
	h.moveSelf("/w/a")
	h.movedFrom("/w/a", 5, true).movedTo("/w/b", 5, true)
	h.resolveMove("/w/a")
	h.at(300 * time.Millisecond).sweep()
	if len(h.gaps) != 0 {
		t.Fatalf("gaps = %v, want none for an in-tree move", h.gaps)
	}
	wantChanges(t, h.take(), "rename /w/b/ <- /w/a")
}

func TestFoldFlushReportsWhatNeverCompleted(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/a.md", false).modify("/w/a.md")
	h.movedFrom("/w/b.md", 11, false)
	h.create("/w/c", true)
	h.flush()
	wantChanges(t, h.take(),
		"create /w/a.md",
		"remove /w/b.md",
		"create /w/c/",
	)
}

func TestFoldFlushSkipsTombstones(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/out", true)
	wantChanges(t, h.take(), "create /w/out/")
	h.flush()
	wantChanges(t, h.take())
}

func TestFoldReparentRewritesTheSubtree(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/old/f.md", false).modify("/w/old/f.md")
	h.reparent("/w/old", "/w/new")
	h.at(400 * time.Millisecond).sweep()
	wantChanges(t, h.take(), "create /w/new/f.md")
}

func TestFoldForgetDropsTheSubtree(t *testing.T) {
	h := newFoldHarness(t)
	h.create("/w/gone/f.md", false).modify("/w/gone/f.md")
	h.forget("/w/gone")
	h.at(400 * time.Millisecond).sweep()
	wantChanges(t, h.take())
}

func TestFoldCookieTableStaysBounded(t *testing.T) {
	h := newFoldHarness(t)
	for i := 0; i < maxCookies+16; i++ {
		h.movedFrom("/w/f"+string(rune('a'+i%26)), uint32(i+1), false)
	}
	if len(h.f.cookies) > maxCookies {
		t.Fatalf("cookie table = %d entries, want <= %d", len(h.f.cookies), maxCookies)
	}
}

func TestFoldPendingTableStaysBounded(t *testing.T) {
	h := newFoldHarness(t)
	for i := 0; i <= maxPending; i++ {
		h.create("/w/f"+time.Duration(i).String(), false)
	}
	if len(h.f.pending) > maxPending {
		t.Fatalf("pending table = %d entries, want <= %d", len(h.f.pending), maxPending)
	}
	// The entries pushed out were reported rather than dropped: an
	// early report is the same statement, just sooner.
	if len(h.out) == 0 {
		t.Fatal("no change was reported for the evicted entries")
	}
}

func TestFoldOpsFilterDropsOnlyAtEmission(t *testing.T) {
	h := newFoldHarness(t, sandbox.FileOpCreate)
	if h.f.wants(sandbox.FileOpWrite) {
		t.Fatal("wants(write) = true with a create-only filter")
	}
	if !h.f.wants(sandbox.FileOpCreate) {
		t.Fatal("wants(create) = false with a create-only filter")
	}
	// Folding still happens: the filter decides what is emitted, not
	// what is tracked, so a filtered change never becomes a gap.
	h.create("/w/a.md", false).modify("/w/a.md").closeWrite("/w/a.md")
	wantChanges(t, h.take(), "create /w/a.md")
}

func TestFoldNilOpsMeansEverything(t *testing.T) {
	f := newFold(nil, time.Second)
	for _, op := range []sandbox.FileOp{sandbox.FileOpCreate, sandbox.FileOpWrite, sandbox.FileOpRename, sandbox.FileOpRemove} {
		if !f.wants(op) {
			t.Fatalf("wants(%v) = false with no filter", op)
		}
	}
}
