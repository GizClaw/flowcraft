package realm

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

type mockStore struct {
	apps        map[string]*model.Agent
	kanbanCards map[string][]*model.KanbanCard // runtimeID -> cards
}

func newMockStore() *mockStore {
	return &mockStore{
		apps:        make(map[string]*model.Agent),
		kanbanCards: make(map[string][]*model.KanbanCard),
	}
}

func (s *mockStore) ListAgents(context.Context, model.ListOptions) ([]*model.Agent, *model.ListResult, error) {
	out := make([]*model.Agent, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, a)
	}
	return out, &model.ListResult{}, nil
}

func (s *mockStore) CreateAgent(_ context.Context, a *model.Agent) (*model.Agent, error) {
	s.apps[a.AgentID] = a
	return a, nil
}

func (s *mockStore) GetAgent(_ context.Context, id string) (*model.Agent, error) {
	a, ok := s.apps[id]
	if !ok {
		return nil, errdefs.NotFoundf("agent %q not found", id)
	}
	return a, nil
}

func (s *mockStore) UpdateAgent(_ context.Context, a *model.Agent) (*model.Agent, error) {
	s.apps[a.AgentID] = a
	return a, nil
}

func (s *mockStore) DeleteAgent(_ context.Context, id string) error {
	delete(s.apps, id)
	return nil
}

func (s *mockStore) ListConversations(context.Context, string, model.ListOptions, ...model.ListFilter) ([]*model.Conversation, *model.ListResult, error) {
	return nil, &model.ListResult{}, nil
}

func (s *mockStore) GetConversation(context.Context, string) (*model.Conversation, error) {
	return nil, errdefs.NotFoundf("conversation %q not found", "")
}

func (s *mockStore) CreateConversation(context.Context, *model.Conversation) (*model.Conversation, error) {
	return nil, nil
}

func (s *mockStore) UpdateConversation(context.Context, *model.Conversation) (*model.Conversation, error) {
	return nil, nil
}
func (s *mockStore) SaveWorkflowRun(context.Context, *model.WorkflowRun) error { return nil }
func (s *mockStore) GetWorkflowRun(context.Context, string) (*model.WorkflowRun, error) {
	return nil, errdefs.NotFoundf("workflow_run %q not found", "")
}

func (s *mockStore) ListWorkflowRuns(context.Context, string, model.ListOptions) ([]*model.WorkflowRun, *model.ListResult, error) {
	return nil, &model.ListResult{}, nil
}
func (s *mockStore) SaveKanbanCard(_ context.Context, card *model.KanbanCard) error {
	s.kanbanCards[card.RuntimeID] = append(s.kanbanCards[card.RuntimeID], card)
	return nil
}

func (s *mockStore) ListKanbanCards(_ context.Context, runtimeID string) ([]*model.KanbanCard, error) {
	return s.kanbanCards[runtimeID], nil
}

func (s *mockStore) DeleteKanbanCards(_ context.Context, runtimeID string) error {
	delete(s.kanbanCards, runtimeID)
	return nil
}
func (s *mockStore) ListDatasets(context.Context) ([]*model.Dataset, error) { return nil, nil }
func (s *mockStore) CreateDataset(context.Context, *model.Dataset) (*model.Dataset, error) {
	return nil, nil
}

func (s *mockStore) GetDataset(context.Context, string) (*model.Dataset, error) {
	return nil, errdefs.NotFoundf("dataset %q not found", "")
}
func (s *mockStore) DeleteDataset(context.Context, string) error { return nil }
func (s *mockStore) AddDocument(context.Context, string, string, string) (*model.DatasetDocument, error) {
	return nil, nil
}

func (s *mockStore) GetDocument(context.Context, string, string) (*model.DatasetDocument, error) {
	return nil, nil
}

func (s *mockStore) ListDocuments(context.Context, string) ([]*model.DatasetDocument, error) {
	return nil, nil
}
func (s *mockStore) DeleteDocument(context.Context, string, string) error { return nil }
func (s *mockStore) UpdateDocumentStats(context.Context, string, string, model.DocumentStatsPatch) error {
	return nil
}
func (s *mockStore) UpdateDatasetAbstract(context.Context, string, string) error { return nil }
func (s *mockStore) ListGraphVersions(context.Context, string) ([]*model.GraphVersion, error) {
	return nil, nil
}

func (s *mockStore) GetGraphVersion(context.Context, string, int) (*model.GraphVersion, error) {
	return nil, errdefs.NotFoundf("graph_version %q not found", "")
}
func (s *mockStore) SaveGraphVersion(context.Context, *model.GraphVersion) error { return nil }
func (s *mockStore) PublishGraphVersion(context.Context, string, *model.GraphDefinition, string) (*model.GraphVersion, error) {
	return nil, nil
}

func (s *mockStore) GetLatestPublishedVersion(context.Context, string) (*model.GraphVersion, error) {
	return nil, errdefs.NotFoundf("graph_version %q not found", "")
}

func (s *mockStore) UpdateVersionLock(context.Context, string, string, *model.GraphDefinition) error {
	return nil
}
func (s *mockStore) SaveGraphOperation(context.Context, *model.GraphOperation) error { return nil }
func (s *mockStore) ListGraphOperations(context.Context, string, model.ListOptions) ([]*model.GraphOperation, *model.ListResult, error) {
	return nil, &model.ListResult{}, nil
}

func (s *mockStore) GetStats(context.Context) (*model.StatsOverview, error) {
	return &model.StatsOverview{}, nil
}

func (s *mockStore) GetRunStats(context.Context, string) (*model.RunStats, error) {
	return &model.RunStats{}, nil
}

func (s *mockStore) ListDailyRunStats(context.Context, string, int) ([]*model.DailyRunStats, error) {
	return nil, nil
}

func (s *mockStore) GetMonitoringSummary(context.Context, string, time.Time) (*model.MonitoringSummary, error) {
	return &model.MonitoringSummary{}, nil
}

func (s *mockStore) ListMonitoringTimeseries(context.Context, string, time.Time, time.Duration) ([]*model.MonitoringTimeseriesPoint, error) {
	return nil, nil
}

func (s *mockStore) GetMonitoringDiagnostics(context.Context, string, time.Time, int) (*model.MonitoringDiagnostics, error) {
	return &model.MonitoringDiagnostics{}, nil
}

func (s *mockStore) GetProviderConfig(context.Context, string) (*model.ProviderConfig, error) {
	return nil, errdefs.NotFoundf("provider_config %q not found", "")
}
func (s *mockStore) SetProviderConfig(context.Context, *model.ProviderConfig) error { return nil }
func (s *mockStore) DeleteProviderConfig(context.Context, string) error             { return nil }
func (s *mockStore) ListProviderConfigs(context.Context) ([]*model.ProviderConfig, error) {
	return nil, nil
}
func (s *mockStore) GetModelConfig(context.Context, string, string) (*model.ModelConfig, error) {
	return nil, errdefs.NotFoundf("model_config not found")
}
func (s *mockStore) SetModelConfig(context.Context, *model.ModelConfig) error       { return nil }
func (s *mockStore) DeleteModelConfig(context.Context, string, string) error        { return nil }
func (s *mockStore) ListModelConfigs(context.Context) ([]*model.ModelConfig, error) { return nil, nil }
func (s *mockStore) GetDefaultModel(context.Context) (*model.DefaultModelRef, error) {
	return nil, errdefs.NotFoundf("default model not set")
}
func (s *mockStore) SetDefaultModel(context.Context, *model.DefaultModelRef) error { return nil }
func (s *mockStore) ClearDefaultModel(context.Context) error                       { return nil }
func (s *mockStore) ListTemplates(ctx context.Context) ([]*model.Template, error)  { return nil, nil }
func (s *mockStore) SaveTemplate(ctx context.Context, t *model.Template) error     { return nil }
func (s *mockStore) DeleteTemplate(ctx context.Context, name string) error         { return nil }
func (s *mockStore) GetOwnerCredential(context.Context) (*model.OwnerCredential, error) {
	return nil, errdefs.NotFoundf("not initialized")
}
func (s *mockStore) SetOwnerCredential(context.Context, *model.OwnerCredential) error { return nil }
func (s *mockStore) GetSetting(context.Context, string) (string, error) {
	return "", errdefs.NotFoundf("not found")
}
func (s *mockStore) SetSetting(context.Context, string, string) error { return nil }
func (s *mockStore) Close() error                                     { return nil }

func mockResult(answer string) *workflow.Result {
	return &workflow.Result{
		Status:   workflow.StatusCompleted,
		Messages: []sdkmodel.Message{sdkmodel.NewTextMessage(sdkmodel.RoleAssistant, answer)},
		State:    make(map[string]any),
	}
}

func mockResultWithRunID(answer, runID string) *workflow.Result {
	r := mockResult(answer)
	r.State["run_id"] = runID
	return r
}
