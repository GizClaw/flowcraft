package metatool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func buildSchemaTools(deps *Deps) []tool.Tool {
	schema := tool.DefineSchema("schema",
		"Query platform schemas and registry. "+
			"action=node_list: list all available node types. "+
			"action=node_usage: get detailed config/ports for a node type (requires type). "+
			"action=model_list: list configured LLM models. "+
			"action=tool_list: list tools assignable to user agents.",
		tool.EnumProperty("action", "string", "Operation to perform", "node_list", "node_usage", "model_list", "tool_list"),
		tool.Property("type", "string", "Node type name, required when action=node_usage (e.g. llm, router, loopguard)"),
	).Required("action").Build()

	return []tool.Tool{
		tool.FuncTool(schema, func(ctx context.Context, args string) (string, error) {
			var p struct {
				Action string `json:"action"`
				Type   string `json:"type"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}

			switch p.Action {
			case "node_list":
				return schemaNodeList(deps)
			case "node_usage":
				return schemaNodeUsage(deps, p.Type)
			case "model_list":
				return schemaModelList(ctx, deps)
			case "tool_list":
				return schemaToolList(deps)
			default:
				return "", errdefs.Validationf("unknown action %q, expected node_list|node_usage|model_list|tool_list", p.Action)
			}
		}),
	}
}

func schemaNodeList(deps *Deps) (string, error) {
	if deps == nil || deps.SchemaReg == nil {
		return "", errNotAvailable
	}
	all := deps.SchemaReg.All()
	type nodeSummary struct {
		Type        string `json:"type"`
		Label       string `json:"label"`
		Category    string `json:"category"`
		Description string `json:"description"`
	}
	out := make([]nodeSummary, len(all))
	for i, s := range all {
		out[i] = nodeSummary{Type: s.Type, Label: s.Label, Category: s.Category, Description: s.Description}
	}
	return jsonResult(out)
}

func schemaNodeUsage(deps *Deps, nodeType string) (string, error) {
	if deps == nil || deps.SchemaReg == nil {
		return "", errNotAvailable
	}
	if nodeType == "" {
		return "", errdefs.Validationf("type is required for action=node_usage")
	}
	s, ok := deps.SchemaReg.Get(nodeType)
	if !ok {
		return "", fmt.Errorf("unknown node type %q, use schema(action=node_list) to see available types", nodeType)
	}
	type fieldInfo struct {
		Key         string `json:"key"`
		Type        string `json:"type"`
		Required    bool   `json:"required,omitempty"`
		Description string `json:"description,omitempty"`
		Default     any    `json:"default,omitempty"`
	}
	type portInfo struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Required bool   `json:"required,omitempty"`
	}
	fields := make([]fieldInfo, len(s.Fields))
	for i, f := range s.Fields {
		fields[i] = fieldInfo{Key: f.Key, Type: f.Type, Required: f.Required, Description: f.Label, Default: f.DefaultValue}
	}
	inputs := make([]portInfo, len(s.InputPorts))
	for i, p := range s.InputPorts {
		inputs[i] = portInfo{Name: p.Name, Type: p.Type, Required: p.Required}
	}
	outputs := make([]portInfo, len(s.OutputPorts))
	for i, p := range s.OutputPorts {
		outputs[i] = portInfo{Name: p.Name, Type: p.Type, Required: p.Required}
	}
	result := map[string]any{
		"type":         s.Type,
		"label":        s.Label,
		"category":     s.Category,
		"description":  s.Description,
		"config":       fields,
		"input_ports":  inputs,
		"output_ports": outputs,
	}
	if s.Runtime != nil {
		result["runtime"] = s.Runtime
	}
	return jsonResult(result)
}

func schemaModelList(ctx context.Context, deps *Deps) (string, error) {
	store, err := storeRequired(deps)
	if err != nil {
		return "", err
	}
	configs, err := store.ListModelConfigs(ctx)
	if err != nil {
		return "", err
	}

	globalDefault := ""
	if ref, gErr := store.GetDefaultModel(ctx); gErr == nil && ref != nil {
		globalDefault = ref.Provider + "/" + ref.Model
	}

	type modelEntry struct {
		Label     string `json:"label"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		IsDefault bool   `json:"is_default,omitempty"`
	}
	models := make([]modelEntry, 0, len(configs))
	for _, c := range configs {
		key := c.Provider + "/" + c.Model
		models = append(models, modelEntry{
			Label:     key,
			Provider:  c.Provider,
			Model:     c.Model,
			IsDefault: key == globalDefault,
		})
	}
	return jsonResult(map[string]any{
		"models":         models,
		"global_default": globalDefault,
		"hint":           "Use 'provider/model' format in LLM node config. Leave model empty to use the global default.",
	})
}

func schemaToolList(deps *Deps) (string, error) {
	if deps == nil || deps.ToolRegistry == nil {
		return "", errNotAvailable
	}
	type toolEntry struct {
		Name string `json:"name"`
		Desc string `json:"description"`
	}
	var tools []toolEntry
	for _, d := range deps.ToolRegistry.DefinitionsByScope(tool.ScopeAgent) {
		tools = append(tools, toolEntry{Name: d.Name, Desc: d.Description})
	}
	if tools == nil {
		tools = []toolEntry{}
	}
	return jsonResult(map[string]any{
		"tools": tools,
		"hint":  "Add tool names to the LLM node's tool_names array. Only listed tools are injected; empty tool_names means no tool-calling ability.",
	})
}
