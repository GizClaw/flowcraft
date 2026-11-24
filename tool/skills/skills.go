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
	"github.com/GizClaw/flowcraft/core/tool/platform"
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

// Tool is the skill tool group.
//
// It holds nothing: the catalog and the file boundary it reads through arrive with each call, so one
// group serves every session. The index is the host's — it is built once per process from the roots
// the deployment configured — which is what makes "what the tools search" and "what the host reports"
// the same catalog rather than two discoveries of it.
type Tool struct{}

// New builds the skill tools.
func New() *Tool { return &Tool{} }

// catalog reads the session's skill index and file boundary out of the call.
func catalog(binding tool.Binding) (*skill.Index, *fsx.Tree, error) {
	index, ok := platform.SkillsSlot.Get(binding.Caps)
	if !ok || index == nil {
		return nil, nil, errdefs.NotAvailablef("skills: this session has no skill catalog")
	}
	tree, err := boundary(binding)
	if err != nil {
		return nil, nil, err
	}
	return index, tree, nil
}

// boundary reads the session's file boundary, which is how a skill body is read: through the same
// handle the rest of the session reads with, not through the os package.
func boundary(binding tool.Binding) (*fsx.Tree, error) {
	tree, ok := platform.TreeSlot.Get(binding.Caps)
	if !ok || tree == nil {
		return nil, errdefs.NotAvailablef("skills: this session has no file boundary")
	}
	return tree, nil
}

// Tools returns the skill tools sharing this catalog.
// LazyTools is empty: skill documents are already discovered at build time.
func (t *Tool) LazyTools() []tool.LazyTool { return nil }

func (t *Tool) Tools() []tool.Tool {
	return []tool.Tool{&searchTool{}, &readTool{}}
}

type searchTool struct{}

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

// Needs declares the catalog and the file boundary this tool reads.
func (*searchTool) Needs() []tool.Need {
	return []tool.Need{platform.SkillsSlot, platform.TreeSlot}
}

func (t *searchTool) Execute(_ context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	index, _, err := catalog(binding)
	if err != nil {
		return message.Content{}, err
	}
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
		for _, meta := range index.All() {
			if len(hits) >= limit {
				break
			}
			hits = append(hits, searchHit{Name: meta.Name, Description: meta.Description, Path: meta.Path})
		}
	} else {
		for _, scored := range index.RankScored(params.Query, limit, 0) {
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

type readTool struct{}

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

// Needs declares the catalog and the file boundary this tool reads.
func (*readTool) Needs() []tool.Need {
	return []tool.Need{platform.SkillsSlot, platform.TreeSlot}
}

func (t *readTool) Execute(ctx context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	index, tree, err := catalog(binding)
	if err != nil {
		return message.Content{}, err
	}
	var params struct {
		Name string `json:"name"`
	}
	if err := tool.DecodeArgs(ReadName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	meta, ok := index.ByName(params.Name)
	if !ok {
		return message.Content{}, errdefs.Validationf("%s: no skill named %q", ReadName, params.Name)
	}
	body, err := tree.Read(ctx, meta.Path)
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
