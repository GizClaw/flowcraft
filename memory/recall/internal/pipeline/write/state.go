// Package write owns the write-flow pipeline State and Runner.
// Stages (validate / ingest / resolve / append / validity_close /
// evidence_mirror / project_required / project_optional /
// evolution_after_save) live under write/stages/ and mutate
// WriteState; this package owns the State schema so each stage stays
// narrow.
package write

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// WriteState is the per-call workspace threaded through every Stage
// of the write pipeline. Each stage reads inputs from previous
// fields and populates its own output slot; no Stage is allowed to
// route data through context.Value.
//
// Field ownership by stage (one slot per stage so compensators can
// address exactly what they wrote):
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
//
// Async semantic write lane fields (Mode, AsyncRequestID,
// EpisodeFacts, EvidenceAppliedEpisode, SemanticPending) are populated
// by the four episode-lane stages
// (build_episode → append_episode → project_episode_evidence →
// write_semantic_outbox) when Save runs in WriteModeAsyncSemantic.
// In the synchronous path they remain zero.
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

	// Tier is the SaveRequest importance intent.
	Tier string

	// DiagnosticsIncludeRaw opts into raw dropped-fact payloads in
	// trace/telemetry (SaveExplainDebug). Default Save paths keep
	// drops redacted.
	DiagnosticsIncludeRaw bool

	// RecentMessages / ExistingFactHints are caller-composed LLM
	// context.
	RecentMessages     []domain.Message
	ExistingFactHints  []domain.TemporalFact
	EvidenceWindowRefs []domain.EvidenceWindowRef

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
	// evidence_mirror stage. The stage detail surfaces it via the
	// trace; StageDiagnostic.Err is reserved for fatal failures
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

	// EvolutionErr captures a non-fatal AfterSave failure. The
	// evolution_after_save stage detail surfaces it via the
	// Stages-only trace.
	EvolutionErr error

	// Async lane. These fields stay zero in the synchronous path; they
	// are populated only when the facade dispatches to the async
	// episode runner.

	// Mode mirrors SaveRequest.Mode so episode-lane stages can
	// disambiguate from the synchronous semantic path when sharing
	// a WriteState shape.
	Mode domain.WriteMode

	// AsyncRequestID is the durable work-item key generated by
	// build_episode and propagated through every downstream stage's
	// diagnostic so sync + async trace can be joined (see
	// recall-v2-async-semantic-write.md §7.1).
	AsyncRequestID string

	// EpisodeFacts is the slice of KindEpisode facts built by
	// build_episode and appended by append_episode. append_episode's
	// Compensator deletes these by ID on rollback.
	EpisodeFacts []domain.TemporalFact

	// EvidenceAppliedEpisode flips to true once
	// project_episode_evidence successfully mirrored the episode
	// facts through the evidence projection. The compensator uses
	// this marker to decide whether to issue a Forget on rollback.
	EvidenceAppliedEpisode bool

	// SemanticPending is set true by write_semantic_outbox after a
	// successful Enqueue. It feeds SaveResult.SemanticPending so
	// callers can distinguish "raw episode landed; semantic
	// extraction will run later" from "fully synchronous Save".
	SemanticPending bool

	// SaveOutboxID groups commit-after side-effect jobs for one Save.
	// Set before the canonical locked pipeline runs.
	SaveOutboxID string

	// ScopeGeneration is captured under the scope write lock. Commit-after
	// side-effect jobs use it as a stale-job fence after hard forget / expiry.
	ScopeGeneration uint64

	// SideEffectsEnqueued counts jobs written by enqueue_side_effects.
	SideEffectsEnqueued int

	// GraphDelta is the experimental Observation/Assertion/Link commit unit
	// derived from the resolved write. commit_graph owns this field.
	GraphDelta domain.MemoryGraphDelta

	// RawObservationIDs are turn observations committed before assertion
	// extraction. They intentionally survive extractor failures so raw evidence
	// can be recalled or re-extracted later.
	RawObservationIDs []string

	// SourceEvidenceSpans are canonical, extractable evidence spans resolved by
	// commit_observations from current Turns and explicit EvidenceWindowRefs.
	SourceEvidenceSpans []domain.SourceEvidenceSpan

	// GraphObservationIDs are newly-created observation rows commit_graph wrote.
	// Existing canonical observations are never rewritten in commit_graph; links
	// point at them directly.
	GraphObservationIDs []string
	GraphLinkIDs        []string

	// SemanticDerivationOrigin is stamped onto every appended fact by
	// origin_stamp in the async worker lane. Zero in sync and episode
	// paths.
	SemanticDerivationOrigin domain.FactOrigin

	// FailedStage names the stage whose Run returned the error
	// that triggered the pipeline's reverse-order compensation
	// pass. Stages set it before returning an error so upstream
	// compensators (notably append) can branch on the originating
	// stage when emitting Compensated diagnostics.
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
//
// When the async lane has stamped state.AsyncRequestID (build_episode
// runs first in the episode pipeline), enrich every downstream stage's
// StageDiagnostic so observers can join sync + async traces on a
// single key. Stages that already populated the field via their Detail
// are not overwritten.
func (s *WriteState) AppendStage(d diagnostic.StageDiagnostic) {
	if s.Trace == nil {
		return
	}
	if d.AsyncRequestID == "" && s.AsyncRequestID != "" {
		d.AsyncRequestID = s.AsyncRequestID
	}
	s.Trace.Stages = append(s.Trace.Stages, d)
}
