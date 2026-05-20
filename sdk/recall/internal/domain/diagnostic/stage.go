// Package diagnostic owns the v2 structured pipeline observation
// surface. Every pipeline stage emits a StageDiagnostic; the
// framework is the single point that writes it to trace.Stages and
// pushes it to telemetry. Diagnose / Attribute / Health code reads
// trace.Stages only — no parallel observation channels.
package diagnostic

import "time"

// StageDiagnostic is the structured output of one Stage execution.
// The framework appends it to State.Trace.Stages and calls
// TelemetryHook.OnStage with the same value, so all downstream
// observers see one consistent record per stage.
type StageDiagnostic struct {
	Stage    string
	Phase    Phase
	Order    int
	StartAt  time.Time
	Duration time.Duration

	Status   Status
	Err      string
	ErrClass ErrClass

	// Detail is the strongly-typed per-stage payload. Every stage
	// has its own Detail type implementing StageDetail; using a
	// map[string]any is forbidden.
	Detail StageDetail
}

// Phase identifies which pipeline a stage belongs to.
type Phase string

const (
	PhaseWrite   Phase = "write"
	PhaseRead    Phase = "read"
	PhaseRebuild Phase = "rebuild"
)

// Status captures a stage's terminal state. ShortCircuit is a
// non-error early exit; Skipped marks stages a Conditional decided
// not to run; Compensated marks stages whose Compensator ran after
// a downstream failure.
type Status string

const (
	StatusOK           Status = "ok"
	StatusShortCircuit Status = "short_circuit"
	StatusSkipped      Status = "skipped"
	StatusFailed       Status = "failed"
	StatusCompensated  Status = "compensated"
)

// ErrClass classifies a stage failure so retry orchestrators can
// distinguish permanent (do not retry) from transient (back off
// and retry) errors. Default is unknown — callers decide.
type ErrClass string

const (
	ErrClassUnknown   ErrClass = ""
	ErrClassPermanent ErrClass = "permanent"
	ErrClassTransient ErrClass = "transient"
)

// StageDetail is the marker interface every per-stage Detail
// implements via an unexported method. Keeping it unexported
// prevents foreign packages from declaring their own Detail types
// and bypassing the diagnostic/ home.
type StageDetail interface {
	isStageDetail()
}
