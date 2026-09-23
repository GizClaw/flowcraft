//go:build darwin

package journal

import (
	"encoding/binary"
	"io/fs"
	"os"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"golang.org/x/sys/unix"
)

// macOS has no recursive watch and no per-directory filter: kqueue's
// EVFILT_VNODE attaches to a single file descriptor, and a directory's
// descriptor reports only that its own entry set changed — never which
// entry, and never that a file's content changed. This source therefore
// watches two layers and derives entry-level events from the filesystem
// itself:
//
//   - One descriptor per directory: the membership layer. A directory
//     note means "re-read me": the directory is read and diffed against
//     the snapshot taken when it was registered, and what appeared,
//     vanished or was replaced is reported, with renames paired by
//     inode identity. That read is one pass over the directory's own
//     records, which carry each entry's type and inode number alongside
//     its name, so re-reading a wide directory costs a pass rather than
//     a syscall per entry (dirWatch.read).
//   - One descriptor per non-directory entry: the content layer. A file
//     note is only ever an observation — WRITE/EXTEND says the bytes
//     changed, an ATTRIB note that moved the size is a truncation, and
//     DELETE/RENAME retire the entry. kqueue has no close edge, so a
//     leaf never reports a name-level fact: that is what the directory
//     diff is for, and it is why a leaf cannot name a path that is gone.
//
// The cost is one descriptor per watched *entry*. That is the honest
// unit on this platform, it is what WatchBudget counts, and a watch set
// that does not fit is reported as a capacity gap, never hidden. Two
// properties are better than inotify's: notes coalesce but are never
// dropped, so there is no queue overflow to report, and a coalesced
// change costs a merged report rather than a lost one.

// available reports whether this build has a watch source: kqueue via
// the already-required golang.org/x/sys, with no cgo and no extra
// dependency, on the same terms as inotify on Linux.
func available() bool { return true }

const (
	// budgetFloor and budgetCap bound the watch budget the way the
	// inotify source bounds its share of max_user_watches: a floor so
	// a small tree is never budget-limited, and a ceiling so a host
	// with a huge descriptor limit cannot turn a settings typo into a
	// hundred thousand open descriptors.
	budgetFloor = 1024
	budgetCap   = 16384
	// fdHeadroom is the part of the descriptor ceiling the source
	// leaves to the rest of the process: the budget is a share of
	// what remains, and the soft limit is raised to the budget plus
	// this much.
	fdHeadroom = 64
	// budgetFallback is used when the descriptor ceiling cannot be
	// read at all. It is deliberately the inotify fallback: the
	// budget is reported through Capabilities either way.
	budgetFallback = 16384
	// readDirBufSize is the getdirentries buffer one scan reads with,
	// and the floor a scan of a directory with a handful of entries
	// gets. It is allocated per scan rather than held per watched
	// directory: a snapshot is metadata, a read buffer is scratch.
	readDirBufSize = 8 * 1024
	// readDirBufMax bounds that buffer. Past it a wide directory is
	// read in several calls rather than asking for scratch the size of
	// the directory's own snapshot: see readBufSize for why the number
	// of calls is what a scan is paying for.
	readDirBufMax = 4 << 20
)

// defaultBudget derives the watch budget from the descriptor ceiling.
//
// A quarter of what the process may hold is a deliberately conservative
// share: the daemon may host more than one runner, the host may have
// other descriptor-hungry components, and running out is not silent
// either way — the journal reports a capacity gap — so the conservative
// number buys headroom, not correctness.
func defaultBudget() int {
	return budgetFor(descriptorCeiling())
}

// budgetFor is the budget as a function of the descriptor ceiling, split
// out so the arithmetic is testable without touching the process's
// limits. A ceiling of zero means "not determinable".
func budgetFor(ceiling int64) int {
	if ceiling <= 0 {
		return budgetFallback
	}
	avail := ceiling - fdHeadroom
	if avail <= 0 {
		avail = ceiling
	}
	share := avail / 4
	if share < budgetFloor {
		share = budgetFloor
	}
	if share > budgetCap {
		share = budgetCap
	}
	if share > avail {
		share = avail
	}
	return int(share)
}

// descriptorCeiling is how many descriptors this process may hold: the
// smaller of a finite hard limit and kern.maxfilesperproc, the
// per-process cap macOS enforces on top of RLIMIT_NOFILE. Zero means
// "not determinable".
func descriptorCeiling() int64 {
	var ceiling int64
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rl); err == nil {
		if rl.Max != unix.RLIM_INFINITY {
			ceiling = int64(rl.Max)
		}
	}
	if v, err := unix.SysctlUint32("kern.maxfilesperproc"); err == nil && v > 0 {
		if ceiling == 0 || int64(v) < ceiling {
			ceiling = int64(v)
		}
	}
	return ceiling
}

// raiseDescriptorLimit lifts the soft descriptor limit toward what the
// budget needs.
//
// kqueue charges one descriptor per watched entry, and macOS starts most
// processes at 256 — a journal that silently watched a couple of hundred
// entries would report a capacity gap for a tree nobody would call
// large. The raise is bounded (budget plus headroom, never past the hard
// limit), it only ever raises, and failing it is not fatal: the same gap
// reports the shortfall, which is what the budget exists for.
func raiseDescriptorLimit() {
	want := uint64(defaultBudget() + fdHeadroom)
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rl); err != nil {
		return
	}
	if rl.Cur >= want {
		return
	}
	if rl.Max != unix.RLIM_INFINITY && rl.Max < want {
		want = rl.Max
	}
	if want <= rl.Cur {
		return
	}
	_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: want, Max: rl.Max})
}

const (
	// dirNotes is what a directory's descriptor is asked about. The
	// entry-set changes arrive as NOTE_WRITE; NOTE_LINK and NOTE_EXTEND
	// cover directory size and link-count changes, which are the same
	// statement in different words. Attribute-only changes are
	// deliberately absent: a chmod on a directory is not a write.
	dirNotes = unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_LINK |
		unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_REVOKE
	// leafNotes is what a file's descriptor is asked about. NOTE_ATTRIB
	// is included on purpose: a truncate is only an attribute change on
	// this platform, and comparing the size is what keeps a chmod or a
	// utimes from turning into an event about content that did not
	// change.
	leafNotes = unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB |
		unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_REVOKE
)

// retirement is what an entry's own descriptor said about its name:
// nothing yet, that the name was unlinked, or that it was renamed. The
// directory diff needs it because a directory note says only "something
// changed" — the entry's own watch is what tells a move apart from a
// deletion.
type retirement uint8

const (
	notRetired retirement = iota
	retiredUnlinked
	retiredRenamed
)

// inoKey is the identity a rename preserves: the device and inode pair.
type inoKey struct {
	dev uint64
	ino uint64
}

// valid reports whether the pair identifies anything: a zero pair is
// "identity unknown" (the stat failed), and pairing on it would match
// unrelated entries.
func (k inoKey) valid() bool { return k.dev != 0 || k.ino != 0 }

// entryState is one entry as the last scan saw it: what it is, and the
// device/inode pair that identifies it when a name changes. The pair is
// the entry's own — for a mount point that is the mounted volume, not
// the directory holding the name.
type entryState struct {
	isDir bool
	dev   uint64
	ino   uint64
}

func (e entryState) key() inoKey { return inoKey{dev: e.dev, ino: e.ino} }

// direntRecord is one entry as the directory's own records describe it:
// the name, the inode number it holds in the directory that was read,
// and the type the kernel knows that inode to have.
type direntRecord struct {
	name   string
	ino    uint64
	reclen int
	typ    uint8
}

// The record's fields at the offsets this platform's struct dirent puts
// them at, taken from the struct rather than written down as constants:
// it is the kernel's own layout that is being read.
const (
	direntInoOff    = unsafe.Offsetof(unix.Dirent{}.Ino)
	direntReclenOff = unsafe.Offsetof(unix.Dirent{}.Reclen)
	direntNamlenOff = unsafe.Offsetof(unix.Dirent{}.Namlen)
	direntTypeOff   = unsafe.Offsetof(unix.Dirent{}.Type)
	direntNameOff   = unsafe.Offsetof(unix.Dirent{}.Name)
)

// errDirentRecord is what a record this source cannot read turns into.
// It is not a boundary case dressed up as one: a directory read returns
// whole records, and the longest record a filesystem's name limit can
// produce is a small fraction of the buffer one read fills. So a record
// that runs past the bytes it arrived in means the read cannot be
// trusted — and a name dropped here would reach the diff as a deletion
// nobody performed, while an error ends the watch and reports the gap.
var errDirentRecord = errdefs.Internalf("sandbox/journal: unreadable directory record")

// direntRecordAt parses the record at the start of buf, reporting false
// when what is left of the buffer holds no complete record.
func direntRecordAt(buf []byte) (direntRecord, bool) {
	if len(buf) < int(direntNameOff) {
		return direntRecord{}, false
	}
	reclen := int(binary.NativeEndian.Uint16(buf[direntReclenOff:]))
	if reclen < int(direntNameOff) || reclen > len(buf) {
		return direntRecord{}, false
	}
	rec := buf[:reclen]
	namlen := int(binary.NativeEndian.Uint16(rec[direntNamlenOff:]))
	if int(direntNameOff)+namlen > len(rec) {
		return direntRecord{}, false
	}
	return direntRecord{
		name:   string(rec[direntNameOff : int(direntNameOff)+namlen]),
		ino:    binary.NativeEndian.Uint64(rec[direntInoOff:]),
		reclen: reclen,
		typ:    rec[direntTypeOff],
	}, true
}

// dirWatch is one watched directory: its descriptor and the snapshot
// the next diff compares against. Reads go through the descriptor
// rather than the path — and each entry's state is read inside this
// directory too, never from a joined path, which is why a watched
// directory that is renamed stays readable and keeps reporting what
// happens inside it, without the source having to track where it went.
// The directory's own device is part of what it knows: the records'
// inode numbers are read against it.
type dirWatch struct {
	fd      int
	key     inoKey
	entries map[string]entryState
	// bytes is how many record bytes the last scan of this directory
	// read, which is what sizes the buffer for the next one.
	bytes int
}

func (w *dirWatch) close() error { return unix.Close(w.fd) }

// read returns the directory's current entries. The descriptor's offset
// is rewound first: reading a directory advances it, and a scan that
// started where the last one stopped would see only later entries.
//
// One re-read is what one directory note costs, which makes this the
// hottest path in the source. Two things it does are worth their
// comments: it takes the entries from the records rather than from a
// stat each (recordIdentifies), and it reads the directory in as few
// calls as it can (readBufSize, and the room the loop makes when a
// read comes back short of the buffer).
func (w *dirWatch) read() (map[string]entryState, error) {
	if _, err := unix.Seek(w.fd, 0, 0); err != nil {
		return nil, err
	}
	// The snapshot being replaced is the best guess at the size of the
	// one replacing it, and sizing the map up front is what keeps a
	// wide directory's re-read from rehashing as it fills.
	out := make(map[string]entryState, len(w.entries))
	buf := make([]byte, readBufSize(w.bytes))
	var read int
	for {
		n, err := unix.ReadDirent(w.fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return nil, err
		}
		if n == 0 {
			break
		}
		read += n
		if err := w.collect(buf[:n], out); err != nil {
			return nil, err
		}
		if n < len(buf) && len(buf) < readDirBufMax {
			// Short of the buffer is not the end of the directory on
			// this platform: the kernel returns fewer bytes than it was
			// asked for whenever it likes, and only an empty read says
			// there is nothing left. So the rest of this scan is read
			// with more room — a directory wider than the last scan of
			// it saw, registration of a wide directory included, is
			// read in a handful of calls rather than one per floor.
			buf = make([]byte, min(2*len(buf), readDirBufMax))
		}
	}
	w.bytes = read
	return out, nil
}

// readBufSize is the buffer one scan reads the directory with: what the
// last scan read, with a quarter more room so that a directory which
// grew a little is still read in one call, and a floor for a directory
// with a handful of entries.
//
// The number of calls is what this is about, and it is not the usual
// arithmetic. Reading a directory on this platform is not a stream the
// kernel resumes where the last read left off: every call walks the
// directory again, so a call that returns eight kilobytes of a wide
// directory costs what a call returning all of it costs. Measured on
// APFS, a directory of 4098 records in 164 KB: one 164 KB call, 3.6 ms;
// twenty-one 8 KB calls, 19.8 ms. The buffer is therefore sized from
// what the directory actually holds rather than from a constant, and a
// stable directory is re-read in one call plus the read that ends it.
func readBufSize(last int) int {
	size := last + last/4
	if size < readDirBufSize {
		return readDirBufSize
	}
	if size > readDirBufMax {
		return readDirBufMax
	}
	return size
}

// collect takes the records of one read of the directory into out.
func (w *dirWatch) collect(buf []byte, out map[string]entryState) error {
	for len(buf) > 0 {
		rec, ok := direntRecordAt(buf)
		if !ok {
			return errDirentRecord
		}
		buf = buf[rec.reclen:]
		if rec.name == "." || rec.name == ".." {
			continue
		}
		state, ok := w.state(rec)
		if !ok {
			continue
		}
		out[rec.name] = state
	}
	return nil
}

// state is what one record says its entry is: taken from the record
// where the record is enough, from a stat where it is not.
func (w *dirWatch) state(rec direntRecord) (entryState, bool) {
	if w.recordIdentifies(rec) {
		return entryState{dev: w.key.dev, ino: rec.ino}, true
	}
	var st unix.Stat_t
	if err := unix.Fstatat(w.fd, rec.name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		// The entry was removed between the read and the stat.
		// Whatever changed it already queued a note, and the next diff
		// describes the result.
		return entryState{}, false
	}
	return entryState{
		isDir: st.Mode&unix.S_IFMT == unix.S_IFDIR,
		dev:   uint64(st.Dev),
		ino:   uint64(st.Ino),
	}, true
}

// recordIdentifies reports whether one record identifies its entry on
// its own, which is what lets a scan take it as it stands: the entry is
// not a directory, the record says what it is, and it names an inode
// number against a directory whose own device is known.
//
// Two entries can share an inode number only by being the same entry
// when they share a device, and an entry's device is the device of the
// directory that holds its name: a file, a link, a socket or a device
// node lives where the name that reaches it does. So the pair a record
// yields is the pair a stat would have returned, and asking for it
// again would be the per-entry syscall that made re-reading a wide
// directory cost more than the change that prompted it.
//
// A directory is the exception, and a measured one rather than a
// theoretical one: mounting a volume over a name leaves that name's
// record reporting the inode of the directory that was mounted over —
// an inode of the *parent* filesystem — while the name now holds the
// mounted volume's root, on that volume's own device. Only a stat sees
// that, so directory entries keep theirs, and so does any record that
// carries no inode number or does not say what the entry is.
func (w *dirWatch) recordIdentifies(rec direntRecord) bool {
	return w.key.dev != 0 && rec.ino != 0 && rec.typ != unix.DT_UNKNOWN && rec.typ != unix.DT_DIR
}

// leafWatch is one watched non-directory entry. The descriptor is the
// entry itself (O_SYMLINK keeps a link from standing in for its
// target), and size is the last observed length, which is what tells a
// truncate apart from any other attribute change.
type leafWatch struct {
	fd   int
	key  inoKey
	size int64
}

// nameChange is one changed name as a directory diff found it: what the
// name held before and what it holds now. A nil side is "did not exist".
type nameChange struct {
	name string
	was  *entryState
	now  *entryState
}

// renameSource is a name that lost its entry, and renameDest a name that
// gained one. Slices of them are paired by identity; the indexes are how
// the two halves refer to each other, so no pointer survives an append.
type renameSource struct {
	handle Handle
	name   string
	state  entryState
	cookie uint32
	// mate is the destination at the same name: a name that still
	// exists, holding a different entry. It is what distinguishes a
	// deletion from a name taken over by someone else's move.
	mate   int
	paired int
	held   bool
}

type renameDest struct {
	handle Handle
	name   string
	state  entryState
	source int
}

// heldDeparture is a departure carried into the next poll: a move across
// directories lands in two diffs, and the second one may still be queued
// when the first is processed.
type heldDeparture struct {
	handle Handle
	name   string
	state  entryState
	cookie uint32
}

// kqueueSource is the kqueue implementation of [Source] and
// [leafSource].
type kqueueSource struct {
	// mu serialises Poll and Close, which is what makes closing the
	// kqueue safe: an in-flight poll finishes first, and no later poll
	// can start on a closed descriptor.
	mu     sync.Mutex
	kq     int
	closed bool

	next    Handle
	cookie  uint32
	dirs    map[Handle]*dirWatch
	leaves  map[Handle]*leafWatch
	byIdent map[uint64]Handle
	// retired remembers what an entry's own descriptor said about a
	// name that just left, keyed by identity, until the naming diff has
	// used it.
	retired map[inoKey]retirement
	held    []heldDeparture
	buf     []unix.Kevent_t
}

func openSource() (Source, error) {
	raiseDescriptorLimit()
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, errdefs.NotAvailablef("sandbox/journal: kqueue: %v", err)
	}
	unix.CloseOnExec(kq)
	return &kqueueSource{
		kq:      kq,
		dirs:    make(map[Handle]*dirWatch),
		leaves:  make(map[Handle]*leafWatch),
		byIdent: make(map[uint64]Handle),
		retired: make(map[inoKey]retirement),
		buf:     make([]unix.Kevent_t, 256),
	}, nil
}

// Add implements Source.
//
// The order matters: register first, snapshot second. A change that
// lands between the two is remembered as a note even though the
// snapshot may already contain it, which costs one wasted re-read; the
// other order would put a change outside both the snapshot and the note
// queue, and nothing later would ever mention it.
func (s *kqueueSource) Add(dir string) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errSourceClosed
	}
	fd, err := unix.Open(dir, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	w := &dirWatch{fd: fd, entries: map[string]entryState{}}
	if err := s.attach(fd, dirNotes); err != nil {
		_ = w.close()
		return 0, err
	}
	// The directory's own device is taken before the first scan, not
	// after it: the scan reads every record's inode number against it.
	var st unix.Stat_t
	if unix.Fstat(fd, &st) == nil {
		w.key = inoKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	}
	entries, err := w.read()
	if err != nil {
		_ = w.close()
		return 0, err
	}
	w.entries = entries
	return s.track(fd, w.key, func(h Handle) { s.dirs[h] = w }), nil
}

// AddLeaf implements [leafSource].
func (s *kqueueSource) AddLeaf(path string) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errSourceClosed
	}
	// Only regular files and symbolic links carry content a journal
	// reports. The lstat is what keeps the watch goroutine out of an
	// open that would never return: opening a FIFO for monitoring
	// blocks until the other end appears, and the walk reaches entries
	// like that through a directory that legitimately contains them.
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	mode := info.Mode()
	if !mode.IsRegular() && mode&fs.ModeSymlink == 0 {
		return 0, errNoLeaf
	}
	fd, err := unix.Open(path, unix.O_EVTONLY|unix.O_SYMLINK|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return 0, err
	}
	if err := s.attach(fd, leafNotes); err != nil {
		_ = unix.Close(fd)
		return 0, err
	}
	w := &leafWatch{fd: fd, size: st.Size, key: inoKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}}
	return s.track(fd, w.key, func(h Handle) { s.leaves[h] = w }), nil
}

// track registers one subscription under a fresh handle.
func (s *kqueueSource) track(fd int, key inoKey, keep func(Handle)) Handle {
	s.next++
	h := s.next
	keep(h)
	s.byIdent[uint64(fd)] = h
	if key.valid() {
		s.retired[key] = notRetired
	}
	return h
}

// attach registers one descriptor with the kqueue. EV_CLEAR is what
// makes a note fire once: without it the filter stays asserted and every
// poll returns the same event until the descriptor is closed, which
// would turn the engine's watch loop into a busy loop.
func (s *kqueueSource) attach(fd int, notes uint32) error {
	change := unix.Kevent_t{
		Ident:  uint64(fd),
		Filter: unix.EVFILT_VNODE,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
		Fflags: notes,
	}
	_, err := unix.Kevent(s.kq, []unix.Kevent_t{change}, nil, nil)
	return err
}

// Remove implements Source. Removing an unknown handle is not an error:
// a leaf whose own name left may already be gone, and the engine drops
// the handle when the directory diff reports the name.
func (s *kqueueSource) Remove(h Handle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if w, ok := s.dirs[h]; ok {
		delete(s.dirs, h)
		delete(s.byIdent, uint64(w.fd))
		if w.key.valid() {
			delete(s.retired, w.key)
		}
		return w.close()
	}
	if w, ok := s.leaves[h]; ok {
		delete(s.leaves, h)
		delete(s.byIdent, uint64(w.fd))
		if w.key.valid() {
			delete(s.retired, w.key)
		}
		return unix.Close(w.fd)
	}
	return nil
}

// Poll implements Source. It blocks for at most timeout, then returns
// the events that are ready; a nil slice with a nil error is the timeout
// the engine uses to flush folded changes.
func (s *kqueueSource) Poll(timeout time.Duration) ([]rawEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSourceClosed
	}
	notes, err := s.readNotes(timeout)
	if err != nil {
		return nil, err
	}

	// The notes only decide which directories to re-read and which
	// entries spoke about themselves: everything a consumer sees is
	// derived below, from the filesystem and from that evidence.
	var (
		reread  []Handle
		self    []rawEvent
		content []rawEvent
	)
	for _, note := range notes {
		h, ok := s.byIdent[uint64(note.Ident)]
		if !ok {
			// A note for a subscription the engine already dropped:
			// what it describes left with the watch.
			continue
		}
		if w, ok := s.dirs[h]; ok {
			// The flags are read together rather than switched on,
			// because one note can carry several of them: a directory
			// that is renamed *and* changed before the note is read
			// coalesces into one note, and the membership change must
			// survive that.
			switch {
			case note.Fflags&unix.NOTE_DELETE != 0:
				// The directory itself is gone. Its entries went with
				// it, and the engine tears the subtree down from this
				// one event: re-reading it would only fail.
				s.retire(w.key, retiredUnlinked)
				self = append(self, rawEvent{Op: rawDeleteSelf, Handle: h})
				continue
			case note.Fflags&unix.NOTE_REVOKE != 0:
				self = append(self, rawEvent{Op: rawIgnored, Handle: h})
				continue
			}
			if note.Fflags&unix.NOTE_RENAME != 0 {
				s.retire(w.key, retiredRenamed)
				self = append(self, rawEvent{Op: rawMoveSelf, Handle: h})
			}
			if note.Fflags&(unix.NOTE_WRITE|unix.NOTE_EXTEND|unix.NOTE_LINK) != 0 {
				if !containsHandle(reread, h) {
					reread = append(reread, h)
				}
			}
			continue
		}
		if w, ok := s.leaves[h]; ok {
			s.observeLeaf(h, w, note.Fflags, &content)
		}
	}

	// Departures held from an earlier poll get their last chance before
	// this poll's own diffs are paired: a move across directories puts
	// one note on each parent, and the second may only have been queued
	// by the time the first was processed.
	sources := make([]renameSource, 0, len(s.held)+8)
	for _, hd := range s.held {
		sources = append(sources, renameSource{
			handle: hd.handle, name: hd.name, state: hd.state,
			cookie: hd.cookie, mate: -1, paired: -1, held: true,
		})
	}
	s.held = nil

	var dests []renameDest
	for _, h := range reread {
		changes, err := s.diffDir(s.dirs[h])
		if err != nil {
			// The directory cannot be read any more: whatever happens
			// under it from here on is invisible, and that is a lost
			// watch, not a quiet ending.
			self = append(self, rawEvent{Op: rawIgnored, Handle: h})
			continue
		}
		for _, c := range changes {
			mate := -1
			if c.now != nil {
				dests = append(dests, renameDest{handle: h, name: c.name, state: *c.now, source: -1})
				mate = len(dests) - 1
			}
			if c.was != nil {
				sources = append(sources, renameSource{
					handle: h, name: c.name, state: *c.was,
					cookie: s.nextCookie(), mate: mate, paired: -1,
				})
			}
		}
	}

	// Pair by identity: a rename preserves the inode, and the entry's
	// own descriptor is what rules an unlink out.
	s.pairRenames(sources, dests)

	out := make([]rawEvent, 0, len(self)+len(content)+2*len(sources))
	out = append(out, self...)

	// Renames before content, sources before destinations: a rename is
	// one event and the fold needs the source side to arrive first, and
	// a content change inside a directory that moved in this same batch
	// has to be resolved after the engine has learned where that
	// directory went.
	for i := range sources {
		if sources[i].paired >= 0 {
			out = append(out, sources[i].movedFrom())
		}
	}
	for i := range dests {
		if dests[i].source >= 0 {
			out = append(out, dests[i].movedTo(sources[dests[i].source].cookie))
		}
	}

	// Content changes follow the name-level ones they may depend on,
	// but still precede the removals: a write whose entry is deleted in
	// this same batch describes bytes that no longer exist, and the
	// removal is what should stand.
	out = append(out, content...)

	// What is left of the departures: a deletion is reported now, a
	// name taken over by someone else's move is the move's business,
	// and everything else is held for one poll in case its destination
	// is still queued.
	for i := range sources {
		dep := &sources[i]
		if dep.paired >= 0 {
			continue
		}
		evidence := notRetired
		if key := dep.state.key(); key.valid() {
			evidence = s.retired[key]
		}
		switch {
		case evidence == retiredUnlinked:
			out = append(out, rawEvent{
				Op: rawDelete, Handle: dep.handle, Name: dep.name, IsDir: dep.state.isDir,
			})
		case dep.mate >= 0 && dests[dep.mate].source >= 0:
			// The name still exists because an entry moved onto it:
			// the entry it held before is gone, but a removal for a
			// name that survives would be the wrong statement.
		case dep.held:
			// Held once already, and this poll's diffs did not claim
			// it: it left the watched set. The fold's own deadline
			// turns the unmatched source into a removal.
			out = append(out, dep.movedFrom())
		case evidence == retiredRenamed:
			// It moved, but its destination has not shown up yet: a
			// move across directories lands in two diffs, and the
			// second one may still be queued.
			s.held = append(s.held, heldDeparture{
				handle: dep.handle, name: dep.name, state: dep.state, cookie: dep.cookie,
			})
		default:
			// The entry's own watch never said anything about the name,
			// because it appeared and left again before its content
			// watch existed. A deletion is the statement that cannot be
			// wrong about content — nothing was produced — while a move
			// the journal could observe at all would have left evidence.
			out = append(out, rawEvent{
				Op: rawDelete, Handle: dep.handle, Name: dep.name, IsDir: dep.state.isDir,
			})
		}
	}

	// ... and what is left of the arrivals: an entry that came from
	// somewhere the journal does not watch simply appeared.
	for i := range dests {
		if dests[i].source < 0 {
			out = append(out, rawEvent{
				Op: rawCreate, Handle: dests[i].handle, Name: dests[i].name, IsDir: dests[i].state.isDir,
			})
		}
	}
	return out, nil
}

// readNotes waits for notes and drains everything already queued, so one
// poll describes one state of the filesystem rather than an arbitrary
// slice of it.
func (s *kqueueSource) readNotes(timeout time.Duration) ([]unix.Kevent_t, error) {
	var notes []unix.Kevent_t
	ts := unix.NsecToTimespec(int64(timeout))
	for {
		n, err := unix.Kevent(s.kq, nil, s.buf, &ts)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return nil, err
		}
		if n == 0 {
			return notes, nil
		}
		notes = append(notes, s.buf[:n]...)
		if n < len(s.buf) {
			return notes, nil
		}
		// The buffer filled: the rest of the drain must not wait.
		ts = unix.Timespec{}
	}
}

// observeLeaf turns one file note into a content change, or into the
// evidence a directory diff needs to name what happened to the entry.
// A file note is never itself a name-level statement: a leaf whose entry
// left must not report a path that is gone, and only the directory diff
// knows whether its name moved, was taken over or disappeared.
func (s *kqueueSource) observeLeaf(h Handle, w *leafWatch, fflags uint32, content *[]rawEvent) {
	switch {
	case fflags&unix.NOTE_DELETE != 0:
		s.retire(w.key, retiredUnlinked)
	case fflags&unix.NOTE_RENAME != 0:
		s.retire(w.key, retiredRenamed)
	case fflags&unix.NOTE_REVOKE != 0:
		// The filesystem went away under the whole tree. The
		// directories above report the same loss, with the gap that
		// belongs to it, so a leaf stays quiet.
	case fflags&(unix.NOTE_WRITE|unix.NOTE_EXTEND) != 0:
		// The note is the observation; the size is remembered so a
		// later truncate is not mistaken for a repeat of a write that
		// never happened.
		var st unix.Stat_t
		if unix.Fstat(w.fd, &st) == nil {
			w.size = st.Size
		}
		*content = append(*content, rawEvent{Op: rawModify, Handle: h})
	case fflags&unix.NOTE_ATTRIB != 0:
		// A truncate is only an attribute change here. The size is
		// what tells it apart from a chmod or a utimes, which change
		// no content and must never become events.
		var st unix.Stat_t
		if err := unix.Fstat(w.fd, &st); err != nil || st.Size == w.size {
			return
		}
		w.size = st.Size
		*content = append(*content, rawEvent{Op: rawModify, Handle: h})
	}
}

// retire records what an entry's own descriptor said about its name.
func (s *kqueueSource) retire(key inoKey, r retirement) {
	if !key.valid() {
		return
	}
	s.retired[key] = r
}

// diffDir re-reads one directory and reports the names that changed.
func (s *kqueueSource) diffDir(w *dirWatch) ([]nameChange, error) {
	fresh, err := w.read()
	if err != nil {
		return nil, err
	}
	var changes []nameChange
	for name, now := range fresh {
		was, ok := w.entries[name]
		if !ok {
			state := now
			changes = append(changes, nameChange{name: name, now: &state})
			continue
		}
		if was.isDir != now.isDir || was.dev != now.dev || was.ino != now.ino {
			// The name is still there, holding a different entry: the
			// old one left and this one arrived, and which of the two
			// is a move is decided by identity below.
			old, state := was, now
			changes = append(changes, nameChange{name: name, was: &old, now: &state})
		}
	}
	for name, was := range w.entries {
		if _, ok := fresh[name]; !ok {
			state := was
			changes = append(changes, nameChange{name: name, was: &state})
		}
	}
	w.entries = fresh
	// The scan order is the map's otherwise; sorting the names makes
	// one batch's events reproducible.
	sort.Slice(changes, func(i, j int) bool { return changes[i].name < changes[j].name })
	return changes, nil
}

// pairRenames matches departures with arrivals that share an identity.
// The evidence matters as much as the identity: an inode the entry's own
// watch reported as unlinked did not move anywhere, however closely the
// next arrival matches it (a hardlink swap is a removal and an
// appearance, not a rename). A departure with no evidence at all — the
// entry appeared and left again before its content watch existed — can
// only be paired by identity, which is the strongest statement
// available; the alternative would be to call every such move a
// deletion.
func (s *kqueueSource) pairRenames(sources []renameSource, dests []renameDest) {
	byIno := make(map[inoKey][]int, len(dests))
	for i := range dests {
		if key := dests[i].state.key(); key.valid() {
			byIno[key] = append(byIno[key], i)
		}
	}
	for i := range sources {
		key := sources[i].state.key()
		if !key.valid() || s.retired[key] == retiredUnlinked {
			continue
		}
		for _, d := range byIno[key] {
			if dests[d].source >= 0 {
				continue
			}
			dests[d].source = i
			sources[i].paired = d
			break
		}
	}
}

func (d renameSource) movedFrom() rawEvent {
	return rawEvent{Op: rawMovedFrom, Handle: d.handle, Name: d.name, Cookie: d.cookie, IsDir: d.state.isDir}
}

func (d renameDest) movedTo(cookie uint32) rawEvent {
	return rawEvent{Op: rawMovedTo, Handle: d.handle, Name: d.name, Cookie: cookie, IsDir: d.state.isDir}
}

func (s *kqueueSource) nextCookie() uint32 {
	s.cookie++
	return s.cookie
}

// Close implements Source.
func (s *kqueueSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	for _, w := range s.dirs {
		if cerr := w.close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	for _, w := range s.leaves {
		_ = unix.Close(w.fd)
	}
	// Closing a descriptor drops its kqueue registration, so the
	// kqueue itself is the last thing to go.
	s.dirs = map[Handle]*dirWatch{}
	s.leaves = map[Handle]*leafWatch{}
	s.byIdent = map[uint64]Handle{}
	s.retired = map[inoKey]retirement{}
	s.held = nil
	if kerr := unix.Close(s.kq); kerr != nil && err == nil {
		err = kerr
	}
	return err
}

func containsHandle(handles []Handle, h Handle) bool {
	for _, existing := range handles {
		if existing == h {
			return true
		}
	}
	return false
}
