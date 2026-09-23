package journal

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

const (
	// pendingTTL is how long a folded change may wait for its completion
	// edge (a close_write) before the engine reports it anyway. It is
	// also the de-duplication window that keeps a readdir seed and the
	// inotify event it raced with from being reported as two
	// appearances.
	//
	// The value is a trade-off, not a precision knob: writers that hold
	// a file open for a long time (an append-only log, an mmap'd
	// output) have no completion edge to wait for, and reporting their
	// writes only on close would hide them for as long as the process
	// lives.
	pendingTTL = 250 * time.Millisecond
	// maxPending bounds the fold table. A process creating files it
	// never closes would otherwise grow it without limit; the oldest
	// entry is reported early instead, which is the same statement,
	// just sooner.
	maxPending = 4096
	// maxCookies bounds the unmatched-rename table. A rename whose
	// destination is outside the watched tree never pairs, so its
	// source path would otherwise linger; the bounded table turns it
	// into a reported removal after the grace period.
	maxCookies = 128
)

// change is one folded, path-resolved observation. The engine turns it
// into a sandbox.WriteEvent: it assigns the seq, stats the path for
// identity and size, and rewrites the path into the journal's shape.
type change struct {
	op      sandbox.FileOp
	path    string // absolute host path
	oldPath string // absolute host path; FileOpRename only
	isDir   bool
}

// pend is the fold state of one path: the net change observed so far.
type pend struct {
	op sandbox.FileOp
	// emitted marks a reported appearance kept as a tombstone: a
	// directory reports its create the moment it is seen, and the
	// readdir seed of a fresh parent may describe the same directory
	// again. `wrote` is what distinguishes "created and written" (one
	// create event) from "close without a write" (nothing at all).
	emitted  bool
	wrote    bool
	isDir    bool
	deadline time.Time
}

// cookied is an unmatched rename source.
type cookied struct {
	path     string
	isDir    bool
	deadline time.Time
}

// fold turns a raw platform event stream into net state changes: one
// event per path per change, never a syscall trace.
//
// The rules, in one place:
//
//   - create + modify + close_write collapses into a single create (or
//     write) reported on the close_write edge.
//   - a path that appears and disappears again inside the window
//     reports nothing: the net change is zero.
//   - a rename is one event. The source side is remembered by its
//     pairing cookie and the destination publishes it; an appearance
//     that had not been reported yet is published on the way out, since
//     the entry survived as an artifact under a new name. A source that
//     never pairs (the destination is outside the watched tree) is
//     reported as a removal when its grace period expires.
//   - close_write without a preceding modify on an existing path is
//     not a content change and reports nothing (touch, open+close).
//   - attribute changes and reads never reach this code at all.
//
// A fold is single-goroutine state, guarded by the journal mutex. Every
// method takes the output slice by pointer so the caller owns emission
// order within one batch.
type fold struct {
	ttl     time.Duration
	ops     []sandbox.FileOp
	pending map[string]*pend
	cookies map[uint32]cookied
	moves   map[string]time.Time
}

func newFold(ops []sandbox.FileOp, ttl time.Duration) *fold {
	if ttl <= 0 {
		ttl = pendingTTL
	}
	return &fold{
		ttl:     ttl,
		ops:     ops,
		pending: make(map[string]*pend),
		cookies: make(map[uint32]cookied),
		moves:   make(map[string]time.Time),
	}
}

// wants reports whether an op survives the configured filter. Filtering
// happens here, after folding: a filtered event is not a missing event,
// so it must never turn into a gap.
func (f *fold) wants(op sandbox.FileOp) bool {
	if f.ops == nil {
		return true
	}
	for _, o := range f.ops {
		if o == op {
			return true
		}
	}
	return false
}

// Observe folds one raw event whose absolute host path is path. Gaps
// are not part of a single observation — they come from [fold.Sweep],
// which is where a deadline turns "unknown" into a reported loss.
func (f *fold) Observe(ev rawEvent, path string, now time.Time) []change {
	var out []change
	switch ev.Op {
	case rawCreate:
		f.appear(&out, path, ev.IsDir, now)
	case rawModify:
		f.touch(&out, path, now)
	case rawCloseWrite:
		f.complete(&out, path)
	case rawDelete:
		f.gone(&out, path)
	case rawMovedFrom:
		f.depart(&out, path, ev.Cookie, ev.IsDir, now)
	case rawMovedTo:
		f.arrive(&out, path, ev.Cookie, ev.IsDir, now)
	case rawMoveSelf:
		f.moves[path] = now.Add(f.ttl)
	}
	return out
}

// Seed reports an entry found by the readdir pass that follows a newly
// registered directory. Between "the directory was created" and "its
// watch was installed" writes are invisible, and this is what closes
// that window.
//
// A seeded entry is an appearance like any other, so it waits out the
// same dedupe window: everything the kernel queued for that appearance
// between the watch and the readdir is still in flight, and reporting
// the seed before those events have been folded would report one
// appearance twice. The entry is published promptly when they arrive
// (their completion edge is the one the seed cannot have), and on the
// first sweep past the window when the path existed before the watch
// and no events are coming at all.
func (f *fold) Seed(out *[]change, path string, isDir bool, now time.Time) {
	if _, ok := f.pending[path]; ok {
		return
	}
	f.makeRoom(out)
	p := &pend{op: sandbox.FileOpCreate, isDir: isDir, deadline: now.Add(f.ttl)}
	f.pending[path] = p
	if isDir {
		f.publish(out, p, path, now)
	}
}

// Sweep reports folded changes whose deadline expired: pending changes
// that never got a completion edge, rename sources that never paired,
// and directories whose own watch reported a move and were never
// explained by a pairing.
//
// It is called with the same clock the engine feeds Observe, so a test
// can drive it deterministically.
func (f *fold) Sweep(out *[]change, now time.Time) []sandbox.JournalGapReason {
	for path, p := range f.pending {
		if now.Before(p.deadline) {
			continue
		}
		delete(f.pending, path)
		if !p.emitted {
			*out = append(*out, change{op: p.op, path: path, isDir: p.isDir})
		}
	}

	// Rename sources first: a directory that left the tree is reported
	// here, and the engine's teardown of its subtree clears the pending
	// move entries that would otherwise duplicate the removal below.
	removed := make(map[string]bool)
	for cookie, c := range f.cookies {
		if now.Before(c.deadline) {
			continue
		}
		delete(f.cookies, cookie)
		*out = append(*out, change{op: sandbox.FileOpRemove, path: c.path, isDir: c.isDir})
		removed[c.path] = true
	}

	var gaps []sandbox.JournalGapReason
	for path, deadline := range f.moves {
		if now.Before(deadline) {
			continue
		}
		delete(f.moves, path)
		if removed[path] {
			continue
		}
		// A moved directory nobody claimed: it left the watched tree,
		// and what happens to it now is unknowable from here.
		*out = append(*out, change{op: sandbox.FileOpRemove, path: path, isDir: true})
		gaps = append(gaps, sandbox.JournalGapWatchLost)
	}
	return gaps
}

// Flush reports everything still folded at freeze time: the journal
// stops watching, so a pending change will never get its completion
// edge, and an unpaired rename will never be resolved.
func (f *fold) Flush(out *[]change) {
	for path, p := range f.pending {
		delete(f.pending, path)
		if !p.emitted {
			*out = append(*out, change{op: p.op, path: path, isDir: p.isDir})
		}
	}
	for cookie, c := range f.cookies {
		delete(f.cookies, cookie)
		*out = append(*out, change{op: sandbox.FileOpRemove, path: c.path, isDir: c.isDir})
	}
	for path := range f.moves {
		delete(f.moves, path)
		*out = append(*out, change{op: sandbox.FileOpRemove, path: path, isDir: true})
	}
}

// ResolveMove forgets a reported directory move. The engine calls it
// when a pairing explained the move — in-tree renames, and subtrees the
// engine tore down itself — so it reports neither a removal nor a gap.
func (f *fold) ResolveMove(path string) {
	delete(f.moves, path)
}

// Reparent rewrites the folded state of a whole subtree that moved. The
// inotify watches follow the inode, so a directory rename changes the
// path of every entry below it without any further event.
func (f *fold) Reparent(old, new string) {
	rewrite := func(path string) (string, bool) {
		if path == old {
			return new, true
		}
		if strings.HasPrefix(path, old+separator) {
			return new + path[len(old):], true
		}
		return "", false
	}
	move := func(m map[string]*pend) {
		for path, p := range m {
			if next, ok := rewrite(path); ok {
				delete(m, path)
				m[next] = p
			}
		}
	}
	move(f.pending)
	for path, deadline := range f.moves {
		if next, ok := rewrite(path); ok {
			delete(f.moves, path)
			f.moves[next] = deadline
		}
	}
	for cookie, c := range f.cookies {
		if next, ok := rewrite(c.path); ok {
			c.path = next
			f.cookies[cookie] = c
		}
	}
}

// Forget drops the folded state of a subtree the engine stopped
// watching, so tombstones and deadlines cannot outlive the watch.
func (f *fold) Forget(prefix string) {
	drop := func(m map[string]*pend) {
		for path := range m {
			if path == prefix || strings.HasPrefix(path, prefix+separator) {
				delete(m, path)
			}
		}
	}
	drop(f.pending)
	for path := range f.moves {
		if path == prefix || strings.HasPrefix(path, prefix+separator) {
			delete(f.moves, path)
		}
	}
	for cookie, c := range f.cookies {
		if c.path == prefix || strings.HasPrefix(c.path, prefix+separator) {
			delete(f.cookies, cookie)
		}
	}
}

// appear folds an entry appearing in a watched directory.
func (f *fold) appear(out *[]change, path string, isDir bool, now time.Time) {
	if p, ok := f.pending[path]; ok {
		// Already tracked. A readdir seed and the inotify create it
		// raced with describe one appearance, and the second create of
		// a path that is still pending is the same file again.
		if isDir {
			p.isDir = true
		}
		return
	}
	f.makeRoom(out)
	p := &pend{op: sandbox.FileOpCreate, isDir: isDir, deadline: now.Add(f.ttl)}
	f.pending[path] = p
	if isDir {
		// Directories have no completion edge: the create is the whole
		// statement, and the engine uses it to register the subtree.
		f.publish(out, p, path, now)
	}
}

// touch folds a content change on a path.
func (f *fold) touch(out *[]change, path string, now time.Time) {
	p, ok := f.pending[path]
	if !ok {
		f.makeRoom(out)
		p = &pend{op: sandbox.FileOpWrite, deadline: now.Add(f.ttl)}
		f.pending[path] = p
	}
	p.wrote = true
	// A create keeps its op: "created and written" is one event.
}

// complete folds the close_write completion edge.
func (f *fold) complete(out *[]change, path string) {
	p, ok := f.pending[path]
	if !ok {
		// A close on a path nobody opened for writing: no content
		// change was observed, so there is nothing to report.
		return
	}
	delete(f.pending, path)
	if p.emitted {
		return
	}
	if p.op != sandbox.FileOpCreate && !p.wrote {
		// Opened, touched (utimes, chmod) and closed without a single
		// write. Reporting this would be a false positive: the file's
		// bytes never changed.
		return
	}
	*out = append(*out, change{op: p.op, path: path, isDir: p.isDir})
}

// gone folds a removal.
func (f *fold) gone(out *[]change, path string) {
	p, ok := f.pending[path]
	if ok {
		delete(f.pending, path)
		if !p.emitted && p.op == sandbox.FileOpCreate {
			// Appeared and disappeared inside the window: a temporary
			// file doing its job, not an artifact. The net change is
			// zero, and reporting a create for it would be a phantom.
			return
		}
	}
	delete(f.moves, path)
	*out = append(*out, change{op: sandbox.FileOpRemove, path: path, isDir: ok && p.isDir})
}

// depart folds the source side of a rename: the path itself is not
// reported yet, because a rename is one event and its destination has
// not been seen.
//
// An appearance that had not been reported yet is published here, before
// the move. The entry existed, and it survived the rename as an artifact
// under a new name, so the stream says "created, then moved" — the shape
// a writer's close edge produces when it comes before the move. Only a
// *deletion* of an unreported appearance is silent ([fold.gone]): there
// the net change really is zero, because nothing was produced.
func (f *fold) depart(out *[]change, path string, cookie uint32, isDir bool, now time.Time) {
	if p, ok := f.pending[path]; ok {
		delete(f.pending, path)
		if !p.emitted && p.op == sandbox.FileOpCreate {
			*out = append(*out, change{op: sandbox.FileOpCreate, path: path, isDir: p.isDir})
		}
	}
	f.remember(cookie, cookied{
		path:     path,
		isDir:    isDir,
		deadline: now.Add(f.ttl),
	})
}

// arrive folds the destination side of a rename.
func (f *fold) arrive(out *[]change, path string, cookie uint32, isDir bool, now time.Time) {
	c, ok := f.cookies[cookie]
	if !ok {
		// Moved in from outside the watched set: as far as this journal
		// is concerned the path appeared.
		f.appear(out, path, isDir, now)
		return
	}
	delete(f.cookies, cookie)
	// The destination's own previous content is superseded by the move,
	// and the source's pending entry left with the source.
	delete(f.pending, path)
	f.ResolveMove(c.path)
	*out = append(*out, change{
		op:      sandbox.FileOpRename,
		path:    path,
		oldPath: c.path,
		isDir:   isDir || c.isDir,
	})
}

// publish reports a folded change now and keeps a tombstone so the
// duplicate description of the same appearance (the readdir seed and
// the queued inotify event) does not report it twice.
func (f *fold) publish(out *[]change, p *pend, path string, now time.Time) {
	p.emitted = true
	p.deadline = now.Add(f.ttl)
	*out = append(*out, change{op: p.op, path: path, isDir: p.isDir})
}

// remember records an unmatched rename source, evicting the oldest
// boundary when the table is full so a rename that never pairs still
// turns into a removal.
func (f *fold) remember(cookie uint32, c cookied) {
	if _, ok := f.cookies[cookie]; !ok && len(f.cookies) >= maxCookies {
		var oldest uint32
		var at time.Time
		first := true
		for key, existing := range f.cookies {
			if first || existing.deadline.Before(at) {
				oldest, at, first = key, existing.deadline, false
			}
		}
		delete(f.cookies, oldest)
	}
	f.cookies[cookie] = c
}

// makeRoom keeps the fold table bounded: tombstones first (they have
// nothing left to report), then the entry closest to its deadline.
func (f *fold) makeRoom(out *[]change) {
	if len(f.pending) < maxPending {
		return
	}
	for path, p := range f.pending {
		if p.emitted {
			delete(f.pending, path)
			return
		}
	}
	var oldest string
	var at time.Time
	for path, p := range f.pending {
		if oldest == "" || p.deadline.Before(at) {
			oldest, at = path, p.deadline
		}
	}
	if oldest == "" {
		return
	}
	p := f.pending[oldest]
	delete(f.pending, oldest)
	*out = append(*out, change{op: p.op, path: oldest, isDir: p.isDir})
}
