package claw

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	recallworkspace "github.com/GizClaw/flowcraft/memory/recall/store/workspace"
	"github.com/GizClaw/flowcraft/memory/retrieval/bbh"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// MemoryConfig controls Claw's recall integration.
type MemoryConfig struct {
	Enabled   bool                  `json:"enabled,omitempty"`
	Scope     MemoryScopeConfig     `json:"scope,omitempty"`
	Write     MemoryWriteConfig     `json:"write,omitempty"`
	Extract   MemoryExtractConfig   `json:"extract,omitempty"`
	Recall    MemoryRecallConfig    `json:"recall,omitempty"`
	Retrieval MemoryRetrievalConfig `json:"retrieval,omitempty"`
	Embedding MemoryEmbeddingConfig `json:"embedding,omitempty"`

	// Deprecated flat fields kept for old local configs.
	Backend          string     `json:"backend,omitempty"`
	RuntimeID        string     `json:"runtime_id,omitempty"`
	UserID           string     `json:"user_id,omitempty"`
	AgentID          string     `json:"agent_id,omitempty"`
	TopK             int        `json:"top_k,omitempty"`
	Graph            bool       `json:"graph,omitempty"`
	SaveConversation bool       `json:"save_conversation,omitempty"`
	BBH              bbh.Config `json:"bbh,omitempty"`
}

type MemoryScopeConfig struct {
	RuntimeID string `json:"runtime_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

type MemoryWriteConfig struct {
	SaveConversation bool   `json:"save_conversation,omitempty"`
	Mode             string `json:"mode,omitempty"`
	Tier             string `json:"tier,omitempty"`
}

type MemoryExtractConfig struct {
	Enabled      bool                     `json:"enabled,omitempty"`
	Model        string                   `json:"model,omitempty"`
	Mode         recall.LLMExtractionMode `json:"mode,omitempty"`
	SystemPrompt string                   `json:"system_prompt,omitempty"`
	Temperature  *float64                 `json:"temperature,omitempty"`
	SchemaName   string                   `json:"schema_name,omitempty"`
}

type MemoryRecallConfig struct {
	Enabled        bool `json:"enabled,omitempty"`
	TopK           int  `json:"top_k,omitempty"`
	GraphEnabled   bool `json:"graph_enabled,omitempty"`
	IncludeRetired bool `json:"include_retired,omitempty"`
}

type MemoryRetrievalConfig struct {
	Backend string     `json:"backend,omitempty"`
	BBH     bbh.Config `json:"bbh,omitempty"`
}

type MemoryEmbeddingConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Model   string `json:"model,omitempty"`
}

type memoryRuntime struct {
	mem     recall.Memory
	side    recall.SideEffectProcessor
	backend *recallworkspace.Backend
	scope   recall.Scope
	cfg     MemoryConfig
}

func (c *Claw) buildMemory(ctx context.Context) (*memoryRuntime, error) {
	if !c.cfg.Memory.Enabled {
		return nil, nil
	}

	memCfg := c.cfg.Memory.normalized(c.cfg.Agent.ID)
	opts := []recall.Option{recall.WithGraphEnabled(memCfg.Recall.GraphEnabled)}
	memoryWS := sdkworkspace.Sub(c.ws, c.cfg.Workspace.MemoryRoot)
	backend, err := recallworkspace.New(memoryWS)
	if err != nil {
		return nil, err
	}
	opts = append(opts,
		recall.WithTemporalStore(backend.TemporalStore()),
		recall.WithEvidenceStore(backend.EvidenceStore()),
		recall.WithSideEffectOutbox(backend.SideEffectOutbox()),
		recall.WithAsyncSemanticQueue(backend.AsyncSemanticQueue()),
	)

	switch strings.TrimSpace(memCfg.Retrieval.Backend) {
	case "", "memory":
	case "bbh":
		index, err := bbh.New(memoryWS, bbh.WithConfig(memCfg.Retrieval.BBH))
		if err != nil {
			return nil, err
		}
		opts = append(opts, recall.WithRetrievalIndex(index))
	default:
		return nil, fmt.Errorf("claw: unsupported memory backend %q", memCfg.Retrieval.Backend)
	}

	if memCfg.Extract.Enabled && memCfg.Extract.Model != "" {
		extractor, err := c.model(ctx, memCfg.Extract.Model)
		if err != nil {
			return nil, err
		}
		extractOpts := []recall.LLMExtractorOption{}
		if memCfg.Extract.Mode != "" {
			extractOpts = append(extractOpts, recall.WithLLMExtractionMode(memCfg.Extract.Mode))
		}
		if memCfg.Extract.SystemPrompt != "" {
			extractOpts = append(extractOpts, recall.WithLLMExtractorSystemPrompt(memCfg.Extract.SystemPrompt))
		}
		if memCfg.Extract.Temperature != nil {
			extractOpts = append(extractOpts, recall.WithLLMExtractorTemperature(*memCfg.Extract.Temperature))
		}
		if memCfg.Extract.SchemaName != "" {
			extractOpts = append(extractOpts, recall.WithLLMExtractorSchemaName(memCfg.Extract.SchemaName))
		}
		opts = append(opts, recall.WithLLMExtractor(extractor, extractOpts...))
	}
	if memCfg.Embedding.Enabled && memCfg.Embedding.Model != "" {
		emb, err := c.embedderByName(ctx, memCfg.Embedding.Model)
		if err != nil {
			return nil, err
		}
		opts = append(opts, recall.WithEmbedder(emb))
	}

	mem, err := recall.New(opts...)
	if err != nil {
		return nil, err
	}
	side, _ := recall.NewSideEffectProcessor(mem)
	scope := recall.Scope{
		RuntimeID: memCfg.Scope.RuntimeID,
		UserID:    memCfg.Scope.UserID,
		AgentID:   memCfg.Scope.AgentID,
	}
	return &memoryRuntime{
		mem:     mem,
		side:    side,
		backend: backend,
		scope:   scope,
		cfg:     memCfg,
	}, nil
}

func (m *memoryRuntime) recallContext(ctx context.Context, query string) (string, error) {
	if m == nil || m.mem == nil || strings.TrimSpace(query) == "" {
		return "", nil
	}
	if !m.cfg.Recall.Enabled {
		return "", nil
	}
	hits, err := m.mem.Recall(ctx, m.scope, recall.Query{
		Text:           query,
		Limit:          m.cfg.Recall.TopK,
		IncludeRetired: m.cfg.Recall.IncludeRetired,
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Relevant memory:\n")
	for i, hit := range hits {
		if i >= m.cfg.Recall.TopK {
			break
		}
		content := strings.TrimSpace(hit.Fact.Content)
		if content == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", content)
	}
	return b.String(), nil
}

func (m *memoryRuntime) saveTurn(ctx context.Context, contextID, userText string, assistant model.Message) error {
	if m == nil || m.mem == nil || !m.cfg.Write.SaveConversation {
		return nil
	}
	now := time.Now()
	turns := []recall.TurnContext{
		{
			ID:        contextID + ":user:" + now.Format("20060102150405.000000000"),
			SessionID: contextID,
			Role:      "user",
			Speaker:   "user",
			Time:      now,
			Text:      userText,
		},
		{
			ID:        contextID + ":assistant:" + now.Format("20060102150405.000000000"),
			SessionID: contextID,
			Role:      "assistant",
			Speaker:   "assistant",
			Time:      now,
			Text:      assistant.Content(),
		},
	}
	_, err := m.mem.Save(ctx, m.scope, recall.SaveRequest{
		Turns:      turns,
		ObservedAt: now,
		Tier:       m.cfg.Write.Tier,
		Mode:       parseWriteMode(m.cfg.Write.Mode),
	})
	if err != nil {
		return err
	}
	return m.drainSideEffects(ctx)
}

func (m *memoryRuntime) drainSideEffects(ctx context.Context) error {
	if m == nil || m.side == nil {
		return nil
	}
	_, err := m.side.ProcessSideEffects(ctx, recall.SideEffectProcessOptions{
		WorkerID: "claw",
		Scope:    m.scope,
		Limit:    100,
	})
	return err
}

func (m *memoryRuntime) close() error {
	if m == nil {
		return nil
	}
	if m.mem != nil {
		return m.mem.Close()
	}
	return nil
}

func (m MemoryConfig) normalized(agentID string) MemoryConfig {
	out := m
	if out.Scope.RuntimeID == "" {
		out.Scope.RuntimeID = firstNonEmpty(out.RuntimeID, "claw")
	}
	if out.Scope.UserID == "" {
		out.Scope.UserID = firstNonEmpty(out.UserID, "local")
	}
	if out.Scope.AgentID == "" {
		out.Scope.AgentID = firstNonEmpty(out.AgentID, agentID)
	}
	if out.Retrieval.Backend == "" {
		out.Retrieval.Backend = firstNonEmpty(out.Backend, "bbh")
	}
	if out.Retrieval.BBH == (bbh.Config{}) {
		out.Retrieval.BBH = out.BBH
	}
	if out.Recall.TopK <= 0 {
		if out.TopK > 0 {
			out.Recall.TopK = out.TopK
		} else {
			out.Recall.TopK = 5
		}
	}
	if out.Graph {
		out.Recall.GraphEnabled = true
	}
	if out.Recall.Enabled == false {
		out.Recall.Enabled = true
	}
	if out.SaveConversation {
		out.Write.SaveConversation = true
	}
	return out
}

func parseWriteMode(mode string) recall.WriteMode {
	switch strings.TrimSpace(mode) {
	case "async_semantic":
		return recall.WriteModeAsyncSemantic
	default:
		return recall.WriteModeSync
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
