//go:build linux

package journal

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"golang.org/x/sys/unix"
)

// available reports whether this build has a watch source: on Linux,
// inotify via the already-required golang.org/x/sys, with no cgo and no
// extra dependency.
func available() bool { return true }

// defaultBudget derives the watch budget from the kernel's own limit.
//
// A quarter of max_user_watches is a deliberately conservative share:
// the value is per *user*, the daemon may host more than one runner,
// and every desktop session has other inotify users. Running out is not
// silent either way — the journal reports a capacity gap — so the
// conservative number buys headroom, not correctness.
func defaultBudget() int {
	const (
		floor    = 4096
		ceiling  = 65536
		fallback = 16384
	)
	data, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return fallback
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || limit <= 0 {
		return fallback
	}
	share := limit / 4
	if share < floor {
		return floor
	}
	if share > ceiling {
		return ceiling
	}
	return share
}

const (
	// readBufSize is the per-read inotify buffer. 64 KiB holds roughly
	// sixty events; the read loop drains until the queue is empty, so
	// the size only trades syscalls for memory.
	readBufSize = 64 * 1024
	// maxReadsPerPoll bounds one drain so a flooding writer cannot
	// starve the engine's sweep of folded changes.
	maxReadsPerPoll = 64
	// inotifyMask is exactly the set of events the fold understands.
	// Reads (IN_ACCESS, IN_OPEN, IN_CLOSE_NOWRITE) and attribute
	// changes (IN_ATTRIB) are deliberately absent: the kernel never
	// queues what the journal would only have to throw away.
	inotifyMask = unix.IN_CREATE | unix.IN_MODIFY | unix.IN_CLOSE_WRITE |
		unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
		unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
	// addWatchMask adds the boundary flags. IN_DONT_FOLLOW keeps a
	// symlinked directory from dragging the watch (and the reported
	// paths) outside the tree; IN_EXCL_UNLINK drops the tail of events
	// for entries already unlinked, which the fold would report as
	// noise; IN_ONLYDIR refuses anything that is not a directory, so a
	// path swapped for a file between the walk and the watch fails
	// loudly instead of silently.
	addWatchMask = inotifyMask | unix.IN_ONLYDIR | unix.IN_EXCL_UNLINK | unix.IN_DONT_FOLLOW
)

// linuxSource is the inotify implementation of [Source].
//
// Handle is the source's own identifier, never the kernel's watch
// descriptor: the kernel reuses watch descriptors as soon as they are
// freed, and an IN_IGNORED still sitting in the queue would otherwise be
// attributed to the subscription that inherited the number.
type linuxSource struct {
	// mu serialises Poll and Close. It is what makes closing the
	// descriptor safe: an in-flight poll finishes first, and no later
	// poll can start, so the descriptor number cannot be recycled
	// under a stale poll.
	mu     sync.Mutex
	fd     int
	closed bool

	next    Handle
	byWd    map[int32]Handle
	wdOf    map[Handle]int32
	pending map[int32]bool
	buf     []byte
}

func openSource() (Source, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, errdefs.NotAvailablef("sandbox/journal: inotify_init1: %v", err)
	}
	return &linuxSource{
		fd:      fd,
		byWd:    make(map[int32]Handle),
		wdOf:    make(map[Handle]int32),
		pending: make(map[int32]bool),
		buf:     make([]byte, readBufSize),
	}, nil
}

// Add implements Source.
func (s *linuxSource) Add(dir string) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errSourceClosed
	}
	raw, err := unix.InotifyAddWatch(s.fd, dir, addWatchMask)
	if err != nil {
		return 0, err
	}
	wd := int32(raw)
	if previous, ok := s.byWd[wd]; ok {
		// The kernel handed back a descriptor we still mapped. That
		// only happens after the kernel dropped the old watch on its
		// own (a deleted directory) — the engine has already been told
		// through IN_DELETE_SELF — so the stale entry is dropped here.
		delete(s.byWd, wd)
		delete(s.wdOf, previous)
	}
	s.next++
	h := s.next
	s.byWd[wd] = h
	s.wdOf[h] = wd
	return h, nil
}

// Remove implements Source. The kernel answers every successful removal
// with one IN_IGNORED, which is remembered here so the engine is not
// told about a watch it asked to drop itself.
func (s *linuxSource) Remove(h Handle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	wd, ok := s.wdOf[h]
	if !ok {
		return nil
	}
	delete(s.wdOf, h)
	delete(s.byWd, wd)
	// EINVAL means the kernel already dropped the watch on its own (a
	// deleted or unmounted directory), so its IN_IGNORED has already
	// been queued or consumed and no second one will arrive. Recording
	// an expectation anyway would leave pending[wd] set forever, and a
	// later watch that reuses the descriptor would have its own
	// IN_IGNORED taken for the engine's removal — the silently
	// unwatched subtree this package exists to report.
	if _, err := unix.InotifyRmWatch(s.fd, uint32(wd)); err == nil {
		s.pending[wd] = true
	}
	return nil
}

// Poll implements Source.
func (s *linuxSource) Poll(timeout time.Duration) ([]rawEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSourceClosed
	}
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	return s.drainLocked()
}

// Close implements Source.
func (s *linuxSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return unix.Close(s.fd)
}

// drainLocked reads everything the queue holds right now.
func (s *linuxSource) drainLocked() ([]rawEvent, error) {
	var out []rawEvent
	for round := 0; round < maxReadsPerPoll; round++ {
		n, err := unix.Read(s.fd, s.buf)
		switch {
		case err == unix.EAGAIN || err == unix.EWOULDBLOCK:
			return out, nil
		case err == unix.EINTR:
			continue
		case err != nil:
			return out, err
		}
		if n <= 0 {
			return out, nil
		}
		out = s.parseLocked(s.buf[:n], out)
	}
	return out, nil
}

// parseLocked decodes one read buffer. inotify events are a fixed
// header followed by a NUL-padded name, so the fields are read with the
// platform's own byte order rather than by casting the buffer.
func (s *linuxSource) parseLocked(buf []byte, out []rawEvent) []rawEvent {
	const header = unix.SizeofInotifyEvent
	for offset := 0; offset+header <= len(buf); {
		wd := int32(binary.NativeEndian.Uint32(buf[offset : offset+4]))
		mask := binary.NativeEndian.Uint32(buf[offset+4 : offset+8])
		cookie := binary.NativeEndian.Uint32(buf[offset+8 : offset+12])
		nameLen := int(binary.NativeEndian.Uint32(buf[offset+12 : offset+16]))
		offset += header

		var name string
		if nameLen > 0 && offset+nameLen <= len(buf) {
			raw := buf[offset : offset+nameLen]
			offset += nameLen
			for len(raw) > 0 && raw[len(raw)-1] == 0 {
				raw = raw[:len(raw)-1]
			}
			name = string(raw)
		} else {
			offset += nameLen
		}

		out = s.decodeLocked(wd, mask, cookie, name, out)
	}
	return out
}

// decodeLocked maps one inotify record onto the raw events the engine
// consumes. It is split out of parseLocked so the record classes the
// kernel emits only under pressure — a queue overflow, a watch it
// dropped on its own — are testable without provoking the kernel.
func (s *linuxSource) decodeLocked(wd int32, mask, cookie uint32, name string, out []rawEvent) []rawEvent {
	switch {
	case mask&unix.IN_Q_OVERFLOW != 0:
		// The wd is -1 on an overflow event: nothing to resolve.
		return append(out, rawEvent{Op: rawOverflow, Handle: -1})
	case mask&unix.IN_IGNORED != 0:
		if s.pending[wd] {
			// A removal the engine asked for: the events queued
			// before it describe a tree that is gone.
			delete(s.pending, wd)
			return out
		}
		if h, ok := s.byWd[wd]; ok {
			return append(out, rawEvent{Op: rawIgnored, Handle: h})
		}
		return out
	case mask&unix.IN_DELETE_SELF != 0:
		return append(out, rawEvent{Op: rawDeleteSelf, Handle: s.handle(wd)})
	case mask&unix.IN_MOVE_SELF != 0:
		return append(out, rawEvent{Op: rawMoveSelf, Handle: s.handle(wd)})
	}

	h := s.handle(wd)
	isDir := mask&unix.IN_ISDIR != 0
	switch {
	case mask&unix.IN_CREATE != 0:
		out = append(out, rawEvent{Op: rawCreate, Handle: h, Name: name, IsDir: isDir})
	case mask&unix.IN_MODIFY != 0:
		out = append(out, rawEvent{Op: rawModify, Handle: h, Name: name})
	case mask&unix.IN_CLOSE_WRITE != 0:
		out = append(out, rawEvent{Op: rawCloseWrite, Handle: h, Name: name})
	case mask&unix.IN_DELETE != 0:
		out = append(out, rawEvent{Op: rawDelete, Handle: h, Name: name, IsDir: isDir})
	case mask&unix.IN_MOVED_FROM != 0:
		out = append(out, rawEvent{Op: rawMovedFrom, Handle: h, Name: name, Cookie: cookie, IsDir: isDir})
	case mask&unix.IN_MOVED_TO != 0:
		out = append(out, rawEvent{Op: rawMovedTo, Handle: h, Name: name, Cookie: cookie, IsDir: isDir})
	}
	return out
}

// handle resolves a watch descriptor to the subscription the engine
// knows. An unknown descriptor is a watch that was already dropped: the
// events queued behind it describe a tree nobody is tracking.
func (s *linuxSource) handle(wd int32) Handle {
	if h, ok := s.byWd[wd]; ok {
		return h
	}
	return -1
}
