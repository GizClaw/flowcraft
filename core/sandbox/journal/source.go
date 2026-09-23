package journal

import (
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// errSourceClosed tells the engine that a Poll raced with Close, so a
// normal shutdown is not mistaken for a broken source.
var errSourceClosed = errdefs.Abortedf("sandbox/journal: source is closed")

// Handle identifies one directory subscription inside a [Source].
type Handle int

// rawOp is the platform-neutral operation a raw event carries. Mapping
// a platform mask onto these six plus the three self-events is the
// entire job of a Source. Note there is no "attributes" or "access"
// op: those never reach the engine, because a journal of reads and
// chmods would be a journal of false positives.
type rawOp uint8

const (
	// rawCreate: an entry appeared in the subscribed directory.
	rawCreate rawOp = iota
	// rawModify: an entry's content changed.
	rawModify
	// rawCloseWrite: a writer closed an entry it had opened for
	// writing. This is the completion edge — the point at which a
	// file's content stops changing and the fold may report it.
	rawCloseWrite
	// rawDelete: an entry was removed.
	rawDelete
	// rawMovedFrom: an entry left the directory. The event carries the
	// kernel's pairing cookie so the matching rawMovedTo can be
	// reported as one rename.
	rawMovedFrom
	// rawMovedTo: an entry arrived, either moved in from somewhere
	// else (cookie matches a rawMovedFrom) or moved in from outside
	// the watched set (no match).
	rawMovedTo
	// rawMoveSelf: the subscribed directory itself moved.
	rawMoveSelf
	// rawDeleteSelf: the subscribed directory itself was removed. The
	// parent's watch usually reports this as a plain delete first; a
	// top-level watch root has no parent watch, so there it is the only
	// report.
	rawDeleteSelf
	// rawIgnored: the kernel dropped the subscription without the
	// engine asking.
	rawIgnored
	// rawOverflow: the kernel's queue overflowed. What was lost is
	// unknowable, which is why this maps to an unbounded gap rather
	// than to a seq range.
	rawOverflow
)

func (o rawOp) String() string {
	switch o {
	case rawCreate:
		return "create"
	case rawModify:
		return "modify"
	case rawCloseWrite:
		return "close_write"
	case rawDelete:
		return "delete"
	case rawMovedFrom:
		return "moved_from"
	case rawMovedTo:
		return "moved_to"
	case rawMoveSelf:
		return "move_self"
	case rawDeleteSelf:
		return "delete_self"
	case rawIgnored:
		return "ignored"
	case rawOverflow:
		return "overflow"
	default:
		return "unknown"
	}
}

// rawEvent is one observation from a [Source], deliberately unresolved:
// Handle names the subscription, Name the entry inside that directory
// ("" for events about the directory itself). The engine resolves the
// path, because the engine is what keeps directory paths correct when
// a watched subtree moves.
type rawEvent struct {
	Op     rawOp
	Handle Handle
	Name   string
	Cookie uint32
	IsDir  bool
}

// Source is the platform watch primitive — the only OS-specific part of
// the engine. It tracks one subscription per directory and does nothing
// else: no path bookkeeping, no folding, no retention.
type Source interface {
	// Add starts watching dir and returns a handle identifying the
	// subscription. Failure is reported, never swallowed: the engine
	// turns it into a capacity gap.
	Add(dir string) (Handle, error)
	// Remove stops watching h. Removing an unknown handle is not an
	// error, and a later Poll must not report events for it.
	Remove(h Handle) error
	// Poll blocks until at least one raw event is available or timeout
	// elapses, then returns the events ready at that moment. A nil
	// slice with a nil error means "timeout": the engine uses that
	// wakeup to flush folded changes whose completion edge never came.
	// Poll is called from a single goroutine.
	Poll(timeout time.Duration) ([]rawEvent, error)
	// Close releases the platform resources. Poll returns promptly
	// afterwards (with errSourceClosed).
	Close() error
}

// leafSource is an optional [Source] extension for platforms whose
// watch primitive cannot see a file's content change through its
// parent: macOS, where kqueue has no recursive watch and a directory's
// descriptor reports only that its own entry set changed. A source that
// implements it asks the engine for one subscription per non-directory
// entry as well, because the engine is what walks the tree and owns the
// budget:
//
//   - Entries live in the same bookkeeping as directories, so they
//     count against [sandbox.JournalOptions.MaxWatchSet] — which is the
//     honest unit on such a platform: the cost is one descriptor per
//     watched entry, not per directory.
//   - A leaf event carries its own handle, so the engine resolves it to
//     the entry's path exactly like a directory event with an empty
//     name.
//   - Failure to attach one is a capacity gap, except for the race that
//     makes an entry unpairable to a path at all — see [errNoLeaf] and
//     fs.ErrNotExist, both of which the engine treats as "nothing to
//     watch" rather than as a shortfall.
type leafSource interface {
	// AddLeaf starts watching one existing non-directory entry.
	AddLeaf(path string) (Handle, error)
}

// errNoLeaf tells the engine that the entry it asked to watch as a leaf
// is not one: it is gone, it is a directory after all, or it is an
// entry type that has no content to report. It is deliberately not a
// capacity statement — the directory diff that produced the entry keeps
// the watch set honest, and its next scan explains the name either way.
var errNoLeaf = errors.New("sandbox/journal: entry has no content to watch")
