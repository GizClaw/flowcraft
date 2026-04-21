package store

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracingStore wraps a model.Store and adds OpenTelemetry tracing spans
// to every operation.
type TracingStore struct {
	inner  model.Store
	tracer trace.Tracer
}

// Mirror SQLiteStore's resolver-extension assertions: if TracingStore
// ever drops a method, the resolver would silently stop seeing
// ModelConfigStore / DefaultModelStore in the tracing-enabled build
// (prod), while tests — which skip tracing — would still pass.
var (
	_ llm.ModelConfigStore    = (*TracingStore)(nil)
	_ llm.DefaultModelStore   = (*TracingStore)(nil)
	_ llm.ProviderConfigStore = (*TracingStore)(nil)
)

// WithStoreTracing wraps the given store with OTel tracing instrumentation.
func WithStoreTracing(inner model.Store) model.Store {
	return &TracingStore{
		inner:  inner,
		tracer: telemetry.TracerWithSuffix("store"),
	}
}

func (s *TracingStore) start(ctx context.Context, op string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx, span := s.tracer.Start(ctx, "store."+op, trace.WithAttributes(attrs...))
	return ctx, span
}

func finish(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func (s *TracingStore) ListAgents(ctx context.Context, opts model.ListOptions) ([]*model.Agent, *model.ListResult, error) {
	ctx, span := s.start(ctx, "ListAgents")
	agents, lr, err := s.inner.ListAgents(ctx, opts)
	finish(span, err)
	return agents, lr, err
}

func (s *TracingStore) CreateAgent(ctx context.Context, a *model.Agent) (*model.Agent, error) {
	ctx, span := s.start(ctx, "CreateAgent", attribute.String("agent.name", a.Name))
	agent, err := s.inner.CreateAgent(ctx, a)
	finish(span, err)
	return agent, err
}

func (s *TracingStore) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	ctx, span := s.start(ctx, "GetAgent", attribute.String("agent.id", id))
	agent, err := s.inner.GetAgent(ctx, id)
	finish(span, err)
	return agent, err
}

func (s *TracingStore) UpdateAgent(ctx context.Context, a *model.Agent) (*model.Agent, error) {
	ctx, span := s.start(ctx, "UpdateAgent", attribute.String("agent.id", a.AgentID))
	agent, err := s.inner.UpdateAgent(ctx, a)
	finish(span, err)
	return agent, err
}

func (s *TracingStore) DeleteAgent(ctx context.Context, id string) error {
	ctx, span := s.start(ctx, "DeleteAgent", attribute.String("agent.id", id))
	err := s.inner.DeleteAgent(ctx, id)
	finish(span, err)
	return err
}

func (s *TracingStore) ListConversations(ctx context.Context, agentID string, opts model.ListOptions, filters ...model.ListFilter) ([]*model.Conversation, *model.ListResult, error) {
	ctx, span := s.start(ctx, "ListConversations", attribute.String("agent.id", agentID))
	convs, lr, err := s.inner.ListConversations(ctx, agentID, opts, filters...)
	finish(span, err)
	return convs, lr, err
}

func (s *TracingStore) GetConversation(ctx context.Context, id string) (*model.Conversation, error) {
	ctx, span := s.start(ctx, "GetConversation", attribute.String("conversation.id", id))
	conv, err := s.inner.GetConversation(ctx, id)
	finish(span, err)
	return conv, err
}

func (s *TracingStore) CreateConversation(ctx context.Context, conv *model.Conversation) (*model.Conversation, error) {
	ctx, span := s.start(ctx, "CreateConversation", attribute.String("agent.id", conv.AgentID))
	c, err := s.inner.CreateConversation(ctx, conv)
	finish(span, err)
	return c, err
}

func (s *TracingStore) UpdateConversation(ctx context.Context, conv *model.Conversation) (*model.Conversation, error) {
	ctx, span := s.start(ctx, "UpdateConversation", attribute.String("conversation.id", conv.ID))
	c, err := s.inner.UpdateConversation(ctx, conv)
	finish(span, err)
	return c, err
}

func (s *TracingStore) GetMessages(ctx context.Context, conversationID string) ([]*model.Message, error) {
	ctx, span := s.start(ctx, "GetMessages", attribute.String("conversation.id", conversationID))
	msgs, err := s.inner.GetMessages(ctx, conversationID)
	finish(span, err)
	return msgs, err
}

func (s *TracingStore) GetRecentMessages(ctx context.Context, conversationID string, limit int) ([]*model.Message, error) {
	ctx, span := s.start(ctx, "GetRecentMessages", attribute.String("conversation.id", conversationID))
	msgs, err := s.inner.GetRecentMessages(ctx, conversationID, limit)
	finish(span, err)
	return msgs, err
}

func (s *TracingStore) SaveMessage(ctx context.Context, msg *model.Message) error {
	ctx, span := s.start(ctx, "SaveMessage", attribute.String("conversation.id", msg.ConversationID))
	err := s.inner.SaveMessage(ctx, msg)
	finish(span, err)
	return err
}

func (s *TracingStore) SaveWorkflowRun(ctx context.Context, run *model.WorkflowRun) error {
	ctx, span := s.start(ctx, "SaveWorkflowRun", attribute.String("agent.id", run.AgentID))
	err := s.inner.SaveWorkflowRun(ctx, run)
	finish(span, err)
	return err
}

func (s *TracingStore) GetWorkflowRun(ctx context.Context, id string) (*model.WorkflowRun, error) {
	ctx, span := s.start(ctx, "GetWorkflowRun", attribute.String("run.id", id))
	run, err := s.inner.GetWorkflowRun(ctx, id)
	finish(span, err)
	return run, err
}

func (s *TracingStore) ListWorkflowRuns(ctx context.Context, agentID string, opts model.ListOptions) ([]*model.WorkflowRun, *model.ListResult, error) {
	ctx, span := s.start(ctx, "ListWorkflowRuns", attribute.String("agent.id", agentID))
	runs, lr, err := s.inner.ListWorkflowRuns(ctx, agentID, opts)
	finish(span, err)
	return runs, lr, err
}

func (s *TracingStore) SaveExecutionEvent(ctx context.Context, ev *model.ExecutionEvent) error {
	ctx, span := s.start(ctx, "SaveExecutionEvent", attribute.String("run.id", ev.RunID))
	err := s.inner.SaveExecutionEvent(ctx, ev)
	finish(span, err)
	return err
}

func (s *TracingStore) ListExecutionEvents(ctx context.Context, runID string) ([]*model.ExecutionEvent, error) {
	ctx, span := s.start(ctx, "ListExecutionEvents", attribute.String("run.id", runID))
	events, err := s.inner.ListExecutionEvents(ctx, runID)
	finish(span, err)
	return events, err
}

func (s *TracingStore) SaveKanbanCard(ctx context.Context, card *model.KanbanCard) error {
	ctx, span := s.start(ctx, "SaveKanbanCard", attribute.String("runtime.id", card.RuntimeID))
	err := s.inner.SaveKanbanCard(ctx, card)
	finish(span, err)
	return err
}

func (s *TracingStore) ListKanbanCards(ctx context.Context, runtimeID string) ([]*model.KanbanCard, error) {
	ctx, span := s.start(ctx, "ListKanbanCards", attribute.String("runtime.id", runtimeID))
	cards, err := s.inner.ListKanbanCards(ctx, runtimeID)
	finish(span, err)
	return cards, err
}

func (s *TracingStore) DeleteKanbanCards(ctx context.Context, runtimeID string) error {
	ctx, span := s.start(ctx, "DeleteKanbanCards", attribute.String("runtime.id", runtimeID))
	err := s.inner.DeleteKanbanCards(ctx, runtimeID)
	finish(span, err)
	return err
}

func (s *TracingStore) ListTemplates(ctx context.Context) ([]*model.Template, error) {
	ctx, span := s.start(ctx, "ListTemplates")
	templates, err := s.inner.ListTemplates(ctx)
	finish(span, err)
	return templates, err
}

func (s *TracingStore) SaveTemplate(ctx context.Context, t *model.Template) error {
	ctx, span := s.start(ctx, "SaveTemplate", attribute.String("template.name", t.Name))
	err := s.inner.SaveTemplate(ctx, t)
	finish(span, err)
	return err
}

func (s *TracingStore) DeleteTemplate(ctx context.Context, name string) error {
	ctx, span := s.start(ctx, "DeleteTemplate", attribute.String("template.name", name))
	err := s.inner.DeleteTemplate(ctx, name)
	finish(span, err)
	return err
}

func (s *TracingStore) ListDatasets(ctx context.Context) ([]*model.Dataset, error) {
	ctx, span := s.start(ctx, "ListDatasets")
	datasets, err := s.inner.ListDatasets(ctx)
	finish(span, err)
	return datasets, err
}

func (s *TracingStore) CreateDataset(ctx context.Context, ds *model.Dataset) (*model.Dataset, error) {
	ctx, span := s.start(ctx, "CreateDataset", attribute.String("dataset.name", ds.Name))
	dataset, err := s.inner.CreateDataset(ctx, ds)
	finish(span, err)
	return dataset, err
}

func (s *TracingStore) GetDataset(ctx context.Context, id string) (*model.Dataset, error) {
	ctx, span := s.start(ctx, "GetDataset", attribute.String("dataset.id", id))
	dataset, err := s.inner.GetDataset(ctx, id)
	finish(span, err)
	return dataset, err
}

func (s *TracingStore) DeleteDataset(ctx context.Context, id string) error {
	ctx, span := s.start(ctx, "DeleteDataset", attribute.String("dataset.id", id))
	err := s.inner.DeleteDataset(ctx, id)
	finish(span, err)
	return err
}

func (s *TracingStore) AddDocument(ctx context.Context, datasetID, name, content string) (*model.DatasetDocument, error) {
	ctx, span := s.start(ctx, "AddDocument", attribute.String("dataset.id", datasetID))
	doc, err := s.inner.AddDocument(ctx, datasetID, name, content)
	finish(span, err)
	return doc, err
}

func (s *TracingStore) GetDocument(ctx context.Context, datasetID, docID string) (*model.DatasetDocument, error) {
	ctx, span := s.start(ctx, "GetDocument", attribute.String("document.id", docID))
	doc, err := s.inner.GetDocument(ctx, datasetID, docID)
	finish(span, err)
	return doc, err
}

func (s *TracingStore) ListDocuments(ctx context.Context, datasetID string) ([]*model.DatasetDocument, error) {
	ctx, span := s.start(ctx, "ListDocuments", attribute.String("dataset.id", datasetID))
	docs, err := s.inner.ListDocuments(ctx, datasetID)
	finish(span, err)
	return docs, err
}

func (s *TracingStore) DeleteDocument(ctx context.Context, datasetID, docID string) error {
	ctx, span := s.start(ctx, "DeleteDocument", attribute.String("document.id", docID))
	err := s.inner.DeleteDocument(ctx, datasetID, docID)
	finish(span, err)
	return err
}

func (s *TracingStore) UpdateDocumentStats(ctx context.Context, datasetID, docID string, patch model.DocumentStatsPatch) error {
	ctx, span := s.start(ctx, "UpdateDocumentStats", attribute.String("document.id", docID))
	err := s.inner.UpdateDocumentStats(ctx, datasetID, docID, patch)
	finish(span, err)
	return err
}

func (s *TracingStore) UpdateDatasetAbstract(ctx context.Context, datasetID, abstract string) error {
	ctx, span := s.start(ctx, "UpdateDatasetAbstract", attribute.String("dataset.id", datasetID))
	err := s.inner.UpdateDatasetAbstract(ctx, datasetID, abstract)
	finish(span, err)
	return err
}

func (s *TracingStore) ListGraphVersions(ctx context.Context, agentID string) ([]*model.GraphVersion, error) {
	ctx, span := s.start(ctx, "ListGraphVersions", attribute.String("agent.id", agentID))
	versions, err := s.inner.ListGraphVersions(ctx, agentID)
	finish(span, err)
	return versions, err
}

func (s *TracingStore) GetGraphVersion(ctx context.Context, agentID string, version int) (*model.GraphVersion, error) {
	ctx, span := s.start(ctx, "GetGraphVersion", attribute.String("agent.id", agentID), attribute.Int("version", version))
	v, err := s.inner.GetGraphVersion(ctx, agentID, version)
	finish(span, err)
	return v, err
}

func (s *TracingStore) SaveGraphVersion(ctx context.Context, gv *model.GraphVersion) error {
	ctx, span := s.start(ctx, "SaveGraphVersion", attribute.String("agent.id", gv.AgentID), attribute.Int("version", gv.Version))
	err := s.inner.SaveGraphVersion(ctx, gv)
	finish(span, err)
	return err
}

func (s *TracingStore) PublishGraphVersion(ctx context.Context, agentID string, def *model.GraphDefinition, description string) (*model.GraphVersion, error) {
	ctx, span := s.start(ctx, "PublishGraphVersion", attribute.String("agent.id", agentID))
	version, err := s.inner.PublishGraphVersion(ctx, agentID, def, description)
	finish(span, err)
	return version, err
}

func (s *TracingStore) GetLatestPublishedVersion(ctx context.Context, agentID string) (*model.GraphVersion, error) {
	ctx, span := s.start(ctx, "GetLatestPublishedVersion", attribute.String("agent.id", agentID))
	version, err := s.inner.GetLatestPublishedVersion(ctx, agentID)
	finish(span, err)
	return version, err
}

func (s *TracingStore) UpdateVersionLock(ctx context.Context, agentID string, expectedChecksum string, newDef *model.GraphDefinition) error {
	ctx, span := s.start(ctx, "UpdateVersionLock", attribute.String("agent.id", agentID))
	err := s.inner.UpdateVersionLock(ctx, agentID, expectedChecksum, newDef)
	finish(span, err)
	return err
}

func (s *TracingStore) SaveGraphOperation(ctx context.Context, op *model.GraphOperation) error {
	ctx, span := s.start(ctx, "SaveGraphOperation", attribute.String("agent.id", op.AgentID))
	err := s.inner.SaveGraphOperation(ctx, op)
	finish(span, err)
	return err
}

func (s *TracingStore) ListGraphOperations(ctx context.Context, agentID string, opts model.ListOptions) ([]*model.GraphOperation, *model.ListResult, error) {
	ctx, span := s.start(ctx, "ListGraphOperations", attribute.String("agent.id", agentID))
	ops, result, err := s.inner.ListGraphOperations(ctx, agentID, opts)
	finish(span, err)
	return ops, result, err
}

func (s *TracingStore) GetStats(ctx context.Context) (*model.StatsOverview, error) {
	ctx, span := s.start(ctx, "GetStats")
	stats, err := s.inner.GetStats(ctx)
	finish(span, err)
	return stats, err
}

func (s *TracingStore) GetRunStats(ctx context.Context, agentID string) (*model.RunStats, error) {
	ctx, span := s.start(ctx, "GetRunStats", attribute.String("agent.id", agentID))
	stats, err := s.inner.GetRunStats(ctx, agentID)
	finish(span, err)
	return stats, err
}

func (s *TracingStore) ListDailyRunStats(ctx context.Context, agentID string, days int) ([]*model.DailyRunStats, error) {
	ctx, span := s.start(ctx, "ListDailyRunStats", attribute.String("agent.id", agentID))
	stats, err := s.inner.ListDailyRunStats(ctx, agentID, days)
	finish(span, err)
	return stats, err
}

func (s *TracingStore) GetMonitoringSummary(ctx context.Context, agentID string, since time.Time) (*model.MonitoringSummary, error) {
	ctx, span := s.start(ctx, "GetMonitoringSummary", attribute.String("agent.id", agentID))
	stats, err := s.inner.GetMonitoringSummary(ctx, agentID, since)
	finish(span, err)
	return stats, err
}

func (s *TracingStore) ListMonitoringTimeseries(ctx context.Context, agentID string, since time.Time, interval time.Duration) ([]*model.MonitoringTimeseriesPoint, error) {
	ctx, span := s.start(ctx, "ListMonitoringTimeseries", attribute.String("agent.id", agentID))
	points, err := s.inner.ListMonitoringTimeseries(ctx, agentID, since, interval)
	finish(span, err)
	return points, err
}

func (s *TracingStore) GetMonitoringDiagnostics(ctx context.Context, agentID string, since time.Time, limit int) (*model.MonitoringDiagnostics, error) {
	ctx, span := s.start(ctx, "GetMonitoringDiagnostics", attribute.String("agent.id", agentID))
	diag, err := s.inner.GetMonitoringDiagnostics(ctx, agentID, since, limit)
	finish(span, err)
	return diag, err
}

func (s *TracingStore) GetProviderConfig(ctx context.Context, provider string) (*model.ProviderConfig, error) {
	ctx, span := s.start(ctx, "GetProviderConfig", attribute.String("provider", provider))
	pc, err := s.inner.GetProviderConfig(ctx, provider)
	finish(span, err)
	return pc, err
}

func (s *TracingStore) SetProviderConfig(ctx context.Context, pc *model.ProviderConfig) error {
	ctx, span := s.start(ctx, "SetProviderConfig", attribute.String("provider", pc.Provider))
	err := s.inner.SetProviderConfig(ctx, pc)
	finish(span, err)
	return err
}

func (s *TracingStore) DeleteProviderConfig(ctx context.Context, provider string) error {
	ctx, span := s.start(ctx, "DeleteProviderConfig", attribute.String("provider", provider))
	err := s.inner.DeleteProviderConfig(ctx, provider)
	finish(span, err)
	return err
}

func (s *TracingStore) ListProviderConfigs(ctx context.Context) ([]*model.ProviderConfig, error) {
	ctx, span := s.start(ctx, "ListProviderConfigs")
	configs, err := s.inner.ListProviderConfigs(ctx)
	finish(span, err)
	return configs, err
}

func (s *TracingStore) GetModelConfig(ctx context.Context, provider, mdl string) (*model.ModelConfig, error) {
	ctx, span := s.start(ctx, "GetModelConfig",
		attribute.String("provider", provider),
		attribute.String("model", mdl))
	mc, err := s.inner.GetModelConfig(ctx, provider, mdl)
	finish(span, err)
	return mc, err
}

func (s *TracingStore) SetModelConfig(ctx context.Context, mc *model.ModelConfig) error {
	attrs := []attribute.KeyValue{}
	if mc != nil {
		attrs = append(attrs,
			attribute.String("provider", mc.Provider),
			attribute.String("model", mc.Model))
	}
	ctx, span := s.start(ctx, "SetModelConfig", attrs...)
	err := s.inner.SetModelConfig(ctx, mc)
	finish(span, err)
	return err
}

func (s *TracingStore) DeleteModelConfig(ctx context.Context, provider, mdl string) error {
	ctx, span := s.start(ctx, "DeleteModelConfig",
		attribute.String("provider", provider),
		attribute.String("model", mdl))
	err := s.inner.DeleteModelConfig(ctx, provider, mdl)
	finish(span, err)
	return err
}

func (s *TracingStore) ListModelConfigs(ctx context.Context) ([]*model.ModelConfig, error) {
	ctx, span := s.start(ctx, "ListModelConfigs")
	configs, err := s.inner.ListModelConfigs(ctx)
	finish(span, err)
	return configs, err
}

func (s *TracingStore) GetDefaultModel(ctx context.Context) (*model.DefaultModelRef, error) {
	ctx, span := s.start(ctx, "GetDefaultModel")
	ref, err := s.inner.GetDefaultModel(ctx)
	finish(span, err)
	return ref, err
}

func (s *TracingStore) SetDefaultModel(ctx context.Context, ref *model.DefaultModelRef) error {
	attrs := []attribute.KeyValue{}
	if ref != nil {
		attrs = append(attrs,
			attribute.String("provider", ref.Provider),
			attribute.String("model", ref.Model))
	}
	ctx, span := s.start(ctx, "SetDefaultModel", attrs...)
	err := s.inner.SetDefaultModel(ctx, ref)
	finish(span, err)
	return err
}

func (s *TracingStore) ClearDefaultModel(ctx context.Context) error {
	ctx, span := s.start(ctx, "ClearDefaultModel")
	err := s.inner.ClearDefaultModel(ctx)
	finish(span, err)
	return err
}

func (s *TracingStore) GetOwnerCredential(ctx context.Context) (*model.OwnerCredential, error) {
	ctx, span := s.start(ctx, "GetOwnerCredential")
	cred, err := s.inner.GetOwnerCredential(ctx)
	finish(span, err)
	return cred, err
}

func (s *TracingStore) SetOwnerCredential(ctx context.Context, cred *model.OwnerCredential) error {
	ctx, span := s.start(ctx, "SetOwnerCredential")
	err := s.inner.SetOwnerCredential(ctx, cred)
	finish(span, err)
	return err
}

func (s *TracingStore) GetSetting(ctx context.Context, key string) (string, error) {
	ctx, span := s.start(ctx, "GetSetting", attribute.String("key", key))
	val, err := s.inner.GetSetting(ctx, key)
	finish(span, err)
	return val, err
}

func (s *TracingStore) SetSetting(ctx context.Context, key, value string) error {
	ctx, span := s.start(ctx, "SetSetting", attribute.String("key", key))
	err := s.inner.SetSetting(ctx, key, value)
	finish(span, err)
	return err
}

func (s *TracingStore) Close() error {
	return s.inner.Close()
}
