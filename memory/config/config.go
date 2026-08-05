// Package config assembles the canonical memory capability system.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	rootmemory "github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/derive"
	summaryderive "github.com/GizClaw/flowcraft/memory/derive/summary"
	"github.com/GizClaw/flowcraft/memory/lifecycle"
	"github.com/GizClaw/flowcraft/memory/lines/chat"
	"github.com/GizClaw/flowcraft/memory/lines/knowledge"
	"github.com/GizClaw/flowcraft/memory/projection/bm25"
	"github.com/GizClaw/flowcraft/memory/projection/entity"
	"github.com/GizClaw/flowcraft/memory/projection/vector"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/memory/retrieval/pack"
	"github.com/GizClaw/flowcraft/memory/sources"
	docsource "github.com/GizClaw/flowcraft/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/memory/sources/message"
	docview "github.com/GizClaw/flowcraft/memory/views/document"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	observationview "github.com/GizClaw/flowcraft/memory/views/observation"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	"github.com/GizClaw/flowcraft/memory/worker"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const (
	defaultInterval   = time.Minute
	defaultProjection = "default"
	defaultChunkRunes = 1600
	defaultOverlap    = 160
)

type ChunkSettings struct {
	MaxRunes     int                      `yaml:"max_runes,omitempty"`
	OverlapRunes int                      `yaml:"overlap_runes,omitempty"`
	Summary      KnowledgeSummarySettings `yaml:"summary,omitempty"`
}

// KnowledgeSummarySettings explicitly enables deterministic hierarchy
// summaries; disabled fields produce no summary records.
type KnowledgeSummarySettings struct {
	Document bool `yaml:"document,omitempty"`
	Sections bool `yaml:"sections,omitempty"`
	MaxRunes int  `yaml:"max_runes,omitempty"`
}

// RecentSettings bounds canonical conversation reads before final packing.
type RecentSettings struct {
	MaxItems  int `yaml:"max_items,omitempty"`
	MaxTokens int `yaml:"max_tokens,omitempty"`
}

// SummarySettings enables the durable summary branch by default. Disabled is
// the typed opt-out; zero algorithm fields select compactor defaults.
type SummarySettings struct {
	Disabled          bool `yaml:"disabled,omitempty"`
	ChunkSize         int  `yaml:"chunk_size,omitempty"`
	CondenseThreshold int  `yaml:"condense_threshold,omitempty"`
	GroupSize         int  `yaml:"group_size,omitempty"`
	MaxDepth          int  `yaml:"max_depth,omitempty"`
}

// FactSettings selects extraction detail and explicit resource caps.
type FactSettings struct {
	Strategy               chat.FactStrategy `yaml:"strategy,omitempty"`
	TailMaxChars           int               `yaml:"tail_max_chars,omitempty"`
	MaxFacts               int               `yaml:"max_facts,omitempty"`
	MaxFactChars           int               `yaml:"max_fact_chars,omitempty"`
	MaxQueryChars          int               `yaml:"max_query_chars,omitempty"`
	MaxEmbeddingInputChars int               `yaml:"max_embedding_input_chars,omitempty"`
}

// CalibrationSettings selects one supported score calibration.
type CalibrationSettings struct {
	Kind          string  `yaml:"kind,omitempty"`
	Version       string  `yaml:"version,omitempty"`
	Slope         float64 `yaml:"slope,omitempty"`
	Midpoint      float64 `yaml:"midpoint,omitempty"`
	Scale         float64 `yaml:"scale,omitempty"`
	SemanticFloor float64 `yaml:"semantic_floor,omitempty"`
	DisableFloor  bool    `yaml:"disable_floor,omitempty"`
}

type LaneSettings struct {
	Weight      float64             `yaml:"weight,omitempty"`
	Calibration CalibrationSettings `yaml:"calibration,omitempty"`
}

// LanesSettings deliberately has exactly three fields. The architecture has
// exactly three lanes; it is not an extensible YAML list.
type LanesSettings struct {
	Vector LaneSettings `yaml:"vector,omitempty"`
	BM25   LaneSettings `yaml:"bm25,omitempty"`
	Entity LaneSettings `yaml:"entity,omitempty"`
}

type BM25Settings struct {
	Version string  `yaml:"version,omitempty"`
	K1      float64 `yaml:"k1,omitempty"`
	B       float64 `yaml:"b,omitempty"`
}

type ProjectionStorageSettings struct {
	AlgorithmVersion string `json:"algorithm_version" yaml:"algorithm_version,omitempty"`
	MaxSegments      int    `json:"max_segments" yaml:"max_segments,omitempty"`
	MaxDeltaBytes    int64  `json:"max_delta_bytes" yaml:"max_delta_bytes,omitempty"`
}

// RerankerSettings enables an optional programmatic post-fusion reranker.
// Version is required when enabled and participates in policy identity.
type RerankerSettings struct {
	Enabled bool               `yaml:"enabled,omitempty"`
	Version string             `yaml:"version,omitempty"`
	Value   component.Reranker `yaml:"-"`
}

// Algorithm identifies one semantic implementation version. Catalog order is
// irrelevant; names must be unique.
type Algorithm struct {
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"`
}

// AlgorithmCatalog contributes custom algorithm versions to policy identity.
type AlgorithmCatalog []Algorithm

// FactoryCatalog stores custom typed factories. It is programmatic-only;
// YAML may select only the explicit built-in unions below.
type FactoryCatalog = component.Registry

// NewFactoryCatalog returns an empty custom typed factory catalog.
func NewFactoryCatalog() *FactoryCatalog { return component.NewRegistry() }

// ChatNodeSettings is a closed YAML union plus an opaque programmatic custom
// typed factory selection. Exactly one algorithm must be selected.
type ChatNodeSettings struct {
	ID        string        `yaml:"id"`
	DependsOn []string      `yaml:"depends_on,omitempty"`
	Fact      *FactSettings `yaml:"fact,omitempty"`
	custom    *component.DeriverSpec
}

type ChatDAGSettings struct {
	Nodes []ChatNodeSettings `yaml:"nodes,omitempty"`
}

// CustomChatNode binds a typed custom factory selection to a chat node.
func CustomChatNode(id string, spec component.DeriverSpec, dependsOn ...string) ChatNodeSettings {
	return ChatNodeSettings{ID: id, DependsOn: append([]string(nil), dependsOn...), custom: &spec}
}

type KnowledgeNodeSettings struct {
	ID        string         `yaml:"id"`
	DependsOn []string       `yaml:"depends_on,omitempty"`
	Chunk     *ChunkSettings `yaml:"chunk,omitempty"`
	custom    *component.DeriverSpec
}

type KnowledgeDAGSettings struct {
	Nodes []KnowledgeNodeSettings `yaml:"nodes,omitempty"`
}

// CustomKnowledgeNode binds a typed custom factory selection to a knowledge node.
func CustomKnowledgeNode(id string, spec component.DeriverSpec, dependsOn ...string) KnowledgeNodeSettings {
	return KnowledgeNodeSettings{ID: id, DependsOn: append([]string(nil), dependsOn...), custom: &spec}
}

// Settings is the complete programmatic and deploy resource configuration.
type Settings struct {
	Generate                ModelSettings             `yaml:"generate"`
	Embed                   ModelSettings             `yaml:"embed"`
	Scopes                  []ScopeSettings           `yaml:"scopes"`
	Interval                time.Duration             `yaml:"interval,omitempty"`
	Projection              string                    `yaml:"projection,omitempty"`
	Fact                    FactSettings              `yaml:"fact,omitempty"`
	Chunk                   ChunkSettings             `yaml:"chunk,omitempty"`
	Recent                  RecentSettings            `yaml:"recent,omitempty"`
	Summary                 SummarySettings           `yaml:"summary,omitempty"`
	BM25                    BM25Settings              `yaml:"bm25,omitempty"`
	ProjectionStorage       ProjectionStorageSettings `yaml:"projection_storage,omitempty"`
	Reranker                RerankerSettings          `yaml:"reranker,omitempty"`
	Lanes                   LanesSettings             `yaml:"lanes,omitempty"`
	ChatDAG                 ChatDAGSettings           `yaml:"chat_dag,omitempty"`
	KnowledgeDAG            KnowledgeDAGSettings      `yaml:"knowledge_dag,omitempty"`
	LifecycleDAG            LifecycleDAGSettings      `yaml:"lifecycle_dag,omitempty"`
	Lifecycle               LifecycleSettings         `yaml:"lifecycle,omitempty"`
	AlgorithmCatalog        AlgorithmCatalog          `yaml:"algorithm_catalog,omitempty"`
	FactoryCatalog          *FactoryCatalog           `yaml:"-"`
	LifecycleFactoryCatalog *lifecycle.Catalog        `yaml:"-"`
	LifecycleEffects        lifecycle.EffectSink      `yaml:"-"`
	PolicyNamespace         string                    `yaml:"policy_namespace,omitempty"`
}

// Assembly owns the worker runner and exposes the capability System.
type Assembly struct {
	System          *rootmemory.System
	Runner          *worker.Runner
	ChatDAG         *derive.DAG
	KnowledgeDAG    *derive.DAG
	LifecycleDAG    *lifecycle.DAG
	LifecycleRunner *lifecycle.DreamingRunner
	PolicyDigest    string
}

// Close is idempotent. The stores and inference/workspace dependencies are
// borrowed; only the runner belongs to the Assembly.
func (a *Assembly) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	if a.Runner != nil {
		errs = append(errs, a.Runner.Close())
	}
	if a.LifecycleRunner != nil {
		errs = append(errs, a.LifecycleRunner.Close())
	}
	return errors.Join(errs...)
}

// ResolveItem exposes the typed deploy item without transferring ownership.
func (a *Assembly) ResolveItem(ref string) (any, bool) {
	if a == nil || ref != "system" || a.System == nil {
		return nil, false
	}
	return a.System, true
}

// Context implements sdkmemory.ContextProvider.
func (a *Assembly) Context(ctx context.Context, request sdkmemory.ContextRequest) (sdkmemory.ContextResult, error) {
	if a == nil || a.System == nil {
		return sdkmemory.ContextResult{}, errors.New("memory config: assembly has no system")
	}
	return a.System.Context(ctx, request)
}

// CommitTurn implements sdkmemory.TurnSink.
func (a *Assembly) CommitTurn(ctx context.Context, turn sdkmemory.Turn) error {
	if a == nil || a.System == nil {
		return errors.New("memory config: assembly has no system")
	}
	return a.System.CommitTurn(ctx, turn)
}

// PutDocument implements sdkmemory.DocumentSink.
func (a *Assembly) PutDocument(ctx context.Context, document sdkmemory.Document) error {
	if a == nil || a.System == nil {
		return errors.New("memory config: assembly has no system")
	}
	return a.System.PutDocument(ctx, document)
}

// NewAssembly validates settings and wires canonical stores, write-side DAGs,
// exactly three projections, retrieval, the System, Processor, and Runner.
// It performs no background work; callers explicitly start Runner.
func (b *Builder) NewAssembly(_ context.Context, settings Settings) (*Assembly, error) {
	if b == nil || nilInterface(b.workspace) || b.inference == nil {
		return nil, errors.New("memory config: builder is incomplete")
	}
	settings = settings.withDefaults()
	if err := settings.validate(b.inference); err != nil {
		return nil, err
	}

	messages, err := msgsource.NewWorkspaceStore(b.workspace)
	if err != nil {
		return nil, err
	}
	documents, err := docsource.NewWorkspaceStore(b.workspace)
	if err != nil {
		return nil, err
	}
	catalog, err := sources.NewWorkspaceScopeCatalog(b.workspace)
	if err != nil {
		return nil, err
	}
	var outbox *lifecycle.WorkspaceOutbox
	var outboxSink *lifecycle.OutboxSink
	var lifecycleEvents *lifecycle.WorkspaceEventStore
	var repairAudit *lifecycle.WorkspaceRepairAuditStore
	if !settings.Lifecycle.Disabled {
		outbox, err = lifecycle.NewWorkspaceOutbox(b.workspace, nil)
		if err != nil {
			return nil, err
		}
		lifecycleEvents, err = lifecycle.NewWorkspaceEventStore(b.workspace)
		if err != nil {
			return nil, err
		}
		repairAudit, err = lifecycle.NewWorkspaceRepairAuditStore(b.workspace)
		if err != nil {
			return nil, err
		}
		outboxSink = &lifecycle.OutboxSink{Outbox: outbox, Branch: "integrate"}
	}
	factOptions := []factview.Option{}
	if outboxSink != nil {
		factOptions = append(factOptions, factview.WithPublicationSink(outboxSink))
	}
	facts, err := factview.NewWorkspaceStore(b.workspace, factOptions...)
	if err != nil {
		return nil, err
	}
	chunks, err := docview.NewWorkspaceStore(b.workspace)
	if err != nil {
		return nil, err
	}
	checkpoints, err := worker.NewWorkspaceCheckpointStore(b.workspace)
	if err != nil {
		return nil, err
	}
	var summaries summaryview.Store
	var compactor *summaryderive.Compactor
	if !settings.Summary.Disabled {
		summaryStore, summaryErr := summaryview.NewWorkspaceStore(b.workspace)
		if summaryErr != nil {
			return nil, summaryErr
		}
		summaries = summaryStore
		compactor, summaryErr = summaryderive.New(summaryderive.Config{
			ChunkSize: settings.Summary.ChunkSize, CondenseThreshold: settings.Summary.CondenseThreshold,
			GroupSize: settings.Summary.GroupSize, MaxDepth: settings.Summary.MaxDepth,
		}, summaryStore, nil)
		if summaryErr != nil {
			return nil, summaryErr
		}
	}

	generateRef, embedRef := settings.Generate.ref(), settings.Embed.ref()
	vectorIndex, err := vector.New(vector.Config{
		Workspace: b.workspace, Runtime: b.inference, Model: embedRef, Projection: settings.Projection,
		Thresholds: vector.Thresholds{
			MaxSegments:   settings.ProjectionStorage.MaxSegments,
			MaxDeltaBytes: settings.ProjectionStorage.MaxDeltaBytes,
		},
	})
	if err != nil {
		return nil, err
	}
	registry := settings.FactoryCatalog.Clone()
	if err := component.RegisterTypedDeriver(registry, "chat.fact", chat.AlgorithmVersion, component.Ports{
		Inputs: []component.ArtifactKind{chat.KindRawMessage}, Outputs: []component.ArtifactKind{chat.KindFact},
	}, func(config FactSettings) (component.Deriver, error) {
		factConfig := config.extractorConfig()
		factConfig.Runtime, factConfig.Facts = b.inference, facts
		if config.Strategy != chat.StrategyNone {
			factConfig.GenerateModel = &generateRef
		}
		factConfig.EmbedModel = &embedRef
		factConfig.LinkVectorSearcher = vectorIndex
		return chat.NewFactExtractorWithConfig(factConfig)
	}); err != nil {
		return nil, err
	}
	if err := component.RegisterTypedDeriver(registry, "knowledge.chunk", knowledge.AlgorithmVersion, component.Ports{
		Inputs: []component.ArtifactKind{knowledge.KindDocument}, Outputs: []component.ArtifactKind{
			knowledge.KindResource, knowledge.KindSection, knowledge.KindDocumentChunk, knowledge.KindSummary,
		},
	}, func(config ChunkSettings) (component.Deriver, error) {
		return knowledge.NewChunker(knowledge.ChunkerConfig{
			MaxRunes: config.MaxRunes, OverlapRunes: config.OverlapRunes,
			Summary: knowledge.SummaryConfig{
				Document: config.Summary.Document, Sections: config.Summary.Sections,
				MaxRunes: config.Summary.MaxRunes,
			},
		})
	}); err != nil {
		return nil, err
	}
	chatSpec, err := settings.chatSpec()
	if err != nil {
		return nil, err
	}
	chatDAG, err := derive.Build(registry, chatSpec)
	if err != nil {
		return nil, err
	}
	knowledgeSpec, err := settings.knowledgeSpec()
	if err != nil {
		return nil, err
	}
	knowledgeDAG, err := derive.Build(registry, knowledgeSpec)
	if err != nil {
		return nil, err
	}
	lifecycleDAG, err := settings.lifecycleDAG()
	if err != nil {
		return nil, err
	}
	digest, err := computePolicyDigest(settings, registry)
	if err != nil {
		return nil, err
	}
	if outboxSink != nil {
		outboxSink.PolicyDigest = digest
	}

	bm25Index, err := bm25.New(bm25.Config{
		Workspace: b.workspace, Projection: settings.Projection, K1: settings.BM25.K1, B: settings.BM25.B,
		Thresholds: bm25.Thresholds{
			MaxSegments:   settings.ProjectionStorage.MaxSegments,
			MaxDeltaBytes: settings.ProjectionStorage.MaxDeltaBytes,
		},
	})
	if err != nil {
		return nil, err
	}
	entityIndex, err := entity.New(entity.Config{
		Workspace: b.workspace, Projection: settings.Projection,
		Thresholds: entity.Thresholds{
			MaxSegments:   settings.ProjectionStorage.MaxSegments,
			MaxDeltaBytes: settings.ProjectionStorage.MaxDeltaBytes,
		},
	})
	if err != nil {
		return nil, err
	}
	indexes := map[string]component.Indexer{"vector": vectorIndex, "bm25": bm25Index, "entity": entityIndex}
	searchers := map[string]component.Searcher{"vector": vectorIndex, "bm25": bm25Index, "entity": entityIndex}
	for _, name := range []string{"vector", "bm25", "entity"} {
		indexer, searcher := indexes[name], searchers[name]
		if err := registry.RegisterIndexer(name, func(component.Spec) (component.Indexer, error) { return indexer, nil }); err != nil {
			return nil, err
		}
		if err := registry.RegisterSearcher(name, func(component.Spec) (component.Searcher, error) { return searcher, nil }); err != nil {
			return nil, err
		}
	}
	if err := registry.RegisterPacker("deterministic", func(component.Spec) (component.Packer, error) {
		return pack.New(nil), nil
	}); err != nil {
		return nil, err
	}

	var indexers []worker.ProjectionIndexer
	var lanes []fusion.Lane
	laneSettings := map[string]LaneSettings{
		"vector": settings.Lanes.Vector, "bm25": settings.Lanes.BM25, "entity": settings.Lanes.Entity,
	}
	for _, name := range []string{"vector", "bm25", "entity"} {
		indexer, resolveErr := registry.ResolveIndexer(component.Spec{Name: name})
		if resolveErr != nil {
			return nil, resolveErr
		}
		searcher, resolveErr := registry.ResolveSearcher(component.Spec{Name: name})
		if resolveErr != nil {
			return nil, resolveErr
		}
		calibrator, resolveErr := buildCalibrator(laneSettings[name].Calibration)
		if resolveErr != nil {
			return nil, fmt.Errorf("memory config: lane %s: %w", name, resolveErr)
		}
		indexers = append(indexers, worker.ProjectionIndexer{Name: name, Indexer: indexer})
		lanes = append(lanes, fusion.Lane{
			Name: name, Searcher: searcher, Weight: laneSettings[name].Weight, Calibrator: calibrator,
		})
	}
	fusor, err := fusion.New(lanes)
	if err != nil {
		return nil, err
	}
	packer, err := registry.ResolvePacker(component.Spec{Name: "deterministic"})
	if err != nil {
		return nil, err
	}
	var summarySearcher component.Searcher
	if summaries != nil {
		summarySearcher = &summaryview.Searcher{Store: summaries}
	}
	provider, err := retrieval.NewProviderWithConfig(retrieval.ProviderConfig{
		Fusion: fusor, Messages: messages, Hydrator: &hydrate.Composite{
			Messages: messages, Facts: facts, Chunks: chunks, Summaries: summaries,
		}, Summary: summarySearcher, Packer: packer, ExpandParents: true,
		Recent:       retrieval.RecentConfig{MaxItems: settings.Recent.MaxItems, MaxTokens: settings.Recent.MaxTokens},
		Reranker:     retrieval.RerankerConfig{Enabled: settings.Reranker.Enabled, Value: settings.Reranker.Value},
		RecallEvents: lifecycleEvents, Visibility: lifecycleEvents,
	})
	if err != nil {
		return nil, err
	}
	system, err := rootmemory.NewSystem(messages, documents, catalog, provider)
	if err != nil {
		return nil, err
	}
	processor, err := worker.NewProcessor(worker.ProcessorConfig{
		Messages: messages, Documents: documents, Facts: facts, Summaries: summaries, Compactor: compactor, DocumentViews: chunks,
		ChatDAG: chatDAG, KnowledgeDAG: knowledgeDAG, Checkpoints: checkpoints,
		Projection: settings.Projection, PolicyDigest: digest, Indexers: indexers,
	})
	if err != nil {
		return nil, err
	}
	scopes := make([]sdkmemory.Scope, len(settings.Scopes))
	for i, scope := range settings.Scopes {
		scopes[i] = scope.scope()
	}
	runner, err := worker.NewRunner(worker.RunnerConfig{
		Processor: processor, Catalog: catalog, Scopes: scopes, Interval: settings.Interval,
	})
	if err != nil {
		return nil, err
	}
	var lifecycleRunner *lifecycle.DreamingRunner
	if !settings.Lifecycle.Disabled {
		lifecycleCheckpoints, checkpointErr := lifecycle.NewWorkspaceCheckpointStore(b.workspace)
		if checkpointErr != nil {
			return nil, checkpointErr
		}
		observationStore, observationErr := observationview.NewWorkspaceStore(b.workspace)
		if observationErr != nil {
			return nil, observationErr
		}
		decay, decayErr := lifecycle.NewDecay(settings.Lifecycle.Decay, nil)
		if decayErr != nil {
			return nil, decayErr
		}
		compactPhase := lifecycle.TaskPhase(lifecycle.NoopTaskPhase{})
		if compactor != nil {
			compactPhase = lifecycle.TaskPhaseFunc(func(ctx context.Context, task lifecycle.Task) error {
				if task.ConversationID == "" {
					return nil
				}
				values, listErr := facts.List(ctx, task.Scope, task.ConversationID, factview.ListOptions{})
				if listErr != nil {
					return listErr
				}
				inputs := make([]summaryderive.Input, 0, len(values))
				for _, value := range values {
					eventTime := value.EventTime
					if eventTime.IsZero() {
						eventTime = value.CreatedAt
					}
					inputs = append(inputs, summaryderive.Input{
						ID: value.ID, Text: value.Text, Topics: value.Entities, SourceRefs: value.Provenance,
						CoverageRange: summaryview.CoverageRange{StartTime: eventTime, EndTime: eventTime},
					})
				}
				if len(inputs) == 0 {
					return nil
				}
				_, compactErr := compactor.Compact(ctx, summaryderive.CompactRequest{
					Scope: task.Scope, ConversationID: task.ConversationID,
					GenerationID: "lifecycle-" + task.PolicyDigest, PolicySignature: task.PolicyDigest, Inputs: inputs,
				})
				return compactErr
			})
		}
		lifecycleService, serviceErr := lifecycle.NewService(lifecycle.ServiceConfig{
			Facts: facts, Observations: observationStore, Events: lifecycleEvents, Decay: decay,
			Forget:  settings.Lifecycle.Forget,
			Compact: compactPhase, DAG: lifecycleDAG, Checkpoints: lifecycleCheckpoints, Effects: settings.LifecycleEffects,
			Repair: lifecycle.RepairPhaseFunc(func(ctx context.Context, task lifecycle.Task) (lifecycle.RepairPlan, error) {
				values, listErr := facts.List(ctx, task.Scope, task.ConversationID, factview.ListOptions{})
				if listErr != nil {
					return lifecycle.RepairPlan{}, listErr
				}
				evidence := lifecycle.RepairInput{}
				factsByID := make(map[string]factview.Fact, len(values))
				for _, value := range values {
					factsByID[value.ID] = value
					evidence.Facts = append(evidence.Facts, lifecycle.FactEvidence{
						ID: value.ID, LinkedIDs: value.LinkedMemoryIDs,
					})
				}
				sourceEvidence, sourceErr := facts.AuditSourceDigests(ctx, task.Scope, task.ConversationID)
				if sourceErr != nil {
					return lifecycle.RepairPlan{}, sourceErr
				}
				for _, value := range sourceEvidence {
					evidence.Sources = append(evidence.Sources, lifecycle.SourceViewEvidence{
						Name: value.Name, SourceDigest: value.ComputedDigest, ViewDigest: value.StoredDigest,
					})
				}
				observationValues, observationErr := observationStore.List(ctx, task.Scope)
				if observationErr != nil {
					return lifecycle.RepairPlan{}, observationErr
				}
				for _, value := range observationValues {
					evidence.Observations = append(evidence.Observations, lifecycle.ObservationEvidence{
						ID: value.ID, Replaces: value.Replaces,
					})
				}
				if summaries != nil && task.ConversationID != "" {
					summaryValues, summaryErr := summaries.ListActive(ctx, task.Scope, task.ConversationID, summaryview.ListOptions{})
					if summaryErr != nil {
						return lifecycle.RepairPlan{}, summaryErr
					}
					summariesByID := make(map[string]summaryview.Record, len(summaryValues))
					for _, value := range summaryValues {
						summariesByID[value.ID] = value
					}
					for _, value := range summaryValues {
						computed := ""
						inputKind := lifecycle.SummaryInputSummary
						if value.Level == summaryview.L0 && len(value.InputIDs) == 1 {
							inputKind = lifecycle.SummaryInputFact
							if input, ok := factsByID[value.InputIDs[0]]; ok {
								eventTime := input.EventTime
								if eventTime.IsZero() {
									eventTime = input.CreatedAt
								}
								computed = summaryderive.ComputeL0SourceDigest(summaryderive.Input{
									ID: input.ID, Text: input.Text, Topics: input.Entities, SourceRefs: input.Provenance,
									CoverageRange: summaryview.CoverageRange{StartTime: eventTime, EndTime: eventTime},
								})
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
						evidence.Summaries = append(evidence.Summaries, lifecycle.SummaryEvidence{
							ID: value.ID, Level: uint8(value.Level), InputKind: inputKind, InputIDs: value.InputIDs,
							CoverageValid: value.CoverageRange.Validate() == nil,
							SourceDigest:  value.SourceDigest, ComputedSourceDigest: computed,
						})
					}
				}
				for _, projection := range []struct {
					name  string
					audit func(context.Context, sdkmemory.Scope) (string, string, string, string, bool, error)
				}{
					{name: "vector", audit: vectorIndex.AuditDigests},
					{name: "bm25", audit: bm25Index.AuditDigests},
					{name: "entity", audit: entityIndex.AuditDigests},
				} {
					storedSource, computedSource, storedBuild, computedBuild, found, auditErr := projection.audit(ctx, task.Scope)
					if auditErr != nil {
						return lifecycle.RepairPlan{}, auditErr
					}
					if found {
						evidence.Projections = append(evidence.Projections, projectionRepairEvidence(
							projection.name, storedSource, computedSource, storedBuild, computedBuild,
						))
					}
				}
				plan, repairErr := lifecycle.InspectRepairContext(ctx, task.Scope, evidence)
				if repairErr != nil {
					return lifecycle.RepairPlan{}, repairErr
				}
				if repairErr := repairAudit.Save(ctx, plan); repairErr != nil {
					return lifecycle.RepairPlan{}, repairErr
				}
				return plan, nil
			}),
		})
		if serviceErr != nil {
			return nil, serviceErr
		}
		lifecycleRunner, serviceErr = lifecycle.NewDreamingRunner(lifecycle.DreamingRunnerConfig{
			Outbox: outbox, Service: lifecycleService, Catalog: catalog, Scopes: scopes, Owner: settings.Lifecycle.Owner,
			LeaseTTL: settings.Lifecycle.LeaseTTL, Interval: settings.Lifecycle.Interval,
			Periodic: settings.Lifecycle.Periodic, PolicyDigest: digest, Branch: "integrate",
		})
		if serviceErr != nil {
			return nil, serviceErr
		}
	}
	return &Assembly{
		System: system, Runner: runner, ChatDAG: chatDAG, KnowledgeDAG: knowledgeDAG,
		LifecycleDAG: lifecycleDAG, LifecycleRunner: lifecycleRunner, PolicyDigest: digest,
	}, nil
}

func projectionRepairEvidence(
	name, storedSource, computedSource, storedBuild, computedBuild string,
) lifecycle.ProjectionEvidence {
	return lifecycle.ProjectionEvidence{
		Name:               name,
		StoredSourceDigest: storedSource, ComputedSourceDigest: computedSource,
		StoredBuildDigest: storedBuild, ComputedBuildDigest: computedBuild,
	}
}

func (s Settings) withDefaults() Settings {
	if s.Interval == 0 {
		s.Interval = defaultInterval
	}
	if s.Projection == "" {
		s.Projection = defaultProjection
	}
	if s.Recent.MaxItems == 0 {
		s.Recent.MaxItems = 8
	}
	if s.Chunk.MaxRunes == 0 {
		s.Chunk.MaxRunes = defaultChunkRunes
		if s.Chunk.OverlapRunes == 0 {
			s.Chunk.OverlapRunes = defaultOverlap
		}
	}
	if s.BM25.Version == "" {
		s.BM25.Version = bm25.AlgorithmVersion
	}
	storageDefaults := vector.DefaultThresholds()
	if s.ProjectionStorage.AlgorithmVersion == "" {
		s.ProjectionStorage.AlgorithmVersion = vector.StorageAlgorithmVersion
	}
	if s.ProjectionStorage.MaxSegments == 0 {
		s.ProjectionStorage.MaxSegments = storageDefaults.MaxSegments
	}
	if s.ProjectionStorage.MaxDeltaBytes == 0 {
		s.ProjectionStorage.MaxDeltaBytes = storageDefaults.MaxDeltaBytes
	}
	if s.Lifecycle.Interval == 0 {
		s.Lifecycle.Interval = time.Hour
	}
	if s.Lifecycle.LeaseTTL == 0 {
		s.Lifecycle.LeaseTTL = 5 * time.Minute
	}
	if s.Lifecycle.Owner == "" {
		s.Lifecycle.Owner = "sdkx-memory-dreaming"
	}
	if s.Lifecycle.Decay.Version == "" {
		s.Lifecycle.Decay.Version = lifecycle.DecayAlgorithmVersion
	}
	if s.Lifecycle.Decay.HalfLife == 0 {
		s.Lifecycle.Decay.HalfLife = 30 * 24 * time.Hour
	}
	if s.Lifecycle.Decay.RecencyWeight == 0 && s.Lifecycle.Decay.FrequencyWeight == 0 &&
		s.Lifecycle.Decay.RelevanceWeight == 0 {
		s.Lifecycle.Decay.RecencyWeight, s.Lifecycle.Decay.FrequencyWeight, s.Lifecycle.Decay.RelevanceWeight = .5, .3, .2
	}
	if s.Lifecycle.Decay.FrequencyScale == 0 {
		s.Lifecycle.Decay.FrequencyScale = 10
	}
	if s.Lifecycle.Forget.Mode == "" {
		s.Lifecycle.Forget.Mode = lifecycle.ModeAuditOnly
	}
	if s.Lifecycle.Forget.SoftForgetThreshold == 0 {
		s.Lifecycle.Forget.SoftForgetThreshold = .2
	}
	if s.Lifecycle.Forget.ArchiveThreshold == 0 {
		s.Lifecycle.Forget.ArchiveThreshold = .05
	}
	if s.BM25.K1 == 0 && s.BM25.B == 0 {
		s.BM25.K1, s.BM25.B = 1.2, 0.75
	} else if s.BM25.K1 == 0 {
		s.BM25.K1 = 1.2
	}
	factDefaults := chat.DefaultConfig()
	if s.Fact.Strategy == "" {
		s.Fact.Strategy = factDefaults.Strategy
	}
	if s.Fact.TailMaxChars == 0 {
		s.Fact.TailMaxChars = factDefaults.TailMaxChars
	}
	if s.Fact.MaxFacts == 0 {
		s.Fact.MaxFacts = factDefaults.MaxFacts
	}
	if s.Fact.MaxFactChars == 0 {
		s.Fact.MaxFactChars = factDefaults.MaxFactChars
	}
	if s.Fact.MaxQueryChars == 0 {
		s.Fact.MaxQueryChars = factDefaults.MaxQueryChars
	}
	if s.Fact.MaxEmbeddingInputChars == 0 {
		s.Fact.MaxEmbeddingInputChars = factDefaults.MaxEmbeddingInputChars
	}
	applyLaneDefaults := func(lane *LaneSettings, kind string) {
		if lane.Weight == 0 {
			lane.Weight = 1
		}
		if lane.Calibration.Kind == "" {
			lane.Calibration.Kind = kind
		}
		if lane.Calibration.Version == "" {
			switch lane.Calibration.Kind {
			case "cosine":
				lane.Calibration.Version = fusion.CosineCalibrationVersion
			case "bm25_query_sigmoid":
				lane.Calibration.Version = fusion.BM25CalibrationVersion
			case "identity":
				lane.Calibration.Version = "identity-v1"
			}
		}
		if lane.Calibration.Kind == "saturating" && lane.Calibration.Scale == 0 {
			lane.Calibration.Scale = 1
		}
	}
	applyLaneDefaults(&s.Lanes.Vector, "cosine")
	applyLaneDefaults(&s.Lanes.BM25, "bm25_query_sigmoid")
	applyLaneDefaults(&s.Lanes.Entity, "identity")
	if len(s.ChatDAG.Nodes) == 0 {
		fact := s.Fact
		s.ChatDAG.Nodes = []ChatNodeSettings{{ID: "extract-facts", Fact: &fact}}
	}
	for index := range s.ChatDAG.Nodes {
		node := &s.ChatDAG.Nodes[index]
		if node.Fact != nil {
			value := normalizeFactSettings(*node.Fact, s.Fact)
			node.Fact = &value
		}
	}
	if len(s.KnowledgeDAG.Nodes) == 0 {
		chunk := s.Chunk
		s.KnowledgeDAG.Nodes = []KnowledgeNodeSettings{{ID: "chunk-document", Chunk: &chunk}}
	}
	for index := range s.KnowledgeDAG.Nodes {
		node := &s.KnowledgeDAG.Nodes[index]
		if node.Chunk != nil {
			value := normalizeChunkSettings(*node.Chunk, s.Chunk)
			node.Chunk = &value
		}
	}
	if len(s.LifecycleDAG.Nodes) == 0 {
		s.LifecycleDAG.Nodes = []LifecycleNodeSettings{
			{ID: "integrate", Phase: lifecycle.PhaseIntegrate},
			{ID: "compact", Phase: lifecycle.PhaseCompact, DependsOn: []string{"integrate"}},
			{ID: "decay", Phase: lifecycle.PhaseDecay, DependsOn: []string{"compact"}},
			{ID: "forget", Phase: lifecycle.PhaseForget, DependsOn: []string{"decay"}},
			{ID: "repair", Phase: lifecycle.PhaseRepair, DependsOn: []string{"forget"}},
		}
	}
	return s
}

func normalizeFactSettings(value, fallback FactSettings) FactSettings {
	if value.Strategy == "" {
		value.Strategy = fallback.Strategy
	}
	if value.TailMaxChars == 0 {
		value.TailMaxChars = fallback.TailMaxChars
	}
	if value.MaxFacts == 0 {
		value.MaxFacts = fallback.MaxFacts
	}
	if value.MaxFactChars == 0 {
		value.MaxFactChars = fallback.MaxFactChars
	}
	if value.MaxQueryChars == 0 {
		value.MaxQueryChars = fallback.MaxQueryChars
	}
	if value.MaxEmbeddingInputChars == 0 {
		value.MaxEmbeddingInputChars = fallback.MaxEmbeddingInputChars
	}
	return value
}

func normalizeChunkSettings(value, fallback ChunkSettings) ChunkSettings {
	if value.MaxRunes == 0 {
		value.MaxRunes = fallback.MaxRunes
	}
	if value.OverlapRunes == 0 {
		value.OverlapRunes = fallback.OverlapRunes
	}
	if !value.Summary.Document && !value.Summary.Sections && value.Summary.MaxRunes == 0 {
		value.Summary = fallback.Summary
	}
	return value
}

func (s Settings) validate(runtime *inference.Runtime) error {
	if _, err := s.lifecycleDAG(); err != nil {
		return fmt.Errorf("memory config: lifecycle DAG: %w", err)
	}
	requiresGenerate := false
	for _, node := range s.ChatDAG.Nodes {
		if node.Fact != nil && node.Fact.Strategy != chat.StrategyNone {
			requiresGenerate = true
			break
		}
	}
	if requiresGenerate {
		if err := validateModel(runtime, "generate", s.Generate.ref(), inference.OperationGenerate); err != nil {
			return err
		}
	}
	if err := validateModel(runtime, "embed", s.Embed.ref(), inference.OperationEmbed); err != nil {
		return err
	}
	seenScopes := make(map[sdkmemory.Scope]struct{}, len(s.Scopes))
	for i, configured := range s.Scopes {
		scope := configured.scope()
		if err := scope.Validate(); err != nil {
			return fmt.Errorf("memory config: scopes[%d]: %w", i, err)
		}
		if _, duplicate := seenScopes[scope]; duplicate {
			return fmt.Errorf("memory config: duplicate scope %q", scope.String())
		}
		seenScopes[scope] = struct{}{}
	}
	if s.Interval <= 0 {
		return errors.New("memory config: interval must be positive")
	}
	if !s.Lifecycle.Disabled {
		if s.Lifecycle.LeaseTTL <= 0 || (s.Lifecycle.Periodic && s.Lifecycle.Interval <= 0) ||
			strings.TrimSpace(s.Lifecycle.Owner) == "" {
			return errors.New("memory config: lifecycle owner, lease_ttl, and periodic interval are required")
		}
		if _, err := lifecycle.NewDecay(s.Lifecycle.Decay, nil); err != nil {
			return fmt.Errorf("memory config: lifecycle decay: %w", err)
		}
		if s.Lifecycle.Forget.Mode == lifecycle.ModeSoftVisibility && !s.Lifecycle.Forget.EnableSoftVisibility {
			return errors.New("memory config: soft visibility requires explicit enable_soft_visibility")
		}
		if s.Lifecycle.Forget.Mode != lifecycle.ModeAuditOnly && s.Lifecycle.Forget.Mode != lifecycle.ModeSoftVisibility {
			return errors.New("memory config: lifecycle forget mode is invalid")
		}
		for _, threshold := range []float64{s.Lifecycle.Forget.SoftForgetThreshold, s.Lifecycle.Forget.ArchiveThreshold} {
			if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1 {
				return errors.New("memory config: lifecycle thresholds must be in [0,1]")
			}
		}
	}
	if s.Recent.MaxItems < 0 || s.Recent.MaxTokens < 0 {
		return errors.New("memory config: recent limits must not be negative")
	}
	if s.Summary.ChunkSize < 0 || s.Summary.CondenseThreshold < 0 || s.Summary.GroupSize < 0 ||
		s.Summary.MaxDepth < 0 || s.Summary.MaxDepth > 4 {
		return errors.New("memory config: summary limits must be non-negative and max_depth must not exceed 4")
	}
	if strings.TrimSpace(s.Projection) == "" {
		return errors.New("memory config: projection is required")
	}
	if s.BM25.Version != bm25.AlgorithmVersion {
		return fmt.Errorf("memory config: unsupported BM25 version %q", s.BM25.Version)
	}
	if s.ProjectionStorage.AlgorithmVersion != vector.StorageAlgorithmVersion ||
		s.ProjectionStorage.MaxSegments < 1 || s.ProjectionStorage.MaxDeltaBytes < 1 {
		return errors.New("memory config: projection storage algorithm and thresholds are invalid")
	}
	if s.Reranker.Enabled && (s.Reranker.Value == nil || strings.TrimSpace(s.Reranker.Version) == "") {
		return errors.New("memory config: enabled reranker requires value and version")
	}
	if !s.Reranker.Enabled && (s.Reranker.Value != nil || s.Reranker.Version != "") {
		return errors.New("memory config: disabled reranker must not set value or version")
	}
	for index, node := range s.ChatDAG.Nodes {
		if node.Fact != nil {
			if err := node.Fact.extractorConfig().Validate(); err != nil {
				return fmt.Errorf("memory config: chat_dag.nodes[%d].fact: %w", index, err)
			}
		}
	}
	for index, node := range s.KnowledgeDAG.Nodes {
		if node.Chunk != nil {
			if err := validateChunkSettings(*node.Chunk); err != nil {
				return fmt.Errorf("memory config: knowledge_dag.nodes[%d].chunk: %w", index, err)
			}
		}
	}
	for name, lane := range map[string]LaneSettings{
		"vector": s.Lanes.Vector, "bm25": s.Lanes.BM25, "entity": s.Lanes.Entity,
	} {
		if !finitePositive(lane.Weight) {
			return fmt.Errorf("memory config: lane %s weight must be finite and positive", name)
		}
		if _, err := buildCalibrator(lane.Calibration); err != nil {
			return fmt.Errorf("memory config: lane %s: %w", name, err)
		}
	}
	return nil
}

func validateChunkSettings(settings ChunkSettings) error {
	if settings.MaxRunes <= 0 || settings.OverlapRunes < 0 || settings.OverlapRunes >= settings.MaxRunes {
		return errors.New("chunk overlap_runes must be non-negative and less than positive max_runes")
	}
	if (settings.Summary.Document || settings.Summary.Sections) && settings.Summary.MaxRunes <= 0 {
		return errors.New("chunk summary max_runes must be positive when enabled")
	}
	return nil
}

type policyNode struct {
	ID        string          `json:"id"`
	Algorithm string          `json:"algorithm"`
	Config    json.RawMessage `json:"config"`
	DependsOn []string        `json:"depends_on,omitempty"`
}

// ComputePolicyDigest computes the normalized semantic policy identity without
// constructing runtime dependencies.
func ComputePolicyDigest(settings Settings) (string, error) {
	settings = settings.withDefaults()
	return computePolicyDigest(settings, settings.FactoryCatalog)
}

func computePolicyDigest(settings Settings, factories *FactoryCatalog) (string, error) {
	chatSpec, err := settings.chatSpec()
	if err != nil {
		return "", err
	}
	knowledgeSpec, err := settings.knowledgeSpec()
	if err != nil {
		return "", err
	}
	lifecycleDAG, err := settings.lifecycleDAG()
	if err != nil {
		return "", err
	}
	selectedAlgorithms := make(map[string]struct{})
	for _, spec := range []derive.Spec{chatSpec, knowledgeSpec} {
		for _, node := range spec.Nodes {
			selectedAlgorithms[node.Deriver.FactoryName()] = struct{}{}
		}
	}
	algorithms, err := normalizedAlgorithms(settings.AlgorithmCatalog, factories, selectedAlgorithms)
	if err != nil {
		return "", err
	}
	encodeNodes := func(spec derive.Spec) ([]policyNode, error) {
		nodes := make([]policyNode, len(spec.Nodes))
		for index, node := range spec.Nodes {
			config, configErr := node.Deriver.CanonicalConfig()
			if configErr != nil {
				return nil, configErr
			}
			nodes[index] = policyNode{
				ID: node.ID, Algorithm: node.Deriver.FactoryName(), Config: config,
				DependsOn: append([]string(nil), node.DependsOn...),
			}
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
		return nodes, nil
	}
	chatNodes, err := encodeNodes(chatSpec)
	if err != nil {
		return "", err
	}
	knowledgeNodes, err := encodeNodes(knowledgeSpec)
	if err != nil {
		return "", err
	}
	generate := ModelSettings{}
	for _, node := range settings.ChatDAG.Nodes {
		if node.Fact != nil && node.Fact.Strategy != chat.StrategyNone {
			generate = settings.Generate
			break
		}
	}
	payload := struct {
		Generate          ModelSettings             `json:"generate"`
		Embed             ModelSettings             `json:"embed"`
		Projection        string                    `json:"projection"`
		ProjectionStorage ProjectionStorageSettings `json:"projection_storage"`
		BM25              BM25Settings              `json:"bm25"`
		Lanes             LanesSettings             `json:"lanes"`
		Reranker          struct {
			Enabled bool   `json:"enabled"`
			Version string `json:"version,omitempty"`
		} `json:"reranker"`
		Chat                []policyNode      `json:"chat_dag"`
		Knowledge           []policyNode      `json:"knowledge_dag"`
		Lifecycle           string            `json:"lifecycle_dag_digest"`
		Summary             SummarySettings   `json:"summary"`
		LifecycleSettings   LifecycleSettings `json:"lifecycle_settings"`
		Algorithms          []Algorithm       `json:"algorithms"`
		RetrievalAlgorithms []Algorithm       `json:"retrieval_algorithms"`
		Namespace           string            `json:"namespace,omitempty"`
	}{
		Generate: generate, Embed: settings.Embed, Projection: settings.Projection,
		ProjectionStorage: settings.ProjectionStorage,
		BM25:              settings.BM25, Lanes: settings.Lanes, Chat: chatNodes, Knowledge: knowledgeNodes,
		Lifecycle: lifecycleDAG.Digest(), Summary: settings.Summary, Algorithms: algorithms,
		LifecycleSettings: settings.Lifecycle,
		RetrievalAlgorithms: []Algorithm{
			{Name: "chat.fact-link", Version: chat.LinkAlgorithmVersion},
			{Name: "projection.vector", Version: vector.AlgorithmVersion},
			{Name: "projection.bm25", Version: bm25.AlgorithmVersion},
			{Name: "projection.entity", Version: entity.AlgorithmVersion},
			{Name: "retrieval.fusion", Version: fusion.AlgorithmVersion},
		},
		Namespace: settings.PolicyNamespace,
	}
	payload.Reranker.Enabled = settings.Reranker.Enabled
	payload.Reranker.Version = settings.Reranker.Version
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("memory config: encode policy digest: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("flowcraft.memory.policy.v1\x00"))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (settings FactSettings) extractorConfig() chat.Config {
	return chat.Config{
		Strategy: settings.Strategy, TailMaxChars: settings.TailMaxChars,
		MaxFacts: settings.MaxFacts, MaxFactChars: settings.MaxFactChars,
		MaxQueryChars:          settings.MaxQueryChars,
		MaxEmbeddingInputChars: settings.MaxEmbeddingInputChars,
	}
}

func validateModel(runtime *inference.Runtime, name string, ref inference.ModelRef, operation inference.Operation) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("memory config: %s model: %w", name, err)
	}
	descriptor, err := runtime.InspectModel(ref)
	if err != nil {
		return fmt.Errorf("memory config: %s model: %w", name, err)
	}
	for _, supported := range descriptor.Operations {
		if supported == operation {
			return nil
		}
	}
	return fmt.Errorf("memory config: %s model does not support %q", name, operation)
}

func buildCalibrator(settings CalibrationSettings) (fusion.Calibrator, error) {
	switch settings.Kind {
	case "cosine":
		if settings.Version != fusion.CosineCalibrationVersion || settings.Slope != 0 ||
			settings.Midpoint != 0 || settings.Scale != 0 ||
			math.IsNaN(settings.SemanticFloor) || math.IsInf(settings.SemanticFloor, 0) ||
			settings.SemanticFloor < -1 || settings.SemanticFloor > 1 {
			return nil, errors.New("cosine calibration requires version cosine-v1 and semantic_floor only")
		}
		return fusion.Cosine{
			FloorEnabled: !settings.DisableFloor, SemanticFloor: settings.SemanticFloor,
		}, nil
	case "bm25_query_sigmoid":
		if settings.Version != fusion.BM25CalibrationVersion || settings.Slope != 0 ||
			settings.Midpoint != 0 || settings.Scale != 0 || settings.SemanticFloor != 0 || settings.DisableFloor {
			return nil, errors.New("BM25 calibration requires version bm25-query-sigmoid-v1 and no overrides")
		}
		return fusion.BM25QuerySigmoid{}, nil
	case "identity":
		if settings.Version != "identity-v1" || settings.Slope != 0 ||
			settings.Midpoint != 0 || settings.Scale != 0 || settings.SemanticFloor != 0 || settings.DisableFloor {
			return nil, errors.New("identity calibration requires version identity-v1 and no parameters")
		}
		return fusion.Identity{CalibrationVersion: settings.Version}, nil
	case "minmax":
		if settings.Version != "" || settings.Slope != 0 || settings.Midpoint != 0 || settings.Scale != 0 ||
			settings.SemanticFloor != 0 || settings.DisableFloor {
			return nil, errors.New("minmax calibration accepts no parameters")
		}
		return fusion.MinMax{}, nil
	case "logistic":
		if settings.Version != "" || settings.Scale != 0 || settings.SemanticFloor != 0 || settings.DisableFloor ||
			!finitePositive(settings.Slope) || math.IsNaN(settings.Midpoint) || math.IsInf(settings.Midpoint, 0) {
			return nil, errors.New("logistic calibration requires finite positive slope and optional finite midpoint")
		}
		return fusion.Logistic{Slope: settings.Slope, Midpoint: settings.Midpoint}, nil
	case "saturating":
		if settings.Version != "" || settings.Slope != 0 || settings.Midpoint != 0 ||
			settings.SemanticFloor != 0 || settings.DisableFloor || !finitePositive(settings.Scale) {
			return nil, errors.New("saturating calibration requires finite positive scale only")
		}
		return fusion.Saturating{Scale: settings.Scale}, nil
	default:
		return nil, fmt.Errorf("unsupported calibration %q", settings.Kind)
	}
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	ref := reflect.ValueOf(value)
	switch ref.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return ref.IsNil()
	default:
		return false
	}
}
