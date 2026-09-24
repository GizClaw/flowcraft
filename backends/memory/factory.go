package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	summaryderive "github.com/GizClaw/flowcraft/backends/memory/derive/summary"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	"github.com/GizClaw/flowcraft/backends/memory/lines/knowledge"
	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/backends/memory/projection/bm25"
	"github.com/GizClaw/flowcraft/backends/memory/projection/entity"
	"github.com/GizClaw/flowcraft/backends/memory/projection/vector"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/rerank"
	"github.com/GizClaw/flowcraft/backends/memory/sources"
	docsource "github.com/GizClaw/flowcraft/backends/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/backends/memory/verify"
	docview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	"github.com/GizClaw/flowcraft/backends/memory/worker"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// ResourceKind is the deployment resource kind implemented by this module.
const ResourceKind = corememory.AssemblyKind

// Impl is the deployment implementation name registered under ResourceKind.
const Impl = "flowcraft"

// Option configures the factory and every assembly it builds.
type Option func(*factoryOptions)

type factoryOptions struct {
	clock          func() time.Time
	deriver        component.Deriver
	deriverVersion string
}

// deriverPolicy is the policy contribution of a caller-supplied deriver. The
// built-in settings-driven extractor contributes nothing (its algorithm
// versions are already part of the digest); any override contributes at least
// a marker, so swapping the deriver re-derives instead of trusting watermarks
// written by the previous one.
func (options factoryOptions) deriverPolicy() string {
	if options.deriver == nil {
		return ""
	}
	if version := strings.TrimSpace(options.deriverVersion); version != "" {
		return version
	}
	return "custom-deriver"
}

// WithClock replaces the clock used for record timestamps.
func WithClock(clock func() time.Time) Option {
	return func(options *factoryOptions) {
		if clock != nil {
			options.clock = clock
		}
	}
}

// WithDeriver overrides chat fact extraction with a caller-supplied deriver.
// Tests and hosts with custom pipelines use it; nil clears the override.
func WithDeriver(deriver component.Deriver) Option {
	return func(options *factoryOptions) {
		options.deriver = deriver
	}
}

// WithDeriverVersion names the derivation policy a caller-supplied deriver
// implements. The name folds into the assembly's policy digest, which keys the
// worker watermarks: replacing the deriver without it would leave existing
// scopes looking up to date and they would never be re-derived. Omitting the
// version keeps the conservative default ("custom-deriver"), which invalidates
// watermarks written by the built-in deriver.
func WithDeriverVersion(version string) Option {
	return func(options *factoryOptions) {
		options.deriverVersion = version
	}
}

type factory struct {
	options factoryOptions
}

// NewFactory returns the deployment resource factory for this module.
func NewFactory(options ...Option) resource.Factory {
	value := factory{}
	for _, option := range options {
		if option != nil {
			option(&value.options)
		}
	}
	return value
}

// Spec implements resource.Factory. The inference dependency is required
// when generate or embed models are configured.
func (factory) Spec() resource.Spec {
	return resource.Spec{
		Kind:     ResourceKind,
		Impl:     Impl,
		ItemType: "memory.System",
		Deps: []resource.DepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "inference", Type: "inference.Assembly"},
		},
	}
}

// New implements resource.Factory.
func (f factory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: decode settings: %w", err))
	}
	settings.applyDefaults()
	if err := settings.Validate(); err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: %w", err))
	}
	interval, err := settings.interval()
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: %w", err))
	}
	ws, err := resolveWorkspace(in)
	if err != nil {
		return nil, err
	}
	engine, err := resolveInference(in, settings)
	if err != nil {
		return nil, err
	}
	clock := f.options.clock
	if clock == nil {
		clock = time.Now
	}
	logStore, kvStore, closers, err := openStorage(ctx, settings.Storage, ws)
	if err != nil {
		return nil, err
	}
	// Any failure below must release the pools opened above instead of
	// leaking them.
	assemblyBuilt := false
	defer func() {
		if assemblyBuilt {
			return
		}
		for index := len(closers) - 1; index >= 0; index-- {
			_ = closers[index].Close()
		}
	}()
	messages, err := msgsource.NewMessageStore(logStore, msgsource.WithClock(clock))
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: message store: %w", err))
	}
	documents, err := docsource.NewDocumentStore(logStore, kvStore, docsource.WithClock(clock))
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: document store: %w", err))
	}
	documentViews, err := docview.NewDocumentViewStore(kvStore)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: document view: %w", err))
	}
	chunker, err := knowledge.NewChunker(knowledge.ChunkerConfig{
		MaxRunes:     settings.Chunk.MaxRunes,
		OverlapRunes: settings.Chunk.OverlapRunes,
		Summary: knowledge.SummaryConfig{
			Document: settings.Chunk.Summary.Document,
			Sections: settings.Chunk.Summary.Sections,
			MaxRunes: settings.Chunk.Summary.MaxRunes,
		},
	})
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: knowledge chunker: %w", err))
	}
	catalog, err := sources.NewScopeCatalog(kvStore)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: scope catalog: %w", err))
	}
	digest, err := policyDigest(settings, f.options.deriverPolicy())
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: policy digest: %w", err))
	}

	facts, err := factview.NewFactStore(logStore, kvStore, factview.WithClock(clock))
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: fact view: %w", err))
	}
	maintainStore, err := maintain.NewStore(kvStore, maintain.WithClock(clock))
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: maintain store: %w", err))
	}
	maintainService := &maintain.Service{Facts: facts, Store: maintainStore, Clock: clock}
	var summaries *summaryview.SummaryStore
	var compactor *summaryderive.Compactor
	if !settings.Summary.Disabled {
		summaries, err = summaryview.NewSummaryStore(logStore, kvStore, summaryview.WithClock(clock))
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf("memory config: summary view: %w", err))
		}
		// A configured generate model upgrades summaries from the extractive
		// fallback to model-written compression; failures still degrade to
		// the extractive path inside the compactor.
		var summarizer summaryderive.Summarizer
		if !settings.Generate.isZero() {
			summarizer, err = summaryderive.NewLLMSummarizer(engine, settings.Generate.ref())
			if err != nil {
				return nil, errdefs.Validation(fmt.Errorf("memory config: summary summarizer: %w", err))
			}
		}
		compactor, err = summaryderive.New(summaryderive.Config{
			ChunkSize:         settings.Summary.ChunkSize,
			CondenseThreshold: settings.Summary.CondenseThreshold,
			GroupSize:         settings.Summary.GroupSize,
			MaxDepth:          settings.Summary.MaxDepth,
		}, summaries, summarizer)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf("memory config: summary compactor: %w", err))
		}
	}
	indexers, lanes, auditors, err := buildProjections(settings, kvStore, engine)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: projections: %w", err))
	}
	// The summary branch is a first-class fusion lane. Appending its
	// candidates after fusion (as an earlier revision did) skipped lane
	// calibration and weighting, so its raw lexical ratios were not
	// comparable with the calibrated hybrid scores.
	if summaries != nil {
		lanes = append(lanes, fusion.Lane{
			Name: "summary", Searcher: &summaryview.Searcher{Store: summaries},
			Weight: settings.Lanes.Summary.Weight, Calibrator: fusion.Identity{CalibrationVersion: "identity-v1"},
		})
	}
	fusor, err := fusion.NewWithOptions(lanes, fusion.Options{Mode: fusionMode(settings)})
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: fusion: %w", err))
	}
	deriver, err := buildDeriver(f.options.deriver, settings, engine, facts)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: fact derivation: %w", err))
	}
	providerConfig := retrieval.ProviderConfig{
		Fusion:   fusor,
		Messages: messages,
		Hydrator: &hydrate.Composite{Messages: messages, Facts: facts, Chunks: documentViews, Summaries: summaries},
		Packer:   pack.New(nil),
		Recent: retrieval.RecentConfig{
			MaxItems:  settings.Recent.MaxItems,
			MaxTokens: settings.Recent.MaxTokens,
		},
		SourceQuotes:  settings.Retrieval.SourceQuotes,
		ExpandParents: true,
		ScoreAdjuster: maintainStore,
	}
	if settings.Retrieval.Rerank && !settings.Generate.isZero() {
		reranker, rerankErr := rerank.New(engine, settings.Generate.ref())
		if rerankErr != nil {
			return nil, errdefs.Validation(fmt.Errorf("memory config: item reranker: %w", rerankErr))
		}
		providerConfig.ItemReranker = reranker
	}
	provider, err := retrieval.NewProviderWithConfig(providerConfig)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: retrieval provider: %w", err))
	}
	workerCheckpoints, err := worker.NewKVCheckpoints(kvStore, worker.WithClock(clock))
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: checkpoints: %w", err))
	}
	processor, err := worker.NewProcessor(worker.Config{
		Messages: messages, Documents: documents, DocumentViews: documentViews,
		Facts: facts, Deriver: deriver, KnowledgeDeriver: chunker, Compactor: compactor,
		Indexers: indexers, Checkpoints: workerCheckpoints,
		Projection: settings.Projection, PolicyDigest: digest,
		Concurrency: settings.Derive.Concurrency,
	})
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: processor: %w", err))
	}
	seeds := make([]corememory.Scope, 0, len(settings.Scopes))
	for _, seed := range settings.Scopes {
		seeds = append(seeds, seed.scope())
	}
	assembly, err := newAssembly(
		messages, documents, facts, summaries, catalog, seeds, settings.Recent,
		provider, processor, maintainService, interval, clock, closers,
		buildVerifier(facts, summaries, auditors),
	)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: %w", err))
	}
	assemblyBuilt = true
	return assembly, nil
}

// Register adds the memory assembly factory to r.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
}

func resolveWorkspace(in resource.Input) (workspace.Workspace, error) {
	raw, ok := in.Dep("workspace")
	if !ok {
		return nil, errdefs.Validationf("memory config: dependency %q is required", "workspace")
	}
	ws, ok := raw.(workspace.Workspace)
	if !ok || nilInterface(ws) {
		return nil, errdefs.Validationf("memory config: dependency %q has the wrong type", "workspace")
	}
	return ws, nil
}

func resolveInference(in resource.Input, settings Settings) (*inference.Assembly, error) {
	raw, ok := in.Dep("inference")
	var engine *inference.Assembly
	if ok {
		var typed bool
		engine, typed = raw.(*inference.Assembly)
		if !typed || engine == nil {
			return nil, errdefs.Validationf("memory config: dependency %q has the wrong type", "inference")
		}
	}
	if (!settings.Generate.isZero() || !settings.Embed.isZero()) && engine == nil {
		return nil, errdefs.Validationf(
			"memory config: dependency %q is required when generate or embed models are configured", "inference")
	}
	return engine, nil
}

func buildDeriver(
	override component.Deriver,
	settings Settings,
	engine *inference.Assembly,
	facts *factview.FactStore,
) (component.Deriver, error) {
	if override != nil {
		return override, nil
	}
	if settings.Generate.isZero() {
		return nil, nil
	}
	generateRef := settings.Generate.ref()
	config := chat.Config{
		Strategy:               settings.Fact.strategy(),
		TailMaxChars:           settings.Fact.TailMaxChars,
		MaxFacts:               settings.Fact.MaxFacts,
		MaxFactChars:           settings.Fact.MaxFactChars,
		MaxQueryChars:          settings.Fact.MaxQueryChars,
		MaxEmbeddingInputChars: settings.Fact.MaxEmbeddingInputChars,
		Runtime:                engine,
		GenerateModel:          &generateRef,
		Facts:                  facts,
	}
	if !settings.Embed.isZero() {
		embedRef := settings.Embed.ref()
		config.EmbedModel = &embedRef
	}
	return chat.NewFactExtractorWithConfig(config)
}

// projectionAuditor names one projection audit surface used by verification.
type projectionAuditor struct {
	Name    string
	Auditor component.Auditor
}

// collectVerifyEvidence gathers the fact, source, summary, and projection
// evidence the read-only verifier inspects.
func collectVerifyEvidence(
	ctx context.Context,
	facts *factview.FactStore,
	summaries *summaryview.SummaryStore,
	auditors []projectionAuditor,
	scope corememory.Scope,
	conversationID string,
) (verify.Input, error) {
	values, err := facts.List(ctx, scope, conversationID, factview.ListOptions{})
	if err != nil {
		return verify.Input{}, err
	}
	evidence := verify.Input{}
	factsByID := make(map[string]factview.Fact, len(values))
	for _, value := range values {
		factsByID[value.ID] = value
		evidence.Facts = append(evidence.Facts, verify.FactEvidence{
			ID: value.ID, LinkedIDs: append([]string(nil), value.LinkedMemoryIDs...),
		})
	}
	sourceEvidence, err := facts.AuditSourceDigests(ctx, scope, conversationID)
	if err != nil {
		return verify.Input{}, err
	}
	for _, value := range sourceEvidence {
		evidence.Sources = append(evidence.Sources, verify.SourceViewEvidence{
			Name: value.Name, SourceDigest: value.ComputedDigest, ViewDigest: value.StoredDigest,
		})
	}
	if summaries != nil && strings.TrimSpace(conversationID) != "" {
		summaryValues, err := summaries.ListActive(ctx, scope, conversationID, summaryview.ListOptions{})
		if err != nil {
			return verify.Input{}, err
		}
		summariesByID := make(map[string]summaryview.Record, len(summaryValues))
		for _, value := range summaryValues {
			summariesByID[value.ID] = value
		}
		for _, value := range summaryValues {
			computed := ""
			inputKind := verify.SummaryInputSummary
			if value.Level == summaryview.L0 && len(value.InputIDs) == 1 {
				inputKind = verify.SummaryInputFact
				if input, ok := factsByID[value.InputIDs[0]]; ok {
					// Rebuild the input exactly like both compaction
					// callers do; the digest covers the coverage range.
					computed = summaryderive.ComputeL0SourceDigest(summaryderive.InputFromFact(input))
				}
			} else {
				children := make([]summaryview.Record, 0, len(value.InputIDs))
				for _, id := range value.InputIDs {
					if child, ok := summariesByID[id]; ok {
						children = append(children, child)
					}
				}
				if len(children) == len(value.InputIDs) {
					computed = summaryderive.ComputeRollupSourceDigest(children)
				}
			}
			evidence.Summaries = append(evidence.Summaries, verify.SummaryEvidence{
				ID: value.ID, Level: uint8(value.Level), InputKind: inputKind, InputIDs: append([]string(nil), value.InputIDs...),
				CoverageValid: value.CoverageRange.Validate() == nil,
				SourceDigest:  value.SourceDigest, ComputedSourceDigest: computed,
			})
		}
	}
	for _, auditor := range auditors {
		if auditor.Auditor == nil {
			continue
		}
		storedSource, computedSource, storedBuild, computedBuild, found, err := auditor.Auditor.AuditDigests(ctx, scope)
		if err != nil {
			return verify.Input{}, err
		}
		if !found {
			continue
		}
		evidence.Projections = append(evidence.Projections, verify.ProjectionEvidence{
			Name:               auditor.Name,
			StoredSourceDigest: storedSource, ComputedSourceDigest: computedSource,
			StoredBuildDigest: storedBuild, ComputedBuildDigest: computedBuild,
		})
	}
	return evidence, nil
}

// buildVerifier exposes the evidence-based integrity check as an explicit
// read-only entry point used by Assembly.Verify.
func buildVerifier(
	facts *factview.FactStore,
	summaries *summaryview.SummaryStore,
	auditors []projectionAuditor,
) func(context.Context, corememory.Scope, string) (verify.Plan, error) {
	return func(ctx context.Context, scope corememory.Scope, conversationID string) (verify.Plan, error) {
		evidence, err := collectVerifyEvidence(ctx, facts, summaries, auditors, scope, conversationID)
		if err != nil {
			return verify.Plan{}, err
		}
		return verify.InspectContext(ctx, scope, evidence)
	}
}

func buildProjections(
	settings Settings,
	kvStore storage.Store,
	engine *inference.Assembly,
) ([]worker.ProjectionIndexer, []fusion.Lane, []projectionAuditor, error) {
	bm25Index, err := bm25.New(bm25.Config{
		KV: kvStore, Projection: settings.Projection, K1: settings.BM25.K1, B: settings.BM25.B,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	entityIndex, err := entity.New(entity.Config{KV: kvStore, Projection: settings.Projection})
	if err != nil {
		return nil, nil, nil, err
	}
	indexers := []worker.ProjectionIndexer{
		{Name: "bm25", Indexer: bm25Index},
		{Name: "entity", Indexer: entityIndex},
	}
	lanes := []fusion.Lane{
		{Name: "bm25", Searcher: bm25Index, Weight: settings.Lanes.BM25.Weight, Calibrator: fusion.BM25QuerySigmoid{}},
		{Name: "entity", Searcher: entityIndex, Weight: settings.Lanes.Entity.Weight, Calibrator: fusion.Identity{CalibrationVersion: "identity-v1"}},
	}
	auditors := []projectionAuditor{
		{Name: "bm25", Auditor: bm25Index},
		{Name: "entity", Auditor: entityIndex},
	}
	if settings.Embed.isZero() {
		return indexers, lanes, auditors, nil
	}
	vectorIndex, err := vector.New(vector.Config{
		KV: kvStore, Runtime: engine, Model: settings.Embed.ref(), Projection: settings.Projection,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	indexers = append(indexers, worker.ProjectionIndexer{Name: "vector", Indexer: vectorIndex})
	lanes = append(lanes, fusion.Lane{
		Name: "vector", Searcher: vectorIndex, Weight: settings.Lanes.Vector.Weight, Calibrator: fusion.Cosine{},
	})
	auditors = append(auditors, projectionAuditor{Name: "vector", Auditor: vectorIndex})
	return indexers, lanes, auditors, nil
}

// policyInput is the derivation policy identity: only settings that change the
// content of derived records belong here. Retrieval-only knobs, storage
// drivers, and seeded scopes must not invalidate derivation
// watermarks, or an operational tweak would re-derive every scope from
// scratch. Projection stays in: its name namespaces the projection stores, so
// changing it must rebuild the lanes.
type policyInput struct {
	Generate   ModelSettings   `json:"generate,omitempty"`
	Embed      ModelSettings   `json:"embed,omitempty"`
	Fact       FactSettings    `json:"fact,omitempty"`
	Summary    SummarySettings `json:"summary,omitempty"`
	Chunk      ChunkSettings   `json:"chunk,omitempty"`
	Projection string          `json:"projection,omitempty"`
	Deriver    string          `json:"deriver,omitempty"`
	Algorithms []string        `json:"algorithms,omitempty"`
}

func policyDigest(settings Settings, deriverPolicy string) (string, error) {
	encoded, err := json.Marshal(policyInput{
		Generate: settings.Generate, Embed: settings.Embed,
		Fact: settings.Fact, Summary: settings.Summary, Chunk: settings.Chunk,
		Projection: settings.Projection,
		Deriver:    deriverPolicy,
		Algorithms: []string{
			chat.AlgorithmVersion, chat.LinkAlgorithmVersion, chat.CanonicalAlgorithmVersion,
			knowledge.AlgorithmVersion, summaryderive.AlgorithmVersion,
		},
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("memory-policy-v2\x00"), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

// fusionMode maps the settings string onto the fusion algorithm.
func fusionMode(settings Settings) fusion.Mode {
	if settings.Lanes.Fusion == "weighted" {
		return fusion.ModeWeighted
	}
	return fusion.ModeRRF
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
