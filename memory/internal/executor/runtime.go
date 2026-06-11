package executor

import (
	"fmt"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// New constructs one executor from a validated compiler assembly.
func New(deps Deps) (*Executor, error) {
	if err := deps.Assembly.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: invalid assembly: %w", errPrefix, err)
	}

	rt := &Executor{
		assembly: deps.Assembly,

		messageStore:  deps.MessageStore,
		documentStore: deps.DocumentStore,

		index: deps.Index,

		enabled:     make(map[compiler.Capability]compiler.ViewAssembly, len(deps.Assembly.Views)),
		projections: make(map[compiler.Capability]compiler.ProjectionAssembly, len(deps.Assembly.Projections)),
		writers:     make(map[compiler.Capability]*indexed.Writer, len(deps.Assembly.Projections)),

		documentChunker:      deps.DocumentChunker,
		summarizer:           deps.Summarizer,
		observationExtractor: deps.ObservationExtractor,
		factReconciler:       deps.FactReconciler,
		factGraphBuilder:     deps.FactGraphBuilder,
	}

	for _, view := range deps.Assembly.Views {
		rt.enabled[view.Capability] = view
		if view.Required && unsupportedCapability(view.Capability) {
			return nil, errdefs.NotAvailablef("%s: capability %q is not implemented", errPrefix, view.Capability)
		}
	}
	for _, projection := range deps.Assembly.Projections {
		rt.projections[projection.Capability] = projection
	}

	if err := rt.configureSources(); err != nil {
		return nil, err
	}
	if err := rt.configureViews(deps); err != nil {
		return nil, err
	}
	if err := rt.configureProjectionWriters(); err != nil {
		return nil, err
	}

	return rt, nil
}

// Close releases resources owned by the executor. Injected indexes are left open.
func (r *Executor) Close() error {
	return nil
}

// Assembly returns the compiled assembly used by this executor.
func (r *Executor) Assembly() compiler.Assembly {
	if r == nil {
		return compiler.Assembly{}
	}
	return r.assembly
}

// MessageStore returns the configured canonical message store, if any.
func (r *Executor) MessageStore() sourcemessage.Store {
	if r == nil {
		return nil
	}
	return r.messageStore
}

// DocumentStore returns the configured canonical document store, if any.
func (r *Executor) DocumentStore() sourcedocument.Store {
	if r == nil {
		return nil
	}
	return r.documentStore
}

// RetrievalIndex returns the shared retrieval index used by projection writers.
func (r *Executor) RetrievalIndex() retrieval.Index {
	if r == nil {
		return nil
	}
	return r.index
}

// ProjectionNamespace returns the physical namespace bound to a capability.
func (r *Executor) ProjectionNamespace(capability compiler.Capability) (string, bool) {
	if r == nil {
		return "", false
	}
	projection, ok := r.projections[capability]
	if !ok {
		return "", false
	}
	return projection.Binding.Namespace, true
}

func (r *Executor) configureSources() error {
	for _, source := range r.assembly.Sources {
		switch source.Kind {
		case compiler.SourceMessageLog:
			if r.messageStore == nil && source.Required {
				return errdefs.Validationf("%s: required source %q requires MessageStore", errPrefix, source.Kind)
			}
		case compiler.SourceDocumentStore:
			if r.documentStore == nil && source.Required {
				return errdefs.Validationf("%s: required source %q requires DocumentStore", errPrefix, source.Kind)
			}
		default:
			return errdefs.Validationf("%s: unsupported source kind %q", errPrefix, source.Kind)
		}
	}
	return nil
}

func (r *Executor) configureViews(deps Deps) error {
	summaryFlow := r.shouldConfigureFlow(compiler.CapabilitySummaryDAG, deps.SummaryStore != nil, r.summarizer != nil)
	observationFlow := r.shouldConfigureFlow(compiler.CapabilityObservationLedger, deps.ObservationStore != nil, r.observationExtractor != nil)
	factFlow := r.shouldConfigureCompleteFlow(compiler.CapabilityFactLedger, deps.FactStore != nil, r.factReconciler != nil)
	factGraphFlow := r.shouldConfigureCompleteFlow(compiler.CapabilityFactGraph, deps.FactGraphStore != nil, r.factGraphBuilder != nil)

	if r.shouldConfigureRecentWindow(summaryFlow, observationFlow) {
		if r.messageStore == nil {
			if r.required(compiler.CapabilityRecentWindow) ||
				summaryFlow ||
				observationFlow {
				return errdefs.Validationf("%s: message store is required for message-window capabilities", errPrefix)
			}
		} else {
			view := r.enabled[compiler.CapabilityRecentWindow]
			r.recentWindow = recent.NewWindow(r.messageStore, recent.WithID(view.Descriptor.ID), recent.WithVersion(view.Descriptor.Version))
		}
	}

	if err := r.configureSummaryDAG(deps.SummaryStore); err != nil {
		return err
	}
	if err := r.configureDocumentChunks(deps.ChunkStore); err != nil {
		return err
	}
	if err := r.configureObservationLedger(deps.ObservationStore); err != nil {
		return err
	}
	if err := r.configureFactLedger(deps.FactStore, factFlow); err != nil {
		return err
	}
	return r.configureFactGraph(deps.FactGraphStore, factGraphFlow)
}

func (r *Executor) configureSummaryDAG(store recent.SummaryStore) error {
	if !r.shouldConfigureFlow(compiler.CapabilitySummaryDAG, store != nil, r.summarizer != nil) {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires SummaryStore", errPrefix, compiler.CapabilitySummaryDAG)
	}
	if r.summarizer == nil {
		return errdefs.Validationf("%s: capability %q requires Summarizer", errPrefix, compiler.CapabilitySummaryDAG)
	}
	view := r.enabled[compiler.CapabilitySummaryDAG]
	r.summaryDAG = recent.NewSummaryDAG(store, recent.WithID(view.Descriptor.ID), recent.WithVersion(view.Descriptor.Version))
	return nil
}

func (r *Executor) configureDocumentChunks(store viewdocument.ChunkStore) error {
	if !r.shouldConfigureFlow(compiler.CapabilityDocumentChunks, store != nil, r.documentChunker != nil) {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires ChunkStore", errPrefix, compiler.CapabilityDocumentChunks)
	}
	if r.documentChunker == nil {
		return errdefs.Validationf("%s: capability %q requires DocumentChunker", errPrefix, compiler.CapabilityDocumentChunks)
	}
	view := r.enabled[compiler.CapabilityDocumentChunks]
	r.documentChunks = viewdocument.NewChunks(store, viewdocument.WithID(view.Descriptor.ID), viewdocument.WithVersion(view.Descriptor.Version))
	return nil
}

func (r *Executor) configureObservationLedger(store viewobservation.Store) error {
	if !r.shouldConfigureFlow(compiler.CapabilityObservationLedger, store != nil, r.observationExtractor != nil) {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires ObservationStore", errPrefix, compiler.CapabilityObservationLedger)
	}
	if r.observationExtractor == nil {
		return errdefs.Validationf("%s: capability %q requires ObservationExtractor", errPrefix, compiler.CapabilityObservationLedger)
	}
	view := r.enabled[compiler.CapabilityObservationLedger]
	r.observationLedger = viewobservation.NewLedger(store, viewobservation.WithID(view.Descriptor.ID), viewobservation.WithVersion(view.Descriptor.Version))
	return nil
}

func (r *Executor) configureFactLedger(store fact.Store, configure bool) error {
	if !configure {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires FactStore", errPrefix, compiler.CapabilityFactLedger)
	}
	if r.factReconciler == nil {
		return errdefs.Validationf("%s: capability %q requires FactReconciler", errPrefix, compiler.CapabilityFactLedger)
	}
	view := r.enabled[compiler.CapabilityFactLedger]
	r.factLedger = fact.NewLedger(store, fact.WithID(view.Descriptor.ID), fact.WithVersion(view.Descriptor.Version))
	return nil
}

func (r *Executor) configureFactGraph(store fact.GraphStore, configure bool) error {
	if !configure {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires FactGraphStore", errPrefix, compiler.CapabilityFactGraph)
	}
	if r.factGraphBuilder == nil {
		return errdefs.Validationf("%s: capability %q requires FactGraphBuilder", errPrefix, compiler.CapabilityFactGraph)
	}
	view := r.enabled[compiler.CapabilityFactGraph]
	r.factGraph = fact.NewGraph(store, fact.WithGraphID(view.Descriptor.ID), fact.WithGraphVersion(view.Descriptor.Version))
	return nil
}

func (r *Executor) configureProjectionWriters() error {
	projections, err := r.projectionWritersToConfigure()
	if err != nil {
		return err
	}
	if len(projections) == 0 {
		return nil
	}
	if r.index == nil {
		return errdefs.Validationf("%s: projections require Index", errPrefix)
	}
	for _, projection := range projections {
		writer, err := indexed.NewWriter(r.index, projection.Binding)
		if err != nil {
			return fmt.Errorf("%s: create projection writer for %q: %w", errPrefix, projection.Capability, err)
		}
		r.writers[projection.Capability] = writer
	}
	return nil
}

func (r *Executor) projectionWritersToConfigure() ([]compiler.ProjectionAssembly, error) {
	projections := make([]compiler.ProjectionAssembly, 0, len(r.assembly.Projections))
	for _, projection := range r.assembly.Projections {
		configured, supported := r.projectionFlowConfigured(projection.Capability)
		if !supported {
			if r.required(projection.Capability) || projection.Required {
				return nil, errdefs.NotAvailablef("%s: projection for capability %q is not implemented", errPrefix, projection.Capability)
			}
			continue
		}
		if configured {
			projections = append(projections, projection)
			continue
		}
		if r.required(projection.Capability) || projection.Required {
			return nil, errdefs.Validationf("%s: required projection for capability %q cannot be configured because its flow is not configured", errPrefix, projection.Capability)
		}
	}
	return projections, nil
}

func (r *Executor) projectionFlowConfigured(capability compiler.Capability) (configured bool, supported bool) {
	switch capability {
	case compiler.CapabilityDocumentChunks:
		return r.documentChunks != nil, true
	case compiler.CapabilitySummaryDAG:
		return r.summaryDAG != nil, true
	case compiler.CapabilityObservationLedger:
		return r.observationLedger != nil, true
	case compiler.CapabilityFactLedger:
		return r.factLedger != nil, true
	case compiler.CapabilityFactGraph:
		return r.factGraph != nil, true
	default:
		return false, false
	}
}

func (r *Executor) shouldConfigureRecentWindow(summaryFlow, observationFlow bool) bool {
	if _, ok := r.enabled[compiler.CapabilityRecentWindow]; ok {
		return true
	}
	return summaryFlow || observationFlow
}

func (r *Executor) shouldConfigureFlow(capability compiler.Capability, storeAvailable bool, serviceAvailable bool) bool {
	if _, ok := r.enabled[capability]; !ok {
		return false
	}
	return r.required(capability) || storeAvailable || serviceAvailable
}

func (r *Executor) shouldConfigureCompleteFlow(capability compiler.Capability, storeAvailable bool, serviceAvailable bool) bool {
	if _, ok := r.enabled[capability]; !ok {
		return false
	}
	return r.required(capability) || (storeAvailable && serviceAvailable)
}

func (r *Executor) required(capability compiler.Capability) bool {
	view, ok := r.enabled[capability]
	return ok && view.Required
}

func unsupportedCapability(capability compiler.Capability) bool {
	switch capability {
	case compiler.CapabilityEntityProfile,
		compiler.CapabilityEntityTimeline:
		return true
	default:
		return false
	}
}
