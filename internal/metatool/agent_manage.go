package metatool

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func buildAgentManageTools(deps *Deps) []tool.Tool {
	return []tool.Tool{
		tool.FuncTool(
			tool.DefineSchema("agent_create", "Create a new workflow agent",
				tool.Property("name", "string", "Agent name"),
				tool.Property("description", "string", "Agent description"),
			).Required("name").Build(),
			func(ctx context.Context, args string) (string, error) {
				store, err := storeRequired(deps)
				if err != nil {
					return "", err
				}
				var p struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				}
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return "", err
				}
				a := &model.Agent{
					Name:        p.Name,
					Description: p.Description,
					Type:        model.AgentTypeWorkflow,
				}
				created, err := store.CreateAgent(ctx, a)
				if err != nil {
					return "", err
				}
				return jsonResult(map[string]string{
					"status": "created",
					"id":     created.AgentID,
					"name":   created.Name,
					"type":   string(created.Type),
				})
			},
		),
	}
}
