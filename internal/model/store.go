package model

import (
	"context"
	"time"
)

// DefaultPageLimit is the default number of items returned per page.
const DefaultPageLimit = 50

// MaxPageLimit is the upper bound for page sizes.
const MaxPageLimit = 100

// ListOptions configures pagination for list queries.
type ListOptions struct {
	Limit  int    // max items to return (0 = default 50, capped at 100)
	Cursor string // opaque cursor for next page (empty = first page)
}

// EffectiveLimit returns the limit to use, applying defaults and caps.
func (o ListOptions) EffectiveLimit() int {
	if o.Limit <= 0 {
		return DefaultPageLimit
	}
	if o.Limit > MaxPageLimit {
		return MaxPageLimit
	}
	return o.Limit
}

// ListResult wraps a paginated list response with cursor metadata.
type ListResult struct {
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ListFilter is a functional option for filtering list queries.
type ListFilter func(*listFilterOpts)

type listFilterOpts struct {
	RuntimeID string
}

// WithListRuntimeID filters list queries by runtime ID.
func WithListRuntimeID(runtimeID string) ListFilter {
	return func(o *listFilterOpts) { o.RuntimeID = runtimeID }
}

// ApplyFilters returns the effective filter options.
func ApplyFilters(filters []ListFilter) (runtimeID string) {
	opts := &listFilterOpts{}
	for _, f := range filters {
		f(opts)
	}
	return opts.RuntimeID
}

// Store is the unified persistence interface for the FlowCraft platform.
type Store interface {
	// Agent operations.
	ListAgents(ctx context.Context, opts ListOptions) ([]*Agent, *ListResult, error)
	CreateAgent(ctx context.Context, a *Agent) (*Agent, error)
	GetAgent(ctx context.Context, id string) (*Agent, error)
	UpdateAgent(ctx context.Context, a *Agent) (*Agent, error)
	DeleteAgent(ctx context.Context, id string) error

	// Conversation operations.
	ListConversations(ctx context.Context, agentID string, opts ListOptions, filters ...ListFilter) ([]*Conversation, *ListResult, error)
	GetConversation(ctx context.Context, id string) (*Conversation, error)
	CreateConversation(ctx context.Context, conv *Conversation) (*Conversation, error)
	UpdateConversation(ctx context.Context, conv *Conversation) (*Conversation, error)

	// Message persistence is no longer in the SQL store after R5: the
	// ChatProjector materialises messages from chat.message.sent
	// envelopes. Consumers that need history (gateway notifications,
	// memory buffers) read the projector instead.

	// Workflow run operations.
	SaveWorkflowRun(ctx context.Context, run *WorkflowRun) error
	GetWorkflowRun(ctx context.Context, id string) (*WorkflowRun, error)
	ListWorkflowRuns(ctx context.Context, agentID string, opts ListOptions) ([]*WorkflowRun, *ListResult, error)

	// Kanban card operations.
	SaveKanbanCard(ctx context.Context, card *KanbanCard) error
	ListKanbanCards(ctx context.Context, runtimeID string) ([]*KanbanCard, error)
	DeleteKanbanCards(ctx context.Context, runtimeID string) error

	// Dataset operations.
	ListDatasets(ctx context.Context) ([]*Dataset, error)
	CreateDataset(ctx context.Context, ds *Dataset) (*Dataset, error)
	GetDataset(ctx context.Context, id string) (*Dataset, error)
	DeleteDataset(ctx context.Context, id string) error
	AddDocument(ctx context.Context, datasetID, name, content string) (*DatasetDocument, error)
	GetDocument(ctx context.Context, datasetID, docID string) (*DatasetDocument, error)
	ListDocuments(ctx context.Context, datasetID string) ([]*DatasetDocument, error)
	DeleteDocument(ctx context.Context, datasetID, docID string) error
	UpdateDocumentStats(ctx context.Context, datasetID, docID string, patch DocumentStatsPatch) error
	UpdateDatasetAbstract(ctx context.Context, datasetID, abstract string) error

	// Graph version operations.
	ListGraphVersions(ctx context.Context, agentID string) ([]*GraphVersion, error)
	GetGraphVersion(ctx context.Context, agentID string, version int) (*GraphVersion, error)
	SaveGraphVersion(ctx context.Context, gv *GraphVersion) error
	PublishGraphVersion(ctx context.Context, agentID string, def *GraphDefinition, description string) (*GraphVersion, error)
	GetLatestPublishedVersion(ctx context.Context, agentID string) (*GraphVersion, error)
	UpdateVersionLock(ctx context.Context, agentID string, expectedChecksum string, newDef *GraphDefinition) error

	// Graph operation history.
	SaveGraphOperation(ctx context.Context, op *GraphOperation) error
	ListGraphOperations(ctx context.Context, agentID string, opts ListOptions) ([]*GraphOperation, *ListResult, error)

	// Stats operations.
	GetStats(ctx context.Context) (*StatsOverview, error)
	GetRunStats(ctx context.Context, agentID string) (*RunStats, error)
	ListDailyRunStats(ctx context.Context, agentID string, days int) ([]*DailyRunStats, error)
	GetMonitoringSummary(ctx context.Context, agentID string, since time.Time) (*MonitoringSummary, error)
	ListMonitoringTimeseries(ctx context.Context, agentID string, since time.Time, interval time.Duration) ([]*MonitoringTimeseriesPoint, error)
	GetMonitoringDiagnostics(ctx context.Context, agentID string, since time.Time, limit int) (*MonitoringDiagnostics, error)

	// Template operations.
	ListTemplates(ctx context.Context) ([]*Template, error)
	SaveTemplate(ctx context.Context, t *Template) error
	DeleteTemplate(ctx context.Context, name string) error

	// Provider config operations.
	GetProviderConfig(ctx context.Context, provider string) (*ProviderConfig, error)
	SetProviderConfig(ctx context.Context, pc *ProviderConfig) error
	DeleteProviderConfig(ctx context.Context, provider string) error
	ListProviderConfigs(ctx context.Context) ([]*ProviderConfig, error)

	// Per-model config operations. Together these satisfy the SDK's
	// llm.ModelConfigStore optional interface so the resolver picks
	// up per-model caps and extra overrides.
	GetModelConfig(ctx context.Context, provider, model string) (*ModelConfig, error)
	SetModelConfig(ctx context.Context, mc *ModelConfig) error
	DeleteModelConfig(ctx context.Context, provider, model string) error
	ListModelConfigs(ctx context.Context) ([]*ModelConfig, error)

	// Default-model pointer operations. Together these satisfy the
	// SDK's llm.DefaultModelStore optional interface.
	GetDefaultModel(ctx context.Context) (*DefaultModelRef, error)
	SetDefaultModel(ctx context.Context, ref *DefaultModelRef) error
	ClearDefaultModel(ctx context.Context) error

	// Owner credential operations (single-row).
	GetOwnerCredential(ctx context.Context) (*OwnerCredential, error)
	SetOwnerCredential(ctx context.Context, cred *OwnerCredential) error

	// Key-value settings operations.
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error

	// Close releases store resources.
	Close() error
}
