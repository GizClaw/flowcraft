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
)

// MemoryConfig controls Claw's recall integration.
type MemoryConfig struct {
	Enabled          bool       `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Backend          string     `json:"backend,omitempty" yaml:"backend,omitempty"`
	RuntimeID        string     `json:"runtime_id,omitempty" yaml:"runtime_id,omitempty"`
	UserID           string     `json:"user_id,omitempty" yaml:"user_id,omitempty"`
	AgentID          string     `json:"agent_id,omitempty" yaml:"agent_id,omitempty"`
	TopK             int        `json:"top_k,omitempty" yaml:"top_k,omitempty"`
	Graph            bool       `json:"graph,omitempty" yaml:"graph,omitempty"`
	SaveConversation bool       `json:"save_conversation,omitempty" yaml:"save_conversation,omitempty"`
	BBH              bbh.Config `json:"bbh,omitempty" yaml:"bbh,omitempty"`
}

type memoryRuntime struct {
	mem     recall.Memory
	backend *recallworkspace.Backend
	scope   recall.Scope
	topK    int
	save    bool
}

func (c *Claw) buildMemory(ctx context.Context) (*memoryRuntime, error) {
	if !c.cfg.Memory.Enabled {
		return nil, nil
	}

	opts := []recall.Option{recall.WithGraphEnabled(c.cfg.Memory.Graph)}
	backend, err := recallworkspace.New(c.ws, recallworkspace.WithRoot(c.cfg.Workspace.RecallRoot))
	if err != nil {
		return nil, err
	}
	opts = append(opts,
		recall.WithTemporalStore(backend.TemporalStore()),
		recall.WithEvidenceStore(backend.EvidenceStore()),
		recall.WithSideEffectOutbox(backend.SideEffectOutbox()),
		recall.WithAsyncSemanticQueue(backend.AsyncSemanticQueue()),
	)

	switch strings.TrimSpace(c.cfg.Memory.Backend) {
	case "", "memory":
	case "bbh":
		retrievalWS, err := localSubWorkspace(c.ws, c.cfg.Workspace.RetrievalRoot)
		if err != nil {
			return nil, err
		}
		index, err := bbh.New(retrievalWS, bbh.WithConfig(c.cfg.Memory.BBH))
		if err != nil {
			return nil, err
		}
		opts = append(opts, recall.WithRetrievalIndex(index))
	default:
		return nil, fmt.Errorf("claw: unsupported memory backend %q", c.cfg.Memory.Backend)
	}

	if extractor, ok, err := c.extractorModel(ctx); err != nil {
		return nil, err
	} else if ok {
		opts = append(opts, recall.WithLLMExtractor(extractor))
	}
	if emb, ok, err := c.embedder(ctx); err != nil {
		return nil, err
	} else if ok {
		opts = append(opts, recall.WithEmbedder(emb))
	}

	mem, err := recall.New(opts...)
	if err != nil {
		return nil, err
	}
	scope := recall.Scope{
		RuntimeID: c.cfg.Memory.RuntimeID,
		UserID:    c.cfg.Memory.UserID,
		AgentID:   c.cfg.Memory.AgentID,
	}
	if scope.AgentID == "" {
		scope.AgentID = c.cfg.Agent.ID
	}
	return &memoryRuntime{
		mem:     mem,
		backend: backend,
		scope:   scope,
		topK:    c.cfg.Memory.TopK,
		save:    c.cfg.Memory.SaveConversation,
	}, nil
}

func (m *memoryRuntime) recallContext(ctx context.Context, query string) (string, error) {
	if m == nil || m.mem == nil || strings.TrimSpace(query) == "" {
		return "", nil
	}
	hits, err := m.mem.Recall(ctx, m.scope, recall.Query{Text: query, Limit: m.topK})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Relevant memory:\n")
	for i, hit := range hits {
		if i >= m.topK {
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
	if m == nil || m.mem == nil || !m.save {
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
