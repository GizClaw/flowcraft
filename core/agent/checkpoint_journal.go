package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ErrNeedsBase is what a [CheckpointJournal] returns when an incremental
// record cannot be accepted — its chain is at a store-side limit, or the base
// it would extend is gone. The writer answers by persisting a full record,
// which is always allowed.
var ErrNeedsBase = errors.New("agent: checkpoint journal requires a base record")

// CheckpointJournal is the incremental write side of a checkpoint store: a
// self-contained base record followed by append-only continuations.
//
// Replay is not part of this contract. [CheckpointStore.Load] keeps its shape
// and MUST return the record rebuilt from its base plus every appended delta,
// so a caller that only reads checkpoints never learns whether the store wrote
// one row or a hundred. Implementations that satisfy this interface must keep
// read and write consistent: a delta without its base, or a gap in the chain,
// is corruption and Load must fail rather than return partial state.
//
// Save on the same store keeps [CheckpointStore] semantics and MUST behave
// like [CheckpointJournal.WriteBase] — it replaces the record and discards the
// chain — so mixing the two paths cannot leave a stale delta behind.
type CheckpointJournal interface {
	// WriteBase replaces the record for cp.ExecID and discards any deltas
	// previously appended to it.
	WriteBase(ctx context.Context, cp Checkpoint) error

	// Append extends the record for delta.ExecID with one boundary. It
	// returns [ErrNeedsBase] when the store cannot take another delta.
	Append(ctx context.Context, delta CheckpointDelta) error
}

// CheckpointDelta is the append-only continuation of a base [Checkpoint]: the
// board writes since the previous record plus the boundary metadata that
// changed with them.
//
// It carries the same envelope fields as the base so each row is
// self-describing: the latest record's Steps and Iteration describe the resume
// anchor, and a store that lists deltas can report progress without replaying
// them.
type CheckpointDelta struct {
	// ExecID identifies the execution the delta extends. It MUST match the
	// base record's ExecID.
	ExecID string `json:"exec_id"`

	// Steps is the boundary marker set, replacing the base's when applied.
	Steps []string `json:"steps,omitempty"`

	// Iteration replaces the base's iteration counter when applied.
	Iteration int `json:"iteration,omitempty"`

	// Board is the write set since the previous record.
	Board *BoardDelta `json:"board"`

	// Payload replaces the base's engine payload when non-empty.
	Payload json.RawMessage `json:"payload,omitempty"`

	// Attributes replace the base's attributes when non-empty.
	Attributes map[string]string `json:"attributes,omitempty"`

	// Timestamp is when the boundary was produced.
	Timestamp time.Time `json:"timestamp"`

	// OriginalStartedAt and SpecVersion mirror [Checkpoint]; they are only
	// sent on a delta that has something new to say.
	OriginalStartedAt time.Time `json:"original_started_at,omitzero"`
	SpecVersion       string    `json:"spec_version,omitempty"`
}

// Validate checks the invariants a store may rely on before it appends.
func (d CheckpointDelta) Validate() error {
	if d.ExecID == "" {
		return errdefs.Validation(errors.New("agent checkpoint delta: exec_id is required"))
	}
	if d.Board == nil {
		return errdefs.Validation(errors.New("agent checkpoint delta: board delta is required"))
	}
	if len(d.Payload) > 0 && !json.Valid(d.Payload) {
		return errdefs.Validation(errors.New("agent checkpoint delta: payload is not valid JSON"))
	}
	return nil
}

// ApplyTo applies the delta onto a record, which is how a store rebuilds the
// latest checkpoint from its base and chain. The record's Board must be
// non-nil; Load implementations decode the base first.
func (d CheckpointDelta) ApplyTo(cp *Checkpoint) {
	if cp == nil {
		return
	}
	if cp.Board == nil {
		cp.Board = &BoardSnapshot{}
	}
	d.Board.ApplyToSnapshot(cp.Board)
	cp.Steps = slices.Clone(d.Steps)
	cp.Iteration = d.Iteration
	if len(d.Payload) > 0 {
		cp.Payload = slices.Clone(d.Payload)
	}
	if len(d.Attributes) > 0 {
		cp.Attributes = maps.Clone(d.Attributes)
	}
	if !d.Timestamp.IsZero() {
		cp.Timestamp = d.Timestamp
	}
	if !d.OriginalStartedAt.IsZero() {
		cp.OriginalStartedAt = d.OriginalStartedAt
	}
	if d.SpecVersion != "" {
		cp.SpecVersion = d.SpecVersion
	}
}

// CheckpointMeta is the envelope an engine fills at a persistence boundary,
// independent of how it is stored. Building both shapes from one value is what
// keeps a delta from drifting out of sync with the record it extends.
type CheckpointMeta struct {
	Steps             []string
	Iteration         int
	Payload           json.RawMessage
	Attributes        map[string]string
	Timestamp         time.Time
	OriginalStartedAt time.Time
	SpecVersion       string
}

// Checkpoint builds the self-contained record for board.
func (m CheckpointMeta) Checkpoint(execID string, board *Board) Checkpoint {
	return m.record(execID, board.Snapshot())
}

// record builds the self-contained record around an already captured state.
func (m CheckpointMeta) record(execID string, board *BoardSnapshot) Checkpoint {
	return Checkpoint{
		ExecID:            execID,
		Steps:             slices.Clone(m.Steps),
		Iteration:         m.Iteration,
		Board:             board,
		Payload:           slices.Clone(m.Payload),
		Attributes:        maps.Clone(m.Attributes),
		Timestamp:         m.Timestamp,
		OriginalStartedAt: m.OriginalStartedAt,
		SpecVersion:       m.SpecVersion,
	}
}

// Delta builds the incremental record for writes.
func (m CheckpointMeta) Delta(execID string, writes *BoardDelta) CheckpointDelta {
	return CheckpointDelta{
		ExecID:            execID,
		Steps:             slices.Clone(m.Steps),
		Iteration:         m.Iteration,
		Board:             writes,
		Payload:           slices.Clone(m.Payload),
		Attributes:        maps.Clone(m.Attributes),
		Timestamp:         m.Timestamp,
		OriginalStartedAt: m.OriginalStartedAt,
		SpecVersion:       m.SpecVersion,
	}
}
