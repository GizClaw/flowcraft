package claw

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/knowledgenode"
	"github.com/GizClaw/flowcraft/sdk/graph/node/llmnode"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// AgentConfig configures the single agent owned by a Claw.
type AgentConfig struct {
	ID          string                `json:"id,omitempty"`
	Name        string                `json:"name,omitempty"`
	Description string                `json:"description,omitempty"`
	Tools       []string              `json:"tools,omitempty"`
	Graph       graph.GraphDefinition `json:"graph,omitempty"`

	// Fallback fields used only when Graph is omitted.
	SystemPrompt  string   `json:"system_prompt,omitempty"`
	Model         string   `json:"model,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
}

func (c *Claw) buildAgent() agent.Agent {
	return agent.Agent{
		ID:    c.cfg.Agent.ID,
		Tools: append([]string(nil), c.cfg.Agent.Tools...),
		Card: agent.AgentCard{
			Name:        c.cfg.Agent.Name,
			Description: c.cfg.Agent.Description,
			Capabilities: agent.AgentCapabilities{
				Streaming:              true,
				StateTransitionHistory: true,
			},
		},
	}
}

func (c *Claw) buildEngine() (engine.Engine, error) {
	factory := node.NewFactory()
	tools := tool.NewRegistry()
	llmnode.Register(factory, c.resolver, tools)
	knowledgenode.Register(factory, nil)
	scriptnode.Register(factory, scriptnode.Deps{
		ScriptRuntime: jsrt.New(),
		Workspace:     c.ws,
	})

	opts := []runner.Option{}
	if c.cfg.Agent.MaxIterations > 0 {
		opts = append(opts, runner.WithMaxIterations(c.cfg.Agent.MaxIterations))
	}
	r, err := runner.New(&c.cfg.Agent.Graph, factory, opts...)
	if err != nil {
		return nil, fmt.Errorf("claw: build agent graph: %w", err)
	}
	return r, nil
}

func (c *Config) ensureAgentGraph() {
	if c.Agent.Graph.Name != "" {
		return
	}
	model := c.Agent.Model
	if model == "" {
		model = c.modelRef(c.Models.Chat)
	}
	nodeCfg := map[string]any{
		"model":         model,
		"system_prompt": c.Agent.SystemPrompt,
	}
	if c.Agent.Temperature != nil {
		nodeCfg["temperature"] = *c.Agent.Temperature
	}
	c.Agent.Graph = graph.GraphDefinition{
		Name:  c.Agent.ID,
		Entry: "answer",
		Nodes: []graph.NodeDefinition{{
			ID:     "answer",
			Type:   "llm",
			Config: nodeCfg,
		}},
		Edges: []graph.EdgeDefinition{{
			From: "answer",
			To:   graph.END,
		}},
	}
}
