package agent

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// DefaultCheckpointCompaction is how many incremental records an engine may
// append after a base before the next boundary rewrites one.
//
// The number is a resume-latency bound, not a memory bound: a load replays the
// base plus at most this many deltas. Every delta is O(writes at one
// boundary), while the base is O(board state), so the ratio is what keeps a
// long run from writing its whole history once per boundary — the cost this
// writer exists to remove.
const DefaultCheckpointCompaction = 32

// CheckpointWriter persists a board at engine boundaries, choosing between a
// self-contained record and an incremental one.
//
// It exists because "persist the board" and "copy the board" were the same act
// at every call site. Three questions decide what a boundary actually costs:
//
//   - does the host persist anything at all ([CheckpointCapabilities.Enabled])?
//   - can it extend a record instead of replacing it
//     ([CheckpointCapabilities.Journal])?
//   - how long since the last full record (the compaction window)?
//
// A writer is per execution (one ExecID) and not safe for concurrent use; an
// engine stamps from one goroutine, at boundaries it already serialises.
//
// Failure is never fatal: the writer returns the store's error and leaves its
// own state untouched, so the next boundary retries from the same position
// rather than extending a record that is not there.
type CheckpointWriter struct {
	execID    string
	caps      CheckpointCapabilities
	writeBase func(context.Context, Checkpoint) error

	mark    BoardMark
	hasBase bool
	appends int
	window  int
}

// NewCheckpointWriter builds a writer that persists through a host. A nil host,
// an empty execID or a host that stores nothing yields a writer whose Stamp is
// a no-op, so callers need no branch of their own.
func NewCheckpointWriter(host Host, execID string) *CheckpointWriter {
	w := &CheckpointWriter{execID: execID, window: DefaultCheckpointCompaction}
	if execID == "" || ptr.IsNil(host) {
		return w
	}
	w.caps = CheckpointCapabilitiesFromHost(host)
	w.writeBase = func(ctx context.Context, cp Checkpoint) error {
		return host.Checkpoint(ctx, cp)
	}
	return w
}

// NewCheckpointWriterForStore builds a writer that persists straight through a
// store. Use it where no host exists — the runtime's session state writes from
// the session, not from an engine, and manufacturing a Host to reach the store
// would only hide which of the two paths is taken.
//
// A nil store yields a no-op writer.
func NewCheckpointWriterForStore(store CheckpointStore, execID string) *CheckpointWriter {
	w := &CheckpointWriter{execID: execID, window: DefaultCheckpointCompaction}
	if execID == "" || ptr.IsNil(store) {
		return w
	}
	w.caps = CheckpointCapabilitiesForStore(store)
	w.writeBase = store.Save
	return w
}

// Enabled reports whether anything would be persisted. Engines may use it to
// skip building a record themselves, but [CheckpointWriter.Stamp] is already a
// no-op in that case.
func (w *CheckpointWriter) Enabled() bool {
	return w != nil && w.caps.Enabled && w.writeBase != nil
}

// WithCompactionWindow overrides how many incremental records may be appended
// between two full ones. A non-positive window keeps the default.
func (w *CheckpointWriter) WithCompactionWindow(every int) *CheckpointWriter {
	if w != nil && every > 0 {
		w.window = every
	}
	return w
}

// Stamp persists the board at the boundary described by meta.
//
// It writes a base record when there is no chain yet, when the window is full,
// when the store cannot take a delta, or when the store says its chain needs a
// base — and an incremental record otherwise. The deltas a writer appends are
// exactly the writes the board reported since the previous successful Stamp, so
// nothing is re-encoded that was already durable.
func (w *CheckpointWriter) Stamp(ctx context.Context, meta CheckpointMeta, board *Board) error {
	if !w.Enabled() || board == nil {
		return nil
	}

	if w.caps.Journal != nil && w.hasBase && w.appends < w.window {
		// Since and Mark together: a write landing between them would be
		// reported by neither, and the record would silently miss it.
		writes, next := board.SinceMark(w.mark)
		err := w.caps.Journal.Append(ctx, meta.Delta(w.execID, writes))
		switch {
		case err == nil:
			w.mark = next
			w.appends++
			return nil
		case errors.Is(err, ErrNeedsBase):
			// The store asked for a base; fall through and give it one.
		default:
			return err
		}
	}

	cp := meta.Checkpoint(w.execID, board)
	if err := w.writeBase(ctx, cp); err != nil {
		return err
	}
	w.mark = board.Mark()
	w.hasBase = true
	w.appends = 0
	return nil
}

// StampSnapshot persists a state whose writes are not tracked by a live board:
// the caller holds the record the store returned (base) and the state to
// persist on top of it (next), as the runtime's session state does.
//
// It is the same policy as [CheckpointWriter.Stamp] — base when there is no
// chain yet or the window is full, an incremental record otherwise — because
// both write through the same journal. A nil base means "nothing durable yet"
// and starts a chain.
func (w *CheckpointWriter) StampSnapshot(ctx context.Context, meta CheckpointMeta, base, next *BoardSnapshot) error {
	if !w.Enabled() || next == nil {
		return nil
	}
	// A nil base means the caller does not know what the store holds — a fresh
	// record, or one whose load failed. Appending in that state would extend a
	// chain the caller cannot see (duplicating history, or leaving a broken
	// chain in place), so it always starts a new base.
	if w.caps.Journal != nil && base != nil && w.hasBase && w.appends < w.window {
		delta := meta.Delta(w.execID, SnapshotDelta(base, next))
		err := w.caps.Journal.Append(ctx, delta)
		switch {
		case err == nil:
			w.appends++
			return nil
		case errors.Is(err, ErrNeedsBase):
			// The store asked for a base; fall through and give it one.
		default:
			return err
		}
	}

	if err := w.writeBase(ctx, meta.record(w.execID, next)); err != nil {
		return err
	}
	w.hasBase = true
	w.appends = 0
	return nil
}
