package memory

import (
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	internalexecutor "github.com/GizClaw/flowcraft/memory/internal/executor"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	viewfact "github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/embedding"
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

	Index    retrieval.Index
	Embedder embedding.Embedder

	DocumentChunker       derive.DocumentChunker
	Summarizer            derive.Summarizer
	ObservationExtractor  derive.ObservationExtractor
	FactReconciler        derive.FactReconciler
	FactGraphBuilder      derive.FactGraphBuilder
	EntityProfileBuilder  derive.EntityProfileBuilder
	EntityTimelineBuilder derive.EntityTimelineBuilder
	ContextPacker         derive.ContextPacker

	JobStore         LifecycleJobStore
	ReportStore      ReportStore
	DiagnosticProbes *DiagnosticProbeRegistry
}

// System is the public memory facade. It owns stage-driven orchestration while
// delegating vertical capability execution to the single internal executor.
type System struct {
	inner       *internalexecutor.Executor
	assembly    compiler.Assembly
	plan        Plan
	jobStore    LifecycleJobStore
	reportStore ReportStore
	deps        Deps

	writeAvailable     map[Capability]bool
	readAvailable      map[Capability]bool
	runnerRegistry     *LifecycleRunnerRegistry
	diagnosticRegistry *DiagnosticProbeRegistry
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
	if hasAsyncWriteStages(plan.Write) && deps.JobStore == nil {
		return nil, errdefs.Validationf("memory: async write stages require JobStore")
	}
	inner, err := internalexecutor.New(executorDeps(assembly, deps))
	if err != nil {
		return nil, err
	}
	system := &System{
		inner:          inner,
		assembly:       assembly,
		plan:           plan,
		jobStore:       deps.JobStore,
		reportStore:    deps.ReportStore,
		deps:           deps,
		writeAvailable: writeAvailable,
		readAvailable:  readAvailable,
	}
	system.runnerRegistry = system.defaultLifecycleRunnerRegistry()
	system.diagnosticRegistry = system.defaultDiagnosticProbeRegistry()
	system.diagnosticRegistry.mergeFrom(deps.DiagnosticProbes)
	return system, nil
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

		Index:    deps.Index,
		Embedder: deps.Embedder,

		DocumentChunker:       deps.DocumentChunker,
		Summarizer:            deps.Summarizer,
		ObservationExtractor:  deps.ObservationExtractor,
		FactReconciler:        deps.FactReconciler,
		FactGraphBuilder:      deps.FactGraphBuilder,
		EntityProfileBuilder:  deps.EntityProfileBuilder,
		EntityTimelineBuilder: deps.EntityTimelineBuilder,
		ContextPacker:         deps.ContextPacker,
	}
}
