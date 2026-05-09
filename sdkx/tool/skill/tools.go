// Package skill exposes Agent Skills discovery as LLM-callable tools.
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	sdkskill "github.com/GizClaw/flowcraft/sdk/skill"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

const defaultSearchLimit = 5

// Tool provides skill search and progressive disclosure through a
// single action-based tool.
type Tool struct {
	Catalog   sdkskill.Catalog
	Whitelist []string
	Limit     int
}

// New creates a skill meta-tool backed by catalog.
func New(catalog sdkskill.Catalog, opts ...Option) *Tool {
	t := &Tool{
		Catalog: catalog,
		Limit:   defaultSearchLimit,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

// Option configures Tool.
type Option func(*Tool)

// WithWhitelist limits which skills this tool exposes.
func WithWhitelist(names ...string) Option {
	return func(t *Tool) {
		t.Whitelist = append([]string(nil), names...)
	}
}

// WithLimit sets the default search limit.
func WithLimit(limit int) Option {
	return func(t *Tool) {
		if limit > 0 {
			t.Limit = limit
		}
	}
}

func (t *Tool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name: "skill",
		Description: "Discover and load Agent Skills. Skills are reusable procedures and operating guides. " +
			"Use action=search before specialized tasks to find relevant skills, then action=info to load the full guide before acting. " +
			"Supported actions: search, info.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "Operation to perform.",
					"enum":        []string{"search", "info"},
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Search keywords or user intent. Required for action=search.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Skill name. Required for action=info.",
				},
			},
			"required": []string{"action"},
		},
	}
}

func (t *Tool) Execute(ctx context.Context, arguments string) (string, error) {
	if t == nil || t.Catalog == nil {
		return "", fmt.Errorf("skill: catalog is nil")
	}
	var args struct {
		Action string `json:"action"`
		Query  string `json:"query"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("skill: parse args: %w", err)
	}
	switch args.Action {
	case "search":
		return t.search(ctx, args.Query)
	case "info":
		return t.info(ctx, args.Name)
	default:
		return "", fmt.Errorf("skill: invalid action %q", args.Action)
	}
}

func (t *Tool) search(ctx context.Context, query string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("skill: action=search requires query")
	}
	results, err := t.Catalog.Search(ctx, query, sdkskill.SearchOptions{
		Whitelist: t.whitelist(ctx),
		Limit:     t.Limit,
	})
	if err != nil {
		return "", err
	}
	if results == nil {
		results = []sdkskill.Summary{}
	}
	data, err := json.Marshal(results)
	if err != nil {
		return "", fmt.Errorf("skill: marshal search results: %w", err)
	}
	return string(data), nil
}

func (t *Tool) info(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("skill: action=info requires name")
	}
	if err := checkWhitelist(t.whitelist(ctx), name); err != nil {
		return "", err
	}
	sk, err := t.Catalog.Load(ctx, name)
	if err != nil {
		return "", err
	}
	if sk.Gating != nil && !sk.Gating.Available {
		return fmt.Sprintf("[WARNING] This skill is currently unavailable. Missing dependencies: %s\n\n%s",
			strings.Join(sk.MissingDeps, ", "), sk.Body), nil
	}
	return sk.Body, nil
}

func (t *Tool) whitelist(ctx context.Context) []string {
	if len(t.Whitelist) > 0 {
		return append([]string(nil), t.Whitelist...)
	}
	return sdkskill.WhitelistFrom(ctx)
}

func checkWhitelist(whitelist []string, name string) error {
	if len(whitelist) == 0 {
		return nil
	}
	for _, allowed := range whitelist {
		if allowed == name {
			return nil
		}
	}
	return fmt.Errorf("skill %q not in whitelist", name)
}

func (t *Tool) Metadata() tool.ToolMeta {
	return tool.ToolMeta{MutatesState: false}
}

var _ tool.Tool = (*Tool)(nil)
var _ tool.ToolMetadata = (*Tool)(nil)
