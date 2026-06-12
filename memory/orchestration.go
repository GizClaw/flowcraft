package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	internalexecutor "github.com/GizClaw/flowcraft/memory/internal/executor"
	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	defaultContextTopK = 5

	writeStageAppendMessage       = "append_message"
	writeStageChunkDocument       = "chunk_document"
	writeStageExtractObservations = "extract_observations"
	writeStageReconcileFacts      = "reconcile_facts"
	writeStageBuildFactGraph      = "build_fact_graph"
	writeStageBuildEntityProfiles = "build_entity_profiles"
	writeStageBuildEntityTimeline = "build_entity_timeline"
	writeStageBuildSummaryDAG     = "build_summary_dag"

	readStageLoadRecentMessages     = "load_recent_messages"
	readStageRetrieveSummaries      = "retrieve_summaries"
	readStageRetrieveDocuments      = "retrieve_documents"
	readStageRetrieveObs            = "retrieve_observations"
	readStageRetrieveFacts          = "retrieve_facts"
	readStageRetrieveFactGraph      = "retrieve_fact_graph"
	readStageRetrieveEntityProfiles = "retrieve_entity_profiles"
	readStageRetrieveEntityTimeline = "retrieve_entity_timeline"
	readStageExpandFactGraph        = "expand_fact_graph"
	readStagePackContext            = "pack_context"
)

// AppendMessageRequest appends canonical conversation messages and then runs
// the configured write stages that derive semantic memory from them.
type AppendMessageRequest struct {
	TraceID  TraceID
	Messages []sourcemessage.Message
	Scope    views.Scope
}

// AppendMessageResult contains semantic records produced by write stages.
type AppendMessageResult struct {
	Observations   []viewobservation.Observation
	Facts          []fact.Fact
	FactGraph      *FactGraphBuildResult
	EntityProfiles []viewentity.ProfileRecord
	EntityEvents   []viewentity.Event
	Jobs           []LifecycleJobID
}

// ImportDocumentRequest stores one canonical document and runs configured
// document derivation stages for its scope.
type ImportDocumentRequest struct {
	TraceID  TraceID
	Scope    views.Scope
	Document sourcedocument.Document
}

// ImportDocumentResult contains semantic document records produced by import.
type ImportDocumentResult struct {
	Chunks []viewdocument.Chunk
}

// ContextRequest asks the facade to compose read-time context from product
// semantics. Callers provide Scope; the system derives physical projection
// namespaces internally.
type ContextRequest struct {
	TraceID TraceID
	Scope   views.Scope
	Query   string
	TopK    int
	Window  recent.WindowRequest
}

// AppendMessage appends source messages first, then executes configured write
// stages in order. The message log remains the source of truth for derivations.
func (r *System) AppendMessage(ctx context.Context, req AppendMessageRequest) (*AppendMessageResult, error) {
	if r == nil || r.inner == nil {
		return nil, errdefs.NotAvailablef("memory: system is not configured")
	}
	conversationID, scope, err := normalizeAppendMessageRequest(req)
	if err != nil {
		return nil, err
	}
	if r.inner.MessageStore() == nil {
		return nil, errdefs.NotAvailablef("memory: message store is not configured")
	}
	traceID := ensureTraceID(req.TraceID)
	if _, err := r.inner.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: conversationID,
		Messages:       req.Messages,
	}); err != nil {
		return nil, err
	}

	result := &AppendMessageResult{}
	window := recent.WindowRequest{Scope: scope}
	var asyncChain []PlannedStage
	flushAsync := func() error {
		if len(asyncChain) == 0 {
			return nil
		}
		handle, err := r.enqueueWriteChain(ctx, traceID, scope, window, asyncChain)
		if err != nil {
			return err
		}
		result.Jobs = append(result.Jobs, handle)
		asyncChain = nil
		return nil
	}

	for _, stage := range r.plan.Write {
		if stage.Name == writeStageAppendMessage || stage.Name == writeStageChunkDocument {
			continue
		}
		if stage.Async {
			asyncChain = append(asyncChain, stage)
			continue
		}
		if err := flushAsync(); err != nil {
			return nil, err
		}
		if err := r.executeWriteStage(ctx, stage, window, scope, result); err != nil {
			return nil, err
		}
	}
	if err := flushAsync(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *System) enqueueWriteChain(ctx context.Context, traceID TraceID, scope Scope, window recent.WindowRequest, stages []PlannedStage) (LifecycleJobID, error) {
	if r.jobStore == nil {
		return "", errdefs.Validationf("memory: async write stages require JobStore")
	}
	jobStages := clonePlannedStages(stages)
	job := LifecycleJob{
		TraceID:     ensureTraceID(traceID),
		OperationID: newOperationID(),
		Kind:        LifecycleJobKindWriteChain,
		Scope:       scope,
		Window:      window,
		Stages:      jobStages,
		MaxAttempts: 1,
	}
	jobID, err := r.jobStore.Enqueue(ctx, job)
	if err != nil {
		return "", err
	}
	job.ID = jobID
	report := newLifecycleReportForJob(r, job)
	report.Accepted = true
	report.Supported = true
	report.Status = LifecycleStatusEnqueued
	report.Message = fmt.Sprintf("write_chain lifecycle job enqueued")
	report.Steps = append(report.Steps, LifecycleStep{
		Name:    "write_chain.enqueue",
		Status:  LifecycleStatusEnqueued,
		Planned: true,
		Message: fmt.Sprintf("queued %d async write stage(s)", len(jobStages)),
	})
	finalizeLifecycleExecutionReport(&report)
	if err := r.putLifecycleReport(ctx, report); err != nil {
		return "", err
	}
	return jobID, nil
}

func (r *System) executeWriteStages(ctx context.Context, stages []PlannedStage, window recent.WindowRequest, scope viewobservation.Scope) (*AppendMessageResult, error) {
	result := &AppendMessageResult{}
	for _, stage := range stages {
		if stage.Name == writeStageAppendMessage || stage.Name == writeStageChunkDocument {
			continue
		}
		if err := r.executeWriteStage(ctx, stage, window, scope, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (r *System) executeWriteStage(ctx context.Context, stage PlannedStage, window recent.WindowRequest, scope viewobservation.Scope, result *AppendMessageResult) error {
	switch stage.Name {
	case writeStageExtractObservations:
		if err := r.requireWriteStage(stage, CapabilityObservationLedger); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityObservationLedger, scope)
		if err != nil {
			return err
		}
		observations, err := r.inner.ExtractObservations(ctx, window, scope, namespace)
		if err != nil {
			return err
		}
		result.Observations = observations
	case writeStageReconcileFacts:
		if len(result.Observations) == 0 && r.writeAvailable[CapabilityObservationLedger] {
			namespace, err := r.scopedWriteNamespace(CapabilityObservationLedger, scope)
			if err != nil {
				return err
			}
			observations, err := r.inner.ExtractObservations(ctx, window, scope, namespace)
			if err != nil {
				return err
			}
			result.Observations = observations
		}
		if len(result.Observations) == 0 {
			return nil
		}
		if err := r.requireWriteStage(stage, CapabilityFactLedger); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityFactLedger, scope)
		if err != nil {
			return err
		}
		facts, err := r.inner.ReconcileFactsScoped(ctx, result.Observations, namespace)
		if err != nil {
			return err
		}
		result.Facts = facts
	case writeStageBuildFactGraph:
		if len(result.Facts) == 0 {
			if len(result.Observations) == 0 && r.writeAvailable[CapabilityObservationLedger] {
				namespace, err := r.scopedWriteNamespace(CapabilityObservationLedger, scope)
				if err != nil {
					return err
				}
				observations, err := r.inner.ExtractObservations(ctx, window, scope, namespace)
				if err != nil {
					return err
				}
				result.Observations = observations
			}
			if len(result.Observations) > 0 && r.writeAvailable[CapabilityFactLedger] {
				namespace, err := r.scopedWriteNamespace(CapabilityFactLedger, scope)
				if err != nil {
					return err
				}
				facts, err := r.inner.ReconcileFactsScoped(ctx, result.Observations, namespace)
				if err != nil {
					return err
				}
				result.Facts = facts
			}
		}
		if len(result.Facts) == 0 {
			return nil
		}
		if err := r.requireWriteStage(stage, CapabilityFactGraph); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityFactGraph, scope)
		if err != nil {
			return err
		}
		graph, err := r.inner.BuildFactGraphScoped(ctx, result.Facts, namespace)
		if err != nil {
			return err
		}
		result.FactGraph = factGraphBuildResultFromExecutor(graph)
	case writeStageBuildEntityProfiles:
		if err := r.ensureFactGraphEvidence(ctx, window, scope, result); err != nil {
			return err
		}
		if len(result.Facts) == 0 {
			return nil
		}
		if err := r.requireWriteStage(stage, CapabilityEntityProfile); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityEntityProfile, scope)
		if err != nil {
			return err
		}
		profiles, err := r.inner.BuildEntityProfilesScoped(ctx, internalexecutor.EntityBuildInput{
			Scope: scope,
			Facts: result.Facts,
			Graph: factGraphBuildResultToExecutor(result.FactGraph),
		}, namespace)
		if err != nil {
			return err
		}
		result.EntityProfiles = profiles
	case writeStageBuildEntityTimeline:
		if err := r.ensureFactGraphEvidence(ctx, window, scope, result); err != nil {
			return err
		}
		if len(result.Facts) == 0 {
			return nil
		}
		if err := r.requireWriteStage(stage, CapabilityEntityTimeline); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityEntityTimeline, scope)
		if err != nil {
			return err
		}
		events, err := r.inner.BuildEntityTimelineScoped(ctx, internalexecutor.EntityBuildInput{
			Scope: scope,
			Facts: result.Facts,
			Graph: factGraphBuildResultToExecutor(result.FactGraph),
		}, namespace)
		if err != nil {
			return err
		}
		result.EntityEvents = events
	case writeStageBuildSummaryDAG:
		if err := r.requireWriteStage(stage, CapabilitySummaryDAG); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilitySummaryDAG, scope)
		if err != nil {
			return err
		}
		_, err = r.inner.BuildSummaryDAG(ctx, window, namespace)
		return err
	default:
		if !stage.Optional {
			return errdefs.Validationf("memory: unsupported write stage %q", stage.Name)
		}
	}
	return nil
}

func (r *System) ensureFactGraphEvidence(ctx context.Context, window recent.WindowRequest, scope viewobservation.Scope, result *AppendMessageResult) error {
	if len(result.Facts) == 0 {
		if len(result.Observations) == 0 && r.writeAvailable[CapabilityObservationLedger] {
			namespace, err := r.scopedWriteNamespace(CapabilityObservationLedger, scope)
			if err != nil {
				return err
			}
			observations, err := r.inner.ExtractObservations(ctx, window, scope, namespace)
			if err != nil {
				return err
			}
			result.Observations = observations
		}
		if len(result.Observations) > 0 && r.writeAvailable[CapabilityFactLedger] {
			namespace, err := r.scopedWriteNamespace(CapabilityFactLedger, scope)
			if err != nil {
				return err
			}
			facts, err := r.inner.ReconcileFactsScoped(ctx, result.Observations, namespace)
			if err != nil {
				return err
			}
			result.Facts = facts
		}
	}
	if len(result.Facts) == 0 || result.FactGraph != nil || !r.writeAvailable[CapabilityFactGraph] {
		return nil
	}
	namespace, err := r.scopedWriteNamespace(CapabilityFactGraph, scope)
	if err != nil {
		return err
	}
	graph, err := r.inner.BuildFactGraphScoped(ctx, result.Facts, namespace)
	if err != nil {
		return err
	}
	result.FactGraph = factGraphBuildResultFromExecutor(graph)
	return nil
}

// ImportDocument stores the canonical document first, then derives configured
// document chunk records and scoped retrieval projections.
func (r *System) ImportDocument(ctx context.Context, req ImportDocumentRequest) (*ImportDocumentResult, error) {
	if r == nil || r.inner == nil {
		return nil, errdefs.NotAvailablef("memory: system is not configured")
	}
	scope, doc, err := normalizeImportDocumentRequest(req)
	if err != nil {
		return nil, err
	}
	if r.inner.DocumentStore() == nil {
		return nil, errdefs.NotAvailablef("memory: document store is not configured")
	}
	stored, err := r.inner.DocumentStore().Put(ctx, sourcedocument.PutRequest{Document: doc})
	if err != nil {
		return nil, err
	}

	result := &ImportDocumentResult{}
	for _, stage := range r.plan.Write {
		if stage.Name != writeStageChunkDocument {
			continue
		}
		chunks, err := r.executeDocumentWriteStage(ctx, stage, scope, stored)
		if err != nil {
			return nil, err
		}
		result.Chunks = chunks
	}
	return result, nil
}

func (r *System) executeDocumentWriteStage(ctx context.Context, stage PlannedStage, scope views.Scope, doc sourcedocument.Document) ([]viewdocument.Chunk, error) {
	switch stage.Name {
	case writeStageChunkDocument:
		if err := r.requireWriteStage(stage, CapabilityDocumentChunks); err != nil {
			return nil, err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityDocumentChunks, scope)
		if err != nil {
			return nil, err
		}
		return r.inner.IndexDocument(ctx, scope, doc.ID, namespace)
	default:
		if !stage.Optional {
			return nil, errdefs.Validationf("memory: unsupported document write stage %q", stage.Name)
		}
		return nil, nil
	}
}

// PackContext loads the recent message window and any configured retrieval
// projections named by read stages. Callers provide Scope and one product query
// instead of physical namespaces or per-projection search requests.
func (r *System) PackContext(ctx context.Context, req ContextRequest) (*ContextPack, error) {
	if r == nil || r.inner == nil {
		return nil, errdefs.NotAvailablef("memory: system is not configured")
	}
	innerReq, err := r.packContextRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	pack, err := r.inner.PackContext(ctx, innerReq)
	if err != nil {
		return nil, err
	}
	return contextPackFromExecutor(pack), nil
}

func factGraphBuildResultFromExecutor(in *internalexecutor.FactGraphBuildResult) *FactGraphBuildResult {
	if in == nil {
		return nil
	}
	return &FactGraphBuildResult{
		Nodes: append([]fact.Node(nil), in.Nodes...),
		Edges: append([]fact.Edge(nil), in.Edges...),
	}
}

func factGraphBuildResultToExecutor(in *FactGraphBuildResult) *internalexecutor.FactGraphBuildResult {
	if in == nil {
		return nil
	}
	return &internalexecutor.FactGraphBuildResult{
		Nodes: append([]fact.Node(nil), in.Nodes...),
		Edges: append([]fact.Edge(nil), in.Edges...),
	}
}

func contextPackFromExecutor(in *internalexecutor.ContextPack) *ContextPack {
	if in == nil {
		return nil
	}
	return &ContextPack{
		Window:             in.Window,
		SummaryHits:        append([]derive.SummaryNodeSearchHit(nil), in.SummaryHits...),
		DocumentHits:       append([]derive.DocumentChunkSearchHit(nil), in.DocumentHits...),
		ObservationHits:    append([]derive.ObservationSearchHit(nil), in.ObservationHits...),
		FactHits:           append([]derive.FactSearchHit(nil), in.FactHits...),
		FactGraphHits:      append([]derive.FactGraphSearchHit(nil), in.FactGraphHits...),
		EntityProfileHits:  append([]derive.EntityProfileSearchHit(nil), in.EntityProfileHits...),
		EntityTimelineHits: append([]derive.EntityTimelineSearchHit(nil), in.EntityTimelineHits...),
		Items:              append([]derive.ContextItem(nil), in.Items...),
	}
}

func (r *System) packContextRequest(ctx context.Context, req ContextRequest) (internalexecutor.PackContextRequest, error) {
	window, scope, err := normalizeContextRequest(req)
	if err != nil {
		return internalexecutor.PackContextRequest{}, err
	}
	query := strings.TrimSpace(req.Query)
	topK := req.TopK
	if query != "" && topK <= 0 {
		topK = defaultContextTopK
	}
	searchInput := packContextSearchInput{
		query:         query,
		topK:          topK,
		hybridEnabled: r.hybridQueryEmbeddingEnabled(),
		embed: func() ([]float32, error) {
			vector, err := r.deps.Embedder.Embed(ctx, query)
			if err != nil {
				return nil, fmt.Errorf("memory: embed context query: %w", err)
			}
			return vector, nil
		},
	}

	out := internalexecutor.PackContextRequest{
		Scope:  scope,
		Query:  query,
		Window: window,
	}
	for _, stage := range r.plan.Read {
		switch stage.Name {
		case readStageLoadRecentMessages, readStagePackContext:
			continue
		case readStageRetrieveSummaries:
			namespace, err := r.scopedReadNamespace(CapabilitySummaryDAG, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilitySummaryDAG, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, summaryScopeFilter(scope))
				out.SummarySearch = search
				out.SummaryNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveDocuments:
			namespace, err := r.scopedReadNamespace(CapabilityDocumentChunks, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityDocumentChunks, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, documentScopeFilter(scope))
				out.DocumentSearch = search
				out.DocumentNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveObs:
			namespace, err := r.scopedReadNamespace(CapabilityObservationLedger, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityObservationLedger, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, semanticScopeFilter(scope))
				out.ObservationSearch = search
				out.ObservationNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveFacts:
			namespace, err := r.scopedReadNamespace(CapabilityFactLedger, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityFactLedger, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, factScopeFilter(scope))
				out.FactSearch = search
				out.FactNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveFactGraph, readStageExpandFactGraph:
			namespace, err := r.scopedReadNamespace(CapabilityFactGraph, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityFactGraph, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, semanticScopeFilter(scope))
				out.FactGraphSearch = search
				out.FactGraphNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveEntityProfiles:
			namespace, err := r.scopedReadNamespace(CapabilityEntityProfile, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityEntityProfile, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, semanticScopeFilter(scope))
				out.EntityProfileSearch = search
				out.EntityProfileNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveEntityTimeline:
			namespace, err := r.scopedReadNamespace(CapabilityEntityTimeline, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityEntityTimeline, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, semanticScopeFilter(scope))
				out.EntityTimelineSearch = search
				out.EntityTimelineNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		default:
			if !stage.Optional {
				return internalexecutor.PackContextRequest{}, errdefs.Validationf("memory: unsupported read stage %q", stage.Name)
			}
		}
	}
	return out, nil
}

type packContextSearchInput struct {
	query         string
	topK          int
	hybridEnabled bool
	queryVector   []float32
	vectorReady   bool
	embed         func() ([]float32, error)
}

func (in *packContextSearchInput) ensureQueryVector() ([]float32, error) {
	if in == nil || !in.hybridEnabled {
		return nil, nil
	}
	if !in.vectorReady {
		vector, err := in.embed()
		if err != nil {
			return nil, err
		}
		in.queryVector = append([]float32(nil), vector...)
		in.vectorReady = true
	}
	return append([]float32(nil), in.queryVector...), nil
}

func (r *System) populateSearch(stage PlannedStage, capability Capability, input *packContextSearchInput, set func(*retrieval.SearchRequest)) error {
	if !r.readAvailable[capability] {
		if stage.Optional {
			return nil
		}
		return errdefs.NotAvailablef("memory: read stage %q requires configured projection for capability %q", stage.Name, capability)
	}
	if input == nil || strings.TrimSpace(input.query) == "" {
		return errdefs.Validationf("memory: read stage %q requires query", stage.Name)
	}
	search := &retrieval.SearchRequest{QueryText: input.query, TopK: input.topK}
	if input.hybridEnabled {
		vector, err := input.ensureQueryVector()
		if err != nil {
			return err
		}
		search.QueryVector = vector
		search.HybridMode = retrieval.HybridDefault
	}
	set(search)
	return nil
}

func (r *System) hybridQueryEmbeddingEnabled() bool {
	if r == nil || r.deps.Embedder == nil {
		return false
	}
	index := r.inner.RetrievalIndex()
	return retrieval.Supports(index, retrieval.CapabilityVector) &&
		retrieval.Supports(index, retrieval.CapabilityHybrid)
}

func (r *System) requireWriteStage(stage PlannedStage, capability Capability) error {
	if r.writeAvailable[capability] {
		return nil
	}
	if stage.Optional {
		return errdefs.NotAvailablef("memory: optional write stage %q was not skipped", stage.Name)
	}
	return errdefs.NotAvailablef("memory: write stage %q requires configured capability %q", stage.Name, capability)
}

func (r *System) scopedWriteNamespace(capability Capability, scope views.Scope) (string, error) {
	base, ok := r.inner.ProjectionNamespace(capability)
	if !ok {
		return "", nil
	}
	return projectors.ScopedNamespace(base, scope)
}

func (r *System) scopedReadNamespace(capability Capability, scope views.Scope) (string, error) {
	if !r.readAvailable[capability] {
		return "", nil
	}
	base, ok := r.inner.ProjectionNamespace(capability)
	if !ok {
		return "", errdefs.NotAvailablef("memory: projection namespace for capability %q is not configured", capability)
	}
	return projectors.ScopedNamespace(base, scope)
}

func normalizeAppendMessageRequest(req AppendMessageRequest) (string, viewobservation.Scope, error) {
	scope := normalizeScope(req.Scope)
	conversationID := scope.ConversationID
	if conversationID == "" {
		return "", viewobservation.Scope{}, errdefs.Validationf("memory: conversation_id is required to append messages")
	}
	if err := scope.Validate(); err != nil {
		return "", viewobservation.Scope{}, errdefs.Validationf("memory: invalid scope: %w", err)
	}
	return conversationID, scope, nil
}

func normalizeImportDocumentRequest(req ImportDocumentRequest) (views.Scope, sourcedocument.Document, error) {
	scope := normalizeScope(req.Scope)
	if err := scope.Validate(); err != nil {
		return views.Scope{}, sourcedocument.Document{}, errdefs.Validationf("memory: invalid scope: %w", err)
	}

	doc := req.Document
	doc.DatasetID = strings.TrimSpace(doc.DatasetID)
	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return views.Scope{}, sourcedocument.Document{}, errdefs.Validationf("memory: document id is required")
	}
	if doc.DatasetID == "" {
		if scope.DatasetID == "" {
			return views.Scope{}, sourcedocument.Document{}, errdefs.Validationf("memory: document dataset_id is required")
		}
		doc.DatasetID = scope.DatasetID
	}
	if scope.DatasetID != "" && doc.DatasetID != scope.DatasetID {
		return views.Scope{}, sourcedocument.Document{}, errdefs.Validationf("memory: document dataset_id %q does not match scope dataset_id %q", doc.DatasetID, scope.DatasetID)
	}
	scope.DatasetID = doc.DatasetID
	return scope, doc, nil
}

func normalizeContextRequest(req ContextRequest) (recent.WindowRequest, views.Scope, error) {
	window := req.Window
	scope := normalizeScope(req.Scope)
	conversationID := scope.ConversationID
	if conversationID == "" {
		return recent.WindowRequest{}, views.Scope{}, errdefs.Validationf("memory: conversation_id is required")
	}
	if !window.Scope.IsZero() {
		windowScope := normalizeScope(window.Scope)
		if err := validateNestedWindowScope(windowScope, scope); err != nil {
			return recent.WindowRequest{}, views.Scope{}, err
		}
	}
	window.Scope = scope
	if err := scope.Validate(); err != nil {
		return recent.WindowRequest{}, views.Scope{}, errdefs.Validationf("memory: invalid scope: %w", err)
	}
	return window, scope, nil
}

func normalizeScope(scope views.Scope) views.Scope {
	scope.RuntimeID = strings.TrimSpace(scope.RuntimeID)
	scope.UserID = strings.TrimSpace(scope.UserID)
	scope.AgentID = strings.TrimSpace(scope.AgentID)
	scope.ConversationID = strings.TrimSpace(scope.ConversationID)
	scope.DatasetID = strings.TrimSpace(scope.DatasetID)
	scope.EntityID = strings.TrimSpace(scope.EntityID)
	return scope
}

func validateNestedWindowScope(windowScope, scope views.Scope) error {
	if windowScope.RuntimeID != "" && windowScope.RuntimeID != scope.RuntimeID {
		return errdefs.Validationf("memory: window runtime_id %q does not match scope runtime_id %q", windowScope.RuntimeID, scope.RuntimeID)
	}
	if windowScope.UserID != "" && windowScope.UserID != scope.UserID {
		return errdefs.Validationf("memory: window user_id %q does not match scope user_id %q", windowScope.UserID, scope.UserID)
	}
	if windowScope.AgentID != "" && windowScope.AgentID != scope.AgentID {
		return errdefs.Validationf("memory: window agent_id %q does not match scope agent_id %q", windowScope.AgentID, scope.AgentID)
	}
	if windowScope.ConversationID != "" && windowScope.ConversationID != scope.ConversationID {
		return errdefs.Validationf("memory: window conversation_id %q does not match scope conversation_id %q", windowScope.ConversationID, scope.ConversationID)
	}
	if windowScope.DatasetID != "" && windowScope.DatasetID != scope.DatasetID {
		return errdefs.Validationf("memory: window dataset_id %q does not match scope dataset_id %q", windowScope.DatasetID, scope.DatasetID)
	}
	if windowScope.EntityID != "" && windowScope.EntityID != scope.EntityID {
		return errdefs.Validationf("memory: window entity_id %q does not match scope entity_id %q", windowScope.EntityID, scope.EntityID)
	}
	return nil
}

func summaryScopeFilter(scope views.Scope) retrieval.Filter {
	return semanticScopeFilter(scope)
}

func documentScopeFilter(scope views.Scope) retrieval.Filter {
	if scope.DatasetID == "" {
		return retrieval.Filter{}
	}
	return retrieval.Filter{Eq: map[string]any{
		"projector.dataset_id": scope.DatasetID,
	}}
}

func semanticScopeFilter(scope views.Scope) retrieval.Filter {
	eq := map[string]any{}
	if scope.ConversationID != "" {
		eq["projector.conversation_id"] = scope.ConversationID
	}
	if scope.DatasetID != "" {
		eq["projector.dataset_id"] = scope.DatasetID
	}
	if scope.EntityID != "" {
		eq["projector.entity_id"] = scope.EntityID
	}
	filter := retrieval.Filter{}
	if len(eq) > 0 {
		filter.Eq = eq
	}
	if scope.AgentID != "" {
		agentFilter := retrieval.Filter{Or: []retrieval.Filter{
			{Eq: map[string]any{"projector.agent_id": scope.AgentID}},
			{Eq: map[string]any{"projector.agent_id": ""}},
		}}
		if filterIsZero(filter) {
			return agentFilter
		}
		filter.And = append(filter.And, agentFilter)
	}
	return filter
}

func factScopeFilter(scope views.Scope) retrieval.Filter {
	return mergeFilters(semanticScopeFilter(scope), retrieval.Filter{Eq: map[string]any{
		projectors.MetadataStatusKey: string(fact.FactActive),
	}})
}

func mergeFilters(left, right retrieval.Filter) retrieval.Filter {
	if filterIsZero(left) {
		return right
	}
	if filterIsZero(right) {
		return left
	}
	return retrieval.Filter{And: []retrieval.Filter{left, right}}
}

func filterIsZero(filter retrieval.Filter) bool {
	return len(filter.And) == 0 &&
		len(filter.Or) == 0 &&
		filter.Not == nil &&
		len(filter.Eq) == 0 &&
		len(filter.Neq) == 0 &&
		len(filter.In) == 0 &&
		len(filter.NotIn) == 0 &&
		len(filter.Range) == 0 &&
		len(filter.Exists) == 0 &&
		len(filter.Missing) == 0 &&
		len(filter.Match) == 0 &&
		len(filter.Contains) == 0 &&
		len(filter.IContains) == 0 &&
		len(filter.ContainsAny) == 0 &&
		len(filter.ContainsAll) == 0
}

func configuredWriteCapabilities(assembly compiler.Assembly, deps Deps) map[Capability]bool {
	return map[Capability]bool{
		CapabilityDocumentChunks:    assembly.HasCapability(CapabilityDocumentChunks) && deps.DocumentStore != nil && deps.ChunkStore != nil && deps.DocumentChunker != nil,
		CapabilitySummaryDAG:        assembly.HasCapability(CapabilitySummaryDAG) && deps.MessageStore != nil && deps.SummaryStore != nil && deps.Summarizer != nil,
		CapabilityObservationLedger: assembly.HasCapability(CapabilityObservationLedger) && deps.MessageStore != nil && deps.ObservationStore != nil && deps.ObservationExtractor != nil,
		CapabilityFactLedger:        assembly.HasCapability(CapabilityFactLedger) && deps.FactStore != nil && deps.FactReconciler != nil,
		CapabilityFactGraph:         assembly.HasCapability(CapabilityFactGraph) && deps.FactGraphStore != nil && deps.FactGraphBuilder != nil,
		CapabilityEntityProfile:     assembly.HasCapability(CapabilityEntityProfile) && deps.EntityProfileStore != nil && deps.EntityProfileBuilder != nil,
		CapabilityEntityTimeline:    assembly.HasCapability(CapabilityEntityTimeline) && deps.EntityTimelineStore != nil && deps.EntityTimelineBuilder != nil,
	}
}

func configuredReadCapabilities(assembly compiler.Assembly, deps Deps) map[Capability]bool {
	return map[Capability]bool{
		CapabilitySummaryDAG:        readProjectionConfigured(assembly, deps, CapabilitySummaryDAG) && deps.SummaryStore != nil && deps.Summarizer != nil,
		CapabilityDocumentChunks:    readProjectionConfigured(assembly, deps, CapabilityDocumentChunks) && deps.ChunkStore != nil && deps.DocumentChunker != nil,
		CapabilityObservationLedger: readProjectionConfigured(assembly, deps, CapabilityObservationLedger) && deps.ObservationStore != nil && deps.ObservationExtractor != nil,
		CapabilityFactLedger:        readProjectionConfigured(assembly, deps, CapabilityFactLedger) && deps.FactStore != nil && deps.FactReconciler != nil,
		CapabilityFactGraph:         readProjectionConfigured(assembly, deps, CapabilityFactGraph) && deps.FactGraphStore != nil && deps.FactGraphBuilder != nil,
		CapabilityEntityProfile:     readProjectionConfigured(assembly, deps, CapabilityEntityProfile) && deps.EntityProfileStore != nil && deps.EntityProfileBuilder != nil,
		CapabilityEntityTimeline:    readProjectionConfigured(assembly, deps, CapabilityEntityTimeline) && deps.EntityTimelineStore != nil && deps.EntityTimelineBuilder != nil,
	}
}

func readProjectionConfigured(assembly compiler.Assembly, deps Deps, capability Capability) bool {
	if deps.Index == nil || !assembly.HasCapability(capability) {
		return false
	}
	_, ok := assembly.ProjectionNamespace(capability)
	return ok
}
