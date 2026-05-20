// Package write owns the write-flow pipeline State and Runner.
// Stages (validate / ingest / resolve / append / validity_close /
// evidence_mirror / project_required / project_optional /
// evolution_after_save) land in Phase B.2 under write/stages/ and
// mutate WriteState; this package owns the State schema so each
// stage stays narrow.
package write

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// WriteState is the per-call workspace threaded through every Stage
// of the write pipeline. Each stage reads inputs from previous
// fields and populates its own output slot; no Stage is allowed to
// route data through context.Value.
//
// Field ownership by Phase B.2 stage (one slot per stage so
// compensators can address exactly what they wrote):
//
//	validate         → Rejected (only on permanent reject)
//	ingest           → KnownEntities, Ingest
//	resolve          → Resolution
//	append           → AppendedFactIDs (Compensator: store.Delete)
//	validity_close   → AppliedCloses  (Compensator: ReopenValidity
//	                   + reproject prior fact via fanout)
//	evidence_mirror  → EvidenceMirrored
//	project_required → RequiredApplied (Compensator: fanout.Forget
//	                   Required, evidence forget)
//	project_optional → OptionalApplied (no compensator: best-effort
//	                   by definition)
//	evolution_after_save → no state mutation; best-effort
type WriteState struct {
	// Inputs — populated by the runner before Run begins.

	// Scope is the canonical write scope. validate ensures
	// RuntimeID is non-empty.
	Scope domain.Scope

	// Facts is the SaveRequest.Facts passthrough channel. The
	// ingest stage hands it to port.Ingestor verbatim.
	Facts []domain.TemporalFact

	// Turns is the SaveRequest.Turns typed-per-turn channel. The
	// LLM extractor (if wired) reads this; the default
	// passthrough extractor ignores it.
	Turns []port.TurnContext

	// ObservedAt anchors relative-time resolution for turns. Zero
	// means "use Now"; historical replay callers set this
	// explicitly.
	ObservedAt time.Time

	// Now is captured once at Pipeline entry so every stage
	// observes the same wall clock (deterministic ValidFrom /
	// ValidTo windows even across slow stages).
	Now time.Time

	// Tier is the SaveRequest importance intent (Phase D.3).
	Tier string

	// RecentMessages / ExistingFactsAnchor are caller-composed LLM
	// context (Phase D.7).
	RecentMessages      []domain.Message
	ExistingFactsAnchor []domain.TemporalFact

	// Stage outputs — populated in order, each stage owns
	// exactly one field group below.

	// KnownEntities is the entity snapshotter's view of this
	// scope at Save time. ingest stage populates it before
	// invoking the Ingestor so the structurizer can fold
	// canonical entity aliases.
	KnownEntities []port.EntitySnapshot

	// Ingest is the structurized + governance-filtered result.
	// Empty Ingest.Facts is a normal outcome — resolve will skip.
	Ingest port.IngestResult

	// Resolution carries (Facts to append, Closes to apply,
	// Drops to record). Resolve stage owns it.
	Resolution domain.Resolution

	// AppendedFactIDs are the IDs the append stage actually wrote
	// to the canonical store. The append compensator uses this
	// slice to issue store.Delete on rollback.
	AppendedFactIDs []string

	// AppliedCloses is the prefix of Resolution.Closes that the
	// validity_close stage successfully wrote. The compensator
	// reopens exactly these (not the unapplied tail) so a
	// half-finished close window is fully reversed.
	AppliedCloses []domain.ValidityClose

	// EvidenceMirrored counts the EvidenceRef rows mirrored into
	// the secondary lookup store. evidence_mirror failures are
	// non-fatal (telemetry only) so there is no compensator.
	EvidenceMirrored int

	// EvidenceMirrorErr captures a non-fatal failure from the
	// evidence_mirror stage. The legacy bridge reads it to
	// reproduce the OnPipeline(stage="evidence", op="mirror")
	// event that legacy runSave fired with Err set — the new
	// rail's StageDiagnostic.Err is reserved for fatal failures
	// (which evidence_mirror never raises).
	EvidenceMirrorErr error

	// RequiredApplied counts facts pushed through every
	// Required-consistency projection's Project call. The
	// project_required compensator forgets these on rollback.
	RequiredApplied int

	// OptionalApplied counts facts pushed through every
	// Optional-consistency projection. No compensator — by
	// design Optional projections are best-effort.
	OptionalApplied int

	// EvolutionErr captures a non-fatal AfterSave failure. Same
	// rationale as EvidenceMirrorErr: the legacy bridge needs to
	// reproduce the OnPipeline(stage="evolution", op="after_save")
	// event legacy runEvolutionAfterSave emitted only when err
	// was non-nil.
	EvolutionErr error

	// FailedStage names the stage whose Run returned the error
	// that triggered the pipeline's reverse-order compensation
	// pass. Stages set it before returning an error so that
	// upstream compensators (notably append) can pick the
	// legacy-equivalent OnProjection event name during the dual-
	// rail transition (B.2 C13 — distinguishes the "validity_close
	// failed" leg from the "project_required failed" leg in the
	// legacy telemetry stream).
	FailedStage string

	// Result is the value the facade hands back to the caller.
	// runner.Run populates FactIDs from AppendedFactIDs once the
	// pipeline ends successfully.
	Result Result

	// Trace is the in-flight SaveTrace. Pipeline.AppendTrace
	// pushes every emitted StageDiagnostic into Trace.Stages.
	// nil is permitted (Save vs SaveExplain): the runner
	// allocates one only when explain is requested.
	Trace *domain.SaveTrace
}

// Result is the write pipeline's terminal output. It carries
// the appended fact IDs (the canonical "what landed" view) plus
// the trace so callers requesting SaveExplain can render the full
// stage walk.
//
// Result intentionally does NOT mirror the public sdk/recall
// SaveResult shape verbatim — the facade composes its public
// SaveResult from WriteState fields directly. Keeping this struct
// internal-only avoids a domain→facade cycle.
type Result struct {
	FactIDs []string
}

// HasWork reports whether the resolve stage produced any facts to
// append. Stages downstream of resolve check this so empty Saves
// short-circuit cleanly (validate / ingest both accept zero-fact
// requests as a normal outcome).
func (s *WriteState) HasWork() bool {
	return s != nil && len(s.Resolution.Facts) > 0
}

// EnsureTrace allocates the SaveTrace if explain output was
// requested but the caller did not pre-populate it. Returning a
// pointer keeps the call-site one-liner whether or not allocation
// happened.
func (s *WriteState) EnsureTrace() *domain.SaveTrace {
	if s.Trace == nil {
		s.Trace = &domain.SaveTrace{}
	}
	return s.Trace
}

// AppendStage is the TraceAppender the runner registers with the
// pipeline framework. It is a no-op when Trace is nil so callers
// requesting only the SaveResult (no explain) pay zero allocation.
func (s *WriteState) AppendStage(d diagnostic.StageDiagnostic) {
	if s.Trace == nil {
		return
	}
	s.Trace.Stages = append(s.Trace.Stages, d)
}
