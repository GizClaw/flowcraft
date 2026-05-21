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

// Status captures a stage's terminal state. Values:
//
//   - StatusOK: stage Run succeeded; no further action.
//   - StatusShortCircuit: stage requested an early non-error exit
//     (pipeline.ShortCircuit sentinel). Later stages do not run and
//     compensators do NOT fire.
//   - StatusSkipped: stage's Conditional.Skip returned true. The
//     framework records the supplied Detail so observers see why.
//   - StatusDegraded: stage's primary work logically succeeded but a
//     best-effort side effect failed. Emitted via the
//     pipeline.BestEffortFailure wrapper — compensators do NOT fire
//     and the pipeline continues with the next stage.
//   - StatusFailed: stage returned a non-nil, non-sentinel error.
//     Reverse-order compensators fire and the pipeline aborts.
//   - StatusCompensated: stage previously emitted StatusOK and its
//     Compensator subsequently ran (cleanly) after a downstream
//     failure.
type Status string

const (
	StatusOK           Status = "ok"
	StatusShortCircuit Status = "short_circuit"
	StatusSkipped      Status = "skipped"
	StatusDegraded     Status = "degraded"
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

// HasStage reports whether a stage name appears in the trace.
func HasStage(stages []StageDiagnostic, name string) bool {
	for _, st := range stages {
		if st.Stage == name {
			return true
		}
	}
	return false
}
