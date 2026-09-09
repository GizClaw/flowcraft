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
		"Search the tool catalog for tools relevant to the current task. "+
			"Matching tools are loaded and exposed to the model starting from the next round; "+
			"exposed tools stay available while they are used and are evicted after idle rounds "+
			"or when the discovery budget is full.",
		message.ToolProperty("query", "string",
			"natural-language or keyword query describing the capability to find"),
		message.ToolPropertyWithDefault("limit", "integer",
			"maximum number of hits to return and expose", defaultSearchLimit),
	).Required("query").Build()
}

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type searchResult struct {
	Query   string          `json:"query"`
	Hits    []SearchHit     `json:"hits"`
	Exposed []string        `json:"exposed,omitempty"`
	Failed  []searchFailure `json:"failed,omitempty"`
	Evicted []string        `json:"evicted,omitempty"`
}

type searchFailure struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Execute parses the query, ranks hits, and exposes the top results for
// the following rounds through the session discovery pool.
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
	loaded := make([]string, 0, len(hits))
	var failed []searchFailure
	for _, hit := range hits {
		// Exposed tools must be loaded: round N+1 shows the real
		// definition, never the LazyTool placeholder. A hit that
		// cannot load is reported instead of silently skipped.
		if err := session.EnsureLoaded(ctx, hit.Name); err != nil {
			failed = append(failed, searchFailure{Name: hit.Name, Reason: "load_failed"})
			continue
		}
		loaded = append(loaded, hit.Name)
	}
	outcome := session.Discover(loaded...)
	exposed := make([]string, 0, len(outcome.Results))
	for _, result := range outcome.Results {
		if result.Exposed {
			exposed = append(exposed, result.Name)
			continue
		}
		if result.Reason != "" && result.Reason != "unknown" {
			failed = append(failed, searchFailure{Name: result.Name, Reason: result.Reason})
		}
	}
	return compactJSON(searchResult{
		Query:   args.Query,
		Hits:    hits,
		Exposed: exposed,
		Failed:  failed,
		Evicted: outcome.Evicted,
	})
}

func compactJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
