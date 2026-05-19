package recall

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// executeWrite is the single shared body of the sync ([Memory.Save])
// and async ([handleJob]) ingest paths. Both callers funnel the
// durable "validate scope → extract facts → upsert facts → append
// history" portion through this method so any new step (a new
// validation gate, a new pre-extract hook, a new metric attribute,
// …) lands on BOTH surfaces by construction.
//
// Historical context: the two callers used to maintain parallel
// copies of this logic. The drift surfaced as a class of bugs whose
// canonical examples were #149 (the async path missed the
// WithHistoryStore append) and #165 (the async path skipped
// validateScope on consumption — a durable JobQueue adapter or a
// post-Enqueue policy change could feed handleJob a payload the
// configured policy now rejects).
//
// Error stage: failures are wrapped in [writeStageError] so callers
// can route them to stage-specific telemetry / retry decisions
// without re-deriving the stage from the underlying error message.
// "validate" failures are permanent — see [writeStageError.Permanent].
//
// Bookkeeping ctx: executeWrite does NOT own the post-success
// bookkeeping that follows a successful write (JobQueue.Complete,
// success metrics, etc.). Those steps have ctx-lifecycle constraints
// that differ between the sync and async callers — sync caller
// reuses the user ctx; async caller uses a fresh bookkeeping ctx so
// a workerCtx cancel does not orphan a successful job in 'running'.
// Each caller handles its own post-success path; the durable write
// itself uses the supplied ctx end-to-end.
func (m *lt) executeWrite(
	ctx context.Context,
	scope Scope,
	msgs []llm.Message,
	now time.Time,
) ([]string, []ExtractedFact, error) {
	// 1. Validation gate — runs on BOTH paths. SaveAsync already
	// validates at Enqueue time, but a payload that survived a
	// durable adapter (re-decoded JobPayload), a post-Enqueue
	// policy tighten (requireUserID flipped on), or a direct
	// JobQueue.Enqueue call by an advanced caller would otherwise
	// bypass the gate here at Lease time. Issue #165.
	if err := m.validateScope(scope); err != nil {
		return nil, nil, &writeStageError{stage: writeStageValidate, err: err}
	}
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)
	// 2. Extractor options — pulls WithRecentMessages from the
	// history store and WithExistingFacts from saveWithCtx. Both
	// are best-effort quality boosters; a failure inside
	// buildExtractOpts degrades extraction but doesn't fail the
	// write.
	extractOpts := m.buildExtractOpts(ctx, scope, ns, msgs)
	facts, err := m.cfg.extractor.Extract(ctx, scope, msgs, extractOpts...)
	if err != nil {
		return nil, nil, &writeStageError{stage: writeStageExtract, err: err}
	}
	// 3. Durable write — content hashing, embed batch, supersede,
	// resolver, entity-link Link all happen inside upsertFacts.
	ids, err := m.upsertFacts(ctx, scope, msgs, facts, now)
	if err != nil {
		return nil, nil, &writeStageError{stage: writeStageUpsert, err: err}
	}
	return ids, facts, nil
}

// writeStage enumerates the phases of [executeWrite] so callers can
// route failures to phase-specific telemetry / retry decisions.
type writeStage string

const (
	writeStageValidate writeStage = "validate"
	writeStageExtract  writeStage = "extract"
	writeStageUpsert   writeStage = "upsert"
)

// writeStageError wraps the failure of one [executeWrite] phase so
// callers can recover the phase tag via [errors.As] without
// pattern-matching the error message.
//
// Use [writeStageError.Permanent] to ask whether the failure is
// retry-eligible — currently only "validate" failures are permanent,
// because re-running the same JobPayload through the same gate will
// produce the same rejection.
type writeStageError struct {
	stage writeStage
	err   error
}

func (e *writeStageError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *writeStageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// Stage returns the phase that failed.
func (e *writeStageError) Stage() string {
	if e == nil {
		return ""
	}
	return string(e.stage)
}

// Permanent reports whether re-running [executeWrite] with the same
// inputs is guaranteed to fail with the same error class. Only
// "validate" qualifies today; "extract" and "upsert" are typically
// transient (network, content filter, index hiccup).
func (e *writeStageError) Permanent() bool {
	if e == nil {
		return false
	}
	return e.stage == writeStageValidate
}
