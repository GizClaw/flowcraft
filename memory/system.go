package memory

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	internalexecutor "github.com/GizClaw/flowcraft/memory/internal/executor"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	viewfact "github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Deps contains the explicit stores, retrieval index, and services used
// to construct a System. New does not create or choose any of these.
type Deps struct {
	MessageStore  sourcemessage.Store
	DocumentStore sourcedocument.Store

	SummaryStore        viewrecent.SummaryStore
	ChunkStore          viewdocument.ChunkStore
	ObservationStore    viewobservation.Store
	FactStore           viewfact.Store
	FactGraphStore      viewfact.GraphStore
	EntityProfileStore  viewentity.ProfileStore
	EntityTimelineStore viewentity.TimelineStore

	Index retrieval.Index

	DocumentChunker       DocumentChunker
	Summarizer            Summarizer
	ObservationExtractor  ObservationExtractor
	FactReconciler        FactReconciler
	FactGraphBuilder      FactGraphBuilder
	EntityProfileBuilder  EntityProfileBuilder
	EntityTimelineBuilder EntityTimelineBuilder
	ContextPacker         ContextPacker

	Scheduler Scheduler
}

// System is the public memory facade. It owns stage-driven orchestration while
// delegating vertical capability execution to the single internal executor.
type System struct {
	inner     *internalexecutor.Executor
	assembly  compiler.Assembly
	plan      Plan
	scheduler Scheduler
	deps      Deps

	writeAvailable map[Capability]bool
	readAvailable  map[Capability]bool
}

// DocumentChunker derives semantic chunk records from a canonical document.
type DocumentChunker interface {
	ChunkDocument(context.Context, DocumentChunkInput) ([]viewdocument.Chunk, error)
}

// DocumentChunkInput is the evidence and view identity provided to a chunking service.
type DocumentChunkInput struct {
	View     views.Descriptor
	Scope    views.Scope
	Document sourcedocument.Document
}

// Summarizer derives SummaryDAG nodes from a recent message window.
type Summarizer interface {
	Summarize(context.Context, SummaryInput) ([]viewrecent.SummaryNode, error)
}

// SummaryInput is the evidence and view identity provided to a summary service.
type SummaryInput struct {
	View   views.Descriptor
	Scope  views.Scope
	Window viewrecent.WindowResult
}

// ObservationExtractor derives observation records from a recent message window.
type ObservationExtractor interface {
	ExtractObservations(context.Context, ObservationInput) ([]viewobservation.Observation, error)
}

// ObservationInput is the evidence, target scope, and view identity provided to
// an observation extraction service.
type ObservationInput struct {
	View   views.Descriptor
	Window viewrecent.WindowResult
	Scope  viewobservation.Scope
}

// FactReconciler derives durable facts from observation ledger outputs.
type FactReconciler interface {
	ReconcileFacts(context.Context, FactReconcileInput) ([]viewfact.Fact, error)
}

// FactReconcileInput is the evidence and view identity provided to a fact reconciler.
type FactReconcileInput struct {
	View         views.Descriptor
	Scope        views.Scope
	Observations []viewobservation.Observation
	Current      []viewfact.Fact
}

// FactGraphBuilder derives graph nodes and edges from reconciled facts.
type FactGraphBuilder interface {
	BuildFactGraph(context.Context, FactGraphInput) (FactGraphOutput, error)
}

// FactGraphInput is the evidence and view identity provided to a fact graph builder.
type FactGraphInput struct {
	View  views.Descriptor
	Facts []viewfact.Fact
}

// FactGraphOutput is the graph records produced by a FactGraphBuilder.
type FactGraphOutput struct {
	Nodes []viewfact.Node
	Edges []viewfact.Edge
}

// EntityProfileBuilder derives entity profile records from fact graph and fact
// ledger outputs. The System passes evidence explicitly; builders must not read stores.
type EntityProfileBuilder interface {
	BuildEntityProfiles(context.Context, EntityProfileInput) ([]viewentity.ProfileRecord, error)
}

// EntityProfileInput is the evidence and view identity provided to an entity profile builder.
type EntityProfileInput struct {
	View  views.Descriptor
	Scope views.Scope
	Facts []viewfact.Fact
	Graph FactGraphOutput
}

// EntityTimelineBuilder derives entity timeline events from fact graph and fact
// ledger outputs. The System passes evidence explicitly; builders must not read stores.
type EntityTimelineBuilder interface {
	BuildEntityTimeline(context.Context, EntityTimelineInput) ([]viewentity.Event, error)
}

// EntityTimelineInput is the evidence and view identity provided to an entity timeline builder.
type EntityTimelineInput struct {
	View  views.Descriptor
	Scope views.Scope
	Facts []viewfact.Fact
	Graph FactGraphOutput
}

// ContextPacker optionally chooses the final packed context items from
// executor-built candidates. New does not install a default implementation.
type ContextPacker interface {
	PackContext(context.Context, ContextPackInput) (ContextPackOutput, error)
}

// New compiles spec and constructs a system from caller-provided dependencies.
func New(spec Spec, deps Deps) (*System, error) {
	assembly, err := compiler.Compile(spec)
	if err != nil {
		return nil, err
	}
	return newFromAssembly(assembly, deps)
}

// NewFromAssembly constructs a system from a previously compiled assembly and
// caller-provided dependencies.
func newFromAssembly(assembly compiler.Assembly, deps Deps) (*System, error) {
	writeAvailable := configuredWriteCapabilities(assembly, deps)
	readAvailable := configuredReadCapabilities(assembly, deps)
	plan, err := compilePlan(assembly, writeAvailable, readAvailable)
	if err != nil {
		return nil, err
	}
	if hasAsyncWriteStages(plan.Write) && deps.Scheduler == nil {
		return nil, errdefs.Validationf("memory: async write stages require Scheduler")
	}
	inner, err := internalexecutor.New(executorDeps(assembly, deps))
	if err != nil {
		return nil, err
	}
	return &System{
		inner:          inner,
		assembly:       assembly,
		plan:           plan,
		scheduler:      deps.Scheduler,
		deps:           deps,
		writeAvailable: writeAvailable,
		readAvailable:  readAvailable,
	}, nil
}

// Close releases resources owned by the system. Injected indexes are left open.
func (r *System) Close() error {
	if r == nil || r.inner == nil {
		return nil
	}
	return r.inner.Close()
}

// Plan returns the compiled root facade plan.
func (r *System) Plan() Plan {
	if r == nil {
		return Plan{}
	}
	return clonePlan(r.plan)
}

// ProjectionNamespace returns the physical namespace bound to a capability.
func (r *System) ProjectionNamespace(capability Capability) (string, bool) {
	if r == nil || r.inner == nil {
		return "", false
	}
	return r.inner.ProjectionNamespace(capability)
}

// MessageStore returns the configured canonical message store, if any.
func (r *System) MessageStore() sourcemessage.Store {
	if r == nil || r.inner == nil {
		return nil
	}
	return r.inner.MessageStore()
}

// DocumentStore returns the configured canonical document store, if any.
func (r *System) DocumentStore() sourcedocument.Store {
	if r == nil || r.inner == nil {
		return nil
	}
	return r.inner.DocumentStore()
}

// RetrievalIndex returns the shared retrieval index used by projection writers.
func (r *System) RetrievalIndex() retrieval.Index {
	if r == nil || r.inner == nil {
		return nil
	}
	return r.inner.RetrievalIndex()
}

func executorDeps(assembly compiler.Assembly, deps Deps) internalexecutor.Deps {
	return internalexecutor.Deps{
		Assembly: assembly,

		MessageStore:  deps.MessageStore,
		DocumentStore: deps.DocumentStore,

		SummaryStore:        deps.SummaryStore,
		ChunkStore:          deps.ChunkStore,
		ObservationStore:    deps.ObservationStore,
		FactStore:           deps.FactStore,
		FactGraphStore:      deps.FactGraphStore,
		EntityProfileStore:  deps.EntityProfileStore,
		EntityTimelineStore: deps.EntityTimelineStore,

		Index: deps.Index,

		DocumentChunker:       adaptDocumentChunker(deps.DocumentChunker),
		Summarizer:            adaptSummarizer(deps.Summarizer),
		ObservationExtractor:  adaptObservationExtractor(deps.ObservationExtractor),
		FactReconciler:        adaptFactReconciler(deps.FactReconciler),
		FactGraphBuilder:      adaptFactGraphBuilder(deps.FactGraphBuilder),
		EntityProfileBuilder:  adaptEntityProfileBuilder(deps.EntityProfileBuilder),
		EntityTimelineBuilder: adaptEntityTimelineBuilder(deps.EntityTimelineBuilder),
		ContextPacker:         adaptContextPacker(deps.ContextPacker),
	}
}

func adaptDocumentChunker(service DocumentChunker) internalexecutor.DocumentChunker {
	if service == nil {
		return nil
	}
	return documentChunkerAdapter{service: service}
}

type documentChunkerAdapter struct {
	service DocumentChunker
}

func (a documentChunkerAdapter) ChunkDocument(ctx context.Context, input internalexecutor.DocumentChunkInput) ([]viewdocument.Chunk, error) {
	return a.service.ChunkDocument(ctx, DocumentChunkInput{
		View:     input.View,
		Scope:    input.Scope,
		Document: input.Document,
	})
}

func adaptSummarizer(service Summarizer) internalexecutor.Summarizer {
	if service == nil {
		return nil
	}
	return summarizerAdapter{service: service}
}

type summarizerAdapter struct {
	service Summarizer
}

func (a summarizerAdapter) Summarize(ctx context.Context, input internalexecutor.SummaryInput) ([]viewrecent.SummaryNode, error) {
	return a.service.Summarize(ctx, SummaryInput{
		View:   input.View,
		Scope:  input.Scope,
		Window: input.Window,
	})
}

func adaptObservationExtractor(service ObservationExtractor) internalexecutor.ObservationExtractor {
	if service == nil {
		return nil
	}
	return observationExtractorAdapter{service: service}
}

type observationExtractorAdapter struct {
	service ObservationExtractor
}

func (a observationExtractorAdapter) ExtractObservations(ctx context.Context, input internalexecutor.ObservationInput) ([]viewobservation.Observation, error) {
	return a.service.ExtractObservations(ctx, ObservationInput{
		View:   input.View,
		Window: input.Window,
		Scope:  input.Scope,
	})
}

func adaptFactReconciler(service FactReconciler) internalexecutor.FactReconciler {
	if service == nil {
		return nil
	}
	return factReconcilerAdapter{service: service}
}

type factReconcilerAdapter struct {
	service FactReconciler
}

func (a factReconcilerAdapter) ReconcileFacts(ctx context.Context, input internalexecutor.FactReconcileInput) ([]viewfact.Fact, error) {
	return a.service.ReconcileFacts(ctx, FactReconcileInput{
		View:         input.View,
		Scope:        input.Scope,
		Observations: input.Observations,
		Current:      input.Current,
	})
}

func adaptFactGraphBuilder(service FactGraphBuilder) internalexecutor.FactGraphBuilder {
	if service == nil {
		return nil
	}
	return factGraphBuilderAdapter{service: service}
}

type factGraphBuilderAdapter struct {
	service FactGraphBuilder
}

func (a factGraphBuilderAdapter) BuildFactGraph(ctx context.Context, input internalexecutor.FactGraphInput) (internalexecutor.FactGraphOutput, error) {
	output, err := a.service.BuildFactGraph(ctx, FactGraphInput{
		View:  input.View,
		Facts: input.Facts,
	})
	if err != nil {
		return internalexecutor.FactGraphOutput{}, err
	}
	return internalexecutor.FactGraphOutput{
		Nodes: output.Nodes,
		Edges: output.Edges,
	}, nil
}

func adaptEntityProfileBuilder(service EntityProfileBuilder) internalexecutor.EntityProfileBuilder {
	if service == nil {
		return nil
	}
	return entityProfileBuilderAdapter{service: service}
}

type entityProfileBuilderAdapter struct {
	service EntityProfileBuilder
}

func (a entityProfileBuilderAdapter) BuildEntityProfiles(ctx context.Context, input internalexecutor.EntityProfileInput) ([]viewentity.ProfileRecord, error) {
	return a.service.BuildEntityProfiles(ctx, EntityProfileInput{
		View:  input.View,
		Scope: input.Scope,
		Facts: input.Facts,
		Graph: FactGraphOutput{
			Nodes: input.Graph.Nodes,
			Edges: input.Graph.Edges,
		},
	})
}

func adaptEntityTimelineBuilder(service EntityTimelineBuilder) internalexecutor.EntityTimelineBuilder {
	if service == nil {
		return nil
	}
	return entityTimelineBuilderAdapter{service: service}
}

type entityTimelineBuilderAdapter struct {
	service EntityTimelineBuilder
}

func (a entityTimelineBuilderAdapter) BuildEntityTimeline(ctx context.Context, input internalexecutor.EntityTimelineInput) ([]viewentity.Event, error) {
	return a.service.BuildEntityTimeline(ctx, EntityTimelineInput{
		View:  input.View,
		Scope: input.Scope,
		Facts: input.Facts,
		Graph: FactGraphOutput{
			Nodes: input.Graph.Nodes,
			Edges: input.Graph.Edges,
		},
	})
}

func adaptContextPacker(service ContextPacker) internalexecutor.ContextPacker {
	if service == nil {
		return nil
	}
	return contextPackerAdapter{service: service}
}

type contextPackerAdapter struct {
	service ContextPacker
}

func (a contextPackerAdapter) PackContext(ctx context.Context, input internalexecutor.ContextPackInput) (internalexecutor.ContextPackOutput, error) {
	output, err := a.service.PackContext(ctx, contextPackInputFromInternal(input))
	if err != nil {
		return internalexecutor.ContextPackOutput{}, err
	}
	return internalexecutor.ContextPackOutput{Items: output.Items}, nil
}

func contextPackInputFromInternal(input internalexecutor.ContextPackInput) ContextPackInput {
	return ContextPackInput{
		Scope:              input.Scope,
		Query:              input.Query,
		Window:             input.Window,
		Items:              input.Items,
		SummaryHits:        input.SummaryHits,
		DocumentHits:       input.DocumentHits,
		ObservationHits:    input.ObservationHits,
		FactHits:           input.FactHits,
		FactGraphHits:      input.FactGraphHits,
		EntityProfileHits:  input.EntityProfileHits,
		EntityTimelineHits: input.EntityTimelineHits,
	}
}
