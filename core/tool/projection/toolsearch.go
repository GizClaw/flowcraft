package projection

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// ToolName is the built-in tool_search tool name. A projection registers it with
// ExposureAlways, so the model always has a discovery path.
const ToolName = "tool_search"

// SearchTool is the model-facing discovery tool: it ranks the catalog against a query,
// loads the hits, and adds them to the session's discovery pool so they appear in the
// next round's definitions.
//
// It declares no capability. Everything it needs is the session's projection, which
// travels in [tool.Binding.View]; a tool that reached for the catalog directly would
// bypass the policy that decided what this session may discover.
type SearchTool struct{}

var _ tool.Tool = SearchTool{}

// NewSearchTool returns the tool_search implementation.
func NewSearchTool() SearchTool { return SearchTool{} }

func (SearchTool) Definition() message.ToolDefinition {
	return message.DefineSchema(
		ToolName,
		"Search the tool catalog for tools relevant to the current task. "+
			"Matching tools are loaded and exposed to the model starting from the next round "+
			"when they fit that round's tool budget; over-budget matches come back in failed "+
			"with reason visible_budget. Exposed tools stay available while they are used "+
			"and are evicted after idle rounds or when the discovery budget is full.",
		message.ToolProperty("query", "string",
			"natural-language or keyword query describing the capability to find"),
		message.ToolPropertyWithDefault("limit", "integer",
			"maximum number of hits to return and expose; capped at the discovery pool size",
			defaultSearchLimit),
	).Required("query").Build()
}

// Needs reports nothing: discovery reads the binding, not the process.
func (SearchTool) Needs() []tool.Need { return nil }

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

// minHitDescriptionBytes is the smallest excerpt worth returning: below it the model
// learns nothing from the text, and a bare ellipsis is worse than no description at all.
const minHitDescriptionBytes = 96

// hitOverheadBytes is what one hit costs besides its name and description: the field
// names, the punctuation and a float64 score (~18 bytes marshalled). The real floor is
// ~51; the constant is generous because over-reserving costs a few description bytes and
// under-reserving costs the belt loop a re-marshal.
const hitOverheadBytes = 64

// Execute parses the query, ranks hits, and exposes the top results for the following
// rounds through the session's discovery pool.
func (SearchTool) Execute(ctx context.Context, b tool.Binding, arguments string) (message.Content, error) {
	view, ok := b.View.(*View)
	if !ok {
		return message.Content{}, errdefs.NotAvailablef(
			"tool: %s requires this session's discovery projection", ToolName)
	}
	var args searchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return message.Content{}, errdefs.Validationf("tool: %s: parse arguments: %w", ToolName, err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return message.Content{}, errdefs.Validationf("tool: %s: query is required", ToolName)
	}
	hits, err := view.Search(ctx, args.Query, args.Limit)
	if err != nil {
		return message.Content{}, err
	}
	loaded := make([]string, 0, len(hits))
	var failed []searchFailure
	for _, hit := range hits {
		// Exposed tools must be loaded: the next round shows the real definition, never
		// the LazyTool placeholder. A hit that cannot load is reported instead of
		// silently skipped.
		if err := view.EnsureLoaded(ctx, hit.Name); err != nil {
			failed = append(failed, searchFailure{Name: hit.Name, Reason: "load_failed"})
			continue
		}
		loaded = append(loaded, hit.Name)
	}
	outcome := view.Discover(loaded...)
	exposed := make([]string, 0, len(outcome.Results))
	for _, result := range outcome.Results {
		if result.Exposed {
			exposed = append(exposed, result.Name)
			continue
		}
		if result.Reason != "" {
			// "unknown" included: a catalog that changed under the search is exactly the
			// case where the model must see the name it cannot get.
			failed = append(failed, searchFailure{Name: result.Name, Reason: result.Reason})
		}
	}
	answer := searchResult{
		Query:   args.Query,
		Hits:    hits,
		Exposed: exposed,
		Failed:  failed,
		Evicted: outcome.Evicted,
	}
	// The hits are the only unbounded part of the answer: descriptions are third-party
	// text and the caller controls how many come back.
	answer = fitSearchAnswer(answer, view.searchBudgetBytes())
	encoded, err := compactJSON(answer)
	if err != nil {
		return message.Content{}, err
	}
	// The result is a JSON object: it rides as structured data, not as prose, so
	// redaction and result limiting treat it like any other part.
	return message.NewJSONContent([]byte(encoded))
}

// fitSearchAnswer trims hit descriptions until the marshalled answer fits budget.
//
// Names, scores and every trimmed description are measured through the encoder that will
// build the answer, not estimated: a description full of quotes and backslashes costs
// more inside JSON than as a Go string, which is exactly the case a len() comparison gets
// wrong. The loop at the end is the belt for an answer whose names alone do not fit — it
// strips the lowest-scoring descriptions first, and only then drops hits from the tail,
// because a hit the model cannot see is worse than one whose text was shortened.
func fitSearchAnswer(res searchResult, budget int64) searchResult {
	if len(res.Hits) == 0 || budget <= 0 {
		return res
	}
	envelope := res
	envelope.Hits = nil
	reserved := int64(marshalledLen(envelope))
	for _, hit := range res.Hits {
		reserved += int64(marshalledLen(hit.Name)) + hitOverheadBytes
	}
	share := (budget - reserved) / int64(len(res.Hits))
	for i := range res.Hits {
		if share < minHitDescriptionBytes {
			// Nothing useful fits one hit; the answer keeps the names, which are what the
			// model acts on, and drops the text.
			res.Hits[i].Description = ""
			continue
		}
		res.Hits[i].Description = fitDescription(res.Hits[i].Description, share)
	}
	for int64(marshalledLen(res)) > budget {
		trimmed := false
		for i := len(res.Hits) - 1; i >= 0; i-- {
			if res.Hits[i].Description != "" {
				res.Hits[i].Description = ""
				trimmed = true
				break
			}
		}
		if trimmed {
			continue
		}
		if len(res.Hits) == 0 {
			break
		}
		res.Hits = res.Hits[:len(res.Hits)-1]
	}
	return res
}

// fitDescription shortens text to the largest prefix — with an ellipsis — whose encoded
// size fits budget. It returns empty when not even a truncated hint fits.
func fitDescription(text string, budget int64) string {
	if text == "" {
		return ""
	}
	if int64(marshalledLen(text)) <= budget {
		return text
	}
	const suffix = "…"
	runes := []rune(text)
	best := ""
	lo, hi := 0, len(runes)
	for lo <= hi {
		mid := (lo + hi) / 2
		candidate := string(runes[:mid]) + suffix
		if int64(marshalledLen(candidate)) <= budget {
			best = candidate
			lo = mid + 1
			continue
		}
		hi = mid - 1
	}
	return best
}

// marshalledLen is the encoded size of one value, as the encoder that builds the answer
// produces it.
func marshalledLen(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

func compactJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
