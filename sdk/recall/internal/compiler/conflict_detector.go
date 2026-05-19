package compiler

import "github.com/GizClaw/flowcraft/sdk/recall/internal/model"

// ConflictAction tells the compiler how to react to a candidate fact
// after deterministic merge-key analysis.
type ConflictAction int

const (
	// ConflictAppend writes the fact as a fresh append (default).
	ConflictAppend ConflictAction = iota
	// ConflictNoop drops the fact (exact duplicate / dedupe).
	ConflictNoop
	// ConflictSupersede records that the fact replaces the listed
	// prior fact IDs. PR-2 wires this into Supersedes only;
	// validity-window close happens at projection-fanout time in a
	// later PR so we keep write-path linear.
	ConflictSupersede
)

// ConflictDecision is the structured output of ConflictDetector.
type ConflictDecision struct {
	Action       ConflictAction
	Reason       string
	SupersedeIDs []string
}

// ConflictDetector inspects a candidate fact and returns the action
// the compiler should take. Phase 1 ships a no-op; Phase 4 layers in
// merge-key-aware detection backed by Store lookups.
type ConflictDetector interface {
	Detect(f model.TemporalFact) ConflictDecision
}

type noopConflictDetector struct{}

func (noopConflictDetector) Detect(model.TemporalFact) ConflictDecision {
	return ConflictDecision{Action: ConflictAppend}
}
