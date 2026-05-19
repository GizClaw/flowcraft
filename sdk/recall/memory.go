package recall

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/fusion"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/materialize"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	entityproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/entity"
	graphproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/graph"
	profileproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/profile"
	relationproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/relation"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	timelineproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/timeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/source"
	entitysource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/entity"
	graphsource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/graph"
	profilesource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/profile"
	relationsource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/relation"
	retrievalsource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/retrieval"
	timelinesource "github.com/GizClaw/flowcraft/sdk/recall/internal/source/timeline"
	evidencestore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/evidence"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
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
	compiler       compiler.Compiler
	resolver       compiler.ConflictResolver
	fanout         *projection.Fanout
	telemetry      projection.TelemetryHook

	// projections retains the canonical projection set (in
	// registration order) so RebuildProjection can resolve a
	// projection by name without re-deriving it from fanout.
	projections []projection.Projection

	queryCompiler compiler.QueryCompiler
	planner       planner.Planner
	sources       []source.CandidateSource
	fuser         fusion.Fuser
	materializer  materialize.Materializer
	fusionOpts    fusion.Options
	graphEnabled  bool
}

// New constructs a v2 Memory. The defaults wire a fully in-memory
// stack so callers can exercise the write path without external
// dependencies; production callers swap pieces in via Options.
func New(opts ...Option) (Memory, error) {
	cfg := config{
		telemetry: projection.NopTelemetry{},
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
		stages := compiler.Stages{}
		if cfg.llmExtractor != nil {
			ex := compiler.NewLLMExtractor(cfg.llmExtractor.client)
			for _, opt := range cfg.llmExtractor.tune {
				if opt.apply != nil {
					opt.apply(ex)
				}
			}
			stages.Extractor = ex
		}
		cfg.compiler = compiler.New(stages)
	}
	if !cfg.resolverSet {
		cfg.resolver = compiler.NewResolver()
	}

	retrievalProj, err := retrievalproj.New(cfg.retrievalIndex)
	if err != nil {
		return nil, fmt.Errorf("recall.New: %w", err)
	}

	entityProj := entityproj.New()
	timelineProj := timelineproj.New()
	relationProj := relationproj.New()
	profileProj := profileproj.New()
	projections := []projection.Projection{
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
		qc = compiler.DefaultQueryCompiler()
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
	srcs := append([]source.CandidateSource(nil), cfg.sources...)
	if len(srcs) == 0 {
		srcs = []source.CandidateSource{
			retrievalsource.New(cfg.retrievalIndex),
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
	if err := ctx.Err(); err != nil {
		return SaveResult{}, err
	}
	if scope.RuntimeID == "" {
		return SaveResult{}, errdefs.Validationf("recall.Save: scope.runtime_id is required")
	}

	compiled, err := m.compiler.Compile(ctx, compiler.Input{
		Scope: scope,
		Facts: req.Facts,
		Text:  req.Text,
	})
	if err != nil {
		return SaveResult{}, err
	}
	if len(compiled.Facts) == 0 {
		return SaveResult{}, nil
	}

	// Conflict resolution short-circuits noop dedupes and queues
	// supersede closes for after Append succeeds.
	resolution := compiler.Resolution{Facts: compiled.Facts}
	if m.resolver != nil {
		view := compiler.StoreView{
			FindByMergeKeyFn: m.store.FindByMergeKey,
			GetFn:            m.store.Get,
		}
		resolution, err = m.resolver.ResolveConflicts(ctx, view, compiled.Facts)
		if err != nil {
			return SaveResult{}, fmt.Errorf("recall.Save: resolve conflicts: %w", err)
		}
	}
	if len(resolution.Facts) == 0 {
		return SaveResult{}, nil
	}

	if err := m.store.Append(ctx, resolution.Facts); err != nil {
		return SaveResult{}, fmt.Errorf("recall.Save: store append: %w", err)
	}

	ids := make([]string, len(resolution.Facts))
	for i, f := range resolution.Facts {
		ids[i] = f.ID
	}

	// Close validity on superseded prior facts. We do this AFTER
	// Append so a failed close never leaves a closed fact pointing
	// at a missing successor. If close fails partway through, the
	// resolver-issued Supersedes pointer still exists on the new
	// fact (so reconcile can finish the job), but we roll back the
	// new facts so callers see an atomic failure rather than a
	// half-applied write. applied tracks the prefix that did
	// commit, so rollback can reopen exactly those.
	applied, err := m.applyValidityCloses(ctx, resolution.Closes)
	if err != nil {
		m.rollbackAppendedFacts(ctx, scope, ids, applied, err)
		return SaveResult{}, fmt.Errorf("recall.Save: close superseded: %w", err)
	}

	// Mirror evidence into the secondary lookup store BEFORE the
	// projection fanout: if mirroring fails we still hold the
	// pre-projection state and can roll back without sweeping
	// already-built projection entries. Treating evidence mirror
	// as Required (per the F decision) keeps the adapter from
	// going stale on day one.
	if evErr := m.mirrorEvidence(ctx, scope, resolution.Facts); evErr != nil {
		m.rollbackSave(ctx, scope, ids, resolution.Closes, evErr)
		return SaveResult{}, fmt.Errorf("recall.Save: mirror evidence: %w", evErr)
	}

	if projErr := m.fanout.ProjectRequired(ctx, resolution.Facts); projErr != nil {
		m.rollbackSave(ctx, scope, ids, resolution.Closes, projErr)
		return SaveResult{}, projErr
	}

	m.fanout.ProjectOptional(ctx, resolution.Facts)

	return SaveResult{FactIDs: ids}, nil
}

// mirrorEvidence appends EvidenceRefs into the secondary lookup
// store, per fact. No-op when no EvidenceStore is configured or
// when a fact carries no refs (embedded EvidenceText still lives
// on the canonical fact and stays the source of truth).
//
// Append is idempotent on (scope, factID, refs[i].ID) so retries
// and rebuilds replay without producing duplicate index entries.
func (m *memory) mirrorEvidence(ctx context.Context, scope Scope, facts []model.TemporalFact) error {
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

// applyValidityCloses runs UpdateValidity for each close instruction
// the resolver issued. First failure aborts and returns the prefix
// of closes that already committed so the caller's rollback can
// reopen exactly those — leaving any close that never landed alone.
func (m *memory) applyValidityCloses(ctx context.Context, closes []compiler.ValidityClose) ([]compiler.ValidityClose, error) {
	applied := make([]compiler.ValidityClose, 0, len(closes))
	for _, c := range closes {
		if err := m.store.UpdateValidity(ctx, c.Scope, c.FactID, c.ValidTo, c.CorrectedBy); err != nil {
			return applied, fmt.Errorf("update validity %s: %w", c.FactID, err)
		}
		applied = append(applied, c)
	}
	return applied, nil
}

// rollbackAppendedFacts removes just-appended facts after a failed
// downstream step (validity close). It additionally reopens any
// close that DID commit before the failure so the prior fact's
// validity window is restored. Best-effort: cleanup failures only
// surface through telemetry, the user-visible error stays
// attributable to the original cause.
func (m *memory) rollbackAppendedFacts(ctx context.Context, scope Scope, factIDs []string, applied []compiler.ValidityClose, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.store.Delete(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "save_rollback.appended_facts",
			Op:          projection.OpForget,
			Consistency: projection.Required,
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
func (m *memory) rollbackSave(ctx context.Context, scope Scope, factIDs []string, closes []compiler.ValidityClose, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.fanout.ForgetRequired(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "save_rollback.forget_required",
			Op:          projection.OpForget,
			Consistency: projection.Required,
			FactCount:   len(factIDs),
			Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
		})
	}
	m.fanout.ForgetOptional(cleanupCtx, scope, factIDs)
	if m.evidenceStore != nil {
		if err := m.evidenceStore.ForgetByFact(cleanupCtx, scope, factIDs); err != nil {
			hook.OnProjection(projection.ProjectionEvent{
				Projection:  "save_rollback.evidence_forget",
				Op:          projection.OpForget,
				Consistency: projection.Required,
				FactCount:   len(factIDs),
				Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
			})
		}
	}
	if err := m.store.Delete(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "save_rollback.store_delete",
			Op:          projection.OpForget,
			Consistency: projection.Required,
			FactCount:   len(factIDs),
			Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
		})
	}
	m.reopenAfterRollback(cleanupCtx, closes, cause)
}

// reopenAfterRollback walks every close this Save applied and tries
// to revert it via Store.ReopenValidity. The guard (expectedCorrectedBy)
// ensures we only undo our own close — if another writer has since
// re-closed the fact for a different reason we surface that as
// telemetry and leave the fact alone. ErrNotFound is also tolerated
// silently: the prior fact may already have been deleted by an
// unrelated forget.
func (m *memory) reopenAfterRollback(ctx context.Context, closes []compiler.ValidityClose, cause error) {
	if len(closes) == 0 {
		return
	}
	hook := m.fanout.Telemetry()
	for _, c := range closes {
		err := m.store.ReopenValidity(ctx, c.Scope, c.FactID, c.CorrectedBy)
		if err == nil || errors.Is(err, temporalstore.ErrNotFound) {
			continue
		}
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "save_rollback.reopen_validity",
			Op:          projection.OpProject,
			Consistency: projection.Required,
			FactCount:   1,
			Err:         fmt.Errorf("reopen %s after %w: %v", c.FactID, cause, err),
		})
	}
}

// Recall runs the v2 read pipeline:
//
//	planner -> sources -> fusion -> materialize -> Hit
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
	compiled, err := m.queryCompiler.Compile(ctx, compiler.QueryInput{
		Text:      query.Text,
		Entities:  query.Entities,
		Subject:   query.Subject,
		Predicate: query.Predicate,
		Object:    query.Object,
		Kinds:     query.Kinds,
		TimeRange: query.TimeRange,
	})
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: query compiler: %w", err)
	}
	plan, err := m.planner.Plan(ctx, planner.Input{
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
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: planner: %w", err)
	}
	if withTrace {
		trace.Plan = plan
	}

	// Index sources by name so we honour planner.SourceOrder and
	// silently skip sources the planner did not pick (e.g. entity
	// when no entities were supplied).
	byName := make(map[string]source.CandidateSource, len(m.sources))
	for _, s := range m.sources {
		byName[s.Name()] = s
	}

	results := make([]model.SourceResult, 0, len(plan.SourceOrder))
	var sourceErrs []error
	totalCandidates := 0
	for _, name := range plan.SourceOrder {
		s, ok := byName[name]
		if !ok {
			continue
		}
		res := s.Query(ctx, plan)
		results = append(results, res)
		if res.Err != nil {
			sourceErrs = append(sourceErrs, fmt.Errorf("%s: %w", res.Source, res.Err))
		}
		totalCandidates += len(res.Candidates)
		if withTrace {
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
		opts.TotalCap = plan.TotalCap
	}
	fused, fusionDrops, err := m.fuser.Fuse(ctx, results, opts)
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: fusion: %w", err)
	}
	if withTrace {
		trace.FusedCandidates = len(fused)
		trace.Drops = append(trace.Drops, fusionDrops...)
	}

	items, matDrops, err := m.materializer.Materialize(ctx, fused)
	if err != nil {
		return nil, trace, fmt.Errorf("recall.Recall: materialize: %w", err)
	}
	if withTrace {
		trace.Materialized = len(items)
		trace.Drops = append(trace.Drops, matDrops...)
		trace.TotalLatency = time.Since(overall)
	}

	hits := make([]Hit, 0, len(items))
	for _, it := range items {
		hits = append(hits, Hit{Fact: it.Fact, Score: it.Candidate.Score})
	}
	return hits, trace, nil
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
			m.fanout.Telemetry().OnProjection(projection.ProjectionEvent{
				Projection:  "forget.evidence",
				Op:          projection.OpForget,
				Consistency: projection.Optional,
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
func (m *memory) compensateForgetStoreFailure(ctx context.Context, scope Scope, snapshot model.TemporalFact, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.fanout.ProjectRequired(cleanupCtx, []model.TemporalFact{snapshot}); err != nil {
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "forget_compensation.project_required",
			Op:          projection.OpProject,
			Consistency: projection.Required,
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
			m.fanout.Telemetry().OnProjection(projection.ProjectionEvent{
				Projection:  "rebuild.evidence",
				Op:          projection.OpRebuild,
				Consistency: projection.Optional,
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
func (m *memory) rebuildEvidence(ctx context.Context, scope Scope, facts []model.TemporalFact) error {
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
	var target projection.Projection
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
	fact, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		if errors.Is(err, temporalstore.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall.GetEvidence: %w", err)
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
