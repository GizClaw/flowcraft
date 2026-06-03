package recall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/fusion"
	"github.com/GizClaw/flowcraft/memory/recall/internal/graphledger"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	entitylens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/entity"
	evidencelens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/evidence"
	graphlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/graph"
	observationlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/observation"
	profilelens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/profile"
	relationlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/relation"
	retrievallens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/retrieval"
	semanticlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/semantic"
	timelinelens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/timeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/materialize"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/feedback"
	feedbackstages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/feedback/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/forget"
	forgetstages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/forget/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	readstages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild"
	rebuildstages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/revision"
	revisionstages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/revision/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	writestages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ranker"
	linkstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/link"
	observationstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/observation"
	sideeffectstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/sideeffect"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/memory/recall/internal/telemetry"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	retrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrScopeKeyMismatch is returned when ForgetAll's confirmScopeKey
// does not match scope.PartitionKey(). The guard exists because
// ForgetAll(Hard) is irreversible: GDPR Art.17 / CCPA 1798.105 require
// that callers cannot accidentally nuke a sibling tenant by reusing a
// scope struct. AgentID is soft isolation and is NOT part of the wipe
// key — callers must pass scope.PartitionKey() as confirmation;
// errors.Is(err, ErrScopeKeyMismatch) is sentinel-stable.
var ErrScopeKeyMismatch = forgetstages.ErrScopeKeyMismatch

// Memory is the v2 fact-centric facade. See docs §11.1.
type Memory interface {
	Save(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, error)
	Recall(ctx context.Context, scope Scope, query Query) ([]Hit, error)
	Forget(ctx context.Context, scope Scope, factID string, mode ...ForgetMode) error
	ForgetAll(ctx context.Context, scope Scope, mode ForgetMode, confirmScopeKey string) (int, error)
	// ExpireRetired hard-deletes every scope-local fact whose
	// ExpiresAt is non-nil and not after now (TTL sweep). It reuses
	// the forget pipeline so projection fanout + telemetry follow
	// the same contract as ForgetAll, but does NOT require the GDPR
	// confirmScopeKey guard — TTL is an administrative cleanup, not
	// an Art.17 wipe — and ONLY removes matching facts (non-expired
	// rows survive). zero `now` defaults to time.Now(); returns the
	// number of facts physically deleted.
	ExpireRetired(ctx context.Context, scope Scope, now time.Time) (int, error)
	History(ctx context.Context, scope Scope, factID string) ([]FactVersion, error)
	// Lineage returns the full revision DAG rooted at factID — the set
	// of facts reachable via Supersedes / CorrectedBy / Revision.SourceFactID
	// edges, classified by relation (root / supersedes / fork_of /
	// contest_of / merged_from). Output is BFS-sorted (depth asc,
	// FactID asc) for determinism.
	//
	// History(factID) walks only the supersede chain (linear), so it
	// remains the right tool for "what came before / after this exact
	// belief?". Lineage(factID) is the right tool for "what is the
	// family tree of this fact, including dissents (Contest) and
	// alternate branches (Fork)?". See architecture debts §8.3.
	Lineage(ctx context.Context, scope Scope, factID string) ([]FactLineageNode, error)
	// Fork appends a parallel revision without closing the source fact.
	Fork(ctx context.Context, scope Scope, sourceFactID string, newFact TemporalFact) (SaveResult, error)
	// Contest challenges a fact with evidence and applies a penalty.
	Contest(ctx context.Context, scope Scope, factID string, evidence []EvidenceRef) (SaveResult, error)
	// Reinforce / Penalize adjust caller feedback weights on a fact.
	Reinforce(ctx context.Context, scope Scope, factID string, delta float64) error
	Penalize(ctx context.Context, scope Scope, factID string, delta float64) error
	// Close releases the store/evidence/retrieval resources owned by this
	// Memory. It does not drain or close async queues/outboxes; callers own
	// worker shutdown for those contracts.
	Close() error
}

type memory struct {
	store            port.TemporalStore
	evidenceStore    port.EvidenceStore
	observationStore port.ObservationStore
	linkStore        port.LinkStore
	retrievalIndex   retrieval.Index
	compiler         port.Ingestor
	resolver         port.ConflictResolver
	fanout           *pipeline.Fanout
	telemetry        port.TelemetryHook

	// writePreRunner runs validate + ingest without the per-scope
	// write lock (legacy runSave compiled outside the lock).
	// writePostRunner runs canonical stages under the lock
	// (resolve/append/validity_close/enqueue_side_effects).
	// Commit-after projection/evolution/embedding drain outside the lock.
	writePreRunner  *write.Runner
	writePostRunner *write.Runner
	readRunner      *read.Runner
	rebuildRunner   *rebuild.Runner
	forgetRunner    *forget.Runner
	feedbackRunner  *feedback.Runner
	revisionRunner  *revision.Runner

	// asyncEpisodePreRunner compiles/builds episodes and structured
	// facts outside the scope write lock.
	asyncEpisodePreRunner *write.Runner
	// asyncEpisodeCanonicalRunner appends canonical rows and enqueues
	// semantic + side-effect outbox jobs under the lock.
	asyncEpisodeCanonicalRunner *write.Runner

	// asyncSemanticQueue is the durable outbox WithAsyncSemanticQueue
	// configures. nil disables WriteModeAsyncSemantic.
	asyncSemanticQueue port.AsyncSemanticQueue
	sideEffectOutbox   port.SideEffectOutbox

	// asyncSemanticWorkerPreRunner / PostRunner drive
	// ProcessAsyncSemantic: LLM ingest + semantic write path with
	// origin stamping, under the same scope lock as Save.
	asyncSemanticWorkerPreRunner  *write.Runner
	asyncSemanticWorkerPostRunner *write.Runner

	// projections retains the canonical projection set (in
	// registration order) so RebuildProjection can resolve a
	// projection by name without re-deriving it from fanout.
	projections           []port.Projection
	observationProjection port.ObservationProjection

	intentRouter port.IntentRouter
	planner      port.Planner
	sources      []port.Source
	fuser        port.Fuser
	materializer port.Materializer
	fusionOpts   port.FusionOptions
	graphEnabled bool
	reranker     port.Reranker
	evolution    port.EvolutionRunner
	entitySnap   port.EntitySnapshotter

	writeMu    sync.Mutex
	writeLocks map[writeScopeKey]*writeLock

	// deferredTelemetry buffers stage events during scope-locked writes.
	deferredTelemetry *telemetry.Deferred

	// scopeGenMu / scopeGen guard against slow sync Save re-appending
	// after ForgetAll(Hard) returns (pre-runner runs outside the lock).
	scopeGenMu sync.Mutex
	scopeGen   map[writeScopeKey]uint64
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
	if cfg.observationStore == nil {
		cfg.observationStore = observationstore.New()
	}
	if cfg.linkStore == nil {
		cfg.linkStore = linkstore.New()
	}
	if cfg.retrievalIndex == nil {
		cfg.retrievalIndex = retrievalmem.New()
	}
	if cfg.compiler == nil {
		stages := ingest.Stages{}
		if cfg.timeParser != nil || cfg.entityExtractor != nil {
			stages.Structurizer = ingest.DefaultStructurizer{
				EntityExtractor: cfg.entityExtractor,
				TimeParser:      cfg.timeParser,
			}
		}
		if cfg.llmExtractor != nil {
			stages.Extractor = cfg.llmExtractor.build()
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
	wireDefaultLenses(reg, cfg.graphEnabled, cfg.evidenceStore != nil, cfg.observationStore != nil)
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
	var obsProjection port.ObservationProjection
	if cfg.observationStore != nil {
		obsProjection = observationlens.NewProjection(cfg.retrievalIndex)
	}
	projections := reg.Projections(built)
	projections = append(projections, cfg.extraProjections...)
	srcs := reg.Sources(built)
	if len(cfg.sources) > 0 {
		srcs = cfg.sources
	}
	entitySnap := reg.EntitySnapshotter(built)

	ir := cfg.intentRouter
	if ir == nil {
		ir = intent.Default(cfg.embedder)
	}
	specs := reg.Specs()
	planr := cfg.planner
	if planr == nil {
		strategyPlanner := planner.NewFromSpecs(specs)
		strategyPlanner.GraphEnabled = cfg.graphEnabled
		planr = strategyPlanner
	} else if cfg.graphEnabled {
		if strategyPlanner, ok := planr.(*planner.RecallStrategyPlanner); ok {
			strategyPlanner.GraphEnabled = true
		}
	}
	fuser := cfg.fuser
	if fuser == nil {
		fuser = fusion.WeightedRRF{}
	}
	mat := cfg.materializer
	if mat == nil {
		mat = materialize.New(cfg.store, cfg.observationStore, cfg.linkStore, cfg.telemetry)
	}
	fusionOpts := cfg.fusionOpts
	if fusionOpts.Weights == nil {
		fusionOpts.Weights = weightsFromSpecs(specs)
	}
	rnk := cfg.contextRanker
	if rnk == nil {
		rnk = ranker.NewDefault()
	}

	deferredTel := telemetry.NewDeferred(cfg.telemetry)
	cfg.telemetry = deferredTel

	if cfg.sideEffectOutbox == nil {
		cfg.sideEffectOutbox = sideeffectstore.New()
	}

	fanout := pipeline.NewFanout(projections, cfg.telemetry)
	needsEmbedding := cfg.embedder != nil
	m := &memory{
		store:                 cfg.store,
		evidenceStore:         cfg.evidenceStore,
		observationStore:      cfg.observationStore,
		linkStore:             cfg.linkStore,
		retrievalIndex:        cfg.retrievalIndex,
		compiler:              cfg.compiler,
		resolver:              cfg.resolver,
		fanout:                fanout,
		telemetry:             cfg.telemetry,
		projections:           projections,
		observationProjection: obsProjection,
		intentRouter:          ir,
		planner:               planr,
		sources:               srcs,
		fuser:                 fuser,
		materializer:          mat,
		fusionOpts:            fusionOpts,
		graphEnabled:          cfg.graphEnabled,
		reranker:              cfg.reranker,
		evolution:             cfg.evolution,
		entitySnap:            entitySnap,
		writeLocks:            make(map[writeScopeKey]*writeLock),
		deferredTelemetry:     deferredTel,
		asyncSemanticQueue:    cfg.asyncSemanticQueue,
		sideEffectOutbox:      cfg.sideEffectOutbox,
	}
	tel := cfg.telemetry
	enqueueSide := writestages.NewEnqueueSideEffects(cfg.sideEffectOutbox, needsEmbedding, cfg.evolution)
	m.writePreRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewValidate(),
		writestages.NewCommitObservations(cfg.observationStore, obsProjection),
		writestages.NewIngest(cfg.compiler, m.entitySnapshots),
	}, tel)
	m.writePostRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewResolve(cfg.resolver, cfg.store),
		writestages.NewAppend(cfg.store, tel),
		writestages.NewValidityClose(cfg.store, fanout, tel),
		writestages.NewCommitGraph(cfg.observationStore, cfg.linkStore, obsProjection),
		enqueueSide,
	}, tel)
	m.asyncEpisodePreRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewValidate(),
		writestages.NewCommitObservations(cfg.observationStore, obsProjection),
		writestages.NewBuildEpisode(),
		writestages.NewStructuredIngest(cfg.compiler, m.entitySnapshots),
	}, tel)
	m.asyncEpisodeCanonicalRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewAppendEpisode(cfg.store, tel),
		writestages.NewResolve(cfg.resolver, cfg.store),
		writestages.NewAppend(cfg.store, tel),
		writestages.NewValidityClose(cfg.store, fanout, tel),
		writestages.NewCommitGraph(cfg.observationStore, cfg.linkStore, obsProjection),
		enqueueSide,
		writestages.NewWriteSemanticOutbox(m.asyncSemanticQueue, tel),
	}, tel)
	m.asyncSemanticWorkerPreRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewValidate(),
		writestages.NewIngest(cfg.compiler, m.entitySnapshots),
	}, tel)
	m.asyncSemanticWorkerPostRunner = write.NewRunner([]pipeline.Stage[*write.WriteState]{
		writestages.NewResolve(cfg.resolver, cfg.store),
		writestages.NewOriginStamp(),
		writestages.NewAppend(cfg.store, tel),
		writestages.NewValidityClose(cfg.store, fanout, tel),
		writestages.NewCommitGraph(cfg.observationStore, cfg.linkStore, obsProjection),
		enqueueSide,
	}, tel)
	var rerank port.Reranker
	if cfg.reranker != nil {
		rerank = cfg.reranker
	}
	readStages := []pipeline.Stage[*read.ReadState]{
		readstages.NewIntentRoute(ir),
		readstages.NewPlan(planr, cfg.graphEnabled, m.entitySnapshotsForScopes),
		readstages.NewCandidateFanout(
			func() []port.Source { return m.sources },
		),
		readstages.NewCandidateMergeAndMaterialize(
			fuser,
			fusionOpts,
			planner.FusionCandidateCap,
			mat,
		),
		readstages.NewCandidateExpansion(cfg.store),
	}
	readStages = append(readStages, readstages.NewLinkExpansion(cfg.store, cfg.observationStore, cfg.linkStore))
	if cfg.observationStore != nil {
		readStages = append(readStages, readstages.NewObservationRecall(cfg.observationStore))
	}
	readStages = append(readStages,
		readstages.NewPolicyFilter(),
		readstages.NewCandidateAssessment(),
		readstages.NewRank(rnk, cfg.reranker != nil),
		readstages.NewContextPack(rerank),
		readstages.NewBuildGroundedHits(readstages.WithGroundedHitGraph(cfg.observationStore, cfg.linkStore)),
		readstages.NewEvolutionAfterRecall(cfg.evolution),
	)
	m.readRunner = read.NewRunner(readStages, tel)
	m.rebuildRunner = rebuild.NewRunner([]pipeline.Stage[*rebuild.RebuildState]{
		rebuildstages.NewScan(cfg.store),
		rebuildstages.NewProject(fanout, projections),
		rebuildstages.NewGraphLedger(cfg.observationStore, cfg.linkStore, obsProjection),
	}, tel)
	m.forgetRunner = forget.NewRunner([]pipeline.Stage[*forget.State]{
		forgetstages.NewForgetAll(cfg.store, fanout, projections, cfg.evidenceStore, cfg.observationStore, cfg.linkStore, obsProjection),
	}, tel)
	m.feedbackRunner = feedback.NewRunner([]pipeline.Stage[*feedback.State]{
		feedbackstages.NewApplyFeedback(cfg.store, fanout),
	}, tel)
	m.revisionRunner = revision.NewRunner([]pipeline.Stage[*revision.State]{
		revisionstages.NewLookupSource(cfg.store),
		revisionstages.NewAttachRevision(),
		revisionstages.NewSave(m.saveRevisionFact, cfg.store),
	}, tel)
	return m, nil
}

// saveRevisionFact drives the canonical write pipeline (pre + post
// runners) under the assumption that the caller already holds the
// per-scope write lock — Memory.Fork / Memory.Contest take the lock
// before invoking revisionRunner.Run. Returning the freshly-stored
// canonical fact lets the revision_save stage populate state.Created
// without an extra store.Get round-trip from the facade.
func (m *memory) saveRevisionFact(ctx context.Context, scope domain.Scope, fact domain.TemporalFact) (domain.TemporalFact, error) {
	state := &write.WriteState{
		Scope:      scope,
		Facts:      []domain.TemporalFact{fact},
		ObservedAt: fact.ObservedAt,
	}
	if err := m.writePreRunner.Run(ctx, state); err != nil {
		return domain.TemporalFact{}, err
	}
	if len(state.Ingest.Facts) == 0 {
		return domain.TemporalFact{}, errdefs.Internalf("recall.Revision: ingest produced no facts")
	}
	allocateSaveOutboxID(state)
	if err := m.writePostRunner.Run(ctx, state); err != nil {
		return domain.TemporalFact{}, err
	}
	if len(state.AppendedFactIDs) == 0 {
		return domain.TemporalFact{}, errdefs.Internalf("recall.Revision: no fact id returned")
	}
	created, err := m.store.Get(ctx, scope, state.AppendedFactIDs[0])
	if err != nil {
		return domain.TemporalFact{}, fmt.Errorf("recall.Revision: re-get: %w", err)
	}
	return created, nil
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
	res, _, err := m.runSave(ctx, scope, req, false, false)
	return res, err
}

// SaveExplain runs the canonical write pipeline like Save and also
// returns the compiled facts and compiler drops so callers can run
// diagnostics on the extractor / compiler / resolver stages.
func (m *memory) SaveExplain(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, SaveTrace, error) {
	return m.runSave(ctx, scope, req, true, false)
}

// SaveExplainDebug is like SaveExplain but retains raw dropped-fact
// payloads in the trace. Not safe for production telemetry export.
func (m *memory) SaveExplainDebug(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, SaveTrace, error) {
	return m.runSave(ctx, scope, req, true, true)
}

func (m *memory) runSave(ctx context.Context, scope Scope, req SaveRequest, withTrace, includeRawDiagnostics bool) (SaveResult, SaveTrace, error) {
	if err := ctx.Err(); err != nil {
		return SaveResult{}, SaveTrace{}, err
	}
	// Async semantic mode degrades cleanly when there are no Turns (no LLM work
	// to defer) and rejects without side effects when no queue is wired.
	// Anything else falls through to the sync path so existing callers see
	// byte-identical behaviour at WriteModeSync (the zero value).
	if req.Mode == domain.WriteModeAsyncSemantic {
		if len(req.Turns) == 0 {
			factsOnly := req
			factsOnly.Mode = domain.WriteModeSync
			return m.runSaveSync(ctx, scope, factsOnly, withTrace, includeRawDiagnostics)
		}
		if m.asyncSemanticQueue == nil {
			return SaveResult{}, SaveTrace{}, errdefs.Validationf(
				"recall.Save: WriteModeAsyncSemantic requires WithAsyncSemanticQueue option")
		}
		return m.runSaveAsync(ctx, scope, req, withTrace, includeRawDiagnostics)
	}
	return m.runSaveSync(ctx, scope, req, withTrace, includeRawDiagnostics)
}

// runSaveSync is the canonical synchronous Save body: pre-runner
// outside the scope lock, post-runner inside. It serves both
// WriteModeSync calls and the Facts-only leg of an async request.
func (m *memory) runSaveSync(ctx context.Context, scope Scope, req SaveRequest, withTrace, includeRawDiagnostics bool) (SaveResult, SaveTrace, error) {
	state := &write.WriteState{
		Scope:                 scope,
		Facts:                 req.Facts,
		Turns:                 req.Turns,
		SaveOutboxID:          req.RequestID,
		ObservedAt:            req.ObservedAt,
		Tier:                  req.Tier,
		DiagnosticsIncludeRaw: includeRawDiagnostics,
		RecentMessages:        req.RecentMessages,
		ExistingFactsAnchor:   req.ExistingFactsAnchor,
		// Now left zero so the ingestor's Clock (or time.Now
		// fallback inside ingest) anchors relative-time resolution,
		// matching the legacy runSave path that did not pass Now on
		// IngestInput.
	}
	if withTrace {
		state.EnsureTrace()
	}

	startGen, _, err := m.scopeGeneration(ctx, scope)
	if err != nil {
		return SaveResult{}, publicSaveTrace(state), fmt.Errorf("recall.Save: scope generation: %w", err)
	}
	allocateSaveOutboxID(state)
	if err := m.writePreRunner.Run(ctx, state); err != nil {
		return SaveResult{}, publicSaveTrace(state), err
	}
	if len(state.Ingest.Facts) == 0 {
		return SaveResult{}, publicSaveTrace(state), nil
	}

	m.holdWriteTelemetry()
	unlock, err := m.enterScopeWrite(ctx, scope, startGen)
	if err != nil {
		m.cleanupSaveGraphArtifacts(context.Background(), scope, state)
		m.flushWriteTelemetry()
		return SaveResult{}, publicSaveTrace(state), err
	}
	state.ScopeGeneration = startGen

	if err := m.writePostRunner.Run(ctx, state); err != nil {
		unlock()
		m.flushWriteTelemetry()
		return SaveResult{}, publicSaveTrace(state), err
	}
	if err := m.abortIfScopeGenChanged(scope, startGen, state); err != nil {
		unlock()
		m.flushWriteTelemetry()
		return SaveResult{}, publicSaveTrace(state), err
	}
	unlock()
	m.flushWriteTelemetry()
	if len(state.AppendedFactIDs) == 0 {
		return SaveResult{}, publicSaveTrace(state), nil
	}
	return SaveResult{FactIDs: append([]string(nil), state.AppendedFactIDs...)}, publicSaveTrace(state), nil
}

// runSaveAsync is the async semantic facade. Episode lane and
// structured Facts share one locked canonical pipeline: Save returns
// after episode / structured facts and durable outbox jobs are
// committed. Projection, embedding, and evolution converge later via
// SideEffectProcessor.
func (m *memory) runSaveAsync(ctx context.Context, scope Scope, req SaveRequest, withTrace, includeRawDiagnostics bool) (SaveResult, SaveTrace, error) {
	state := newEpisodeState(scope, req, withTrace, includeRawDiagnostics)
	startGen, _, err := m.scopeGeneration(ctx, scope)
	if err != nil {
		return SaveResult{}, publicSaveTrace(state), fmt.Errorf("recall.Save: scope generation: %w", err)
	}
	allocateSaveOutboxID(state)
	if err := m.asyncEpisodePreRunner.Run(ctx, state); err != nil {
		m.cleanupSaveGraphArtifacts(context.Background(), scope, state)
		return SaveResult{}, publicSaveTrace(state), err
	}
	m.holdWriteTelemetry()
	unlock, err := m.enterScopeWrite(ctx, scope, startGen)
	if err != nil {
		m.cleanupSaveGraphArtifacts(context.Background(), scope, state)
		m.flushWriteTelemetry()
		return SaveResult{}, publicSaveTrace(state), err
	}
	state.ScopeGeneration = startGen
	if err := m.asyncEpisodeCanonicalRunner.Run(ctx, state); err != nil {
		unlock()
		m.flushWriteTelemetry()
		return SaveResult{}, publicSaveTrace(state), err
	}
	if err := m.abortIfScopeGenChanged(scope, startGen, state); err != nil {
		unlock()
		m.flushWriteTelemetry()
		return SaveResult{}, publicSaveTrace(state), err
	}
	unlock()
	m.flushWriteTelemetry()
	episodeIDs := episodeFactIDsFromState(state)
	structuredIDs := append([]string(nil), state.AppendedFactIDs...)
	return SaveResult{
		AsyncRequestID:  state.AsyncRequestID,
		EpisodeFactIDs:  episodeIDs,
		SemanticPending: state.SemanticPending,
		FactIDs:         append(structuredIDs, episodeIDs...),
	}, publicSaveTrace(state), nil
}

func episodeFactIDsFromState(state *write.WriteState) []string {
	if state == nil || len(state.EpisodeFacts) == 0 {
		return nil
	}
	out := make([]string, 0, len(state.EpisodeFacts))
	for _, f := range state.EpisodeFacts {
		if f.ID != "" {
			out = append(out, f.ID)
		}
	}
	return out
}

// newEpisodeState builds the WriteState the asyncEpisodeRunner
// consumes. Facts and Turns may both be present in mixed-mode saves;
// Turns feed build_episode while Facts feed structured_ingest before
// write_semantic_outbox.
func newEpisodeState(scope Scope, req SaveRequest, withTrace, includeRawDiagnostics bool) *write.WriteState {
	state := &write.WriteState{
		Scope:                 scope,
		Facts:                 req.Facts,
		Turns:                 req.Turns,
		SaveOutboxID:          req.RequestID,
		ObservedAt:            req.ObservedAt,
		Tier:                  req.Tier,
		DiagnosticsIncludeRaw: includeRawDiagnostics,
		RecentMessages:        req.RecentMessages,
		ExistingFactsAnchor:   req.ExistingFactsAnchor,
		Mode:                  domain.WriteModeAsyncSemantic,
	}
	if withTrace {
		state.EnsureTrace()
	}
	return state
}

// publicSaveTrace copies the in-flight domain.SaveTrace (when
// explain was requested) into the public SaveTrace surface.
func publicSaveTrace(state *write.WriteState) SaveTrace {
	if state == nil || state.Trace == nil {
		return SaveTrace{}
	}
	return SaveTrace{Stages: append([]diagnostic.StageDiagnostic(nil), state.Trace.Stages...)}
}

// wireDefaultLenses registers canonical fact projections in planner source
// order. Observation indexing is wired separately as a raw-evidence rescue lane
// so raw observations do not compete as ordinary projection votes.
func wireDefaultLenses(reg *lens.Registry, graphEnabled, withEvidence, _ bool) {
	if reg == nil {
		return
	}
	reg.Register(retrievallens.Lens{})
	reg.Register(entitylens.Lens{})
	if graphEnabled {
		reg.Register(graphlens.Lens{})
	}
	reg.Register(relationlens.Lens{})
	reg.Register(semanticlens.AssertionLens())
	reg.Register(profilelens.Lens{})
	reg.Register(timelinelens.Lens{})
	if withEvidence {
		reg.Register(evidencelens.Lens{})
	}
}

// Forget removes a fact. Optional mode defaults to ForgetHard for backward
// compatibility. Use ForgetSoft to retract without deleting audit history
// (equivalent to v1 Retract / D.1 guidance).
func (m *memory) Forget(ctx context.Context, scope Scope, factID string, mode ...ForgetMode) error {
	mmode := domain.ForgetHard
	if len(mode) > 0 {
		mmode = domain.NormalizeForgetMode(mode[0])
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if factID == "" {
		return errdefs.Validationf("recall.Forget: fact id is required")
	}

	m.holdWriteTelemetry()
	unlock := m.lockWriteScope(scope)
	defer func() {
		unlock()
		m.flushWriteTelemetry()
	}()

	snapshot, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		if errors.Is(err, temporalstore.ErrNotFound) {
			if mmode == domain.ForgetHard {
				deletedObservationIDs, planErr := graphledger.PlanClearAssertion(ctx, scope, factID, m.observationStore, m.linkStore)
				if planErr != nil {
					return fmt.Errorf("recall.Forget: graph cleanup plan: %w", planErr)
				}
				if err := m.fanout.ForgetRequired(ctx, scope, []string{factID}); err != nil {
					return err
				}
				m.fanout.ForgetOptional(ctx, scope, []string{factID})
				if len(deletedObservationIDs) > 0 && m.observationProjection != nil {
					if err := m.observationProjection.ForgetObservations(ctx, scope, deletedObservationIDs); err != nil {
						return fmt.Errorf("recall.Forget: observation projection cleanup: %w", err)
					}
				}
				if _, _, _, err := graphledger.ClearAssertion(ctx, scope, factID, m.observationStore, m.linkStore); err != nil {
					return fmt.Errorf("recall.Forget: graph cleanup: %w", err)
				}
			}
			return nil
		}
		return fmt.Errorf("recall.Forget: store get: %w", err)
	}

	switch mmode {
	case domain.ForgetSoft:
		if err := m.store.MarkClosed(ctx, scope, factID, true); err != nil {
			return fmt.Errorf("recall.Forget: soft close: %w", err)
		}
		snapshot.Closed = true
		if err := m.fanout.ProjectRequired(ctx, []domain.TemporalFact{snapshot}); err != nil {
			return fmt.Errorf("recall.Forget: reproject closed fact: %w", err)
		}
		m.fanout.ProjectOptional(ctx, []domain.TemporalFact{snapshot})
		return nil
	default:
		graphSnapshot, err := m.snapshotForgetGraphCleanup(ctx, scope, factID)
		if err != nil {
			return fmt.Errorf("recall.Forget: graph cleanup plan: %w", err)
		}
		if err := m.fanout.ForgetRequired(ctx, scope, []string{factID}); err != nil {
			m.compensateForgetFailure(ctx, scope, snapshot, graphSnapshot, err)
			return err
		}
		if len(graphSnapshot.deletedObservationIDs) > 0 && m.observationProjection != nil {
			if err := m.observationProjection.ForgetObservations(ctx, scope, graphSnapshot.deletedObservationIDs); err != nil {
				m.compensateForgetFailure(ctx, scope, snapshot, graphSnapshot, err)
				return fmt.Errorf("recall.Forget: observation projection cleanup: %w", err)
			}
		}
		if _, _, _, err := graphledger.ClearAssertion(ctx, scope, factID, m.observationStore, m.linkStore); err != nil {
			m.compensateForgetFailure(ctx, scope, snapshot, graphSnapshot, err)
			return fmt.Errorf("recall.Forget: graph cleanup: %w", err)
		}
		if err := m.store.Delete(ctx, scope, []string{factID}); err != nil {
			m.compensateForgetFailure(ctx, scope, snapshot, graphSnapshot, err)
			return fmt.Errorf("recall.Forget: store delete: %w", err)
		}
		m.fanout.ForgetOptional(ctx, scope, []string{factID})
		return nil
	}
}

// ForgetAll retires every fact in the primary scope. Federation sub-scopes are
// not recursed.
//
// Mode semantics:
//
//   - ForgetSoft marks every active fact Closed=true and re-projects
//     them. The canonical store rows survive, evidence is preserved
//     for audit, and Memory.History keeps working on the supersede
//     chain. This path is reversible by lifting Closed via Save.
//
//   - ForgetHard is irreversible: it invokes Projection.ClearScope on
//     every registered projection, then store.DeleteByScope. Evidence
//     refs are wiped through the evidence projection's ClearScope.
//     This is the GDPR Art.17 / CCPA 1798.105 "delete me" path.
//
// confirmScopeKey is the GDPR guard: it MUST equal
// scope.PartitionKey() for Hard mode. Soft mode skips the check for
// ergonomics — a Soft is reversible. errors.Is(err,
// ErrScopeKeyMismatch) is sentinel-stable.
//
// The operation goes through the forget pipeline so each call emits one
// ForgetAllDetail diagnostic via the registered TelemetryHook. The returned int
// matches Detail.Deleted.
func (m *memory) ForgetAll(ctx context.Context, scope Scope, mode ForgetMode, confirmScopeKey string) (deleted int, retErr error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if scope.RuntimeID == "" {
		return 0, errdefs.Validationf("recall.ForgetAll: scope.runtime_id is required")
	}

	if domain.NormalizeForgetMode(mode) == domain.ForgetHard {
		if confirmScopeKey != scope.PartitionKey() {
			return 0, ErrScopeKeyMismatch
		}
	}

	m.holdWriteTelemetry()
	unlock := m.lockWriteScope(scope)
	defer func() {
		unlock()
		m.flushWriteTelemetry()
	}()

	state := &forget.State{
		Scope:           scope,
		Mode:            mode,
		ConfirmScopeKey: confirmScopeKey,
	}
	state.EnsureTrace()
	var preDeleteCancel asyncJobCancelResult
	hard := domain.NormalizeForgetMode(mode) == domain.ForgetHard
	markerSet := false
	if hard {
		if _, err := m.bumpScopeGenDeleting(ctx, scope, true); err != nil {
			return 0, fmt.Errorf("recall.ForgetAll: bump scope generation: %w", err)
		}
		markerSet = true
		defer func() {
			if !markerSet {
				return
			}
			if err := m.setScopeDeleting(context.Background(), scope, false); err != nil {
				cleanupErr := fmt.Errorf("recall.ForgetAll: clear deleting marker: %w", err)
				if retErr != nil {
					retErr = errors.Join(retErr, cleanupErr)
				} else {
					retErr = cleanupErr
				}
			}
		}()
		preDeleteCancel = m.cancelAsyncJobsAfterForget(ctx, state)
		if preDeleteCancel.Err != nil {
			m.emitAsyncJobCancelTelemetry(state, preDeleteCancel, "forget_all_pre_delete")
			return 0, fmt.Errorf("recall.ForgetAll: async job pre-delete purge: %w", preDeleteCancel.Err)
		}
	}
	if err := m.forgetRunner.Run(ctx, state); err != nil {
		return 0, err
	}
	if hard {
		m.patchForgetTraceAsyncCancel(state, preDeleteCancel)
		m.emitAsyncJobCancelTelemetry(state, preDeleteCancel, "forget_all")
	} else {
		cancel := m.cancelAsyncJobsAfterForget(ctx, state)
		m.patchForgetTraceAsyncCancel(state, cancel)
		m.emitAsyncJobCancelTelemetry(state, cancel, "forget_all")
		if cancel.Err != nil {
			return state.Deleted, fmt.Errorf("recall.ForgetAll: async job cancel: %w", cancel.Err)
		}
	}
	return state.Deleted, nil
}

// ExpireRetired hard-deletes every scope-local fact whose ExpiresAt
// is non-nil and not after now.
//
// ExpireRetired is a periodic TTL sweep, not a full-scope transactional
// boundary: it scans the expired IDs first, installs a generation fence, then
// deletes only that preselected set. A fact appended by another process after
// the scan but before the fence is intentionally left for the next sweep. The
// fence prevents pre-scan/old-generation writers from committing after this
// destructive delete starts.
//
// The call routes through the forget pipeline with State.Filter set,
// which:
//   - implicitly applies Hard mode semantics (TTL is destructive);
//   - skips the GDPR confirmScopeKey guard (sweep is administrative);
//   - leaves non-matching facts and their projection entries intact
//     (per-id Delete rather than DeleteByScope).
func (m *memory) ExpireRetired(ctx context.Context, scope Scope, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if scope.RuntimeID == "" {
		return 0, errdefs.Validationf("recall.ExpireRetired: scope.runtime_id is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	m.holdWriteTelemetry()
	unlock := m.lockWriteScope(scope)
	defer func() {
		unlock()
		m.flushWriteTelemetry()
	}()
	state := &forget.State{
		Scope:  scope,
		Mode:   domain.ForgetHard,
		Filter: &forget.ForgetFilter{ExpiresBefore: &now},
		Now:    now,
	}
	state.EnsureTrace()
	expireIDs, scanned, err := m.scanExpiredFacts(ctx, scope, now)
	if err != nil {
		return 0, fmt.Errorf("recall.ExpireRetired: scan expired facts: %w", err)
	}
	// Delete only the pre-fence scan result; later expired writes are swept by
	// a future call, while the generation bump blocks old-generation commits.
	state.ExpirePreselected = true
	state.ExpireScanned = scanned
	state.ExpireFactIDs = expireIDs
	if len(expireIDs) > 0 {
		if _, err := m.bumpScopeGenDeleting(ctx, scope, false); err != nil {
			return 0, fmt.Errorf("recall.ExpireRetired: bump scope generation: %w", err)
		}
	}
	if err := m.forgetRunner.Run(ctx, state); err != nil {
		return 0, err
	}
	var cancel asyncJobCancelResult
	if state.Deleted > 0 {
		cancel = m.cancelAsyncJobsAfterForget(ctx, state)
		m.patchForgetTraceAsyncCancel(state, cancel)
		m.emitAsyncJobCancelTelemetry(state, cancel, "expire_retired")
		if cancel.Err != nil {
			return state.Deleted, fmt.Errorf("recall.ExpireRetired: async job cancel: %w", cancel.Err)
		}
	}
	return state.Deleted, nil
}

func (m *memory) scanExpiredFacts(ctx context.Context, scope Scope, now time.Time) ([]string, int, error) {
	facts, err := m.store.List(ctx, scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return nil, 0, err
	}
	ids := make([]string, 0, len(facts))
	for _, fact := range facts {
		if fact.ExpiresAt != nil && !fact.ExpiresAt.IsZero() && !fact.ExpiresAt.After(now) {
			ids = append(ids, fact.ID)
		}
	}
	return ids, len(facts), nil
}

// Lineage walks the revision DAG rooted at factID via the temporal
// store's supersede + revision-source lookups and projects the
// internal domain nodes to the public surface. See the interface
// godoc for the History-vs-Lineage contract.
func (m *memory) Lineage(ctx context.Context, scope Scope, factID string) ([]FactLineageNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scope.RuntimeID == "" {
		return nil, errdefs.Validationf("recall.Lineage: scope.runtime_id is required")
	}
	if factID == "" {
		return nil, errdefs.Validationf("recall.Lineage: factID is required")
	}
	root, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		return nil, err
	}
	lookups := domain.LineageLookups{
		Get:                  m.store.Get,
		FindByRevisionSource: m.store.FindByRevisionSource,
		FindSupersededBy:     m.store.FindSupersededBy,
	}
	nodes, err := domain.BuildLineage(ctx, root, lookups)
	if err != nil {
		return nil, err
	}
	return toPublicLineage(nodes), nil
}

// toPublicLineage copies the internal lineage nodes into the public
// shape. The struct fields are isomorphic (LineageRelation and
// TemporalFact are both aliases of their domain counterparts) so the
// loop is a one-shot field-by-field copy.
func toPublicLineage(nodes []domain.FactLineageNode) []FactLineageNode {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]FactLineageNode, len(nodes))
	for i, n := range nodes {
		out[i] = FactLineageNode{
			Fact:         n.Fact,
			Relation:     n.Relation,
			SourceFactID: n.SourceFactID,
			Depth:        n.Depth,
		}
	}
	return out
}

func (m *memory) entitySnapshots(scope Scope) []port.EntitySnapshot {
	if m.entitySnap == nil {
		return nil
	}
	return m.entitySnap.Snapshot(scope)
}

// entitySnapshotsForScopes is the read-path plan stage's wiring. It returns
// the raw per-scope EntitySnapshot concatenation so the Plan stage can run the
// dedup-and-max merge across sub-scopes itself; memory does not perform the
// merge to keep snapshotter ownership at the lens layer.
func (m *memory) entitySnapshotsForScopes(scopes []domain.Scope) []port.EntitySnapshot {
	if m.entitySnap == nil || len(scopes) == 0 {
		return nil
	}
	var out []port.EntitySnapshot
	for _, sc := range scopes {
		out = append(out, m.entitySnap.Snapshot(sc)...)
	}
	return out
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

	now := time.Now()
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{
			Text:           query.Text,
			Entities:       query.Entities,
			Limit:          query.Limit,
			Subject:        query.Subject,
			Predicate:      query.Predicate,
			Object:         query.Object,
			Kinds:          query.Kinds,
			TimeRange:      query.TimeRange,
			GraphHops:      query.GraphHops,
			Trust:          trustToDomain(query.Trust),
			IncludeRetired: query.IncludeRetired,
		},
		Now:       now,
		StartedAt: now,
	}
	// Trace is a diagnostic artifact. Stages route inter-stage data through
	// ReadState (e.g. state.MaterializeDrops), so Recall (non-explain) can leave
	// Trace nil and skip per-stage diagnostic allocations entirely.
	// Only RecallExplain (withTrace=true) installs a Trace for the
	// framework's AppendStage hook to populate.
	if withTrace {
		state.EnsureTrace()
	}

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
	if len(hits) == 0 {
		return nil
	}
	out := make([]Hit, len(hits))
	copy(out, hits)
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

type forgetGraphSnapshot struct {
	links                 []domain.FactLink
	observations          []domain.Observation
	deletedObservationIDs []string
}

func (m *memory) snapshotForgetGraphCleanup(ctx context.Context, scope Scope, factID string) (forgetGraphSnapshot, error) {
	var out forgetGraphSnapshot
	if m.linkStore == nil || factID == "" {
		return out, nil
	}
	node := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	links, err := m.linkStore.FindByNode(ctx, scope, node)
	if err != nil {
		return out, fmt.Errorf("graph ledger: snapshot assertion links: %w", err)
	}
	out.links = append([]domain.FactLink(nil), links...)
	deletedObservationIDs, err := graphledger.PlanClearAssertion(ctx, scope, factID, m.observationStore, m.linkStore)
	if err != nil {
		return out, err
	}
	out.deletedObservationIDs = append([]string(nil), deletedObservationIDs...)
	if m.observationStore == nil || len(deletedObservationIDs) == 0 {
		return out, nil
	}
	out.observations = make([]domain.Observation, 0, len(deletedObservationIDs))
	for _, observationID := range deletedObservationIDs {
		obs, err := m.observationStore.Get(ctx, scope, observationID)
		if err != nil {
			return out, fmt.Errorf("graph ledger: snapshot observation: %w", err)
		}
		out.observations = append(out.observations, obs)
	}
	return out, nil
}

// compensateForgetFailure runs when hard Forget fails after any derived state
// may have been removed but before the canonical fact is deleted. The canonical
// fact still exists, so best-effort compensation restores graph/observation
// derived state and re-projects the fact snapshot.
func (m *memory) compensateForgetFailure(ctx context.Context, scope Scope, snapshot domain.TemporalFact, graphSnapshot forgetGraphSnapshot, cause error) {
	cleanupCtx := pipeline.DetachCancel(ctx)
	if len(graphSnapshot.observations) > 0 && m.observationStore != nil {
		_ = m.observationStore.Append(cleanupCtx, append([]domain.Observation(nil), graphSnapshot.observations...))
	}
	if len(graphSnapshot.links) > 0 && m.linkStore != nil {
		_ = m.linkStore.Append(cleanupCtx, append([]domain.FactLink(nil), graphSnapshot.links...))
	}
	if len(graphSnapshot.observations) > 0 && m.observationProjection != nil {
		_ = m.observationProjection.ProjectObservations(cleanupCtx, append([]domain.Observation(nil), graphSnapshot.observations...))
	}
	_ = m.fanout.ProjectRequired(cleanupCtx, []domain.TemporalFact{snapshot})
	_ = cause
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
	m.holdWriteTelemetry()
	unlock := m.lockWriteScope(scope)
	defer func() {
		unlock()
		m.flushWriteTelemetry()
	}()

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
	m.holdWriteTelemetry()
	unlock := m.lockWriteScope(scope)
	defer func() {
		unlock()
		m.flushWriteTelemetry()
	}()

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
	if m.observationStore != nil {
		if err := m.observationStore.Close(); err != nil {
			return err
		}
	}
	if m.linkStore != nil {
		if err := m.linkStore.Close(); err != nil {
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
