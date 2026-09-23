package sandbox

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// FileOp classifies one observed change inside a runner's write
// boundary. Only write-class operations become events: attribute
// changes (chmod, utimes) and reads never do — reporting them would
// turn "the file was inspected" into "the file was produced".
type FileOp uint8

const (
	// FileOpCreate means the path appeared inside the boundary.
	FileOpCreate FileOp = iota
	// FileOpWrite means the content of an already-existing path
	// changed. Creating a file and writing it in one go reports
	// FileOpCreate, not a create/write pair.
	FileOpWrite
	// FileOpRename means a path moved; OldPath names where it came
	// from. Renames are folded into one event instead of a
	// remove/create pair whenever the backend can pair them.
	FileOpRename
	// FileOpRemove means the path disappeared. It is the one event a
	// consumer needs to retire a chip.
	FileOpRemove
)

func (o FileOp) String() string {
	switch o {
	case FileOpCreate:
		return "create"
	case FileOpWrite:
		return "write"
	case FileOpRename:
		return "rename"
	case FileOpRemove:
		return "remove"
	default:
		return "unknown"
	}
}

// ParseFileOp parses the deployment spelling of a [FileOp]. It is the
// inverse of [FileOp.String] so settings files can name op filters.
func ParseFileOp(s string) (FileOp, bool) {
	switch s {
	case "create":
		return FileOpCreate, true
	case "write":
		return FileOpWrite, true
	case "rename":
		return FileOpRename, true
	case "remove":
		return FileOpRemove, true
	default:
		return 0, false
	}
}

// WriteEvent is one observed change inside the runner's write boundary.
//
// Path is relative to the runner root with "/" separators and no
// leading slash when the change happened under the root; changes under
// an explicitly writable path outside the root carry that cleaned
// absolute path instead. Session is the id of the sandbox session that
// made the change when the backend can attribute it precisely, and ""
// when it cannot: unattributed is a valid answer, a wrong attribution
// is a data-correctness bug. Linux inotify cannot report the writing
// process without elevated privileges, so every event from that source
// is unattributed rather than guessed from timing.
type WriteEvent struct {
	// Seq is the journal cursor: strictly increasing, gap-free for
	// events that were recorded, and replayable from any retained
	// position.
	Seq int64
	// At is when the change was observed, not when the writer made it.
	At time.Time
	Op FileOp
	// Path is the changed path; see the type doc for its shape.
	Path string
	// OldPath is the previous path of a FileOpRename, empty otherwise.
	// Both paths are host path strings, bounded by the platform
	// rather than by this contract — the batch limit bounds event
	// count, not bytes — so a consumer that renders them applies its
	// own byte cap.
	OldPath string
	// IsDir marks directory events. Directory creates are reported so
	// a consumer learns a whole subtree appeared; per-file events
	// inside it follow on their own.
	IsDir bool
	// Dev and Ino are identity hints taken at observation time. They
	// are useful for a consumer's own de-duplication and are not
	// stable across mounts or remounts — never use them as a durable
	// key.
	Dev uint64
	Ino uint64
	// Size is the size stat'ed when the event was emitted; -1 when it
	// could not be determined (typically because the path is gone by
	// then, as with FileOpRemove).
	Size int64
	// Session is the attribute sandbox session id, "" when the source
	// cannot name the writer. See the type doc.
	Session string
}

// JournalGapReason explains why part of the stream is missing.
type JournalGapReason uint8

const (
	// JournalGapOverflow means the kernel's event queue overflowed and
	// events were dropped before the journal saw them.
	JournalGapOverflow JournalGapReason = iota
	// JournalGapRetention means the reader fell behind the bounded
	// retention window: the events are gone from this journal.
	JournalGapRetention
	// JournalGapCapacity means the watch could not be registered —
	// inotify watch limits, file-descriptor limits, or a missing
	// directory. Places nobody is watching produce no events at all,
	// which is exactly what this gap reports.
	JournalGapCapacity
	// JournalGapWatchLost means a watched subtree was moved or
	// detached, so the journal no longer sees it.
	JournalGapWatchLost
	// JournalGapBackend means the backend reported a loss of its own.
	JournalGapBackend
)

func (r JournalGapReason) String() string {
	switch r {
	case JournalGapOverflow:
		return "overflow"
	case JournalGapRetention:
		return "retention"
	case JournalGapCapacity:
		return "capacity"
	case JournalGapWatchLost:
		return "watch_lost"
	case JournalGapBackend:
		return "backend"
	default:
		return "unknown"
	}
}

// JournalGap reports that coverage is incomplete. Events in
// [FirstMissing, NextSeq] may be missing, and the correct reading of
// that range is "unknown" — never "nothing was written".
//
// When the extent of the loss is unknown (a kernel queue overflow does
// not say how many events were dropped), FirstMissing is larger than
// the returned NextSeq: coverage after FirstMissing-1 is not guaranteed,
// and the range itself carries no exact seq. A reader must treat a
// non-nil Gap as "this window is incomplete" rather than as a hole to
// skip.
type JournalGap struct {
	FirstMissing int64
	Reason       JournalGapReason
}

// JournalBatch is one [FileJournal.Read] result. The invariant that
// makes the stream replayable is: every seq in (afterSeq, NextSeq] is
// accounted for — it was either returned in Events or declared lost in
// Gap. Nothing is ever skipped silently.
type JournalBatch struct {
	// Events are the observed changes with Seq > afterSeq, ascending,
	// at most max entries.
	Events []WriteEvent
	// NextSeq is the cursor to pass as afterSeq on the next Read.
	NextSeq int64
	// Gap is non-nil when this read window is incomplete.
	Gap *JournalGap
	// Closed reports that the owning runner was closed and the journal
	// is frozen: no further events will arrive, but the retained
	// residue stays readable until the reader is closed.
	Closed bool
}

// FileJournal is a cursor-based, replayable record of the writes that
// happened under a runner's write boundary. Its lifetime is the
// lifetime of the runner that created it.
//
// A journal is an observation stream, not an enforcement boundary and
// not an audit log: a backend with real write confinement still fails
// the offending write, and the journal reports what it saw, not what
// was attempted. Events may be missing — that is what [JournalGap] is
// for — and the retention window is bounded, so a consumer that needs
// history drains and persists it.
type FileJournal interface {
	// Read returns the events with Seq > afterSeq, at most max of
	// them, in ascending order. It never blocks and never waits for
	// future events; a run that has produced nothing yet simply
	// returns no events. max <= 0 and a negative afterSeq are
	// validation errors, and a cancelled ctx returns ctx.Err().
	//
	// Reading is a per-reader cursor operation: several readers hold
	// independent positions, and re-reading from an earlier cursor
	// replays. Replay is available while the events are inside the
	// retention window.
	Read(ctx context.Context, afterSeq int64, max int) (JournalBatch, error)
	// Close releases the reader's handle. It does not stop the
	// journal; the owning runner does that on Close.
	Close() error
}

// DefaultJournalRetention is the number of events a journal retains for
// replay when JournalOptions.Retention is zero.
const DefaultJournalRetention = 4096

// JournalOptions is the construction-time configuration of a file
// journal. It answers "what to watch and how much to keep"; what a
// consumer does with the events (artifact chips, artifact filters) is
// not the journal's business.
type JournalOptions struct {
	// Exclude lists directories — relative to the runner root — whose
	// entire subtree is never registered for watching. It is the cost
	// control: a watch costs kernel memory per directory, and build
	// trees such as .git, node_modules or dist are noise for a
	// consumer interested in artifacts. nil excludes nothing, and the
	// journal applies no default list of its own: an implicit filter
	// would silently hide writes a deployment believes it is
	// watching.
	Exclude []string
	// Ops filters which event classes are delivered; nil delivers
	// every write-class op. Filtering happens after folding, so it
	// removes noise without losing the events that explain the ones
	// that remain, and it never produces a [JournalGap]: a filtered
	// event is not a missing event.
	Ops []FileOp
	// Retention is how many events the journal keeps readable for
	// replay. <= 0 applies [DefaultJournalRetention]. A full window
	// drops the oldest events and reports JournalGapRetention to the
	// readers that needed them; it never blocks the watcher.
	Retention int
	// MaxWatchSet bounds how many directories the journal registers,
	// which is what turns an unaffordable watch into a reported
	// JournalGapCapacity instead of a silent hole. <= 0 uses the
	// implementation default, reported through
	// [JournalCapabilities.WatchBudget]. On a platform whose watch
	// primitive is charged per entry rather than per directory (kqueue
	// on macOS), every watched entry — file or directory — counts.
	MaxWatchSet int
}

// JournalCapabilities is the journal surface a runner can offer. The
// zero value means "no journal": OpenJournal fails with
// errdefs.NotAvailable and Capabilities.Journal.Enabled is false.
type JournalCapabilities struct {
	// Enabled reports that this runner instance has a journal attached
	// right now — a deployment or construction-time fact, not a
	// platform one. It travels with Capabilities so a caller can
	// explain "this sandbox reports no file writes" without probing.
	Enabled bool
	// WriterID reports that events can name the writing session.
	WriterID bool
	// RenamePairing reports that a move is delivered as one
	// FileOpRename instead of a remove/create pair.
	RenamePairing bool
	// FileIdentity reports that Dev/Ino are filled in.
	FileIdentity bool
	// WatchBudget is the number of watches the journal registers before
	// it starts reporting JournalGapCapacity, in the unit the platform
	// charges for: directories where the kernel watches a directory and
	// reports its entries (inotify), entries — directories and files —
	// where a watch costs a descriptor per entry (kqueue on macOS).
	// Zero means the implementation does not bound itself (kernel
	// limits still surface as gaps).
	WatchBudget int
}

// JournalProvider is implemented by runners that can attach a file
// journal. Runners expose it through [OpenJournal] rather than being
// asserted at every call site: feature discovery by interface assertion
// is exactly what [Runner.Capabilities] exists to avoid, and the
// package-internal assertion below is the single place that knows the
// optional interface.
type JournalProvider interface {
	// OpenJournal returns a new reader on the runner's journal. It
	// fails with errdefs.NotAvailable when no journal is attached, or
	// when the runner is already closed.
	OpenJournal(ctx context.Context) (FileJournal, error)
}

// OpenJournal opens a reader on r's file journal. It is the only
// supported way to obtain one: callers ask the runner, they do not
// assert the optional interface themselves, so an implementation can
// keep the journal behind a decorator chain and behind the wire.
//
// It fails with errdefs.NotAvailable when the runner has no journal —
// either the deployment did not enable one or the platform has no watch
// source — which is a contract-level answer, not an error to work
// around with a directory scan. A runner whose attached journal could
// not start reports that construction failure instead of flattening it
// into NotAvailable.
func OpenJournal(ctx context.Context, r Runner) (FileJournal, error) {
	if r == nil {
		return nil, errdefs.Validationf("sandbox: OpenJournal: nil runner")
	}
	provider, ok := r.(JournalProvider)
	if !ok {
		return nil, errdefs.NotAvailablef(
			"sandbox: runner %T does not provide a file journal", r)
	}
	return provider.OpenJournal(ctx)
}
