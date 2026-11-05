//go:build darwin

package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"golang.org/x/sys/unix"
)

// darwinSource opens this platform's source together with its leaf
// half: it is one object with two faces, and the tests need both.
func darwinSource(t *testing.T) (Source, leafSource) {
	t.Helper()
	src, err := openSource()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	leaf, ok := src.(leafSource)
	if !ok {
		t.Fatal("the darwin source does not watch leaves")
	}
	return src, leaf
}

func wantRaw(t *testing.T, events []rawEvent, want ...string) {
	t.Helper()
	got := describeRaw(events)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

// wantLeafRaw asserts exactly one leaf event. A content change carries
// the entry's handle and no name: resolving a handle to a path is the
// engine's job, not the source's.
func wantLeafRaw(t *testing.T, events []rawEvent, handle Handle, op string) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("events = %v, want one %s from the leaf", describeRaw(events), op)
	}
	ev := events[0]
	if ev.Op.String() != op || ev.Handle != handle || ev.Name != "" {
		t.Fatalf("event = %s handle=%d name=%q, want %s from handle %d with no name",
			ev.Op, ev.Handle, ev.Name, op, handle)
	}
}

// TestDarwinSourceSeesContentChanges pins the leaf layer: kqueue reports
// a directory's entry set, never a file's content, so content changes
// are what the per-entry descriptors exist for. The negatives matter as
// much as the positives: a chmod and a utimes are attribute changes, and
// a journal of those would be a journal of false positives.
func TestDarwinSourceSeesContentChanges(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.md")
	if err := os.WriteFile(file, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src, leaf := darwinSource(t)
	if _, err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	leafHandle, err := leaf.AddLeaf(file)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(file, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantLeafRaw(t, collect(t, src, 1, 3*time.Second), leafHandle, "modify")

	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(file, now, now); err != nil {
		t.Fatal(err)
	}
	if events := settle(t, src, 300*time.Millisecond); len(events) != 0 {
		t.Fatalf("chmod and utimes produced %v; attribute changes are not writes", describeRaw(events))
	}

	// A truncate is only an attribute change on this platform: what
	// makes it a content change is that the size moved.
	if err := os.Truncate(file, 0); err != nil {
		t.Fatal(err)
	}
	wantLeafRaw(t, collect(t, src, 1, 3*time.Second), leafHandle, "modify")
}

// TestDarwinSourceReportsDeletesAndRenamesApart covers the evidence the
// directory diff needs: a directory note says only "something changed",
// and the entry's own descriptor is what tells an unlink from a move.
func TestDarwinSourceReportsDeletesAndRenamesApart(t *testing.T) {
	root := t.TempDir()
	from := filepath.Join(root, "from.md")
	to := filepath.Join(root, "to.md")
	if err := os.WriteFile(from, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	src, leaf := darwinSource(t)
	if _, err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.AddLeaf(from); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	events := collect(t, src, 2, 3*time.Second)
	wantRaw(t, events, "moved_from from.md", "moved_to to.md")
	if events[0].Cookie == 0 || events[0].Cookie != events[1].Cookie {
		t.Fatalf("cookies = %d and %d, want one shared non-zero cookie",
			events[0].Cookie, events[1].Cookie)
	}

	// An unlink is not a move: the entry's own watch said so, and the
	// diff reports the deletion instead of waiting for a destination
	// that will never come.
	if _, err := leaf.AddLeaf(to); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	wantRaw(t, collect(t, src, 1, 3*time.Second), "delete to.md")
}

// TestDarwinSourceHoldsAnUnclaimedMoveOnePoll covers the cross-directory
// case: one rename puts a note on each parent, and the departure is held
// for a poll so the destination's diff can still claim it.
func TestDarwinSourceHoldsAnUnclaimedMoveOnePoll(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(root, "leaving.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	src, leaf := darwinSource(t)
	if _, err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.AddLeaf(file); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(file, filepath.Join(outside, "leaving.md")); err != nil {
		t.Fatal(err)
	}
	// The first poll holds it: the destination is outside the watched
	// set, so its diff will never come, and the next poll reports the
	// move source. The fold then turns it into a removal at its own
	// deadline, exactly like an unmatched inotify MOVED_FROM.
	wantRaw(t, collect(t, src, 1, 3*time.Second), "moved_from leaving.md")
}

// TestDarwinSourceRefusesEntriesWithoutContent is the regression guard
// for a measured hazard: opening a FIFO for monitoring blocks until the
// other end appears, which would park the watch goroutine inside an open
// that nobody wakes. Such entries are refused up front instead.
func TestDarwinSourceRefusesEntriesWithoutContent(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	src, leaf := darwinSource(t)
	if _, err := src.Add(root); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := leaf.AddLeaf(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errNoLeaf) {
			t.Fatalf("AddLeaf(fifo) = %v, want errNoLeaf", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AddLeaf(fifo) blocked: the watch goroutine must never park in an open")
	}

	// The directory still reports the entry, so the name is not lost.
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	wantRaw(t, collect(t, src, 1, 3*time.Second), "delete pipe")
}

// TestDarwinSourceReadsThroughTheDescriptor covers the reason reads go
// through the descriptor instead of the path: a watched directory that
// is renamed is still the same directory, and the engine reparents its
// bookkeeping while the source keeps reporting what happens inside it.
func TestDarwinSourceReadsThroughTheDescriptor(t *testing.T) {
	root := t.TempDir()
	before := filepath.Join(root, "before")
	if err := os.MkdirAll(before, 0o755); err != nil {
		t.Fatal(err)
	}

	src, _ := darwinSource(t)
	if _, err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	handle, err := src.Add(before)
	if err != nil {
		t.Fatal(err)
	}

	after := filepath.Join(root, "after")
	if err := os.Rename(before, after); err != nil {
		t.Fatal(err)
	}
	collect(t, src, 2, 3*time.Second)

	if err := os.WriteFile(filepath.Join(after, "inside.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	events := collect(t, src, 1, 3*time.Second)
	if len(events) != 1 || events[0].Op != rawCreate || events[0].Handle != handle || events[0].Name != "inside.md" {
		t.Fatalf("events = %v, want one create inside the moved directory", describeRaw(events))
	}
}

// watcher returns the directory watch behind one handle. The source
// keeps no goroutine of its own — the engine's watch loop is what calls
// Poll — so a test that does not poll reads its bookkeeping directly.
func watcher(t *testing.T, src Source, h Handle) *dirWatch {
	t.Helper()
	s, ok := src.(*kqueueSource)
	if !ok {
		t.Fatalf("source is %T, not a kqueue source", src)
	}
	w, ok := s.dirs[h]
	if !ok {
		t.Fatalf("handle %d is not a directory watch", h)
	}
	return w
}

// scanStatPerEntry is the scan this source did before it read the
// directory's own records for their identities: the names, then a stat
// for each one inside the directory. It is the reference the correctness
// test compares against and the yardstick the benchmark is measured
// against — one implementation, so the two cannot drift apart.
func scanStatPerEntry(tb testing.TB, w *dirWatch) map[string]entryState {
	tb.Helper()
	if _, err := unix.Seek(w.fd, 0, 0); err != nil {
		tb.Fatalf("seek: %v", err)
	}
	var names []string
	buf := make([]byte, readDirBufSize)
	for {
		n, err := unix.ReadDirent(w.fd, buf)
		if err != nil {
			tb.Fatalf("readdirent: %v", err)
		}
		if n == 0 {
			break
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
	out := make(map[string]entryState, len(names))
	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(w.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			// Gone between the read and the stat, exactly as in a scan.
			continue
		}
		out[name] = entryState{
			isDir: st.Mode&unix.S_IFMT == unix.S_IFDIR,
			dev:   uint64(st.Dev),
			ino:   uint64(st.Ino),
		}
	}
	return out
}

// compareEntries reports the disagreement between two scans by name, so
// a wide directory's failure names the entry a scan lost rather than
// saying that two maps differ.
func compareEntries(t *testing.T, got, want map[string]entryState) {
	t.Helper()
	if maps.Equal(got, want) {
		return
	}
	names := make([]string, 0, len(got)+len(want))
	for name := range got {
		names = append(names, name)
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var lines []string
	for _, name := range names {
		g, inGot := got[name]
		w, inWant := want[name]
		switch {
		case !inGot:
			lines = append(lines, name+": missing from the scan")
		case !inWant:
			lines = append(lines, name+": in the scan but not in the directory")
		case g != w:
			lines = append(lines, fmt.Sprintf("%s: %+v, want %+v", name, g, w))
		}
	}
	total := len(lines)
	if len(lines) > 6 {
		lines = append(lines[:6:6], fmt.Sprintf("... and %d more", total-6))
	}
	t.Fatalf("the scan and a stat per entry disagree on %d of %d entries:\n%s",
		total, len(want), strings.Join(lines, "\n"))
}

// TestDarwinScanAgreesWithAStatPerEntry is the invariant the record scan
// rests on: taking an entry's identity from the directory's own records
// gives exactly what a stat per entry gave, for every kind of entry a
// sandbox root holds. The directory is deliberately wider than one read
// of it and its names are long, so a record misparsed or dropped at a
// read boundary shows up as an entry the scan lost instead of hiding
// behind the two scans agreeing on everything else.
func TestDarwinScanAgreesWithAStatPerEntry(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("l", 200)
	for i := range 300 {
		name := fmt.Sprintf("file%03d-%s.txt", i, long)
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"sub", "sub/deep", "empty"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	twin := "file000-" + long + ".txt"
	if err := os.Link(filepath.Join(root, twin), filepath.Join(root, "twin.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".hidden", "spaced name.md", strings.Repeat("n", 255)} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	src, _ := darwinSource(t)
	handle, err := src.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	w := watcher(t, src, handle)
	got, err := w.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	compareEntries(t, got, scanStatPerEntry(t, w))

	// The identities are the entries' own and not their targets': a link
	// to a directory is neither the directory nor a directory itself,
	// while two names hardlinked together are one entry under both.
	if got["link"] == got["sub"] || got["link"].isDir || !got["sub"].isDir {
		t.Fatalf("link = %+v and sub = %+v: a symlink must not take its target's identity",
			got["link"], got["sub"])
	}
	if got["twin.md"] != got[twin] {
		t.Fatalf("hardlinked names = %+v and %+v, want one identity", got["twin.md"], got[twin])
	}
}

// direntBytes writes one directory record the way the kernel does, so
// the parser can be handed bytes no filesystem would have to produce: a
// name that runs past its record, a record with no length, a record the
// bytes it arrived in cut short.
func direntBytes(name string, ino uint64, typ uint8) []byte {
	reclen := (int(direntNameOff) + len(name) + 3) &^ 3
	rec := make([]byte, reclen)
	binary.NativeEndian.PutUint64(rec[direntInoOff:], ino)
	binary.NativeEndian.PutUint16(rec[direntReclenOff:], uint16(reclen))
	binary.NativeEndian.PutUint16(rec[direntNamlenOff:], uint16(len(name)))
	rec[direntTypeOff] = typ
	copy(rec[direntNameOff:], name)
	return rec
}

// TestDarwinScanReadsRecords covers the bytes a scan is built on: the
// fields at the offsets the platform's struct dirent puts them at, and
// the records a scan has to refuse. Refusing them matters out of
// proportion to how often it can happen — the kernel returns whole
// records, so a record that does not fit is a read this source cannot
// trust, and reporting that is what keeps the names it would have
// dropped out of the diff, where they would read as deletions nobody
// performed.
func TestDarwinScanReadsRecords(t *testing.T) {
	rec := direntBytes("a.md", 7, unix.DT_REG)
	got, ok := direntRecordAt(rec)
	if !ok {
		t.Fatalf("direntRecordAt(%d bytes) read nothing", len(rec))
	}
	if got.name != "a.md" || got.ino != 7 || got.typ != unix.DT_REG || got.reclen != len(rec) {
		t.Fatalf("direntRecordAt = %+v, want the record it was handed", got)
	}

	overlong := direntBytes("a.md", 7, unix.DT_REG)
	binary.NativeEndian.PutUint16(overlong[direntNamlenOff:], uint16(len(overlong)))
	for _, tc := range []struct {
		name string
		buf  []byte
	}{
		{"a buffer shorter than one header", rec[:int(direntNamlenOff)]},
		{"a record cut short by the bytes it arrived in", rec[:len(rec)-1]},
		{"a record of no length", make([]byte, direntNameOff)},
		{"a name running past its record", overlong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := direntRecordAt(tc.buf); ok {
				t.Fatalf("direntRecordAt(%d bytes) read a record that is not there", len(tc.buf))
			}
		})
	}
}

// TestDarwinScanCollectsRecords covers what one read of a directory
// turns into: the two names every directory carries and never holds, the
// entries whose records are enough, the one whose record is not, and the
// names that are gone by the time a scan asks about them.
func TestDarwinScanCollectsRecords(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	src, _ := darwinSource(t)
	handle, err := src.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	w := watcher(t, src, handle)

	out := map[string]entryState{}
	if err := w.collect(nil, out); err != nil || len(out) != 0 {
		t.Fatalf("an empty read produced %v (%v)", out, err)
	}
	for _, name := range []string{".", ".."} {
		if err := w.collect(direntBytes(name, 1, unix.DT_DIR), out); err != nil {
			t.Fatalf("collect(%q): %v", name, err)
		}
	}
	if len(out) != 0 {
		t.Fatalf("collecting the names a directory calls itself produced %v", out)
	}

	// A record the scan cannot read ends it, and read() drops the map it
	// was filling: what the diff sees is the loss of the watch rather
	// than a directory that appears to have lost entries.
	pair := append(direntBytes("a.md", 7, unix.DT_REG), direntBytes("b.md", 8, unix.DT_REG)...)
	if err := w.collect(pair, out); err != nil || len(out) != 2 {
		t.Fatalf("collect(two records) = %v, %v", out, err)
	}
	if err := w.collect(pair[:len(pair)-1], map[string]entryState{}); !errors.Is(err, errDirentRecord) {
		t.Fatalf("collect(a truncated record) = %v, want errDirentRecord", err)
	}

	// A name whose entry left between the read and the stat: nothing to
	// say about it, and nothing invented for it.
	if err := w.collect(direntBytes("gone.md", 9, unix.DT_DIR), out); err != nil || len(out) != 2 {
		t.Fatalf("collect(a record for a name that is not there) = %v, %v", out, err)
	}

	// A record that does not say what the entry is — no type, or no
	// inode number — is filled in by the stat.
	for _, rec := range []direntRecord{
		{name: "a.md", ino: 0, typ: unix.DT_UNKNOWN},
		{name: "a.md", ino: 0, typ: unix.DT_REG},
	} {
		typed := map[string]entryState{}
		if err := w.collect(direntBytes(rec.name, rec.ino, rec.typ), typed); err != nil {
			t.Fatalf("collect(%+v): %v", rec, err)
		}
		if want := scanStatPerEntry(t, w)["a.md"]; typed["a.md"] != want {
			t.Fatalf("collect(%+v) gave %+v, want the stat's %+v", rec, typed["a.md"], want)
		}
	}
}

// TestDarwinScanTakesRecordsAsIdentities covers the decision a scan
// makes for every record: take the entry as the record describes it, or
// stat it. The directory case is the one that exists because of a
// mounted volume rather than in theory — with a volume mounted over a
// name, that name's record keeps reporting the inode of the directory
// that was mounted over, an inode of the *parent* filesystem, while the
// name now holds the mounted volume's root on the volume's own device.
// Taking the record there would report a mount as no change at all.
func TestDarwinScanTakesRecordsAsIdentities(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	src, _ := darwinSource(t)
	handle, err := src.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	w := watcher(t, src, handle)

	cases := []struct {
		name  string
		key   inoKey
		rec   direntRecord
		trust bool
	}{
		{"a file's record", w.key, direntRecord{name: "a.md", ino: 4242, typ: unix.DT_REG}, true},
		{"a link's record", w.key, direntRecord{name: "a.md", ino: 4242, typ: unix.DT_LNK}, true},
		{"a directory's record", w.key, direntRecord{name: "sub", ino: 4242, typ: unix.DT_DIR}, false},
		{"a record that does not say what the entry is", w.key, direntRecord{name: "a.md", ino: 4242, typ: unix.DT_UNKNOWN}, false},
		{"a record without an inode number", w.key, direntRecord{name: "a.md", ino: 0, typ: unix.DT_REG}, false},
		{"a directory with no device of its own", inoKey{}, direntRecord{name: "a.md", ino: 4242, typ: unix.DT_REG}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := *w
			probe.key = tc.key
			if got := probe.recordIdentifies(tc.rec); got != tc.trust {
				t.Fatalf("recordIdentifies(%+v) = %v, want %v", tc.rec, got, tc.trust)
			}
			want := scanStatPerEntry(t, w)[tc.rec.name]
			if tc.trust {
				// The record's own inode number survives into the
				// snapshot: nothing was asked of the filesystem about
				// this entry.
				want = entryState{dev: tc.key.dev, ino: tc.rec.ino}
			}
			if got, ok := probe.state(tc.rec); !ok || got != want {
				t.Fatalf("state(%+v) = %+v (%v), want %+v", tc.rec, got, ok, want)
			}
		})
	}
}

// TestDarwinReadBufferSize pins the shape of the buffer a scan reads
// with: a floor for a directory holding a handful of entries, a quarter
// more than the last scan of it read, and a ceiling so that scratch
// never grows past the snapshot the directory already costs.
func TestDarwinReadBufferSize(t *testing.T) {
	cases := []struct {
		last int
		want int
	}{
		{last: 0, want: readDirBufSize},
		{last: 16, want: readDirBufSize},
		{last: readDirBufSize, want: readDirBufSize + readDirBufSize/4},
		{last: 160 << 10, want: 200 << 10},
		{last: readDirBufMax, want: readDirBufMax},
		{last: 16 << 20, want: readDirBufMax},
	}
	for _, tc := range cases {
		if got := readBufSize(tc.last); got != tc.want {
			t.Errorf("readBufSize(%d) = %d, want %d", tc.last, got, tc.want)
		}
	}
}

// TestDarwinBudgetArithmetic pins the shape of the budget: a share of
// the descriptor ceiling, floored so small trees are never
// budget-limited, capped so a huge limit cannot become a lot of open
// files, and never larger than what the process may actually hold.
func TestDarwinBudgetArithmetic(t *testing.T) {
	cases := []struct {
		ceiling int64
		want    int
	}{
		{ceiling: 0, want: budgetFallback},
		{ceiling: 512, want: 448},
		{ceiling: 61440, want: 15344},
		{ceiling: 4 << 20, want: budgetCap},
	}
	for _, tc := range cases {
		if got := budgetFor(tc.ceiling); got != tc.want {
			t.Errorf("budgetFor(%d) = %d, want %d", tc.ceiling, got, tc.want)
		}
	}
	if got := budgetFor(16); got > 16 {
		t.Errorf("budgetFor(16) = %d, which is more descriptors than the ceiling", got)
	}
}

// TestDarwinRaisesTheDescriptorLimitTowardTheBudget covers the promise
// the budget rests on: kqueue charges one descriptor per watched entry,
// macOS starts most processes at 256, and a journal is expected to lift
// its own soft limit rather than report a capacity gap for a tree
// nobody would call large.
func TestDarwinRaisesTheDescriptorLimitTowardTheBudget(t *testing.T) {
	var before unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &before); err != nil {
		t.Skipf("cannot read the descriptor limit: %v", err)
	}
	want := uint64(defaultBudget() + fdHeadroom)
	if before.Max != unix.RLIM_INFINITY && before.Max < want {
		t.Skipf("hard limit %d cannot hold the budget %d; the source reports gaps instead", before.Max, want)
	}
	lowered := unix.Rlimit{Cur: 256, Max: before.Max}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lowered); err != nil {
		t.Skipf("cannot lower the soft limit: %v", err)
	}
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &before) })

	raiseDescriptorLimit()

	var after unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &after); err != nil {
		t.Fatal(err)
	}
	if after.Cur < want {
		t.Fatalf("soft limit = %d after the raise, want at least %d", after.Cur, want)
	}
}

// TestDarwinJournalCountsEveryEntryAgainstTheBudget is the honest-cost
// promise as a test: on this platform a watch is one descriptor per
// entry, so leaves share the budget directories already use, and a set
// that does not fit is reported rather than trimmed in silence.
func TestDarwinJournalCountsEveryEntryAgainstTheBudget(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "one"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Root + one directory + three files: five entries, and room for
	// exactly five.
	roomy, err := New(Config{Root: root, Options: sandbox.JournalOptions{MaxWatchSet: 5}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roomy.Close() }()
	if batch := readOnce(t, roomy, 0, 16); batch.Gap != nil {
		t.Fatalf("a budget of five entries for a five-entry tree reported %+v", batch.Gap)
	}

	// One entry less than the tree holds: the watch set stops there,
	// and the shortfall is a capacity gap, not a silent hole.
	tight, err := New(Config{Root: root, Options: sandbox.JournalOptions{MaxWatchSet: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tight.Close() }()
	batch := readOnce(t, tight, 0, 16)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapCapacity {
		t.Fatalf("gap = %+v, want a capacity gap for the entries that did not fit", batch.Gap)
	}

	// The files that did fit are still watched, content and all.
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("yy"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		batch := readOnce(t, tight, 0, 16)
		for _, ev := range batch.Events {
			if ev.Op == sandbox.FileOpWrite && ev.Path == "a.md" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the entries inside the budget stayed unwatched")
}
