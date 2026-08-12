package tool

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// ToolName is the built-in tool_search tool name. The assembly
// registers it with ExposureAlways when dynamic injection is enabled,
// so the model always has a discovery path.
const ToolName = "tool_search"

// SearchTool is the model-facing discovery tool. It resolves the
// session from context (see WithSession) so one instance can be
// registered on the shared registry for all sessions.
type SearchTool struct{}

var _ Tool = SearchTool{}

// NewSearchTool returns the tool_search implementation.
func NewSearchTool() SearchTool { return SearchTool{} }

func (SearchTool) Definition() message.ToolDefinition {
	return message.DefineSchema(
		ToolName,
		"Search the available tool catalog for tools relevant to the current task. "+
			"Use select to make the named tools visible to the model starting from the next round; "+
			"selected tools are loaded immediately so the next round sees their real schemas, "+
			"and remain available for the configured number of rounds.",
		message.ToolProperty("query", "string",
			"natural-language or keyword query describing the capability to find"),
		message.ToolPropertyWithDefault("limit", "integer",
			"maximum number of hits to return", defaultSearchLimit),
		message.ToolArrayProperty("select",
			"tool names from the hits to make visible for upcoming rounds",
			message.Items("string")),
	).Required("query").Build()
}

type searchArgs struct {
	Query  string   `json:"query"`
	Limit  int      `json:"limit,omitempty"`
	Select []string `json:"select,omitempty"`
}

type searchResult struct {
	Query    string      `json:"query"`
	Hits     []SearchHit `json:"hits"`
	Selected []string    `json:"selected,omitempty"`
}

// Execute parses the query, ranks hits, and applies the select list.
func (SearchTool) Execute(ctx context.Context, arguments string) (string, error) {
	session, ok := SessionFromContext(ctx)
	if !ok {
		return "", errdefs.NotAvailablef(
			"tool: %s requires a session on the context", ToolName)
	}
	var args searchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", errdefs.Validationf("tool: %s: parse arguments: %v", ToolName, err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", errdefs.Validationf("tool: %s: query is required", ToolName)
	}
	hits, err := session.Search(ctx, args.Query, args.Limit)
	if err != nil {
		return "", err
	}
	selected := make([]string, 0, len(args.Select))
	for _, name := range args.Select {
		// Selected tools must be loaded: round N+1 shows the real
		// definition, never the LazyTool placeholder. A tool that
		// cannot load is simply not selected.
		if err := session.EnsureLoaded(ctx, name); err != nil {
			continue
		}
		session.Select(name)
		selected = append(selected, name)
	}
	return compactJSON(searchResult{Query: args.Query, Hits: hits, Selected: selected})
}

func compactJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
