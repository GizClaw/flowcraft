// Package model defines the shared domain types for the FlowCraft platform.
package model

import (
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/rs/xid"
)

type (
	GraphDefinition = graph.GraphDefinition
	NodeDefinition  = graph.NodeDefinition
	EdgeDefinition  = graph.EdgeDefinition

	// Role is an alias for sdk/model.Role — single source of truth.
	Role = sdkmodel.Role
	// TokenUsage is an alias for sdk/model.TokenUsage — single source of truth.
	TokenUsage = sdkmodel.TokenUsage

	// Memory configuration aliases — single source of truth in SDK.

	MemoryConfig   = memory.Config
	LosslessConfig = memory.LosslessConfig
	LongTermConfig = memory.LongTermConfig
)

// AgentType defines the agent type.
type AgentType string

const (
	AgentTypeWorkflow AgentType = "workflow"
	AgentTypeCoPilot  AgentType = "copilot"

	// CoPilotAgentID is the well-known fixed ID for the CoPilot Dispatcher agent.
	CoPilotAgentID = "copilot"

	// InputKeyCallback is the Request.Inputs key carrying the kanban callback card ID.
	InputKeyCallback = "__callback"
)

// StrategyDef is a generic carrier for an agent's execution definition.
// Kind identifies the strategy type ("graph", "script", "remote", etc.);
// Spec holds the kind-specific definition payload as raw JSON.
type StrategyDef struct {
	Kind string          `json:"kind"`
	Spec json.RawMessage `json:"spec,omitempty"`
}

// NewGraphStrategy wraps a GraphDefinition into a StrategyDef.
func NewGraphStrategy(def *GraphDefinition) *StrategyDef {
	spec, _ := json.Marshal(def)
	return &StrategyDef{Kind: "graph", Spec: spec}
}

// AsGraph extracts the GraphDefinition from a graph-kind StrategyDef.
// Returns nil if the kind is not "graph" or the spec is invalid.
func (s *StrategyDef) AsGraph() *GraphDefinition {
	if s == nil || s.Kind != "graph" {
		return nil
	}
	var gd GraphDefinition
	if err := json.Unmarshal(s.Spec, &gd); err != nil {
		return nil
	}
	return &gd
}

// Agent represents an AI agent. Implements [workflow.Agent].
type Agent struct {
	AgentID      string           `json:"id"`
	Name         string           `json:"name"`
	Type         AgentType        `json:"type"`
	Description  string           `json:"description,omitempty"`
	Config       AgentConfig      `json:"config"`
	StrategyDef  *StrategyDef     `json:"strategy,omitempty"`
	InputSchema  *variable.Schema `json:"input_schema,omitempty"`
	OutputSchema *variable.Schema `json:"output_schema,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`

	resolveStrategy func(*Agent) workflow.Strategy
}

func (a *Agent) ID() string { return a.AgentID }
func (a *Agent) Card() workflow.AgentCard {
	return workflow.AgentCard{Name: a.Name, Description: a.Description}
}

func (a *Agent) Strategy() workflow.Strategy {
	if a.resolveStrategy != nil {
		return a.resolveStrategy(a)
	}
	return nil
}

// SetStrategyResolver injects a strategy resolution function.
func (a *Agent) SetStrategyResolver(fn func(*Agent) workflow.Strategy) {
	a.resolveStrategy = fn
}

func (a *Agent) Tools() []string { return a.Config.SkillWhitelist }

var _ workflow.Agent = (*Agent)(nil)

// ChannelBinding describes an external channel bound to an Agent.
type ChannelBinding struct {
	Type   string         `json:"type"`   // "slack" | "dingtalk" | "feishu"
	Config map[string]any `json:"config"` // channel-specific credentials
}

// AgentConfig holds agent-level configuration.
type AgentConfig struct {
	SubAgents      []string            `json:"sub_agents,omitempty"`
	Memory         MemoryConfig        `json:"memory"`
	SkillWhitelist []string            `json:"skill_whitelist,omitempty"`
	Parallel       *ParallelConfig     `json:"parallel,omitempty"`
	Notification   *NotificationConfig `json:"notification,omitempty"`
	Channels       []ChannelBinding    `json:"channels,omitempty"`
	Schedules      []Schedule          `json:"schedules,omitempty"`
}

// Schedule defines a cron-based recurring execution rule for an Agent.
type Schedule struct {
	ID       string `json:"id"`
	Cron     string `json:"cron"`
	Query    string `json:"query"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Source   string `json:"source,omitempty"`
}

// ParallelConfig controls parallel Fork/Join behaviour.
type ParallelConfig struct {
	Enabled       *bool  `json:"enabled,omitempty"`
	MaxBranches   int    `json:"max_branches,omitempty"`
	MaxNesting    int    `json:"max_nesting,omitempty"`
	MergeStrategy string `json:"merge_strategy,omitempty"`
}

// NotificationConfig controls async workflow completion notification.
type NotificationConfig struct {
	Enabled     bool   `json:"enabled"`
	Granularity string `json:"granularity,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
}

// ConversationStatus represents the state of a conversation.
type ConversationStatus string

const (
	ConvActive   ConversationStatus = "active"
	ConvClosed   ConversationStatus = "closed"
	ConvArchived ConversationStatus = "archived"
)

// Conversation is the container for messages.
type Conversation struct {
	ID        string             `json:"id"`
	AgentID   string             `json:"agent_id"`
	RuntimeID string             `json:"runtime_id,omitempty"`
	Variables map[string]any     `json:"variables,omitempty"`
	Status    ConversationStatus `json:"status"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

const (
	RoleSystem    Role = sdkmodel.RoleSystem
	RoleUser      Role = sdkmodel.RoleUser
	RoleAssistant Role = sdkmodel.RoleAssistant
	RoleTool      Role = sdkmodel.RoleTool
)

// Message is a conversation message persisted in the store.
// Embeds [sdkmodel.Message] (Role + Parts) for zero-conversion multimodal support.
type Message struct {
	sdkmodel.Message           // Role + Parts (multimodal, native)
	ID               string    `json:"id"`
	ConversationID   string    `json:"conversation_id"`
	TokenCount       int64     `json:"token_count,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// FromMessages wraps runtime messages into persistent model.Messages
// with generated IDs, ConversationID, and monotonic timestamps.
func FromMessages(conversationID string, msgs []sdkmodel.Message) []*Message {
	result := make([]*Message, 0, len(msgs))
	baseTime := time.Now()
	for i, m := range msgs {
		result = append(result, &Message{
			Message:        m,
			ID:             xid.New().String(),
			ConversationID: conversationID,
			CreatedAt:      baseTime.Add(time.Duration(i) * time.Microsecond),
		})
	}
	return result
}

// WorkflowRun records a single workflow execution.
type WorkflowRun struct {
	ID             string         `json:"id"`
	AgentID        string         `json:"agent_id"`
	ActorID        string         `json:"actor_id,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Input          string         `json:"input"`
	Output         string         `json:"output"`
	Inputs         map[string]any `json:"inputs,omitempty"`
	Outputs        map[string]any `json:"outputs,omitempty"`
	Status         string         `json:"status"`
	Usage          *TokenUsage    `json:"usage,omitempty"`
	ElapsedMs      int64          `json:"elapsed_ms"`
	CreatedAt      time.Time      `json:"created_at"`
}

// ExecutionEvent records a graph execution event.
type ExecutionEvent struct {
	ID        string         `json:"id"`
	RunID     string         `json:"run_id"`
	NodeID    string         `json:"node_id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Dataset is a knowledge base data set.
type Dataset struct {
	ID            string    `json:"id"`
	AgentID       string    `json:"agent_id,omitempty"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	DocumentCount int       `json:"document_count"`
	L0Abstract    string    `json:"l0_abstract,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ProcessingStatus tracks the semantic processing state of a document.
type ProcessingStatus string

const (
	ProcessingPending   ProcessingStatus = "pending"
	ProcessingRunning   ProcessingStatus = "processing"
	ProcessingCompleted ProcessingStatus = "completed"
	ProcessingFailed    ProcessingStatus = "failed"
)

// DatasetDocument is a single document within a dataset.
type DatasetDocument struct {
	ID               string           `json:"id"`
	DatasetID        string           `json:"dataset_id"`
	Name             string           `json:"name"`
	Content          string           `json:"content"`
	ChunkCount       int              `json:"chunk_count"`
	L0Abstract       string           `json:"l0_abstract,omitempty"`
	L1Overview       string           `json:"l1_overview,omitempty"`
	ProcessingStatus ProcessingStatus `json:"processing_status,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
}

// GraphVersion represents a versioned snapshot of a GraphDefinition.
type GraphVersion struct {
	ID          string           `json:"id"`
	AgentID     string           `json:"agent_id"`
	Version     int              `json:"version"`
	GraphDef    *GraphDefinition `json:"graph_definition"`
	Description string           `json:"description,omitempty"`
	Checksum    string           `json:"checksum"`
	CreatedBy   string           `json:"created_by,omitempty"`
	PublishedAt *time.Time       `json:"published_at,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
}

// GraphOperationType represents the type of graph operation.
type GraphOperationType string

const (
	GraphOperationAddNode     GraphOperationType = "add_node"
	GraphOperationRemoveNode  GraphOperationType = "remove_node"
	GraphOperationUpdateNode  GraphOperationType = "update_node"
	GraphOperationAddEdge     GraphOperationType = "add_edge"
	GraphOperationRemoveEdge  GraphOperationType = "remove_edge"
	GraphOperationUpdateGraph GraphOperationType = "update_graph"
	GraphOperationSetEntry    GraphOperationType = "set_entry"
)

// GraphOperation represents a single graph modification operation.
type GraphOperation struct {
	ID          string             `json:"id"`
	AgentID     string             `json:"agent_id"`
	Type        GraphOperationType `json:"type"`
	NodeID      string             `json:"node_id,omitempty"`
	EdgeFrom    string             `json:"edge_from,omitempty"`
	EdgeTo      string             `json:"edge_to,omitempty"`
	GraphDef    *GraphDefinition   `json:"graph_def,omitempty"`
	Description string             `json:"description,omitempty"`
	CreatedBy   string             `json:"created_by,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
}

// KanbanCard represents a persisted kanban card.
type KanbanCard struct {
	kanban.KanbanCardModel
	Meta map[string]any `json:"meta,omitempty"`
}

// Template represents a persisted graph template.
type Template struct {
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Parameters  string    `json:"parameters"`
	GraphDef    string    `json:"graph_def"`
	IsBuiltin   bool      `json:"is_builtin"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// StatsOverview provides high-level platform statistics.
type StatsOverview struct {
	TotalAgents        int `json:"total_agents"`
	TotalConversations int `json:"total_conversations"`
	TotalRuns          int `json:"total_runs"`
}

// RunStats provides aggregated run statistics for an agent.
type RunStats struct {
	TotalRuns     int     `json:"total_runs"`
	CompletedRuns int     `json:"completed_runs"`
	FailedRuns    int     `json:"failed_runs"`
	AvgElapsedMs  float64 `json:"avg_elapsed_ms"`
}

// DailyRunStats provides per-day aggregated run statistics for time-series charts.
type DailyRunStats struct {
	Date         string  `json:"date"`
	Count        int     `json:"count"`
	AvgElapsedMs float64 `json:"avg_elapsed_ms"`
}

// OwnerCredential is the single-row owner account stored in SQLite.
type OwnerCredential struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ProviderConfig is an alias for the SDK provider configuration type.
type ProviderConfig = llm.ProviderConfig

// ModelConfig is an alias for the SDK per-model overrides type. The
// store satisfies llm.ModelConfigStore by returning *llm.ModelConfig
// directly, which keeps zero conversion at the resolver boundary.
type ModelConfig = llm.ModelConfig

// DefaultModelRef is an alias for the SDK default-model pointer type,
// returned by store.GetDefaultModel to satisfy llm.DefaultModelStore.
type DefaultModelRef = llm.DefaultModelRef
