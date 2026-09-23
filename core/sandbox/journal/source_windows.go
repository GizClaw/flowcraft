//go:build windows

package journal

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"golang.org/x/sys/windows"
)

// available reports whether this build has a watch source: on Windows,
// ReadDirectoryChangesW through the already-required golang.org/x/sys,
// with no cgo and no extra dependency.
func available() bool { return true }

// defaultBudget is how many directories this source watches before it
// starts reporting a capacity gap.
//
// The number is in the unit this platform charges for. A watch is a
// directory handle plus one pending overlapped read, and that read owns
// a change buffer which the kernel holds until the read completes — so
// one watch costs dirBufferSize of user memory and the same again as
// pinned kernel pages, against roughly a kilobyte per watch on Linux.
// At 16 KiB a watch, 4096 of them is 64 MiB of buffers and 64 MiB more
// pinned: enough for a runner's tree, small enough that a pathological
// one is reported as a gap rather than eating the host. The kernel's
// own limits are far higher, which is why this is a memory budget and
// not a limit the platform imposed.
func defaultBudget() int { return 4096 }

const (
	// dirBufferSize is the change buffer one watched directory owns.
	// The kernel writes the changes it records into this buffer until
	// the read completes, so its size is how much a directory can say
	// in one go before the rest comes back as ERROR_NOTIFY_ENUM_DIR —
	// which is reported as a gap, never hidden. It is also memory the
	// kernel pins for as long as the read is pending, in a process that
	// may watch thousands of directories, so this is a compromise: 16
	// KiB holds a few hundred change records, which is the size the
	// other implementations settled on, and it keeps the budget above
	// meaningful.
	dirBufferSize = 16 * 1024
	// maxCompletionsPerPoll bounds one drain, so a flood cannot keep
	// the engine waiting inside a single Poll (and with it, keep folded
	// changes from being swept and reported).
	maxCompletionsPerPoll = 256
	// notifyFilter is exactly the set of change classes the fold
	// understands. Reads (FILE_NOTIFY_CHANGE_LAST_ACCESS), attribute
	// changes (FILE_NOTIFY_CHANGE_ATTRIBUTES) and creation-time changes
	// are deliberately absent for the same reason inotify's filter
	// excludes them: a journal of reads and chmods would be a journal
	// of false positives.
	//
	// SIZE and LAST_WRITE are what a content change is here; the
	// platform may report them late, because it records a size or
	// timestamp change when the write is seen by the filesystem rather
	// than when the writer's buffer was handed over (see the package
	// doc).
	notifyFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME |
		windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE
	// closeDrain bounds how long Close waits for the completions of the
	// reads it cancelled, and closeWaitMS is one wait in that drain.
	closeDrain  = 250 * time.Millisecond
	closeWaitMS = 20
)

// notifyHeaderSize is the offset of the name inside a change record:
// three DWORDs of offset, action and name length, taken from the struct
// itself so it cannot drift from the layout the kernel uses. The header
// itself is read a DWORD at a time rather than through the struct: a
// record only has to start on a two-byte boundary for its name to be
// readable, and a struct read would demand an alignment the platform
// does not promise everywhere it is implemented.
var notifyHeaderSize = int(unsafe.Offsetof(windows.FileNotifyInformation{}.FileName))

// winWatch is one directory subscription: the handle the kernel watches
// and the read that is pending on it.
type winWatch struct {
	id     Handle
	handle windows.Handle

	// dir is the path this subscription was added under. It is a hint,
	// not the engine's bookkeeping: the engine owns paths and reparents
	// them when a watched directory moves, while this copy keeps naming
	// the directory the way it was added, so after an in-tree rename it
	// can be stale. Both of its uses below degrade to "no answer" when
	// that happens, and neither ever names an event — names come from
	// the kernel, relative to a handle that follows the directory
	// wherever it moves.
	dir string

	// buf is the change buffer the pending read writes into, ov is the
	// overlapped structure it was issued with, and ret is where the
	// call's byte count is reported. The kernel owns all three until
	// the completion is dequeued, which is why a removed watch is
	// retired rather than freed.
	buf []byte
	ov  windows.Overlapped
	ret uint32

	// retired marks a watch the engine has removed. Its completion
	// still has to be drained, but nothing in it is reported.
	retired bool
}

// winSource is the ReadDirectoryChangesW implementation of [Source].
//
// Handles are this source's own identifiers, never kernel handles. A
// kernel handle is reused the moment it is closed, and the completion
// of a read from a closed handle would otherwise be attributed to the
// subscription that inherited the number.
type winSource struct {
	// mu serialises Poll and Close, the way the other sources do: a
	// Close waits for the poll in flight instead of closing a handle
	// under it.
	mu     sync.Mutex
	port   windows.Handle
	closed bool

	next   Handle
	cookie uint32
	// live is the engine's view: which subscription each handle is.
	live map[Handle]*winWatch
	// byPath is the same set keyed by path. It answers the two
	// questions the kernel's records leave open — whether a name is a
	// directory this source watches — and nothing else.
	byPath map[string]Handle
	// reads holds every read that has been issued and not yet
	// completed, keyed by the OVERLAPPED the kernel hands back on
	// completion. This is what keeps a watch's buffer and OVERLAPPED
	// alive while the kernel still owns them, retired or not.
	reads map[*windows.Overlapped]*winWatch
}

func openSource() (Source, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
	if err != nil {
		return nil, errdefs.NotAvailablef("sandbox/journal: CreateIoCompletionPort: %v", err)
	}
	return &winSource{
		port:   port,
		live:   make(map[Handle]*winWatch),
		byPath: make(map[string]Handle),
		reads:  make(map[*windows.Overlapped]*winWatch),
	}, nil
}

// Add implements Source. It opens the directory for notification and
// issues the read that will report what happens inside it. A path that
// is not a directory opens as one anyway — this platform has no
// equivalent of inotify's IN_ONLYDIR — but the read that follows the
// open is a directory read, so it fails there and the failure is
// reported to the engine instead of a watch that quietly sees nothing.
func (s *winSource) Add(dir string) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errSourceClosed
	}
	// FILE_SHARE_DELETE matters: a directory that is being renamed or
	// removed while it is watched must still be openable, and a watcher
	// that held it open exclusively would change the observable
	// behaviour of the tree it is watching.
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(openPath(dir)),
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED,
		0)
	if err != nil {
		return 0, err
	}
	w := &winWatch{id: s.next + 1, handle: handle, dir: dir, buf: make([]byte, dirBufferSize)}
	// The handle has to be associated with the port before the read is
	// issued: the association decides where the completion goes, and an
	// operation that started before it would complete somewhere this
	// source is not listening.
	if _, err := windows.CreateIoCompletionPort(handle, s.port, 0, 0); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	if err := s.arm(w); err != nil {
		// The read never carried anything, but a refused issue may have
		// queued a completion anyway; retiring it here has that dropped
		// rather than reported against a handle the engine never saw.
		s.retireLocked(w)
		return 0, err
	}
	s.next = w.id
	s.live[w.id] = w
	s.byPath[dir] = w.id
	return w.id, nil
}

// Remove implements Source. The watch is taken out of service, and the
// kernel is asked to cancel the read it holds: the completion of that
// cancellation is what releases the buffer, and it is dropped rather
// than reported, because a removal the engine asked for is not a loss —
// the tree those events describe is gone.
func (s *winSource) Remove(h Handle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(h)
	return nil
}

func (s *winSource) removeLocked(h Handle) {
	w, ok := s.live[h]
	if !ok {
		return
	}
	delete(s.live, h)
	if cur, ok := s.byPath[w.dir]; ok && cur == h {
		// The path may have been added again under a new handle since
		// (a directory renamed away leaves its old name free): only the
		// entry that is still this watch's is dropped.
		delete(s.byPath, w.dir)
	}
	s.retireLocked(w)
}

// retireLocked takes a watch out of service: the read it issued may
// still be in flight, so it stays in reads until its completion has
// been dequeued. Cancel first, then close: closing a handle cancels its
// pending I/O as well, but asking makes the completion unconditional
// instead of a property of the filesystem.
func (s *winSource) retireLocked(w *winWatch) {
	w.retired = true
	_ = windows.CancelIoEx(w.handle, nil)
	_ = windows.CloseHandle(w.handle)
}

// Poll implements Source: it waits on the completion port for the reads
// that finished, turns each one into events, and starts the next read
// for the directory it came from.
func (s *winSource) Poll(timeout time.Duration) ([]rawEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSourceClosed
	}
	deadline := time.Now().Add(timeout)
	var out []rawEvent
	for round := 0; round < maxCompletionsPerPoll; round++ {
		ms := int(time.Until(deadline) / time.Millisecond)
		if len(out) > 0 {
			// Events are in hand: take what is ready right now instead of
			// waiting out the rest of the window, so a poll returns the
			// events that were ready when its wait ended rather than a
			// window's worth of them.
			ms = 0
		} else if ms < 1 {
			if round > 0 {
				// The window is over and nothing came of it: that is the
				// engine's cue to sweep folded changes.
				return out, nil
			}
			// The first wait always happens, so a caller asking for a
			// zero timeout still gets the completions already queued,
			// exactly as the other sources' first wait does.
			ms = 1
		}
		var (
			qty uint32
			key uintptr
			ov  *windows.Overlapped
		)
		err := windows.GetQueuedCompletionStatus(s.port, &qty, &key, &ov, uint32(ms))
		if ov == nil {
			// No completion: either nothing happened before the
			// timeout, which is the engine's cue to sweep folded
			// changes, or the port itself failed, which ends coverage.
			if err == nil || errors.Is(err, windows.WAIT_TIMEOUT) {
				return out, nil
			}
			return out, err
		}
		out = s.completeLocked(out, ov, qty, err)
	}
	return out, nil
}

// Close implements Source: every watch is cancelled, the completions of
// those cancellations are drained, and the port is closed.
func (s *winSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for h := range s.live {
		s.removeLocked(h)
	}
	s.live, s.byPath = nil, nil

	// A buffer is the kernel's until the completion that describes it
	// has been dequeued, so the drain is what releases them. What never
	// arrives is parked below rather than handed back.
	deadline := time.Now().Add(closeDrain)
	for len(s.reads) > 0 && time.Now().Before(deadline) {
		var (
			qty uint32
			key uintptr
			ov  *windows.Overlapped
		)
		err := windows.GetQueuedCompletionStatus(s.port, &qty, &key, &ov, closeWaitMS)
		if ov != nil {
			delete(s.reads, ov)
			continue
		}
		if err != nil && !errors.Is(err, windows.WAIT_TIMEOUT) {
			break
		}
	}
	for ov, w := range s.reads {
		park(w)
		delete(s.reads, ov)
	}
	return windows.CloseHandle(s.port)
}

// issue starts the read that makes a directory's changes come to this
// source. There is always one pending per watch: a completion is what
// the next read is issued from, and a directory with nothing pending is
// a directory that reports nothing.
func (s *winSource) issue(w *winWatch) error {
	w.ov = windows.Overlapped{}
	w.ret = 0
	err := windows.ReadDirectoryChanges(w.handle, &w.buf[0], uint32(len(w.buf)),
		false, notifyFilter, &w.ret, &w.ov, 0)
	// A read that is on its way reports ERROR_IO_PENDING; one that
	// completed so early that the kernel had the answer ready reports
	// success. Both leave a completion on the port.
	if err == nil || errors.Is(err, windows.ERROR_IO_PENDING) {
		return nil
	}
	return err
}

// arm registers a watch as having a read on its way and issues it. The
// registration comes first on purpose: from the moment the read is
// issued the kernel may write into the buffer, and the completion that
// releases it has to find the watch it belongs to. A refused issue
// leaves the registration behind, because a failure deep enough to be
// reported by the filesystem still queues a completion packet — the one
// that says the read failed — and that packet must be matched rather
// than left to arrive for a watch nobody tracks.
func (s *winSource) arm(w *winWatch) error {
	s.reads[&w.ov] = w
	return s.issue(w)
}

// completeLocked handles one finished read: it turns what the kernel
// reported into raw events and starts the next read for that directory.
func (s *winSource) completeLocked(out []rawEvent, ov *windows.Overlapped, qty uint32, err error) []rawEvent {
	w, ok := s.reads[ov]
	if !ok {
		// A completion for a read this source no longer tracks. It can
		// name no path and no handle, so there is nothing to report.
		return out
	}
	delete(s.reads, ov)
	if w.retired {
		return out
	}
	switch {
	case err == nil && qty > 0:
		if int(qty) > len(w.buf) {
			// More bytes than the buffer the read was given: whatever
			// the platform is describing, this source cannot read it,
			// and a slice past the end would take the process down
			// rather than report a gap.
			out = append(out, rawEvent{Op: rawOverflow, Handle: -1})
			return s.rearmLocked(out, w)
		}
		return s.rearmLocked(s.parseLocked(out, w, w.buf[:qty]), w)
	case err == nil:
		// A completion with no bytes in it. The API documents one reason
		// for that — the buffer could not hold everything the directory
		// did, in which case its contents are discarded — and leaves one
		// unsaid: a directory that left while the read was pending has
		// nothing left to report. So the filesystem is asked which it
		// was, and the answer decides between a watch that has to keep
		// reporting and a directory that has to be reported gone.
		if !isDir(w.dir) {
			return append(out, rawEvent{Op: rawDeleteSelf, Handle: w.id})
		}
		out = append(out, rawEvent{Op: rawOverflow, Handle: -1})
		return s.rearmLocked(out, w)
	case errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR):
		// The kernel could not record everything the directory did. How
		// much is missing is exactly what it does not say, which is what
		// the unbounded gap reason exists for.
		out = append(out, rawEvent{Op: rawOverflow, Handle: -1})
		return s.rearmLocked(out, w)
	case !isDir(w.dir):
		// The directory is gone, and its own handle is the only thing
		// left to say so: a watch root has no parent watch to report its
		// removal, and the parent of anything else reports it as well.
		return append(out, rawEvent{Op: rawDeleteSelf, Handle: w.id})
	default:
		// The directory is there and the read still failed: this source
		// can no longer see inside it, and coverage ends where the
		// failure is. The engine turns this into a watch-lost gap and
		// tears the subtree down; the handle goes when it asks.
		return append(out, rawEvent{Op: rawIgnored, Handle: w.id})
	}
}

// rearmLocked starts the next read for a watch whose completion has just
// been consumed. It is what keeps a watched directory reporting.
func (s *winSource) rearmLocked(out []rawEvent, w *winWatch) []rawEvent {
	if err := s.arm(w); err != nil {
		if !isDir(w.dir) {
			return append(out, rawEvent{Op: rawDeleteSelf, Handle: w.id})
		}
		return append(out, rawEvent{Op: rawIgnored, Handle: w.id})
	}
	return out
}

// parseLocked turns one completed read into raw events. The bytes are a
// packed sequence of change records, each naming the entry it is about
// relative to the directory the read was issued on.
func (s *winSource) parseLocked(out []rawEvent, w *winWatch, data []byte) []rawEvent {
	// old holds a rename's source half until the record after it says
	// whether the destination came with it. Two records that are not
	// adjacent are not a pair, and this platform gives no cookie to join
	// them with: an unmatched half is reported as the removal or the
	// appearance it also is, never guessed into a rename, because a
	// pairing invented here would name an old path that never moved.
	var (
		old       string
		oldIsDir  bool
		oldCookie uint32
		haveOld   bool
	)
	flushOld := func() {
		if !haveOld {
			return
		}
		out = append(out, rawEvent{Op: rawMovedFrom, Handle: w.id, Name: old, Cookie: oldCookie, IsDir: oldIsDir})
		haveOld = false
	}

	for offset := 0; ; {
		if offset+notifyHeaderSize > len(data) {
			if offset != len(data) {
				// A partial record at the end: the buffer is not the
				// list of records it claims to be.
				out = append(out, rawEvent{Op: rawOverflow, Handle: -1})
			}
			break
		}
		next := int(binary.LittleEndian.Uint32(data[offset:]))
		action := binary.LittleEndian.Uint32(data[offset+4:])
		nameLen := int(binary.LittleEndian.Uint32(data[offset+8:]))
		extent := len(data) - offset
		if next > 0 {
			extent = next
		}
		if offset+next > len(data) || nameLen%2 != 0 || notifyHeaderSize+nameLen > extent {
			// A record that runs past the bytes the kernel said it
			// wrote. What it described cannot be trusted, and a journal
			// that reported half of it would be worse than one that
			// reports the loss.
			out = append(out, rawEvent{Op: rawOverflow, Handle: -1})
			break
		}
		name := windows.UTF16ToString(
			unsafe.Slice((*uint16)(unsafe.Pointer(&data[offset+notifyHeaderSize])), nameLen/2))

		switch action {
		case windows.FILE_ACTION_RENAMED_OLD_NAME:
			flushOld()
			haveOld, old, oldIsDir, oldCookie = true, name, w.isDirName(name), s.nextCookie()
		case windows.FILE_ACTION_RENAMED_NEW_NAME:
			if haveOld {
				out = append(out,
					rawEvent{Op: rawMovedFrom, Handle: w.id, Name: old, Cookie: oldCookie, IsDir: oldIsDir},
					rawEvent{Op: rawMovedTo, Handle: w.id, Name: name, Cookie: oldCookie, IsDir: w.isDirName(name)})
				haveOld = false
			} else {
				// Moved in from outside this directory: as far as this
				// watch can tell, the entry appeared.
				out = append(out, rawEvent{Op: rawMovedTo, Handle: w.id, Name: name, IsDir: w.isDirName(name)})
			}
		case windows.FILE_ACTION_ADDED:
			flushOld()
			out = append(out, rawEvent{Op: rawCreate, Handle: w.id, Name: name, IsDir: w.isDirName(name)})
		case windows.FILE_ACTION_REMOVED:
			flushOld()
			// The entry is gone, so nothing can be asked what it was;
			// the subscription is what answers for the directories.
			out = append(out, rawEvent{Op: rawDelete, Handle: w.id, Name: name, IsDir: s.isWatchedDir(w, name)})
		case windows.FILE_ACTION_MODIFIED:
			flushOld()
			if s.isWatchedDir(w, name) {
				// A directory this source watches, timestamped because
				// something inside it changed. Its own watch is what
				// reports that change, in detail, and calling it a write
				// would say a directory has content that can be written.
				break
			}
			out = append(out, rawEvent{Op: rawModify, Handle: w.id, Name: name})
		default:
			flushOld()
		}
		if next == 0 {
			break
		}
		offset += next
	}
	flushOld()
	return out
}

// nextCookie hands out the cookies this source pairs its own renames
// with. It never returns zero, which is reserved for "the other half of
// this move is not something I can see": an arrival that nobody paired
// must not match a departure nobody paired either.
func (s *winSource) nextCookie() uint32 {
	s.cookie++
	if s.cookie == 0 {
		s.cookie = 1
	}
	return s.cookie
}

// isDirName reports whether the entry a record names is a directory.
//
// The kernel does not say: inotify hands over an IN_ISDIR bit and a
// kqueue directory record carries the entry's type, while a change
// record here names its subject and stops. One Lstat per arrival is
// what answering costs — the same stat the engine takes when it emits
// the event — and the answer is not optional for an arrival: a
// directory the engine does not recognize as one is a subtree whose
// contents are never watched, which is exactly the silent hole this
// package exists to avoid.
func (w *winWatch) isDirName(name string) bool {
	if name == "" {
		return false
	}
	info, err := os.Lstat(filepath.Join(w.dir, name))
	return err == nil && info.IsDir()
}

// isWatchedDir reports whether a record's name is a directory this
// source holds a subscription on. It answers two questions a removal
// has no way to ask the filesystem: a directory that left was one, and
// a modification of one describes its entry set changing — which its
// own watch reports — rather than a file whose content changed.
func (s *winSource) isWatchedDir(w *winWatch, name string) bool {
	if name == "" {
		return false
	}
	_, ok := s.byPath[filepath.Join(w.dir, name)]
	return ok
}

// openPath prepares a path for CreateFile. Paths beyond MAX_PATH need
// the extended-length prefix, which the os package applies to the calls
// it makes and a direct call has to apply itself; anything short, or
// already prefixed, is returned as it came.
func openPath(path string) string {
	const maxNormalPath = 248
	if len(path) < maxNormalPath || !filepath.IsAbs(path) {
		return path
	}
	if len(path) >= 4 && (path[:4] == `\\?\` || path[:4] == `\??\`) {
		return path
	}
	// The extended form takes no forward slashes: the prefix turns path
	// parsing off, so a separator the filesystem does not use would
	// become part of a name.
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

// unreclaimed keeps the watches of reads that were cancelled and never
// came back. The kernel owns a watch's buffer and its OVERLAPPED until
// the completion that describes it has been dequeued, so memory whose
// completion never arrives cannot be given back: parking it keeps it
// reachable rather than letting the collector hand the kernel a page it
// may still write to. Nothing is parked in a run where cancellations
// complete, which is every run observed.
var (
	unreclaimedMu sync.Mutex
	unreclaimed   []*winWatch
)

func park(w *winWatch) {
	unreclaimedMu.Lock()
	unreclaimed = append(unreclaimed, w)
	unreclaimedMu.Unlock()
}
