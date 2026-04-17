package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// SkillTool provides skill search, info, and call via a single tool with action parameter.
type SkillTool struct {
	Store     *SkillStore
	Executor  *SkillExecutor
	Whitelist []string
}

func (t *SkillTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: "skill",
		Description: "Skill operations. action=search: search skills by keyword, returns list with gating info. " +
			"action=info: get SKILL.md content (usage, commands, examples) — call before executing. " +
			"action=call: execute a skill by name; for doc-only skills returns SKILL.md guide.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "Operation: search | info | call",
					"enum":        []string{"search", "info", "call"},
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Search keywords (required when action=search)",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Skill name (required when action=info or action=call)",
				},
				"args": map[string]any{
					"type":        "string",
					"description": "Arguments to pass to the skill (optional when action=call)",
				},
			},
			"required": []string{"action"},
		},
	}
}

func (t *SkillTool) isAllowed(ctx context.Context, name string) error {
	whitelist := t.Whitelist
	if len(whitelist) == 0 {
		whitelist = SkillWhitelistFrom(ctx)
	}
	return checkWhitelist(ctx, whitelist, name)
}

func (t *SkillTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args struct {
		Action string `json:"action"`
		Query  string `json:"query"`
		Name   string `json:"name"`
		Args   string `json:"args"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("skill: parse args: %w", err)
	}

	switch args.Action {
	case "search":
		return t.executeSearch(ctx, args.Query)
	case "info":
		return t.executeInfo(ctx, args.Name)
	case "call":
		return t.executeCall(ctx, args.Name, args.Args)
	default:
		return "", fmt.Errorf("skill: invalid action %q", args.Action)
	}
}

func (t *SkillTool) executeSearch(ctx context.Context, query string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("skill: action=search requires query")
	}
	whitelist := t.Whitelist
	if len(whitelist) == 0 {
		whitelist = SkillWhitelistFrom(ctx)
	}
	results := t.Store.Search(query, whitelist)

	type skillResult struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags,omitempty"`
		Available   bool     `json:"available"`
		MissingDeps []string `json:"missing_deps,omitempty"`
	}
	var output []skillResult
	for _, r := range results {
		if !t.Store.IsEnabled(r.Name) {
			continue
		}
		output = append(output, skillResult{
			Name:        r.Name,
			Description: r.Description,
			Tags:        r.Tags,
			Available:   r.Gating == nil || r.Gating.Available,
			MissingDeps: gatingDeps(r.Gating),
		})
	}

	data, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("skill: marshal output: %w", err)
	}
	return string(data), nil
}

func (t *SkillTool) executeInfo(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("skill: action=info requires name")
	}
	if err := t.isAllowed(ctx, name); err != nil {
		return "", err
	}
	if !t.Store.IsEnabled(name) {
		return "", fmt.Errorf("skill %q is disabled", name)
	}

	content, ok := t.Store.GetReadme(name)
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}

	meta, metaOK := t.Store.Get(name)
	if metaOK && meta.Gating != nil && !meta.Gating.Available {
		deps := gatingDeps(meta.Gating)
		warning := fmt.Sprintf("[WARNING] This skill is currently unavailable. Missing dependencies: %s\n\n",
			strings.Join(deps, ", "))
		content = warning + content
	}

	return content, nil
}

func (t *SkillTool) executeCall(ctx context.Context, name, skillArgs string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("skill: action=call requires name")
	}
	if err := t.isAllowed(ctx, name); err != nil {
		return "", err
	}
	if !t.Store.IsEnabled(name) {
		return "", fmt.Errorf("skill %q is disabled", name)
	}

	meta, ok := t.Executor.store.Get(name)
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}

	unavailMsg := formatGatingMessage(meta)

	if meta.Entry == "" {
		content, ok := t.Executor.store.GetReadme(name)
		if !ok {
			return "", fmt.Errorf("skill %q SKILL.md not found", name)
		}
		if unavailMsg != "" {
			content = unavailMsg + "\n\n" + content
		}
		return content, nil
	}

	if unavailMsg != "" {
		return "", fmt.Errorf("skill %q is not available: %s", name, unavailMsg)
	}

	return t.Executor.Execute(ctx, name, skillArgs)
}
