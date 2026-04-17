package metatool

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func buildAgentQueryTools(deps *Deps) []tool.Tool {
	return []tool.Tool{
		tool.FuncTool(
			tool.DefineSchema("agent", "Query agents. Two modes: (1) omit agent_id to list all user-created agents with ID, name, type, and description; (2) provide agent_id to get full agent details including config and graph definition",
				tool.Property("agent_id", "string", "Optional. The agent ID. Omit to list all agents; provide to get details for that agent"),
			).Build(),
			func(ctx context.Context, args string) (string, error) {
				store, err := storeRequired(deps)
				if err != nil {
					return "", err
				}
				var p struct {
					AgentID string `json:"agent_id"`
				}
				_ = json.Unmarshal([]byte(args), &p)
				if p.AgentID != "" {
					agent, err := store.GetAgent(ctx, p.AgentID)
					if err != nil {
						return "", err
					}
					return jsonResult(agent)
				}
				agents, _, err := store.ListAgents(ctx, model.ListOptions{Limit: 100})
				if err != nil {
					return "", err
				}
				type summary struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					Type        string `json:"type"`
					Description string `json:"description,omitempty"`
					HasGraph    bool   `json:"has_graph"`
				}
				out := make([]summary, 0, len(agents))
				for _, a := range agents {
					if a.Type == model.AgentTypeCoPilot {
						continue
					}
					out = append(out, summary{a.AgentID, a.Name, string(a.Type), a.Description, a.StrategyDef.AsGraph() != nil})
				}
				return jsonResult(out)
			},
		),
	}
}
