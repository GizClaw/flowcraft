package dynamic

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

// ToolName is the built-in tool_search tool name. Register it on the
// shared registry with ExposureAlways so the model always has a way to
// discover deferred tools.
const ToolName = "tool_search"

// SearchTool is the model-facing discovery tool. It resolves the
// session catalog from context (see WithCatalog) so one instance can be
// registered on the shared registry for all sessions.
type SearchTool struct{}

var _ sdktool.Tool = SearchTool{}

// NewSearchTool returns the tool_search implementation.
func NewSearchTool() SearchTool { return SearchTool{} }

// RegisterSearchTool registers tool_search on the shared registry and
// marks it ExposureAlways so the model always has a discovery path. The
// catalog itself is resolved per call from context, so one registration
// serves every session.
func RegisterSearchTool(reg *sdktool.Registry, catalog *Catalog) error {
	if reg == nil {
		return errdefs.Validationf("dynamic: RegisterSearchTool: registry is nil")
	}
	if catalog == nil {
		return errdefs.Validationf("dynamic: RegisterSearchTool: catalog is nil")
	}
	return catalog.Register(NewSearchTool(), ExposureAlways)
}

func (SearchTool) Definition() message.Definition {
	return message.DefineSchema(
		ToolName,
		"Search the available tool catalog for tools relevant to the current task. "+
			"Use select to make the named tools visible to the model starting from the next round; "+
			"selected tools remain available for the configured number of rounds.",
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
	Query string      `json:"query"`
	Hits  []SearchHit `json:"hits"`
}

// Execute parses the query, triggers a catalog load, ranks hits, and
// applies the select list.
func (SearchTool) Execute(ctx context.Context, arguments string) (string, error) {
	catalog, ok := FromContext(ctx)
	if !ok {
		return "", errdefs.NotAvailablef(
			"dynamic: %s requires a dynamic catalog on the context", ToolName)
	}
	var args searchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", errdefs.Validationf("dynamic: %s: parse arguments: %v", ToolName, err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", errdefs.Validationf("dynamic: %s: query is required", ToolName)
	}
	hits, err := catalog.Search(ctx, args.Query, args.Limit)
	if err != nil {
		return "", err
	}
	if len(args.Select) > 0 {
		catalog.Select(args.Select...)
	}
	return compactJSON(searchResult{Query: args.Query, Hits: hits})
}
