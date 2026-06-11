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
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Deps contains the explicit stores, retrieval index, and services used
// to construct a System. New does not create or choose any of these.
type Deps struct {
	MessageStore  sourcemessage.Store
	DocumentStore sourcedocument.Store

	SummaryStore     recent.SummaryStore
	ChunkStore       viewdocument.ChunkStore
	ObservationStore viewobservation.Store
	FactStore        fact.Store
	FactGraphStore   fact.GraphStore

	Index retrieval.Index

	DocumentChunker      DocumentChunker
	Summarizer           Summarizer
	ObservationExtractor ObservationExtractor
	FactReconciler       FactReconciler
	FactGraphBuilder     FactGraphBuilder

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
	Summarize(context.Context, SummaryInput) ([]recent.SummaryNode, error)
}

// SummaryInput is the evidence and view identity provided to a summary service.
type SummaryInput struct {
	View   views.Descriptor
	Scope  views.Scope
	Window recent.WindowResult
}

// ObservationExtractor derives observation records from a recent message window.
type ObservationExtractor interface {
	ExtractObservations(context.Context, ObservationInput) ([]viewobservation.Observation, error)
}

// ObservationInput is the evidence, target scope, and view identity provided to
// an observation extraction service.
type ObservationInput struct {
	View   views.Descriptor
	Window recent.WindowResult
	Scope  viewobservation.Scope
}

// FactReconciler derives durable facts from observation ledger outputs.
type FactReconciler interface {
	ReconcileFacts(context.Context, FactReconcileInput) ([]fact.Fact, error)
}

// FactReconcileInput is the evidence and view identity provided to a fact reconciler.
type FactReconcileInput struct {
	View         views.Descriptor
	Observations []viewobservation.Observation
}

// FactGraphBuilder derives graph nodes and edges from reconciled facts.
type FactGraphBuilder interface {
	BuildFactGraph(context.Context, FactGraphInput) (FactGraphOutput, error)
}

// FactGraphInput is the evidence and view identity provided to a fact graph builder.
type FactGraphInput struct {
	View  views.Descriptor
	Facts []fact.Fact
}

// FactGraphOutput is the graph records produced by a FactGraphBuilder.
type FactGraphOutput struct {
	Nodes []fact.Node
	Edges []fact.Edge
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

		SummaryStore:     deps.SummaryStore,
		ChunkStore:       deps.ChunkStore,
		ObservationStore: deps.ObservationStore,
		FactStore:        deps.FactStore,
		FactGraphStore:   deps.FactGraphStore,

		Index: deps.Index,

		DocumentChunker:      adaptDocumentChunker(deps.DocumentChunker),
		Summarizer:           adaptSummarizer(deps.Summarizer),
		ObservationExtractor: adaptObservationExtractor(deps.ObservationExtractor),
		FactReconciler:       adaptFactReconciler(deps.FactReconciler),
		FactGraphBuilder:     adaptFactGraphBuilder(deps.FactGraphBuilder),
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

func (a summarizerAdapter) Summarize(ctx context.Context, input internalexecutor.SummaryInput) ([]recent.SummaryNode, error) {
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

func (a factReconcilerAdapter) ReconcileFacts(ctx context.Context, input internalexecutor.FactReconcileInput) ([]fact.Fact, error) {
	return a.service.ReconcileFacts(ctx, FactReconcileInput{
		View:         input.View,
		Observations: input.Observations,
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
