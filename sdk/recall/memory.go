package recall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/fusion"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/intent"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/lens"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/materialize"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	readstages "github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/rebuild"
	rebuildstages "github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/rebuild/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	writestages "github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
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
	// Fork appends a parallel revision without closing the source fact.
	Fork(ctx context.Context, scope Scope, sourceFactID string, newFact TemporalFact) (SaveResult, error)
	// Contest challenges a fact with evidence and applies a penalty.
	Contest(ctx context.Context, scope Scope, factID string, evidence []EvidenceRef) (SaveResult, error)
	// Reinforce / Penalize adjust caller feedback weights on a fact.
	Reinforce(ctx context.Context, scope Scope, factID string, delta float64) error
	Penalize(ctx context.Context, scope Scope, factID string, delta float64) error
	Close() error
}

type memory struct {
	store          port.TemporalStore
	evidenceStore  port.EvidenceStore
	retrievalIndex retrieval.Index
	compiler       port.Ingestor
	resolver       port.ConflictResolver
	fanout         *projection.Fanout
	telemetry      port.TelemetryHook

	// writePreRunner runs validate + ingest without the per-scope
	// write lock (legacy runSave compiled outside the lock).
	// writePostRunner runs resolve through evolution_after_save
	// under the lock. Both are wired at memory.New().
	writePreRunner  *write.Runner
	writePostRunner *write.Runner
	readRunner      *read.Runner
	rebuildRunner   *rebuild.Runner

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
	entitySnap    port.EntitySnapshotter

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

	reg := lens.NewRegistry()
	wireDefaultLenses(reg, cfg.graphEnabled, cfg.evidenceStore != nil)
	lensDeps := lens.Deps{
		Store:         cfg.store,
		EvidenceStore: cfg.evidenceStore,
		Index:         cfg.retrievalIndex,
		Telemetry:     cfg.telemetry,
		Embedder:      cfg.embedder,
		GraphEnabled:  cfg.graphEnabled,
	}
	built, err := reg.BuildAll(lensDeps)
	if err != nil {
		return nil, fmt.Errorf("recall.New: %w", err)
	}
	projections := reg.Projections(built)
	projections = append(projections, cfg.extraProjections...)
	srcs := reg.Sources(built)
	if len(cfg.sources) > 0 {
		srcs = cfg.sources
	}
	entitySnap := reg.EntitySnapshotter(built)

	qc := cfg.queryCompiler
	if qc == nil {
		qc = intent.Default()
	}
	specs := reg.Specs()
	planr := cfg.planner
	if planr == nil {
		rb := planner.NewFromSpecs(specs)
		rb.GraphEnabled = cfg.graphEnabled
		planr = rb
	} else if cfg.graphEnabled {
		if rb, ok := planr.(*planner.RuleBased); ok {
			rb.GraphEnabled = true
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
		fusionOpts.Weights = weightsFromSpecs(specs)
	}

	fanout := projection.New(projections, cfg.telemetry)
	m := &memory{
		store:          cfg.store,
		evidenceStore:  cfg.evidenceStore,
		retrievalIndex: cfg.retrievalIndex,
		compiler:       cfg.compiler,
		resolver:       cfg.resolver,
		fanout:         fanout,
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
		entitySnap:     entitySnap,
		writeLocks:     make(map[writeScopeKey]*writeLock),
	}
	tel := cfg.telemetry
	m.writePreRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewValidate(),
		writestages.NewIngest(cfg.compiler, m.entitySnapshots),
	}, tel)
	m.writePostRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewResolve(cfg.resolver, cfg.store),
		writestages.NewAppend(cfg.store, tel),
		writestages.NewValidityClose(cfg.store, fanout, tel),
		writestages.NewProjectRequired(fanout, tel),
		writestages.NewProjectOptional(fanout),
		writestages.NewEvolutionAfterSave(cfg.evolution),
	}, tel)
	var rerank readstages.HitReranker
	if cfg.reranker != nil {
		rerank = &recallHitReranker{r: cfg.reranker}
	}
	// TODO(D.5): wrap source_fanout→materialize in federation_{fanout,merge}
	m.readRunner = read.NewRunner([]pipeline.Stage[*read.ReadState]{
		readstages.NewIntent(qc),
		readstages.NewPlan(planr, cfg.graphEnabled),
		readstages.NewSourceFanout(func() []port.Source { return m.sources }),
		readstages.NewFuse(fuser, fusionOpts, fusionCandidateCap),
		readstages.NewMaterialize(mat),
		readstages.NewTrustFilter(),
		readstages.NewRank(rankContextItems, cfg.reranker != nil),
		readstages.NewBuildHits(rerank),
		readstages.NewEvolutionAfterRecall(cfg.evolution),
	}, tel)
	m.rebuildRunner = rebuild.NewRunner([]pipeline.Stage[*rebuild.RebuildState]{
		rebuildstages.NewScan(cfg.store),
		rebuildstages.NewProject(fanout, projections),
	}, tel)
	return m, nil
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
	if err := ctx.Err(); err != nil {
		return SaveResult{}, SaveTrace{}, err
	}

	state := &write.WriteState{
		Scope:      scope,
		Facts:      req.Facts,
		Turns:      req.Turns,
		ObservedAt: req.ObservedAt,
		Tier:       req.Tier,
		// Now left zero so the ingestor's Clock (or time.Now
		// fallback inside ingest) anchors relative-time resolution,
		// matching the legacy runSave path that did not pass Now on
		// IngestInput.
	}
	if withTrace {
		state.EnsureTrace()
	}

	if err := m.writePreRunner.Run(ctx, state); err != nil {
		return SaveResult{}, publicSaveTrace(state), err
	}
	if len(state.Ingest.Facts) == 0 {
		return SaveResult{}, publicSaveTrace(state), nil
	}

	unlock := m.lockWriteScope(scope)
	defer unlock()

	if err := m.writePostRunner.Run(ctx, state); err != nil {
		return SaveResult{}, publicSaveTrace(state), err
	}
	if len(state.AppendedFactIDs) == 0 {
		return SaveResult{}, publicSaveTrace(state), nil
	}
	return SaveResult{FactIDs: append([]string(nil), state.AppendedFactIDs...)}, publicSaveTrace(state), nil
}

// publicSaveTrace copies the in-flight domain.SaveTrace (when
// explain was requested) into the public SaveTrace surface.
func publicSaveTrace(state *write.WriteState) SaveTrace {
	if state == nil || state.Trace == nil {
		return SaveTrace{}
	}
	d := state.Trace
	out := SaveTrace{
		CompiledFacts:     append([]TemporalFact(nil), d.CompiledFacts...),
		Appended:          append([]TemporalFact(nil), d.Appended...),
		KnownEntitiesSeen: d.KnownEntitiesSeen,
		StructurizerCoverage: StructurizerCoverage{
			TotalFactsSeen:      d.StructurizerCoverage.TotalFactsSeen,
			KindFilled:          d.StructurizerCoverage.KindFilled,
			EntitiesFilled:      d.StructurizerCoverage.EntitiesFilled,
			SubjectFilled:       d.StructurizerCoverage.SubjectFilled,
			ValidFromHintFilled: d.StructurizerCoverage.ValidFromHintFilled,
		},
	}
	if len(d.Dropped) > 0 {
		out.Dropped = make([]DroppedFact, len(d.Dropped))
		for i, drop := range d.Dropped {
			f, _ := drop.Fact.(domain.TemporalFact)
			out.Dropped[i] = DroppedFact{Fact: f, Reason: drop.Reason}
		}
	}
	return out
}

// recallHitReranker adapts the public Reranker to domain.Hit for the
// read pipeline build_hits stage.
type recallHitReranker struct {
	r Reranker
}

func (a *recallHitReranker) Rerank(ctx context.Context, query string, hits []domain.Hit) ([]domain.Hit, error) {
	in := make([]Hit, len(hits))
	for i, h := range hits {
		in[i] = Hit{Fact: h.Fact, Score: h.Score, Sources: append([]string(nil), h.Sources...)}
	}
	out, err := a.r.Rerank(ctx, query, in)
	if err != nil {
		return hits, err
	}
	res := make([]domain.Hit, len(out))
	for i, h := range out {
		res[i] = domain.Hit{Fact: h.Fact, Score: h.Score, Sources: h.Sources}
	}
	return res, nil
}

func (m *memory) entitySnapshots(scope Scope) []port.EntitySnapshot {
	if m.entitySnap == nil {
		return nil
	}
	return m.entitySnap.Snapshot(scope)
}

func weightsFromSpecs(specs []planner.LensSpec) map[string]float64 {
	weights := make(map[string]float64, len(specs))
	for _, s := range specs {
		if s.Name == "" || s.Weight == 0 {
			continue
		}
		weights[s.Name] = s.Weight
	}
	return weights
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
	if err := ctx.Err(); err != nil {
		return nil, RecallTrace{}, err
	}
	if scope.RuntimeID == "" {
		return nil, RecallTrace{}, errdefs.Validationf("recall.Recall: scope.runtime_id is required")
	}

	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{
			Text:      query.Text,
			Entities:  query.Entities,
			Limit:     query.Limit,
			Subject:   query.Subject,
			Predicate: query.Predicate,
			Object:    query.Object,
			Kinds:     query.Kinds,
			TimeRange: query.TimeRange,
			GraphHops: query.GraphHops,
			Trust:     trustToDomain(query.Trust),
		},
		StartedAt: time.Now(),
	}
	// Always allocate trace so evolution / legacy fields populate even
	// when the caller only invoked Recall (not RecallExplain).
	state.EnsureTrace()

	if err := m.readRunner.Run(ctx, state); err != nil {
		return nil, publicRecallTrace(state), err
	}
	pubHits := domainHitsToPublic(state.Hits)
	trace := publicRecallTrace(state)
	if !withTrace {
		return pubHits, RecallTrace{}, nil
	}
	return pubHits, trace, nil
}

func publicRecallTrace(state *read.ReadState) RecallTrace {
	return read.PublicRecallTrace(state)
}

func domainHitsToPublic(hits []domain.Hit) []Hit {
	out := make([]Hit, len(hits))
	for i, h := range hits {
		out[i] = Hit{
			Fact:    TemporalFact(h.Fact),
			Score:   h.Score,
			Sources: append([]string(nil), h.Sources...),
		}
	}
	return out
}

func trustToDomain(t *TrustContext) *domain.TrustContext {
	if t == nil {
		return nil
	}
	out := &domain.TrustContext{
		MaxSensitivity: t.MaxSensitivity,
		ActorID:        t.ActorID,
	}
	if len(t.Scopes) > 0 {
		out.Scopes = make([]domain.Scope, len(t.Scopes))
		copy(out.Scopes, t.Scopes)
	}
	return out
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
	return m.runRebuild(ctx, scope, "")
}

// RebuildScope is equivalent to RebuildAll (v1 SyncSideStores parity).
func (m *memory) RebuildScope(ctx context.Context, scope Scope) error {
	return m.runRebuild(ctx, scope, "")
}

// RebuildProjection implements ProjectionRebuilder. It rebuilds the
// single projection registered under name (including "evidence" when
// an EvidenceStore is configured). Useful for targeted incident
// playbooks; ErrProjectionDisabled-style errors surface as
// errdefs.NotFound so callers can distinguish "typo" from "actual
// rebuild failure".
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
	return m.runRebuild(ctx, scope, name)
}

func (m *memory) runRebuild(ctx context.Context, scope Scope, projectionFilter string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall: scope.runtime_id is required")
	}
	unlock := m.lockWriteScope(scope)
	defer unlock()

	state := &rebuild.RebuildState{
		Scope:            scope,
		ProjectionFilter: projectionFilter,
	}
	if err := m.rebuildRunner.Run(ctx, state); err != nil {
		if projectionFilter == "" {
			return fmt.Errorf("recall.RebuildAll: %w", err)
		}
		return err
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
