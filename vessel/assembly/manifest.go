package assembly

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

const (
	WorkspaceBackendMemory     = "memory"
	WorkspaceBackendFilesystem = "filesystem"

	RecallBackendMemory    = "memory"
	RecallBackendWorkspace = "workspace"

	KnowledgeBackendNone      = "none"
	KnowledgeBackendWorkspace = "workspace"

	HistoryKindBuffer = "buffer"
)

// Defaults controls how omitted manifest backend fields are interpreted.
type Defaults struct {
	Workspace   WorkspaceBackend
	Recall      RecallBackend
	Knowledge   KnowledgeBackend
	HistoryKind string
}

// DefaultDefaults returns the package's conservative local defaults.
func DefaultDefaults() Defaults {
	return Defaults{
		Workspace:   MemoryWorkspaceBackend(),
		Recall:      MemoryRecallBackend(),
		Knowledge:   WorkspaceKnowledgeBackend(),
		HistoryKind: HistoryKindBuffer,
	}
}

func normalizeDefaults(in Defaults) Defaults {
	out := DefaultDefaults()
	if in.Workspace != nil {
		out.Workspace = in.Workspace
	}
	if in.Recall != nil {
		out.Recall = in.Recall
	}
	if in.Knowledge != nil {
		out.Knowledge = in.Knowledge
	}
	if strings.TrimSpace(in.HistoryKind) != "" {
		out.HistoryKind = in.HistoryKind
	}
	return out
}

// Manifest is the single-file declarative shape consumed by Build.
type Manifest struct {
	ID          string            `json:"id,omitempty" yaml:"id,omitempty"`
	Workspace   WorkspaceSpec     `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	Recall      *RecallSpec       `json:"recall,omitempty" yaml:"recall,omitempty"`
	Knowledge   *KnowledgeSpec    `json:"knowledge,omitempty" yaml:"knowledge,omitempty"`
	History     *HistorySpec      `json:"history,omitempty" yaml:"history,omitempty"`
	Agents      []AgentSpec       `json:"agents" yaml:"agents"`
	LLM         *LLMSpec          `json:"llm,omitempty" yaml:"llm,omitempty"`
	Resources   spec.Resources    `json:"resources,omitempty" yaml:"resources,omitempty"`
	Kanban      *spec.Kanban      `json:"kanban,omitempty" yaml:"kanban,omitempty"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

type WorkspaceSpec struct {
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty"`
	Root    string `json:"root,omitempty" yaml:"root,omitempty"`
}

type RecallSpec struct {
	Backend       string  `json:"backend,omitempty" yaml:"backend,omitempty"`
	AsyncSemantic bool    `json:"async_semantic,omitempty" yaml:"asyncSemantic,omitempty"`
	Prefix        string  `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Ops           OpsSpec `json:"ops,omitempty" yaml:"ops,omitempty"`
}

type OpsSpec struct {
	Enabled             bool          `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	WorkerID            string        `json:"worker_id,omitempty" yaml:"workerID,omitempty"`
	BatchSize           int           `json:"batch_size,omitempty" yaml:"batchSize,omitempty"`
	IdleInterval        time.Duration `json:"idle_interval,omitempty" yaml:"idleInterval,omitempty"`
	ErrorBackoff        time.Duration `json:"error_backoff,omitempty" yaml:"errorBackoff,omitempty"`
	MaxConcurrentScopes int           `json:"max_concurrent_scopes,omitempty" yaml:"maxConcurrentScopes,omitempty"`
	Scopes              []ScopeSpec   `json:"scopes,omitempty" yaml:"scopes,omitempty"`
}

type ScopeSpec struct {
	RuntimeID string `json:"runtime_id,omitempty" yaml:"runtimeID,omitempty"`
	AgentID   string `json:"agent_id,omitempty" yaml:"agentID,omitempty"`
	UserID    string `json:"user_id,omitempty" yaml:"userID,omitempty"`
}

type KnowledgeSpec struct {
	Backend  string        `json:"backend,omitempty" yaml:"backend,omitempty"`
	Prefix   string        `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Datasets []DatasetSpec `json:"datasets,omitempty" yaml:"datasets,omitempty"`
}

type DatasetSpec struct {
	ID string `json:"id" yaml:"id"`
}

type HistorySpec struct {
	Kind        string `json:"kind,omitempty" yaml:"kind,omitempty"`
	MaxMessages int    `json:"max_messages,omitempty" yaml:"maxMessages,omitempty"`
	TokenBudget int    `json:"token_budget,omitempty" yaml:"tokenBudget,omitempty"`
}

type AgentSpec struct {
	Name          string             `json:"name" yaml:"name"`
	Card          any                `json:"card,omitempty" yaml:"card,omitempty"`
	Engine        string             `json:"engine,omitempty" yaml:"engine,omitempty"`
	EngineKind    string             `json:"engine_kind,omitempty" yaml:"engineKind,omitempty"`
	Tools         []string           `json:"tools,omitempty" yaml:"tools,omitempty"`
	HistoryAccess spec.HistoryAccess `json:"history_access,omitempty" yaml:"historyAccess,omitempty"`
	Sidecar       bool               `json:"sidecar,omitempty" yaml:"sidecar,omitempty"`
	SubscribeTo   string             `json:"subscribe_to,omitempty" yaml:"subscribeTo,omitempty"`
	Dispatcher    bool               `json:"dispatcher,omitempty" yaml:"dispatcher,omitempty"`
	ProducerChain int                `json:"producer_chain,omitempty" yaml:"producerChain,omitempty"`
}

type LLMSpec struct {
	Default string `json:"default,omitempty" yaml:"default,omitempty"`
}

func (m Manifest) Validate() error {
	return m.ValidateWithDefaults(DefaultDefaults())
}

// ValidateWithDefaults validates m using caller-supplied backend defaults for
// omitted backend fields.
func (m Manifest) ValidateWithDefaults(defaults Defaults) error {
	return m.validate(defaults, nil)
}

// ValidateWithCatalog validates m using caller-supplied defaults and registered
// backend factories for explicit custom backend names.
func (m Manifest) ValidateWithCatalog(defaults Defaults, catalog *Catalog) error {
	return m.validate(defaults, catalog)
}

func (m Manifest) validate(defaults Defaults, catalog *Catalog) error {
	defaults = normalizeDefaults(defaults)
	if strings.TrimSpace(m.ID) == "" {
		return errdefs.Validationf("vessel assembly: id is required")
	}
	if len(m.Agents) == 0 {
		return errdefs.Validationf("vessel assembly: agents must contain at least one entry")
	}
	if err := validateWorkspace(m.Workspace, defaults, catalog); err != nil {
		return err
	}
	if m.Recall != nil {
		if err := validateRecall(*m.Recall, defaults, catalog); err != nil {
			return err
		}
	}
	if m.Knowledge != nil {
		if err := validateKnowledge(*m.Knowledge, defaults, catalog); err != nil {
			return err
		}
	}
	if m.History != nil {
		if err := validateHistory(*m.History, defaults); err != nil {
			return err
		}
	}
	vs := m.VesselSpecWithDefaults(defaults)
	return vs.Validate()
}

// VesselSpec projects the assembly manifest into the data-only runtime spec.
func (m Manifest) VesselSpec() spec.Spec {
	return m.VesselSpecWithDefaults(DefaultDefaults())
}

// VesselSpecWithDefaults projects the assembly manifest into a vessel spec
// using caller-supplied defaults for omitted assembly-level fields.
func (m Manifest) VesselSpecWithDefaults(defaults Defaults) spec.Spec {
	defaults = normalizeDefaults(defaults)
	agents := make([]spec.Agent, 0, len(m.Agents))
	for _, a := range m.Agents {
		engineKind := strings.TrimSpace(a.EngineKind)
		if engineKind == "" {
			engineKind = strings.TrimSpace(a.Engine)
		}
		agents = append(agents, spec.Agent{
			Name:          a.Name,
			Card:          a.Card,
			EngineKind:    engineKind,
			Tools:         append([]string(nil), a.Tools...),
			HistoryAccess: a.HistoryAccess,
			Sidecar:       a.Sidecar,
			SubscribeTo:   a.SubscribeTo,
			Dispatcher:    a.Dispatcher,
			ProducerChain: a.ProducerChain,
		})
	}
	var history *spec.History
	if m.History != nil {
		kind := m.History.Kind
		if kind == "" {
			kind = defaults.HistoryKind
		}
		history = &spec.History{
			Kind:        kind,
			MaxMessages: m.History.MaxMessages,
			TokenBudget: m.History.TokenBudget,
		}
	}
	return spec.Spec{
		ID:          m.ID,
		Agents:      agents,
		History:     history,
		Resources:   m.Resources,
		Kanban:      m.Kanban,
		Labels:      copyStringMap(m.Labels),
		Annotations: copyStringMap(m.Annotations),
	}
}

func validateWorkspace(ws WorkspaceSpec, defaults Defaults, catalog *Catalog) error {
	backend, err := resolveWorkspaceBackend(ws.Backend, defaults, catalog)
	if err != nil {
		return err
	}
	return backend.ValidateWorkspace(ws)
}

func validateRecall(r RecallSpec, defaults Defaults, catalog *Catalog) error {
	backend, err := resolveRecallBackend(r.Backend, defaults, catalog)
	if err != nil {
		return err
	}
	if err := backend.ValidateRecall(r); err != nil {
		return err
	}
	if r.Ops.BatchSize < 0 || r.Ops.MaxConcurrentScopes < 0 {
		return errdefs.Validationf("vessel assembly: recall.ops numeric values must be >= 0")
	}
	return nil
}

func validateKnowledge(k KnowledgeSpec, defaults Defaults, catalog *Catalog) error {
	backend, err := resolveKnowledgeBackend(k.Backend, defaults, catalog)
	if err != nil {
		return err
	}
	if err := backend.ValidateKnowledge(k); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for i, ds := range k.Datasets {
		id := strings.TrimSpace(ds.ID)
		if id == "" {
			return errdefs.Validationf("vessel assembly: knowledge.datasets[%d].id is required", i)
		}
		if _, ok := seen[id]; ok {
			return errdefs.Validationf("vessel assembly: knowledge dataset %q is duplicated", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validateHistory(h HistorySpec, defaults Defaults) error {
	switch defaultString(h.Kind, defaults.HistoryKind) {
	case HistoryKindBuffer:
	default:
		return errdefs.Validationf("vessel assembly: unsupported history kind %q", h.Kind)
	}
	if h.MaxMessages < 0 || h.TokenBudget < 0 {
		return errdefs.Validationf("vessel assembly: history numeric values must be >= 0")
	}
	return nil
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
