package executor

import (
	"fmt"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
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

		index:            deps.Index,
		embedder:         deps.Embedder,
		embeddingTimeout: deps.EmbeddingTimeout,

		enabled:     make(map[compiler.Capability]compiler.ViewAssembly, len(deps.Assembly.Views)),
		projections: make(map[compiler.Capability]compiler.ProjectionAssembly, len(deps.Assembly.Projections)),
		writers:     make(map[compiler.Capability]*indexed.Writer, len(deps.Assembly.Projections)),

		documentChunker:     deps.DocumentChunker,
		summarizer:          deps.Summarizer,
		entityFactExtractor: deps.EntityFactExtractor,
		contextPacker:       deps.ContextPacker,
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
	summaryFlow := r.shouldConfigureStoredView(compiler.CapabilitySummaryDAG, deps.SummaryStore != nil)
	entityFactFlow := r.shouldConfigureStoredView(compiler.CapabilityEntityFactIndex, deps.EntityFactStore != nil)

	if r.shouldConfigureRecentWindow(summaryFlow || entityFactFlow) {
		if r.messageStore == nil {
			if r.required(compiler.CapabilityRecentWindow) || summaryFlow || entityFactFlow {
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
	if err := r.configureEntityFacts(deps.EntityFactStore); err != nil {
		return err
	}
	return nil
}

func (r *Executor) configureSummaryDAG(store recent.SummaryStore) error {
	if !r.shouldConfigureStoredView(compiler.CapabilitySummaryDAG, store != nil) {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires SummaryStore", errPrefix, compiler.CapabilitySummaryDAG)
	}
	view := r.enabled[compiler.CapabilitySummaryDAG]
	r.summaryDAG = recent.NewSummaryDAG(store, recent.WithID(view.Descriptor.ID), recent.WithVersion(view.Descriptor.Version))
	return nil
}

func (r *Executor) configureDocumentChunks(store viewdocument.ChunkStore) error {
	if !r.shouldConfigureStoredView(compiler.CapabilityDocumentChunks, store != nil) {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires ChunkStore", errPrefix, compiler.CapabilityDocumentChunks)
	}
	view := r.enabled[compiler.CapabilityDocumentChunks]
	r.documentChunks = viewdocument.NewChunks(store, viewdocument.WithID(view.Descriptor.ID), viewdocument.WithVersion(view.Descriptor.Version))
	return nil
}

func (r *Executor) configureEntityFacts(store viewentityfact.Store) error {
	if !r.shouldConfigureStoredView(compiler.CapabilityEntityFactIndex, store != nil) {
		return nil
	}
	if store == nil {
		return errdefs.Validationf("%s: capability %q requires EntityFactStore", errPrefix, compiler.CapabilityEntityFactIndex)
	}
	view := r.enabled[compiler.CapabilityEntityFactIndex]
	r.entityFacts = viewentityfact.NewGraph(store, viewentityfact.WithID(view.Descriptor.ID), viewentityfact.WithVersion(view.Descriptor.Version))
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
		writer, err := r.newProjectionWriter(projection.Binding)
		if err != nil {
			return fmt.Errorf("%s: create projection writer for %q: %w", errPrefix, projection.Capability, err)
		}
		r.writers[projection.Capability] = writer
	}
	return nil
}

func (r *Executor) newProjectionWriter(binding indexed.Binding) (*indexed.Writer, error) {
	return indexed.NewWriter(r.index, binding, r.projectionWriterOptions()...)
}

func (r *Executor) projectionWriterOptions() []indexed.WriterOption {
	if r == nil || r.embedder == nil || !retrieval.Supports(r.index, retrieval.CapabilityVector) {
		return nil
	}
	return []indexed.WriterOption{
		indexed.WithEmbedder(r.embedder),
		indexed.WithVectorize(true),
		indexed.WithEmbeddingTimeout(r.embeddingTimeout),
	}
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
	case compiler.CapabilityMessageIndex:
		return r.messageStore != nil, true
	case compiler.CapabilityDocumentChunks:
		return r.documentChunks != nil, true
	case compiler.CapabilitySummaryDAG:
		return r.summaryDAG != nil, true
	case compiler.CapabilityEntityFactIndex:
		return r.entityFacts != nil, true
	default:
		return false, false
	}
}

func (r *Executor) shouldConfigureRecentWindow(summaryFlow bool) bool {
	if _, ok := r.enabled[compiler.CapabilityRecentWindow]; ok {
		return true
	}
	return summaryFlow
}

func (r *Executor) shouldConfigureStoredView(capability compiler.Capability, storeAvailable bool) bool {
	if _, ok := r.enabled[capability]; !ok {
		return false
	}
	return r.required(capability) || storeAvailable
}

func (r *Executor) required(capability compiler.Capability) bool {
	view, ok := r.enabled[capability]
	return ok && view.Required
}

func unsupportedCapability(capability compiler.Capability) bool {
	switch capability {
	default:
		return false
	}
}
