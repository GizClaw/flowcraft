package metatool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func buildGraphTools(deps *Deps) []tool.Tool {
	schema := tool.DefineSchema("graph",
		"Manage an agent's graph. "+
			"action=get: retrieve the current graph definition. "+
			"action=update: replace the entire graph definition (requires graph_def). "+
			"action=compile: validate the graph and return diagnostics. "+
			"action=publish: publish the current draft as a new version.",
		tool.EnumProperty("action", "string", "Operation to perform", "get", "update", "compile", "publish"),
		tool.Property("agent_id", "string", "The agent ID"),
		tool.ObjectProperty("graph_def", "Complete graph definition to replace the current one (action=update)", map[string]any{
			"name":  map[string]any{"type": "string", "description": "Graph name"},
			"entry": map[string]any{"type": "string", "description": "Entry node ID"},
			"nodes": map[string]any{"type": "array", "description": "Node definitions", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     map[string]any{"type": "string"},
					"type":   map[string]any{"type": "string"},
					"config": map[string]any{"type": "object"},
				},
				"required": []string{"id", "type"},
			}},
			"edges": map[string]any{"type": "array", "description": "Edge definitions", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from":      map[string]any{"type": "string"},
					"to":        map[string]any{"type": "string"},
					"condition": map[string]any{"type": "string"},
				},
				"required": []string{"from", "to"},
			}},
		}),
		tool.Property("version", "integer", "Draft version number to publish, 0 = auto-detect latest draft (action=publish)"),
		tool.Property("description", "string", "Version description (action=publish)"),
	).Required("action", "agent_id").Build()

	return []tool.Tool{
		tool.FuncTool(schema, func(ctx context.Context, args string) (string, error) {
			var p struct {
				Action      string                `json:"action"`
				AgentID     string                `json:"agent_id"`
				GraphDef    model.GraphDefinition `json:"graph_def"`
				Version     int                   `json:"version"`
				Description string                `json:"description"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}

			switch p.Action {
			case "get":
				return graphGet(ctx, deps, p.AgentID)
			case "update":
				return graphUpdate(ctx, deps, p.AgentID, p.GraphDef)
			case "compile":
				return graphCompile(ctx, deps, p.AgentID)
			case "publish":
				return graphPublish(ctx, deps, p.AgentID, p.Version, p.Description)
			default:
				return "", errdefs.Validationf("unknown action %q, expected get|update|compile|publish", p.Action)
			}
		}),
	}
}

func graphGet(ctx context.Context, deps *Deps, agentID string) (string, error) {
	store, err := storeRequired(deps)
	if err != nil {
		return "", err
	}
	agent, err := store.GetAgent(ctx, agentID)
	if err != nil {
		return "", err
	}
	if agent.StrategyDef.AsGraph() == nil {
		return jsonResult(map[string]string{
			"status":  "empty",
			"message": fmt.Sprintf("agent %q exists but has no graph definition yet; use graph with action=update to create one", agentID),
		})
	}
	return jsonResult(agent.StrategyDef.AsGraph())
}

func graphUpdate(ctx context.Context, deps *Deps, agentID string, graphDef model.GraphDefinition) (string, error) {
	store, err := storeRequired(deps)
	if err != nil {
		return "", err
	}
	defer deps.lockAgent(agentID)()
	a, err := store.GetAgent(ctx, agentID)
	if err != nil {
		return "", err
	}
	a.StrategyDef = model.NewGraphStrategy(&graphDef)
	if _, err := store.UpdateAgent(ctx, a); err != nil {
		return "", err
	}
	deps.publishGraphChanged(ctx, agentID)
	deps.saveDraftAfterGraphChange(ctx, agentID, a.StrategyDef.AsGraph())
	return jsonResult(map[string]any{
		"status":     "updated",
		"agent_id":   agentID,
		"entry":      graphDef.Entry,
		"node_count": len(graphDef.Nodes),
		"edge_count": len(graphDef.Edges),
	})
}

func graphCompile(ctx context.Context, deps *Deps, agentID string) (string, error) {
	store, err := storeRequired(deps)
	if err != nil {
		return "", err
	}
	if deps.Compiler == nil {
		return "", errNotAvailable
	}
	agent, err := store.GetAgent(ctx, agentID)
	if err != nil {
		return "", err
	}
	if agent.StrategyDef.AsGraph() == nil {
		return "", errdefs.Validationf("agent has no graph definition")
	}
	compiled, compileErr := deps.Compiler.Compile(agent.StrategyDef.AsGraph())
	if compileErr != nil {
		return jsonResult(map[string]any{
			"success": false,
			"errors":  []string{compileErr.Error()},
		})
	}
	warnings := make([]string, len(compiled.Warnings))
	for i, w := range compiled.Warnings {
		warnings[i] = w.Message
	}
	return jsonResult(map[string]any{
		"success":    true,
		"node_count": compiled.Metadata.NodeCount,
		"edge_count": compiled.Metadata.EdgeCount,
		"warnings":   warnings,
	})
}

func graphPublish(ctx context.Context, deps *Deps, agentID string, ver int, description string) (string, error) {
	if deps == nil || deps.VersionStore == nil {
		return "", errNotAvailable
	}
	targetVersion := ver
	if targetVersion == 0 {
		draft, err := deps.VersionStore.GetDraft(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("no draft found: %w", err)
		}
		targetVersion = draft.Version
	}
	v, err := deps.VersionStore.Publish(ctx, agentID, targetVersion, description)
	if err != nil {
		return "", err
	}
	return jsonResult(v)
}
