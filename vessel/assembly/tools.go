package assembly

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/knowledge"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

const (
	ToolRecallSave      = "recall.save"
	ToolRecallSearch    = "recall.search"
	ToolKnowledgeSearch = "knowledge.search"
	ToolKnowledgePut    = "knowledge.put"
)

func buildToolRegistry(ctx context.Context, m Manifest, cat *Catalog, ws WorkspaceHandle, mem recall.Memory, svc *knowledge.Service) (*tool.Registry, error) {
	registry := tool.NewRegistry()
	if mem != nil {
		registry.Register(newRecallSaveTool(m.ID, mem))
		registry.Register(newRecallSearchTool(m.ID, mem))
	}
	if svc != nil {
		registry.Register(newKnowledgeSearchTool(svc))
		registry.Register(newKnowledgePutTool(svc))
	}
	if cat != nil {
		deps := ToolDeps{
			Manifest:  m,
			Workspace: ws,
			Recall:    mem,
			Knowledge: svc,
		}
		for name, factory := range cat.toolFactories() {
			t, err := factory(ctx, deps)
			if err != nil {
				return nil, fmt.Errorf("vessel assembly: build tool %q: %w", name, err)
			}
			if t == nil {
				return nil, errdefs.Validationf("vessel assembly: tool factory %q returned nil", name)
			}
			registry.Register(t)
		}
	}
	return registry, nil
}

type recallSaveTool struct {
	runtimeID string
	mem       recall.Memory
}

func newRecallSaveTool(runtimeID string, mem recall.Memory) tool.Tool {
	return &recallSaveTool{runtimeID: runtimeID, mem: mem}
}

func (t *recallSaveTool) Definition() model.ToolDefinition {
	return tool.DefineSchema(
		ToolRecallSave,
		"Save a durable recall fact for the current user/runtime scope.",
		tool.Property("content", "string", "Fact content to remember."),
		tool.Property("kind", "string", "Fact kind: event, state, preference, procedure, relation, plan, note."),
		tool.Property("subject", "string", "Optional structured subject."),
		tool.Property("predicate", "string", "Optional structured predicate."),
		tool.Property("object", "string", "Optional structured object."),
		tool.Property("runtime_id", "string", "Optional runtime scope. Defaults to manifest id."),
		tool.Property("user_id", "string", "Optional user scope. Defaults to default."),
		tool.Property("agent_id", "string", "Optional agent metadata."),
		tool.ArrayProperty("entities", "Optional entity names.", map[string]any{"type": "string"}),
	).Required("content").Build()
}

func (t *recallSaveTool) Execute(ctx context.Context, arguments string) (string, error) {
	var p struct {
		ID        string   `json:"id"`
		Content   string   `json:"content"`
		Kind      string   `json:"kind"`
		Subject   string   `json:"subject"`
		Predicate string   `json:"predicate"`
		Object    string   `json:"object"`
		RuntimeID string   `json:"runtime_id"`
		UserID    string   `json:"user_id"`
		AgentID   string   `json:"agent_id"`
		Entities  []string `json:"entities"`
		Observed  string   `json:"observed_at"`
	}
	if err := json.Unmarshal([]byte(arguments), &p); err != nil {
		return "", fmt.Errorf("recall.save: parse args: %w", err)
	}
	if p.Content == "" {
		return "", errdefs.Validationf("recall.save: content is required")
	}
	scope := toolScope(t.runtimeID, p.RuntimeID, p.UserID, p.AgentID)
	observed := time.Now().UTC()
	if p.Observed != "" {
		parsed, err := time.Parse(time.RFC3339, p.Observed)
		if err != nil {
			return "", errdefs.Validationf("recall.save: observed_at must be RFC3339: %v", err)
		}
		observed = parsed.UTC()
	}
	kind := recall.FactKind(p.Kind)
	if kind == "" {
		kind = recall.FactNote
	}
	id := p.ID
	if id == "" {
		id = fmt.Sprintf("fact-%d", observed.UnixNano())
	}
	res, err := t.mem.Save(ctx, scope, recall.SaveRequest{
		Facts: []recall.TemporalFact{{
			ID:         id,
			Scope:      scope,
			Kind:       kind,
			Content:    p.Content,
			Subject:    p.Subject,
			Predicate:  p.Predicate,
			Object:     p.Object,
			Entities:   append([]string(nil), p.Entities...),
			ObservedAt: observed,
			Confidence: 1,
		}},
		ObservedAt: observed,
	})
	if err != nil {
		return "", err
	}
	return marshalJSON(map[string]any{"fact_ids": res.FactIDs}), nil
}

type recallSearchTool struct {
	runtimeID string
	mem       recall.Memory
}

func newRecallSearchTool(runtimeID string, mem recall.Memory) tool.Tool {
	return &recallSearchTool{runtimeID: runtimeID, mem: mem}
}

func (t *recallSearchTool) Definition() model.ToolDefinition {
	return tool.DefineSchema(
		ToolRecallSearch,
		"Search durable recall facts.",
		tool.Property("query", "string", "Search query."),
		tool.Property("runtime_id", "string", "Optional runtime scope. Defaults to manifest id."),
		tool.Property("user_id", "string", "Optional user scope. Defaults to default."),
		tool.Property("agent_id", "string", "Optional agent metadata."),
		tool.Property("limit", "integer", "Maximum number of hits. Defaults to 5."),
		tool.ArrayProperty("entities", "Optional entity filters/hints.", map[string]any{"type": "string"}),
	).Required("query").Build()
}

func (t *recallSearchTool) Execute(ctx context.Context, arguments string) (string, error) {
	var p struct {
		Query     string   `json:"query"`
		RuntimeID string   `json:"runtime_id"`
		UserID    string   `json:"user_id"`
		AgentID   string   `json:"agent_id"`
		Limit     int      `json:"limit"`
		Entities  []string `json:"entities"`
	}
	if err := json.Unmarshal([]byte(arguments), &p); err != nil {
		return "", fmt.Errorf("recall.search: parse args: %w", err)
	}
	if p.Query == "" {
		return "", errdefs.Validationf("recall.search: query is required")
	}
	if p.Limit <= 0 {
		p.Limit = 5
	}
	hits, err := t.mem.Recall(ctx, toolScope(t.runtimeID, p.RuntimeID, p.UserID, p.AgentID), recall.Query{
		Text:     p.Query,
		Entities: append([]string(nil), p.Entities...),
		Limit:    p.Limit,
	})
	if err != nil {
		return "", err
	}
	return marshalJSON(hits), nil
}

type knowledgeSearchTool struct{ svc *knowledge.Service }

func newKnowledgeSearchTool(svc *knowledge.Service) tool.Tool { return &knowledgeSearchTool{svc: svc} }

func (t *knowledgeSearchTool) Definition() model.ToolDefinition {
	return tool.DefineSchema(
		ToolKnowledgeSearch,
		"Search the assembled knowledge base.",
		tool.Property("query", "string", "Search query."),
		tool.Property("scope", "string", `"single" or "all" (default "all").`),
		tool.Property("dataset_id", "string", "Dataset id. Required when scope=single."),
		tool.Property("mode", "string", `"bm25", "vector", or "hybrid" (default "bm25").`),
		tool.Property("layer", "string", `"L0", "L1", or "L2" (default "L2").`),
		tool.Property("top_k", "integer", "Maximum number of hits. Defaults to 5."),
	).Required("query").Build()
}

func (t *knowledgeSearchTool) Execute(ctx context.Context, arguments string) (string, error) {
	var p struct {
		Query     string  `json:"query"`
		Scope     string  `json:"scope"`
		DatasetID string  `json:"dataset_id"`
		Mode      string  `json:"mode"`
		Layer     string  `json:"layer"`
		TopK      int     `json:"top_k"`
		Threshold float64 `json:"threshold"`
	}
	if err := json.Unmarshal([]byte(arguments), &p); err != nil {
		return "", fmt.Errorf("knowledge.search: parse args: %w", err)
	}
	q := knowledge.Query{
		Text:      p.Query,
		DatasetID: p.DatasetID,
		Mode:      knowledge.Mode(p.Mode),
		Layer:     knowledge.Layer(p.Layer),
		TopK:      p.TopK,
		Threshold: p.Threshold,
	}
	switch p.Scope {
	case "", "all":
		q.Scope = knowledge.ScopeAllDatasets
	case "single":
		q.Scope = knowledge.ScopeSingleDataset
	default:
		return "", errdefs.Validationf("knowledge.search: invalid scope %q", p.Scope)
	}
	res, err := t.svc.Search(ctx, q)
	if err != nil {
		return "", err
	}
	if res == nil {
		return "[]", nil
	}
	return marshalJSON(res.Hits), nil
}

type knowledgePutTool struct{ svc *knowledge.Service }

func newKnowledgePutTool(svc *knowledge.Service) tool.Tool { return &knowledgePutTool{svc: svc} }

func (t *knowledgePutTool) Definition() model.ToolDefinition {
	return tool.DefineSchema(
		ToolKnowledgePut,
		"Persist a document into the assembled knowledge base.",
		tool.Property("dataset_id", "string", `Target dataset (default "default").`),
		tool.Property("name", "string", "Document name."),
		tool.Property("content", "string", "Document content."),
	).Required("name", "content").Build()
}

func (t *knowledgePutTool) Execute(ctx context.Context, arguments string) (string, error) {
	var p struct {
		DatasetID string `json:"dataset_id"`
		Name      string `json:"name"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal([]byte(arguments), &p); err != nil {
		return "", fmt.Errorf("knowledge.put: parse args: %w", err)
	}
	if p.DatasetID == "" {
		p.DatasetID = "default"
	}
	if err := t.svc.PutDocument(ctx, p.DatasetID, p.Name, p.Content); err != nil {
		return "", err
	}
	doc, err := t.svc.GetDocument(ctx, p.DatasetID, p.Name)
	if err != nil {
		return "", err
	}
	version := uint64(0)
	if doc != nil {
		version = doc.Version
	}
	return marshalJSON(map[string]any{
		"status":     "ok",
		"dataset_id": p.DatasetID,
		"name":       p.Name,
		"version":    version,
	}), nil
}

func toolScope(defaultRuntimeID, runtimeID, userID, agentID string) recall.Scope {
	if runtimeID == "" {
		runtimeID = defaultRuntimeID
	}
	if userID == "" {
		userID = "default"
	}
	return recall.Scope{RuntimeID: runtimeID, UserID: userID, AgentID: agentID}
}

func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal failed"}`
	}
	return string(data)
}
