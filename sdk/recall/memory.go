package recall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/fusion"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/materialize"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	entityproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/entity"
	graphproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/graph"
	profileproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/profile"
	relationproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/relation"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	timelineproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/timeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/intent"
	entitysource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/entity"
	graphsource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/graph"
	profilesource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/profile"
	relationsource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/relation"
	retrievalsource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/retrieval"
	timelinesource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/timeline"
	evidencestore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/evidence"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// Memory is the v2 fact-centric facade. See docs §11.1.
type Memory interface {
	Save(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, error)
	Recall(ctx context.Context, scope Scope, query Query) ([]Hit, error)
	Forget(ctx context.Context, scope Scope, factID string) error
	Close() error
}

type memory struct {
	store          temporalstore.Store
	evidenceStore  evidencestore.Store
	retrievalIndex retrieval.Index
	compiler       port.Ingestor
	resolver       port.ConflictResolver
	fanout         *projection.Fanout
	telemetry      port.TelemetryHook

	// projections retains the canonical projection set (in
	// registration order) so RebuildProjection can resolve a
	// projection by name without re-deriving it from fanout.
	projections []port.Projection

	queryCompiler port.IntentCompiler
	planner       port.Planner
	sources       []port.Source
	fuser         port.Fuser
	materializer  port.Materializer
	fusionOpts    port.FusionOptions
	graphEnabled  bool
	reranker      Reranker
	evolution     port.EvolutionRunner

	writeMu    sync.Mutex
	writeLocks map[writeScopeKey]*writeLock
}

type writeScopeKey struct {
	runtimeID string
	userID    string
}

type writeLock struct {
	mu   sync.Mutex
	refs int
}

// New constructs a v2 Memory. The defaults wire a fully in-memory
// stack so callers can exercise the write path without external
// dependencies; production callers swap pieces in via Options.
func New(opts ...Option) (Memory, error) {
	cfg := config{
		telemetry: telemetry.NopHook{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.store == nil {
		cfg.store = temporalstore.NewMemoryStore()
	}
	if cfg.retrievalIndex == nil {
		cfg.retrievalIndex = retrievalmem.New()
	}
	if cfg.compiler == nil {
		stages := ingest.Stages{}
		if cfg.llmExtractor != nil {
			ex := ingest.NewLLMExtractor(cfg.llmExtractor.client)
			for _, opt := range cfg.llmExtractor.tune {
				if opt.apply != nil {
					opt.apply(ex)
				}
			}
			stages.Extractor = ex
		}
		if cfg.governance != nil {
			stages.Governance = cfg.governance
		}
		cfg.compiler = ingest.New(stages)
	}
	if !cfg.resolverSet {
		cfg.resolver = ingest.NewResolver()
	}

	var retrievalProjOpts []retrievalproj.Option
	if cfg.embedder != nil {
		retrievalProjOpts = append(retrievalProjOpts, retrievalproj.WithEmbedder(cfg.embedder))
	}
	retrievalProj, err := retrievalproj.New(cfg.retrievalIndex, retrievalProjOpts...)
	if err != nil {
		return nil, fmt.Errorf("recall.New: %w", err)
	}

	entityProj := entityproj.New()
	timelineProj := timelineproj.New()
	relationProj := relationproj.New()
	profileProj := profileproj.New()
	projections := []port.Projection{
		retrievalProj, entityProj,
		timelineProj, relationProj, profileProj,
	}
	var graphProj *graphproj.Projection
	if cfg.graphEnabled {
		graphProj = graphproj.New()
		projections = append(projections, graphProj)
	}
	projections = append(projections, cfg.extraProjections...)

	// Default read-path wiring uses the same canonical backends that
	// Save just wrote into: retrieval source on the retrieval index,
	// entity source on the entity projection's read-only Lookup.
	qc := cfg.queryCompiler
	if qc == nil {
		qc = intent.Default()
	}
	planr := cfg.planner
	if planr == nil {
		rb := planner.New()
		if cfg.graphEnabled {
			rb.GraphEnabled = true
		}
		planr = rb
	} else if cfg.graphEnabled {
		if rb, ok := planr.(*planner.RuleBased); ok {
			rb.GraphEnabled = true
		}
	}
	srcs := append([]port.Source(nil), cfg.sources...)
	if len(srcs) == 0 {
		var retrievalSrcOpts []retrievalsource.Option
		if cfg.embedder != nil {
			retrievalSrcOpts = append(retrievalSrcOpts, retrievalsource.WithEmbedder(cfg.embedder))
		}
		srcs = []port.Source{
			retrievalsource.New(cfg.retrievalIndex, retrievalSrcOpts...),
			entitysource.New(entityProj),
			relationsource.New(relationProj),
			profilesource.New(profileProj),
			timelinesource.New(timelineProj),
		}
		if graphProj != nil {
			srcs = append(srcs, graphsource.New(graphProj))
		}
	}
	fuser := cfg.fuser
	if fuser == nil {
		fuser = fusion.WeightedRRF{}
	}
	mat := cfg.materializer
	if mat == nil {
		mat = materialize.New(cfg.store, cfg.telemetry)
	}
	fusionOpts := cfg.fusionOpts
	if fusionOpts.Weights == nil {
		fusionOpts.Weights = map[string]float64{
			planner.SourceRetrieval: planner.WeightRetrieval,
			planner.SourceEntity:    planner.WeightEntity,
			planner.SourceRelation:  planner.WeightRelation,
			planner.SourceProfile:   planner.WeightProfile,
			planner.SourceTimeline:  planner.WeightTimeline,
		}
		if cfg.graphEnabled {
			fusionOpts.Weights[planner.SourceGraph] = planner.WeightGraph
		}
	}

	return &memory{
		store:          cfg.store,
		evidenceStore:  cfg.evidenceStore,
		retrievalIndex: cfg.retrievalIndex,
		compiler:       cfg.compiler,
		resolver:       cfg.resolver,
		fanout:         projection.New(projections, cfg.telemetry),
		telemetry:      cfg.telemetry,
		projections:    projections,
		queryCompiler:  qc,
		planner:        planr,
		sources:        srcs,
		fuser:          fuser,
		materializer:   mat,
		fusionOpts:     fusionOpts,
		graphEnabled:   cfg.graphEnabled,
		reranker:       cfg.reranker,
		evolution:      cfg.evolution,
		writeLocks:     make(map[writeScopeKey]*writeLock),
	}, nil
}

// Save runs the canonical write pipeline with strict transactional
// semantics:
//
//	compile -> conflict resolve -> store.Append -> UpdateValidity
//	         -> fanout.ProjectRequired -> fanout.ProjectOptional
//
// Conflict resolution runs against a read-only store view, so the
// compiler stays free of write side-effects. Resolution emits two
// disjoint outputs:
//
//   - Facts to append (already include Supersedes pointers for
//     state/preference revisions).
//   - Validity closes to apply to prior facts AFTER the new facts
//     have been appended (so the ledger never carries a closed
//     fact pointing at a not-yet-written successor).
//
// If validity close fails after Append succeeds, Save best-effort
// deletes the just-appended facts and returns the original error so
// the ledger does not end up with an orphan close.
//
// If a required projection fails the call rolls back: best-effort
// fanout.ForgetRequired/Optional cleans up partially-projected
// state, then store.Delete drops the canonical fact, and the
// original projection error is returned. Cleanup failures are only
// reported through the telemetry hook so the user-visible error
// stays attributable to the original cause.
//
// Optional projections run after required ones; their failure does
// not affect the Save outcome (telemetry only).
func (m *memory) Save(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, error) {
	res, _, err := m.runSave(ctx, scope, req, false)
	return res, err
}

// SaveExplain runs the canonical write pipeline like Save and also
// returns the compiled facts and compiler drops so callers can run
// diagnostics on the extractor / compiler / resolver stages.
func (m *memory) SaveExplain(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, SaveTrace, error) {
	return m.runSave(ctx, scope, req, true)
}

func (m *memory) runSave(ctx context.Context, scope Scope, req SaveRequest, withTrace bool) (SaveResult, SaveTrace, error) {
	var trace SaveTrace
	if err := ctx.Err(); err != nil {
		return SaveResult{}, trace, err
	}
	if scope.RuntimeID == "" {
		return SaveResult{}, trace, errdefs.Validationf("recall.Save: scope.runtime_id is required")
	}

	stageStarted := time.Now()
	knownEntities := m.snapshotKnownEntities(scope)
	compiled, err := m.compiler.Compile(ctx, port.IngestInput{
		Scope:         scope,
		Facts:         req.Facts,
		Turns:         req.Turns,
		ObservedAt:    req.ObservedAt,
		KnownEntities: knownEntities,
	})
	m.emitPipeline(scope, "compiler", "compile", len(compiled.Facts), stageStarted, err)
	if err != nil {
		return SaveResult{}, trace, err
	}
	if withTrace {
		trace.CompiledFacts = append([]TemporalFact(nil), compiled.Facts...)
		trace.KnownEntitiesSeen = len(knownEntities)
		trace.StructurizerCoverage = StructurizerCoverage{
			TotalFactsSeen:      compiled.StructurizerCoverage.TotalFactsSeen,
			KindFilled:          compiled.StructurizerCoverage.KindFilled,
			EntitiesFilled:      compiled.StructurizerCoverage.EntitiesFilled,
			SubjectFilled:       compiled.StructurizerCoverage.SubjectFilled,
			ValidFromHintFilled: compiled.StructurizerCoverage.ValidFromHintFilled,
		}
		if len(compiled.Dropped) > 0 {
			trace.Dropped = make([]DroppedFact, len(compiled.Dropped))
			for i, d := range compiled.Dropped {
				f, _ := d.Fact.(domain.TemporalFact)
			trace.Dropped[i] = DroppedFact{Fact: f, Reason: d.Reason}
			}
		}
	}
	if len(compiled.Facts) == 0 {
		return SaveResult{}, trace, nil
	}

	unlock := m.lockWriteScope(scope)
	defer unlock()

	// Conflict resolution short-circuits noop dedupes and queues
	// supersede closes for after Append succeeds.
	resolution := domain.Resolution{Facts: compiled.Facts}
	if m.resolver != nil {
		view := ingest.StoreView{
			FindByMergeKeyFn: m.store.FindByMergeKey,
			GetFn:            m.store.Get,
		}
		stageStarted = time.Now()
		resolution, err = m.resolver.ResolveConflicts(ctx, view, compiled.Facts)
		m.emitPipeline(scope, "conflict_resolve", "resolve", len(resolution.Facts), stageStarted, err)
		if err != nil {
			return SaveResult{}, trace, fmt.Errorf("recall.Save: resolve conflicts: %w", err)
		}
	}
	if len(resolution.Facts) == 0 {
		return SaveResult{}, trace, nil
	}

	stageStarted = time.Now()
	if err := m.store.Append(ctx, resolution.Facts); err != nil {
		m.emitPipeline(scope, "store", "append", len(resolution.Facts), stageStarted, err)
		return SaveResult{}, trace, fmt.Errorf("recall.Save: store append: %w", err)
	}
	m.emitPipeline(scope, "store", "append", len(resolution.Facts), stageStarted, nil)

	ids := make([]string, len(resolution.Facts))
	for i, f := range resolution.Facts {
		ids[i] = f.ID
	}
	if withTrace {
		trace.Appended = append([]TemporalFact(nil), resolution.Facts...)
	}

	// Close validity on superseded prior facts. We do this AFTER
	// Append so a failed close never leaves a closed fact pointing
	// at a missing successor. If close fails partway through, the
	// resolver-issued Supersedes pointer still exists on the new
	// fact (so reconcile can finish the job), but we roll back the
	// new facts so callers see an atomic failure rather than a
	// half-applied write. applied tracks the prefix that did
	// commit, so rollback can reopen exactly those.
	stageStarted = time.Now()
	applied, err := m.applyValidityCloses(ctx, resolution.Closes)
	m.emitPipeline(scope, "store", "validity_close", len(resolution.Closes), stageStarted, err)
	if err != nil {
		m.rollbackAppendedFacts(ctx, scope, ids, applied, err)
		return SaveResult{}, trace, fmt.Errorf("recall.Save: close superseded: %w", err)
	}

	// Mirror evidence into the secondary lookup store before the
	// projection fanout. The adapter is a rebuildable derived view:
	// embedded EvidenceRefs on the canonical fact remain authoritative,
	// so adapter failures are telemetry-only and must not block Save.
	stageStarted = time.Now()
	if evErr := m.mirrorEvidence(ctx, scope, resolution.Facts); evErr != nil {
		m.emitPipeline(scope, "evidence", "mirror", len(resolution.Facts), stageStarted, evErr)
		m.fanout.Telemetry().OnProjection(port.ProjectionEvent{
			Projection:  "evidence",
			Op:          port.OpProject,
			Consistency: projection.Optional.String(),
			FactCount:   len(resolution.Facts),
			Err:         evErr,
		})
	} else {
		m.emitPipeline(scope, "evidence", "mirror", len(resolution.Facts), stageStarted, nil)
	}

	stageStarted = time.Now()
	if projErr := m.fanout.ProjectRequired(ctx, resolution.Facts); projErr != nil {
		m.emitPipeline(scope, "projection", "project_required", len(resolution.Facts), stageStarted, projErr)
		m.rollbackSave(ctx, scope, ids, resolution.Closes, projErr)
		return SaveResult{}, trace, projErr
	}
	m.emitPipeline(scope, "projection", "project_required", len(resolution.Facts), stageStarted, nil)

	stageStarted = time.Now()
	m.fanout.ProjectOptional(ctx, resolution.Facts)
	m.emitPipeline(scope, "projection", "project_optional", len(resolution.Facts), stageStarted, nil)

	m.runEvolutionAfterSave(ctx, scope, ids)

	return SaveResult{FactIDs: ids}, trace, nil
}

// mirrorEvidence appends EvidenceRefs into the secondary lookup
// store, per fact. No-op when no EvidenceStore is configured or
// when a fact carries no refs (embedded EvidenceText still lives
// on the canonical fact and stays the source of truth).
//
// Append is idempotent on (scope, factID, refs[i].ID) so retries
// and rebuilds replay without producing duplicate index entries.
func (m *memory) mirrorEvidence(ctx context.Context, scope Scope, facts []domain.TemporalFact) error {
	if m.evidenceStore == nil {
		return nil
	}
	for _, f := range facts {
		if len(f.EvidenceRefs) == 0 {
			continue
		}
		if err := m.evidenceStore.Append(ctx, scope, f.ID, f.EvidenceRefs); err != nil {
			return fmt.Errorf("evidence append %s: %w", f.ID, err)
		}
	}
	return nil
}

func (m *memory) runEvolutionAfterSave(ctx context.Context, scope Scope, factIDs []string) {
	if m.evolution == nil {
		return
	}
	started := time.Now()
	if err := m.evolution.AfterSave(ctx, scope, factIDs); err != nil {
		m.emitPipeline(scope, "evolution", "after_save", len(factIDs), started, err)
	}
}

func (m *memory) runEvolutionAfterRecall(ctx context.Context, scope Scope, trace RecallTrace) {
	if m.evolution == nil {
		return
	}
	started := time.Now()
	if err := m.evolution.AfterRecall(ctx, scope, trace); err != nil {
		m.emitPipeline(scope, "evolution", "after_recall", len(trace.Drops), started, err)
	}
}

func (m *memory) emitPipeline(scope Scope, stage, op string, count int, started time.Time, err error) {
	hook := m.telemetry
	if hook == nil {
		return
	}
	hook.OnPipeline(port.PipelineEvent{
		Scope:   scope,
		Stage:   stage,
		Op:      op,
		Count:   count,
		Latency: time.Since(started),
		Err:     err,
	})
}

// snapshotKnownEntities returns the canonical entities currently
// indexed by the entity projection for this scope. The Structurizer
// uses the snapshot as a soft hint to fold freshly-extracted
// mentions into existing canonical forms.
//
// We deliberately ignore CandidateSources here: the entity projection
// is the canonical source of truth for the write-side canonicalisation
// hint, and it can answer in-process without a Recall round trip.
//
// Missing / disabled projection returns nil — the Structurizer
// degrades to NER-only entity extraction, which is the same path the
// very first Save in a scope already takes.
func (m *memory) snapshotKnownEntities(scope Scope) []port.EntitySnapshot {
	for _, p := range m.projections {
		snap, ok := p.(interface {
			Snapshot(scope domain.Scope) []entityproj.Snapshot
		})
		if !ok {
			continue
		}
		raw := snap.Snapshot(scope)
		if len(raw) == 0 {
			return nil
		}
		out := make([]port.EntitySnapshot, 0, len(raw))
		for _, r := range raw {
			out = append(out, port.EntitySnapshot{
				Canonical: r.Canonical,
				Aliases:   r.Aliases,
			})
		}
		return out
	}
	return nil
}

func (m *memory) lockWriteScope(scope Scope) func() {
	key := writeScopeKey{runtimeID: scope.RuntimeID, userID: scope.UserID}

	m.writeMu.Lock()
	if m.writeLocks == nil {
		m.writeLocks = make(map[writeScopeKey]*writeLock)
	}
	wl := m.writeLocks[key]
	if wl == nil {
		wl = &writeLock{}
		m.writeLocks[key] = wl
	}
	wl.refs++
	m.writeMu.Unlock()

	wl.mu.Lock()
	return func() {
		wl.mu.Unlock()

		m.writeMu.Lock()
		wl.refs--
		if wl.refs == 0 {
			delete(m.writeLocks, key)
		}
		m.writeMu.Unlock()
	}
}

// applyValidityCloses runs UpdateValidity for each close instruction
// the resolver issued. First failure aborts and returns the prefix
// of closes that already committed so the caller's rollback can
// reopen exactly those — leaving any close that never landed alone.
//
// ErrValidityAlreadyClosed is intentionally NOT propagated: the
// resolver decided the prior fact must be closed and another writer
// has already achieved that post-state (with a different correctedBy
// /validTo tuple). Failing the whole Save would roll back the new
// fact we just appended, which is strictly worse than silently
// accepting that the prior fact is closed by someone else — the new
// fact still carries the Supersedes pointer the resolver added, so
// the supersede chain stays reconstructable from the new fact alone.
// The benign close is recorded via pipeline telemetry so operators
// can still spot pathological rates.
func (m *memory) applyValidityCloses(ctx context.Context, closes []domain.ValidityClose) ([]domain.ValidityClose, error) {
	applied := make([]domain.ValidityClose, 0, len(closes))
	for _, c := range closes {
		err := m.store.UpdateValidity(ctx, c.Scope, c.FactID, c.ValidTo, c.CorrectedBy)
		if err == nil {
			applied = append(applied, c)
			continue
		}
		if errors.Is(err, temporalstore.ErrValidityAlreadyClosed) {
			m.emitPipeline(c.Scope, "store", "validity_close_already_closed", 1, time.Now(), err)
			continue
		}
		return applied, fmt.Errorf("update validity %s: %w", c.FactID, err)
	}
	return applied, nil
}

// rollbackAppendedFacts removes just-appended facts after a failed
// downstream step (validity close). It additionally reopens any
// close that DID commit before the failure so the prior fact's
// validity window is restored. Best-effort: cleanup failures only
// surface through telemetry, the user-visible error stays
// attributable to the original cause.
func (m *memory) rollbackAppendedFacts(ctx context.Context, scope Scope, factIDs []string, applied []domain.ValidityClose, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.store.Delete(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(port.ProjectionEvent{
			Projection:  "save_rollback.appended_facts",
			Op:          port.OpForget,
			Consistency: projection.Required.String(),
			FactCount:   len(factIDs),
			Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
		})
	}
	m.reopenAfterRollback(cleanupCtx, applied, cause)
}

// rollbackSave best-effort undoes the canonical effects of a
// partially-completed Save. It:
//
//  1. runs required and optional projection forgets so an
//     upstream Optional projection that happened to succeed before
//     we returned does not leak;
//  2. deletes the canonical facts that were appended; and
//  3. reopens the validity of any prior facts the resolver closed
//     during this Save so a downstream projection failure does not
//     leave the ledger with a closed-but-no-successor revision.
//
// Any failure here is reported via telemetry but never overrides
// the original projection error.
func (m *memory) rollbackSave(ctx context.Context, scope Scope, factIDs []string, closes []domain.ValidityClose, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.fanout.ForgetRequired(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(port.ProjectionEvent{
			Projection:  "save_rollback.forget_required",
			Op:          port.OpForget,
			Consistency: projection.Required.String(),
			FactCount:   len(factIDs),
			Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
		})
	}
	m.fanout.ForgetOptional(cleanupCtx, scope, factIDs)
	if m.evidenceStore != nil {
		if err := m.evidenceStore.ForgetByFact(cleanupCtx, scope, factIDs); err != nil {
			hook.OnProjection(port.ProjectionEvent{
				Projection:  "save_rollback.evidence_forget",
				Op:          port.OpForget,
				Consistency: projection.Required.String(),
				FactCount:   len(factIDs),
				Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
			})
		}
	}
	if err := m.store.Delete(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(port.ProjectionEvent{
			Projection:  "save_rollback.store_delete",
			Op:          port.OpForget,
			Consistency: projection.Required.String(),
			FactCount:   len(factIDs),
			Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
		})
	}
	m.reopenAfterRollback(cleanupCtx, closes, cause)
	m.reprojectReopenedFacts(cleanupCtx, closes, cause)
}

// reopenAfterRollback walks every close this Save applied and tries
// to revert it via Store.ReopenValidity. The guard (expectedCorrectedBy)
// ensures we only undo our own close — if another writer has since
// re-closed the fact for a different reason we surface that as
// telemetry and leave the fact alone. ErrNotFound is also tolerated
// silently: the prior fact may already have been deleted by an
// unrelated forget.
func (m *memory) reopenAfterRollback(ctx context.Context, closes []domain.ValidityClose, cause error) {
	if len(closes) == 0 {
		return
	}
	hook := m.fanout.Telemetry()
	for _, c := range closes {
		err := m.store.ReopenValidity(ctx, c.Scope, c.FactID, c.CorrectedBy)
		if err == nil || errors.Is(err, temporalstore.ErrNotFound) {
			continue
		}
		hook.OnProjection(port.ProjectionEvent{
			Projection:  "save_rollback.reopen_validity",
			Op:          port.OpProject,
			Consistency: projection.Required.String(),
			FactCount:   1,
			Err:         fmt.Errorf("reopen %s after %w: %v", c.FactID, cause, err),
		})
	}
}

func (m *memory) reprojectReopenedFacts(ctx context.Context, closes []domain.ValidityClose, cause error) {
	if len(closes) == 0 {
		return
	}
	hook := m.fanout.Telemetry()
	for _, c := range closes {
		fact, err := m.store.Get(ctx, c.Scope, c.FactID)
		if err != nil {
			if errors.Is(err, temporalstore.ErrNotFound) {
				continue
			}
			hook.OnProjection(port.ProjectionEvent{
				Projection:  "save_rollback.reproject_prior.get",
				Op:          port.OpProject,
				Consistency: projection.Required.String(),
				FactCount:   1,
				Err:         fmt.Errorf("get %s after %w: %v", c.FactID, cause, err),
			})
			continue
		}
		if fact.CorrectedBy != "" {
			continue
		}
		if err := m.fanout.ProjectRequired(ctx, []domain.TemporalFact{fact}); err != nil {
			hook.OnProjection(port.ProjectionEvent{
				Projection:  "save_rollback.reproject_prior",
				Op:          port.OpProject,
				Consistency: projection.Required.String(),
				FactCount:   1,
				Err:         fmt.Errorf("reproject %s after %w: %v", c.FactID, cause, err),
			})
		}
	}
}

// Recall runs the v2 read pipeline:
//
//	query intent -> planner -> sources -> fusion -> materialize -> Hit
//
// Stale candidates (retrieval doc pointing at a missing or
// superseded canonical fact) are dropped at materialization without
// auto-repair — drift attribution flows through the explain trace
// (RecallExplain) and reconcile in a later phase repairs it.
func (m *memory) Recall(ctx context.Context, scope Scope, query Query) ([]Hit, error) {
	hits, _, err := m.runRecall(ctx, scope, query, false)
	return hits, err
}

// RecallExplain returns hits and a structured trace describing how
// the read pipeline produced them. Implements the optional
// RecallExplainer interface so callers can type-assert.
func (m *memory) RecallExplain(ctx context.Context, scope Scope, query Query) ([]Hit, RecallTrace, error) {
	return m.runRecall(ctx, scope, query, true)
}

func (m *memory) runRecall(ctx context.Context, scope Scope, query Query, withTrace bool) ([]Hit, RecallTrace, error) {
	var trace RecallTrace
	if err := ctx.Err(); err != nil {
		return nil, trace, err
	}
	if scope.RuntimeID == "" {
		return nil, trace, errdefs.Validationf("recall.Recall: scope.runtime_id is required")
	}

	overall := time.Now()
	stageStarted := time.Now()
	compiled, err := m.queryCompiler.Compile(ctx, port.IntentInput{
		Text:      query.Text,
		Entities:  query.Entities,
		Subject:   query.Subject,
		Predicate: query.Predicate,
		Object:    query.Object,
		Kinds:     query.Kinds,
		TimeRange: query.TimeRange,
	})
	m.emitPipeline(scope, "query_compile", "compile", len(compiled.Entities), stageStarted, err)
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: query compiler: %w", err)
	}
	stageStarted = time.Now()
	plan, err := m.planner.Plan(ctx, port.PlannerInput{
		Scope:        scope,
		Text:         compiled.Text,
		Entities:     compiled.Entities,
		Limit:        query.Limit,
		Subject:      compiled.Subject,
		Predicate:    compiled.Predicate,
		Object:       compiled.Object,
		Kinds:        compiled.Kinds,
		TimeRange:    compiled.TimeRange,
		GraphEnabled: m.graphEnabled,
		GraphHops:    query.GraphHops,
	})
	m.emitPipeline(scope, "planner", "plan", len(plan.SourceOrder), stageStarted, err)
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: planner: %w", err)
	}
	trace.Plan = plan

	// Index sources by name so we honour planner.SourceOrder and
	// silently skip sources the planner did not pick (e.g. entity
	// when no entities were supplied).
	byName := make(map[string]port.Source, len(m.sources))
	for _, s := range m.sources {
		byName[s.Name()] = s
	}

	results := make([]domain.SourceResult, 0, len(plan.SourceOrder))
	var sourceErrs []error
	totalCandidates := 0
	for _, name := range plan.SourceOrder {
		s, ok := byName[name]
		if !ok {
			continue
		}
		stageStarted = time.Now()
		res := s.Query(ctx, plan)
		m.emitPipeline(scope, "source", name, len(res.Candidates), stageStarted, res.Err)
		results = append(results, res)
		if res.Err != nil {
			sourceErrs = append(sourceErrs, fmt.Errorf("%s: %w", res.Source, res.Err))
		}
		totalCandidates += len(res.Candidates)
		st := SourceTrace{
			Source:    res.Source,
			Budget:    plan.SourceBudgets[res.Source],
			Returned:  len(res.Candidates),
			Truncated: res.Truncated,
			Latency:   res.Latency,
		}
		if res.Err != nil {
			st.Err = res.Err.Error()
		}
		trace.Sources = append(trace.Sources, st)
	}

	// Total source failure (every selected source errored and
	// produced no candidates) bubbles up so callers can distinguish
	// "found nothing" from "could not search at all". Partial
	// failures degrade silently with the trace recording each
	// source's err for attribution.
	if len(sourceErrs) > 0 && len(sourceErrs) == len(results) && totalCandidates == 0 {
		return nil, trace, fmt.Errorf("recall.Recall: all sources failed: %w", errors.Join(sourceErrs...))
	}

	opts := m.fusionOpts
	if opts.TotalCap == 0 {
		opts.TotalCap = fusionCandidateCap(plan.TotalCap)
	}
	stageStarted = time.Now()
	fused, fusionDrops, err := m.fuser.Fuse(ctx, results, opts)
	m.emitPipeline(scope, "fusion", "fuse", len(fused), stageStarted, err)
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: fusion: %w", err)
	}
	trace.FusedCandidates = len(fused)
	trace.Drops = append(trace.Drops, fusionDrops...)

	stageStarted = time.Now()
	items, matDrops, err := m.materializer.Materialize(ctx, fused)
	m.emitPipeline(scope, "materialize", "materialize", len(items), stageStarted, err)
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: materialize: %w", err)
	}
	trace.Drops = append(trace.Drops, matDrops...)
	// When a reranker is wired we defer the TotalCap so it sees the
	// widest fused pool (typically 2× topK). Without a reranker the
	// pre-rerank cap stays in place so legacy callers see the
	// historical pool size in trace.Materialized.
	rankCap := plan.TotalCap
	if m.reranker != nil {
		rankCap = 0
	}
	items = rankContextItems(items, plan.Intent, rankCap)
	trace.Materialized = len(items)

	stageStarted = time.Now()
	hits := hitsFromItems(items)
	m.emitPipeline(scope, "build_hits", "build", len(hits), stageStarted, nil)

	if m.reranker != nil && len(hits) > 0 {
		stageStarted = time.Now()
		reranked, rerr := m.reranker.Rerank(ctx, query.Text, hits)
		m.emitPipeline(scope, "rerank", "rerank", len(reranked), stageStarted, rerr)
		if rerr != nil {
			// Graceful degradation: a rerank outage must never cost
			// availability. We keep the fusion-rank order and
			// surface the error through the trace so operators can
			// attribute precision regressions to the right stage.
			trace.RerankErr = rerr.Error()
		} else {
			hits = reranked
			trace.Reranked = len(hits)
		}
		if plan.TotalCap > 0 && len(hits) > plan.TotalCap {
			hits = hits[:plan.TotalCap]
		}
	}
	trace.TotalLatency = time.Since(overall)
	m.runEvolutionAfterRecall(ctx, scope, trace)
	if !withTrace {
		return hits, RecallTrace{}, nil
	}
	return hits, trace, nil
}

func hitsFromItems(items []domain.ContextItem) []Hit {
	hits := make([]Hit, 0, len(items))
	for _, it := range items {
		hits = append(hits, Hit{
			Fact:    it.Fact,
			Score:   it.Candidate.Score,
			Sources: hitSources(it.Candidate),
		})
	}
	return hits
}

// hitSources returns the (deduped, primary-first) source provenance
// for a candidate. Fusion stores the multi-source list under
// Metadata["sources"]; the primary Candidate.Source captures the
// last writer. We prefer the metadata list when present so callers
// see every source that surfaced the fact, falling back to the
// primary source when no metadata exists (single-source candidate
// or legacy path).
func hitSources(c domain.Candidate) []string {
	if c.Metadata != nil {
		if existing, ok := c.Metadata["sources"].([]string); ok && len(existing) > 0 {
			out := make([]string, len(existing))
			copy(out, existing)
			return out
		}
	}
	if c.Source != "" {
		return []string{c.Source}
	}
	return nil
}

// Forget removes a fact with strict transactional semantics:
//
//  1. Snapshot the canonical fact (used as compensation source).
//  2. fanout.ForgetRequired — on failure the canonical fact is
//     preserved so callers can retry without losing state.
//  3. store.Delete — on failure best-effort re-projects the snapshot
//     through fanout.RebuildRequired so projections do not drift
//     away from the still-present canonical fact.
//  4. fanout.ForgetOptional — best-effort, telemetry only.
//
// A missing fact id is a noop and never raises an error.
func (m *memory) Forget(ctx context.Context, scope Scope, factID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if factID == "" {
		return errdefs.Validationf("recall.Forget: fact id is required")
	}

	unlock := m.lockWriteScope(scope)
	defer unlock()

	snapshot, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		if errors.Is(err, temporalstore.ErrNotFound) {
			// idempotent forget: also sweep projections in case
			// they hold drift, but treat result as success.
			_ = m.fanout.ForgetRequired(ctx, scope, []string{factID})
			m.fanout.ForgetOptional(ctx, scope, []string{factID})
			return nil
		}
		return fmt.Errorf("recall.Forget: store get: %w", err)
	}

	if err := m.fanout.ForgetRequired(ctx, scope, []string{factID}); err != nil {
		return err
	}

	if err := m.store.Delete(ctx, scope, []string{factID}); err != nil {
		m.compensateForgetStoreFailure(ctx, scope, snapshot, err)
		return fmt.Errorf("recall.Forget: store delete: %w", err)
	}

	m.fanout.ForgetOptional(ctx, scope, []string{factID})

	// Sweep the evidence adapter. Best-effort: by this point the
	// canonical fact (and its authoritative EvidenceRefs) are
	// already gone, so a stale lookup entry is a leak, not a
	// safety problem. Telemetry surfaces it so an operator can
	// reconcile if needed.
	if m.evidenceStore != nil {
		if err := m.evidenceStore.ForgetByFact(ctx, scope, []string{factID}); err != nil {
			m.fanout.Telemetry().OnProjection(port.ProjectionEvent{
				Projection:  "forget.evidence",
				Op:          port.OpForget,
				Consistency: projection.Optional.String(),
				FactCount:   1,
				Err:         fmt.Errorf("evidence forget %s: %w", factID, err),
			})
		}
	}
	return nil
}

// compensateForgetStoreFailure runs after store.Delete fails between
// the projection forget and store delete steps. It tries to re-Project
// the snapshot so required projections recover the fact that still
// lives in the canonical store. The compensation is best-effort and
// only reports telemetry on failure; the user already sees the
// store-delete error returned from Forget.
func (m *memory) compensateForgetStoreFailure(ctx context.Context, scope Scope, snapshot domain.TemporalFact, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.fanout.ProjectRequired(cleanupCtx, []domain.TemporalFact{snapshot}); err != nil {
		hook.OnProjection(port.ProjectionEvent{
			Projection:  "forget_compensation.project_required",
			Op:          port.OpProject,
			Consistency: projection.Required.String(),
			FactCount:   1,
			Err:         fmt.Errorf("compensation after store delete failed %w: %v", cause, err),
		})
	}
}

// RebuildAll implements ProjectionRebuilder. It walks the canonical
// store with IncludeSuperseded=true and re-projects every fact via
// fanout.Rebuild{Required,Optional}. Memory deliberately does NOT
// pre-filter superseded facts here — each projection decides how to
// materialize them, matching the write-path semantics where the
// canonical store is the single source of truth (docs §10.1).
//
// When an EvidenceStore is configured the rebuild also re-mirrors
// every fact's EvidenceRefs so the lookup adapter stays consistent
// without needing a separate reconcile path.
//
// Required-projection failures abort the rebuild and surface the
// error; optional-projection / evidence failures only emit
// telemetry. The canonical store is never modified.
func (m *memory) RebuildAll(ctx context.Context, scope Scope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall.RebuildAll: scope.runtime_id is required")
	}
	unlock := m.lockWriteScope(scope)
	defer unlock()

	facts, err := m.store.List(ctx, scope, temporalstore.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return fmt.Errorf("recall.RebuildAll: list canonical facts: %w", err)
	}
	if err := m.fanout.RebuildRequired(ctx, scope, facts); err != nil {
		return err
	}
	m.fanout.RebuildOptional(ctx, scope, facts)
	if m.evidenceStore != nil {
		if err := m.rebuildEvidence(ctx, scope, facts); err != nil {
			m.fanout.Telemetry().OnProjection(port.ProjectionEvent{
				Projection:  "rebuild.evidence",
				Op:          port.OpRebuild,
				Consistency: projection.Optional.String(),
				FactCount:   len(facts),
				Err:         err,
			})
		}
	}
	return nil
}

// rebuildEvidence applies exact-replace semantics to the secondary
// evidence adapter: every fact id the adapter currently knows about
// is forgotten, then refs are re-appended from the canonical
// snapshot. The union sweep is what removes orphan entries (adapter
// has them, canonical no longer does) so the adapter stays a pure
// derived view of the ledger — never a second truth layer.
//
// The store's Append is idempotent on (factID, ref ID) so a partial
// failure can be retried by re-running RebuildAll.
func (m *memory) rebuildEvidence(ctx context.Context, scope Scope, facts []domain.TemporalFact) error {
	adapterIDs, err := m.evidenceStore.ListFactIDs(ctx, scope)
	if err != nil {
		return fmt.Errorf("list evidence fact ids: %w", err)
	}
	ids := make(map[string]struct{}, len(adapterIDs)+len(facts))
	for _, id := range adapterIDs {
		ids[id] = struct{}{}
	}
	for _, f := range facts {
		if f.ID != "" {
			ids[f.ID] = struct{}{}
		}
	}
	if len(ids) > 0 {
		toForget := make([]string, 0, len(ids))
		for id := range ids {
			toForget = append(toForget, id)
		}
		if err := m.evidenceStore.ForgetByFact(ctx, scope, toForget); err != nil {
			return fmt.Errorf("forget evidence: %w", err)
		}
	}
	for _, f := range facts {
		if len(f.EvidenceRefs) == 0 {
			continue
		}
		if err := m.evidenceStore.Append(ctx, scope, f.ID, f.EvidenceRefs); err != nil {
			return fmt.Errorf("append evidence %s: %w", f.ID, err)
		}
	}
	return nil
}

// RebuildProjection implements ProjectionRebuilder. It rebuilds the
// single projection registered under name. Useful for targeted
// incident playbooks; ErrProjectionDisabled-style errors surface as
// errdefs.NotFound so callers can distinguish "typo" from "actual
// rebuild failure".
//
// Evidence is intentionally NOT considered part of the projection
// namespace — use RebuildAll if the evidence adapter also needs to
// be rebuilt.
func (m *memory) RebuildProjection(ctx context.Context, scope Scope, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall.RebuildProjection: scope.runtime_id is required")
	}
	if name == "" {
		return errdefs.Validationf("recall.RebuildProjection: projection name is required")
	}
	unlock := m.lockWriteScope(scope)
	defer unlock()

	var target port.Projection
	for _, p := range m.projections {
		if p != nil && p.Name() == name {
			target = p
			break
		}
	}
	if target == nil {
		return errdefs.NotFoundf("recall.RebuildProjection: projection %q not registered", name)
	}
	facts, err := m.store.List(ctx, scope, temporalstore.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return fmt.Errorf("recall.RebuildProjection: list canonical facts: %w", err)
	}
	if err := target.Rebuild(ctx, scope, facts); err != nil {
		return fmt.Errorf("recall.RebuildProjection %q: %w", name, err)
	}
	return nil
}

// RepairStale implements ProjectionRebuilder. It forgets the listed
// fact ids from required + optional projections WITHOUT touching the
// canonical store and WITHOUT re-projecting. The intended workflow
// is: a reconcile worker subscribes to DriftStaleFact events, batches
// fact ids per scope, and calls RepairStale to evict the orphaned
// projection entries.
//
// Required-projection failures abort with the original error;
// optional-projection failures only emit telemetry, matching
// fanout.ForgetOptional semantics.
func (m *memory) RepairStale(ctx context.Context, scope Scope, factIDs []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall.RepairStale: scope.runtime_id is required")
	}
	if len(factIDs) == 0 {
		return nil
	}
	unlock := m.lockWriteScope(scope)
	defer unlock()

	if err := m.fanout.ForgetRequired(ctx, scope, factIDs); err != nil {
		return err
	}
	m.fanout.ForgetOptional(ctx, scope, factIDs)
	return nil
}

// GetEvidence implements EvidenceLookup. It prefers the secondary
// store when one is configured internally; without one it falls back
// to the embedded TemporalFact.EvidenceRefs so callers always get a
// consistent view regardless of deployment topology.
//
// Validation rules match Save/Recall/Forget: an empty fact id and
// missing scope.RuntimeID are Validation; a missing fact is not an
// error — the call returns nil so callers can distinguish "no
// evidence" from "fact gone".
func (m *memory) GetEvidence(ctx context.Context, scope Scope, factID string) ([]EvidenceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scope.RuntimeID == "" {
		return nil, errdefs.Validationf("recall.GetEvidence: scope.runtime_id is required")
	}
	if factID == "" {
		return nil, errdefs.Validationf("recall.GetEvidence: fact id is required")
	}
	fact, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		if errors.Is(err, temporalstore.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall.GetEvidence: %w", err)
	}
	if m.evidenceStore != nil {
		refs, err := m.evidenceStore.ListByFact(ctx, scope, factID)
		if err != nil {
			return nil, fmt.Errorf("recall.GetEvidence: %w", err)
		}
		if len(refs) > 0 {
			return refs, nil
		}
		// fall through to canonical store fallback below; the
		// adapter may have been wiped or never warmed for this
		// fact, but the embedded refs are authoritative.
	}
	if len(fact.EvidenceRefs) == 0 {
		return nil, nil
	}
	out := make([]EvidenceRef, len(fact.EvidenceRefs))
	copy(out, fact.EvidenceRefs)
	return out, nil
}

// Close releases backend resources. Memory takes ownership of the
// store, evidence store, and retrieval index it was constructed
// with (whether default or injected): callers wiring their own
// backend should not also call Close on it.
func (m *memory) Close() error {
	if m.store != nil {
		if err := m.store.Close(); err != nil {
			return err
		}
	}
	if m.evidenceStore != nil {
		if err := m.evidenceStore.Close(); err != nil {
			return err
		}
	}
	if m.retrievalIndex != nil {
		if err := m.retrievalIndex.Close(); err != nil {
			return err
		}
	}
	return nil
}

// detachCancel returns a context that keeps the parent's values but
// is not cancelled when the parent is. Compensation paths must run
// to completion even if the inbound RPC was cancelled mid-flight,
// otherwise rollback itself becomes the source of drift.
func detachCancel(parent context.Context) context.Context {
	return cleanupCtx{parent: parent}
}

type cleanupCtx struct {
	parent context.Context
}

func (cleanupCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (cleanupCtx) Done() <-chan struct{}       { return nil }
func (cleanupCtx) Err() error                  { return nil }
func (c cleanupCtx) Value(key any) any         { return c.parent.Value(key) }
