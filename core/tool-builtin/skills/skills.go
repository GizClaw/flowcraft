// Package skills implements the built-in skill tools: search a discovered
// catalog and load one skill's full instructions.
//
// The catalog is data the host produced with skill.Discover; this package adds
// only the two actions a model can take on it. Installation and authoring are
// host concerns and are not tools here.
package skills

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/skill"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// Tool names, as the model sees them.
const (
	SearchName = "skill_search"
	ReadName   = "skill_read"
)

const defaultSearchLimit = 10

// maxSearchLimit bounds one search; a variable so tests can exercise the clamp.
var maxSearchLimit = 50

// Tool is the skill tool group over the host's catalog and the tree it was
// discovered from: search reads the index, read loads a body through the same tree.
//
// The index is owned by the caller (the runtime), not built here: one catalog, one
// index, so what the tools search is exactly what the host reports through
// Runtime.Skills().
type Tool struct {
	tree  *fsx.Tree
	index *skill.Index
}

// New builds the tools over an existing catalog.
func New(tree *fsx.Tree, index *skill.Index) (*Tool, error) {
	if tree == nil {
		return nil, errdefs.Validationf("skills: an fsx tree is required")
	}
	if index == nil {
		return nil, errdefs.Validationf("skills: a catalog index is required")
	}
	return &Tool{tree: tree, index: index}, nil
}

// Tools returns the skill tools sharing this catalog.
// LazyTools is empty: skill documents are already discovered at build time.
func (t *Tool) LazyTools() []tool.LazyTool { return nil }

func (t *Tool) Tools() []tool.Tool {
	return []tool.Tool{&searchTool{tool: t}, &readTool{tool: t}}
}

type searchTool struct{ tool *Tool }

var _ tool.Tool = (*searchTool)(nil)

func (t *searchTool) Definition() message.ToolDefinition {
	return definition(
		SearchName,
		"Search the discovered skills catalog and return matching metadata "+
			"(name, description, path). Use "+ReadName+" to load a skill's full "+
			"instructions. An empty query lists the first skills.",
		map[string]any{
			"query": map[string]any{"type": "string", "description": "Search text; empty lists the catalog."},
			"limit": map[string]any{
				"type":        "integer",
				"default":     defaultSearchLimit,
				"minimum":     1,
				"maximum":     maxSearchLimit,
				"description": "Maximum results.",
			},
		},
	)
}

func (t *searchTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

type searchHit struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Path        string  `json:"path"`
	Score       float64 `json:"score,omitempty"`
}

func (t *searchTool) Execute(_ context.Context, arguments string) (message.Content, error) {
	var params struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := tool.DecodeArgs(SearchName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	hits := make([]searchHit, 0, limit)
	if strings.TrimSpace(params.Query) == "" {
		for _, meta := range t.tool.index.All() {
			if len(hits) >= limit {
				break
			}
			hits = append(hits, searchHit{Name: meta.Name, Description: meta.Description, Path: meta.Path})
		}
	} else {
		for _, scored := range t.tool.index.RankScored(params.Query, limit, 0) {
			hits = append(hits, searchHit{
				Name:        scored.Skill.Name,
				Description: scored.Skill.Description,
				Path:        scored.Skill.Path,
				Score:       scored.Score,
			})
		}
	}
	payload, err := json.Marshal(hits)
	if err != nil {
		return message.Content{}, errdefs.Internalf("%s: encode result: %v", SearchName, err)
	}
	return message.NewTextContent(string(payload)), nil
}

type readTool struct{ tool *Tool }

var _ tool.Tool = (*readTool)(nil)

func (t *readTool) Definition() message.ToolDefinition {
	return definition(
		ReadName,
		"Load the full SKILL.md instructions of one discovered skill.",
		map[string]any{
			"name": map[string]any{"type": "string", "description": "The skill name."},
		},
		"name",
	)
}

func (t *readTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

func (t *readTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := tool.DecodeArgs(ReadName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	meta, ok := t.tool.index.ByName(params.Name)
	if !ok {
		return message.Content{}, errdefs.Validationf("%s: no skill named %q", ReadName, params.Name)
	}
	body, err := t.tool.tree.Read(ctx, meta.Path)
	if err != nil {
		return message.Content{}, err
	}
	return message.NewTextContent(
		"# Skill: " + meta.Name + " (file: " + meta.Path + ")\n\n" + string(body)), nil
}

// definition builds a closed object schema.
func definition(name, description string, properties map[string]any, required ...string) message.ToolDefinition {
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		panic(fmt.Sprintf("skills: schema for %s: %v", name, err))
	}
	return message.ToolDefinition{Name: name, Description: description, InputSchema: raw}
}
