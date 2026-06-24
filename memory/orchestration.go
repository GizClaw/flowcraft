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
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	defaultContextTopK = 5

	writeStageAppendMessage    = "append_message"
	writeStageChunkDocument    = "chunk_document"
	writeStageBuildSummaryDAG  = "build_summary_dag"
	writeStageBuildEntityFacts = "build_entity_facts"
	writeStageIndexMessages    = "index_messages"

	readStageLoadRecentMessages  = "load_recent_messages"
	readStageRetrieveMessages    = "retrieve_messages"
	readStageRetrieveSummaries   = "retrieve_summaries"
	readStageRetrieveEntityFacts = "retrieve_entity_facts"
	readStageRetrieveDocuments   = "retrieve_documents"
	readStagePackContext         = "pack_context"
)

// AppendMessageRequest appends canonical conversation messages and then runs
// configured write stages.
type AppendMessageRequest struct {
	TraceID  TraceID
	Messages []sourcemessage.Message
	Scope    views.Scope
}

// AppendMessageResult contains root-facade write results.
type AppendMessageResult struct {
	Jobs []LifecycleJobID
}

// ImportDocumentRequest stores one canonical document and runs configured
// document derivation stages for its scope.
type ImportDocumentRequest struct {
	TraceID  TraceID
	Scope    views.Scope
	Document sourcedocument.Document
}

// ImportDocumentResult contains document records produced by import.
type ImportDocumentResult struct {
	Chunks []viewdocument.Chunk
}

// ContextRequest asks the facade to compose read-time context.
type ContextRequest struct {
	TraceID     TraceID
	Scope       views.Scope
	Query       string
	TopK        int
	Window      recent.WindowRequest
	PackOptions derive.ContextPackOptions
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
	appended, err := r.inner.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: conversationID,
		Messages:       req.Messages,
	})
	if err != nil {
		return nil, err
	}
	if len(appended) == 0 {
		return &AppendMessageResult{}, nil
	}

	result := &AppendMessageResult{}
	window := appendedMessagesWindow(scope, appended)
	var syncStages []PlannedStage
	var asyncChain []PlannedStage
	flushSync := func() error {
		if len(syncStages) == 0 {
			return nil
		}
		if err := r.executeWriteStages(ctx, syncStages, window, scope); err != nil {
			return err
		}
		syncStages = nil
		return nil
	}
	flushAsync := func() error {
		if len(asyncChain) == 0 {
			return nil
		}
		handle, err := r.enqueueWriteChain(ctx, ensureTraceID(req.TraceID), scope, window, asyncChain)
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
			if err := flushSync(); err != nil {
				return nil, err
			}
			asyncChain = append(asyncChain, stage)
			continue
		}
		if err := flushAsync(); err != nil {
			return nil, err
		}
		syncStages = append(syncStages, stage)
	}
	if err := flushSync(); err != nil {
		return nil, err
	}
	if err := flushAsync(); err != nil {
		return nil, err
	}
	return result, nil
}

func appendedMessagesWindow(scope views.Scope, appended []sourcemessage.Message) recent.WindowRequest {
	firstSeq := appended[0].Seq
	afterSeq := uint64(0)
	if firstSeq > 0 {
		afterSeq = firstSeq - 1
	}
	return recent.WindowRequest{
		Scope:    scope,
		AfterSeq: afterSeq,
		Budget: &recent.WindowBudget{
			MaxMessages: len(appended),
		},
	}
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
	report.Message = "write_chain lifecycle job enqueued"
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

func (r *System) executeWriteStages(ctx context.Context, stages []PlannedStage, window recent.WindowRequest, scope views.Scope) error {
	for _, stage := range stages {
		if err := r.executeWriteStage(ctx, stage, window, scope); err != nil {
			return err
		}
	}
	return nil
}

func (r *System) executeWriteStage(ctx context.Context, stage PlannedStage, window recent.WindowRequest, scope views.Scope) error {
	switch stage.Name {
	case writeStageIndexMessages:
		if err := r.requireWriteStage(stage, CapabilityMessageIndex); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityMessageIndex, scope)
		if err != nil {
			return err
		}
		_, err = r.inner.IndexMessages(ctx, window, namespace)
		return err
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
	case writeStageBuildEntityFacts:
		if err := r.requireWriteStage(stage, CapabilityEntityFactIndex); err != nil {
			return err
		}
		namespace, err := r.scopedWriteNamespace(CapabilityEntityFactIndex, scope)
		if err != nil {
			return err
		}
		_, err = r.inner.BuildEntityFacts(ctx, window, namespace)
		return err
	default:
		if !stage.Optional {
			return errdefs.Validationf("memory: unsupported write stage %q", stage.Name)
		}
		return nil
	}
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

// PackContext loads the recent message window and configured retrieval
// projections named by read stages.
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

func contextPackFromExecutor(in *internalexecutor.ContextPack) *ContextPack {
	if in == nil {
		return nil
	}
	return &ContextPack{
		Window:       in.Window,
		MessageHits:  append([]derive.SourceMessageSearchHit(nil), in.MessageHits...),
		SummaryHits:  append([]derive.SummaryNodeSearchHit(nil), in.SummaryHits...),
		DocumentHits: append([]derive.DocumentChunkSearchHit(nil), in.DocumentHits...),
		EntityHits:   append([]derive.EntityFactSearchHit(nil), in.EntityHits...),
		Items:        append([]derive.ContextItem(nil), in.Items...),
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
			embedCtx, cancel := r.embeddingContext(ctx)
			if cancel != nil {
				defer cancel()
			}
			vector, err := r.deps.Embedder.Embed(embedCtx, query)
			if err != nil {
				return nil, fmt.Errorf("memory: embed context query: %w", err)
			}
			return vector, nil
		},
	}

	out := internalexecutor.PackContextRequest{
		Scope:       scope,
		Query:       query,
		Window:      window,
		PackOptions: req.PackOptions,
	}
	for _, stage := range r.plan.Read {
		switch stage.Name {
		case readStageLoadRecentMessages, readStagePackContext:
			continue
		case readStageRetrieveMessages:
			namespace, err := r.scopedReadNamespace(CapabilityMessageIndex, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityMessageIndex, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, messageScopeFilter(scope))
				out.MessageSearch = search
				out.MessageNamespace = namespace
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveSummaries:
			namespace, err := r.scopedReadNamespace(CapabilitySummaryDAG, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			summaryRetrieval, err := summaryRetrievalConfigFromStage(stage)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilitySummaryDAG, &searchInput, func(search *retrieval.SearchRequest) {
				search.Filter = mergeFilters(search.Filter, summaryScopeFilter(scope))
				out.SummarySearch = search
				out.SummaryNamespace = namespace
				out.SummaryRetrieval = summaryRetrieval
			}); err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
		case readStageRetrieveEntityFacts:
			namespace, err := r.scopedReadNamespace(CapabilityEntityFactIndex, scope)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			entityQuery, err := r.entityFactExpandedQuery(ctx, scope, query)
			if err != nil {
				return internalexecutor.PackContextRequest{}, err
			}
			if err := r.populateSearch(stage, CapabilityEntityFactIndex, &searchInput, func(search *retrieval.SearchRequest) {
				if entityQuery != "" {
					search.QueryText = entityQuery
				}
				search.Filter = mergeFilters(search.Filter, entityFactScopeFilter(scope))
				out.EntityFactSearch = search
				out.EntityFactNamespace = namespace
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
		default:
			if !stage.Optional {
				return internalexecutor.PackContextRequest{}, errdefs.Validationf("memory: unsupported read stage %q", stage.Name)
			}
		}
	}
	return out, nil
}

func (r *System) entityFactExpandedQuery(ctx context.Context, scope views.Scope, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || r == nil || r.deps.EntityFactStore == nil {
		return query, nil
	}
	entities, err := r.deps.EntityFactStore.ListEntities(ctx, scope, viewentityfact.ListOptions{Limit: 2000})
	if err != nil {
		return "", err
	}
	normalizedQuery := viewentityfact.NormalizeAlias(query)
	if normalizedQuery == "" {
		return query, nil
	}
	seen := map[string]bool{strings.ToLower(query): true}
	additions := make([]string, 0)
	for _, entity := range entities {
		matched := false
		for _, aliasKey := range entity.AliasKeys() {
			if aliasKey != "" && strings.Contains(normalizedQuery, aliasKey) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, value := range append([]string{entity.Name}, entity.Aliases...) {
			value = strings.TrimSpace(value)
			key := strings.ToLower(value)
			if value == "" || seen[key] {
				continue
			}
			seen[key] = true
			additions = append(additions, value)
		}
	}
	if len(additions) == 0 {
		return query, nil
	}
	return query + " " + strings.Join(additions, " "), nil
}

const (
	stageConfigSummaryDrillDownMaxDepth  = "drilldown_max_depth"
	stageConfigSummaryDrillDownChildTopK = "drilldown_child_top_k"
)

func summaryRetrievalConfigFromStage(stage PlannedStage) (internalexecutor.SummaryRetrievalConfig, error) {
	cfg := internalexecutor.SummaryRetrievalConfig{
		DrillDownMaxDepth:  -1,
		DrillDownChildTopK: 2,
	}
	for key, value := range stage.Config {
		switch key {
		case stageConfigSummaryDrillDownMaxDepth:
			intValue, err := stageConfigInt(key, value)
			if err != nil {
				return internalexecutor.SummaryRetrievalConfig{}, err
			}
			cfg.DrillDownMaxDepth = intValue
		case stageConfigSummaryDrillDownChildTopK:
			intValue, err := stageConfigInt(key, value)
			if err != nil {
				return internalexecutor.SummaryRetrievalConfig{}, err
			}
			if intValue < 1 {
				return internalexecutor.SummaryRetrievalConfig{}, errdefs.Validationf("memory: retrieve_summaries config %q must be >= 1", key)
			}
			cfg.DrillDownChildTopK = intValue
		default:
			return internalexecutor.SummaryRetrievalConfig{}, errdefs.Validationf("memory: unsupported retrieve_summaries config key %q", key)
		}
	}
	return cfg, nil
}

func stageConfigInt(key string, value any) (int, error) {
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int64:
		return int(typed), nil
	case int32:
		return int(typed), nil
	case uint:
		return int(typed), nil
	case uint64:
		return int(typed), nil
	case uint32:
		return int(typed), nil
	case float64:
		if typed != float64(int(typed)) {
			return 0, errdefs.Validationf("memory: retrieve_summaries config %q must be an integer", key)
		}
		return int(typed), nil
	case float32:
		if typed != float32(int(typed)) {
			return 0, errdefs.Validationf("memory: retrieve_summaries config %q must be an integer", key)
		}
		return int(typed), nil
	default:
		return 0, errdefs.Validationf("memory: retrieve_summaries config %q must be an integer", key)
	}
}

func (r *System) embeddingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r == nil || r.deps.Embedding.Timeout <= 0 {
		return ctx, nil
	}
	return context.WithTimeout(ctx, r.deps.Embedding.Timeout)
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

func normalizeAppendMessageRequest(req AppendMessageRequest) (string, views.Scope, error) {
	scope := normalizeScope(req.Scope)
	conversationID := scope.ConversationID
	if conversationID == "" {
		return "", views.Scope{}, errdefs.Validationf("memory: conversation_id is required to append messages")
	}
	if err := scope.Validate(); err != nil {
		return "", views.Scope{}, errdefs.Validationf("memory: invalid scope: %w", err)
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
	return nil
}

func summaryScopeFilter(scope views.Scope) retrieval.Filter {
	return messageScopeFilter(scope)
}

func entityFactScopeFilter(scope views.Scope) retrieval.Filter {
	return messageScopeFilter(scope)
}

func messageScopeFilter(scope views.Scope) retrieval.Filter {
	eq := map[string]any{}
	if scope.ConversationID != "" {
		eq["projector.conversation_id"] = scope.ConversationID
	}
	if scope.DatasetID != "" {
		eq["projector.dataset_id"] = scope.DatasetID
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

func documentScopeFilter(scope views.Scope) retrieval.Filter {
	if scope.DatasetID == "" {
		return retrieval.Filter{}
	}
	return retrieval.Filter{Eq: map[string]any{
		"projector.dataset_id": scope.DatasetID,
	}}
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
		CapabilityDocumentChunks:  assembly.HasCapability(CapabilityDocumentChunks) && deps.DocumentStore != nil && deps.ChunkStore != nil && deps.DocumentChunker != nil,
		CapabilitySummaryDAG:      assembly.HasCapability(CapabilitySummaryDAG) && deps.MessageStore != nil && deps.SummaryStore != nil && deps.Summarizer != nil,
		CapabilityMessageIndex:    readProjectionConfigured(assembly, deps, CapabilityMessageIndex) && deps.MessageStore != nil,
		CapabilityEntityFactIndex: assembly.HasCapability(CapabilityEntityFactIndex) && deps.MessageStore != nil && deps.EntityFactStore != nil && deps.EntityFactExtractor != nil,
	}
}

func configuredWritePlanCapabilities(assembly compiler.Assembly, deps Deps) map[Capability]bool {
	return map[Capability]bool{
		CapabilityDocumentChunks:  assembly.HasCapability(CapabilityDocumentChunks) && deps.DocumentStore != nil && deps.ChunkStore != nil,
		CapabilitySummaryDAG:      assembly.HasCapability(CapabilitySummaryDAG) && deps.MessageStore != nil && deps.SummaryStore != nil,
		CapabilityMessageIndex:    readProjectionConfigured(assembly, deps, CapabilityMessageIndex) && deps.MessageStore != nil,
		CapabilityEntityFactIndex: assembly.HasCapability(CapabilityEntityFactIndex) && deps.MessageStore != nil && deps.EntityFactStore != nil,
	}
}

func configuredReadCapabilities(assembly compiler.Assembly, deps Deps) map[Capability]bool {
	return map[Capability]bool{
		CapabilityMessageIndex:    readProjectionConfigured(assembly, deps, CapabilityMessageIndex) && deps.MessageStore != nil,
		CapabilitySummaryDAG:      readProjectionConfigured(assembly, deps, CapabilitySummaryDAG) && deps.SummaryStore != nil,
		CapabilityDocumentChunks:  readProjectionConfigured(assembly, deps, CapabilityDocumentChunks) && deps.ChunkStore != nil,
		CapabilityEntityFactIndex: readProjectionConfigured(assembly, deps, CapabilityEntityFactIndex) && deps.EntityFactStore != nil,
	}
}

func readProjectionConfigured(assembly compiler.Assembly, deps Deps, capability Capability) bool {
	if deps.Index == nil || !assembly.HasCapability(capability) {
		return false
	}
	_, ok := assembly.ProjectionNamespace(capability)
	return ok
}
