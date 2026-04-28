package engine

import (
	"context"
	"encoding/json"
	"time"
)

// Checkpoint is the engine-agnostic persistence record produced at a
// safe boundary during execution. Each engine decides what its own
// step marker / payload looks like; this struct only owns the common
// envelope shape.
//
// Engines populate Checkpoint and hand it to [Checkpointer.Checkpoint]
// (the host method). The host is responsible for writing it durably;
// engines must not assume the call has persisted anything.
type Checkpoint struct {
	// ExecID identifies the engine execution this checkpoint belongs
	// to. MUST equal the producing [Run.ID].
	ExecID string `json:"exec_id"`

	// Step is an opaque, engine-defined marker that locates "where"
	// the run is. For graph it is typically the next node id; for a
	// script engine it might be a continuation id. The host treats
	// this as opaque bytes.
	Step string `json:"step,omitempty"`

	// Iteration is an optional monotonic counter for engines that
	// loop (graph re-entry counter, scheduler tick, …). Zero is fine
	// when the engine doesn't track iterations.
	Iteration int `json:"iteration,omitempty"`

	// Board is the Board state at the boundary. Always non-nil.
	Board *BoardSnapshot `json:"board"`

	// Payload is engine-specific extra state the engine wants to
	// persist alongside the Board. Treated as opaque JSON by the
	// store; the producing engine is the only consumer that knows
	// how to decode it.
	Payload json.RawMessage `json:"payload,omitempty"`

	// Attributes mirrors [Run.Attributes] at the time the checkpoint
	// was produced (run id at the agent layer, tenant, graph id, …).
	// Stores may use these for indexing/lookup.
	Attributes map[string]string `json:"attributes,omitempty"`

	// Timestamp is the wall-clock time the engine produced the
	// checkpoint. Hosts may overwrite when they actually persist.
	Timestamp time.Time `json:"timestamp"`
}

// CheckpointStore is the host-side persistence contract. The host's
// [Checkpointer.Checkpoint] implementation typically delegates to a
// CheckpointStore. The interface is intentionally narrow: Save
// persists; Load returns the most-recent persisted record for the
// given exec id, or (nil, nil) if absent. All methods must be safe
// for concurrent use.
type CheckpointStore interface {
	Save(ctx context.Context, cp Checkpoint) error
	Load(ctx context.Context, execID string) (*Checkpoint, error)
}

// CheckpointLister optionally extends [CheckpointStore] with the
// ability to enumerate persisted exec ids. Stores that support
// listing satisfy this interface; agent-level resume / dashboard
// code can type-assert to it.
type CheckpointLister interface {
	List(ctx context.Context) ([]string, error)
}

// CheckpointDeleter optionally extends [CheckpointStore] with the
// ability to delete a single execution's checkpoints. Used by the
// host when a run completes successfully and its checkpoints are no
// longer needed.
type CheckpointDeleter interface {
	Delete(ctx context.Context, execID string) error
}

// NoopCheckpointStore drops every checkpoint and reports no state.
// Use as a default when checkpointing is not configured.
type NoopCheckpointStore struct{}

// Save satisfies [CheckpointStore].
func (NoopCheckpointStore) Save(context.Context, Checkpoint) error { return nil }

// Load satisfies [CheckpointStore].
func (NoopCheckpointStore) Load(context.Context, string) (*Checkpoint, error) {
	return nil, nil
}
