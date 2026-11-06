// Package files implements the built-in file tools: read_file, write_file and
// list_dir.
//
// They are tools, not the fsx contract. Every path they accept is absolute and
// the boundary is the fsx root handle: a write outside the declared roots is
// refused by the kernel, not by a string comparison here. Reads are unrestricted
// by the contract, so read_file is bounded by size, not by containment.
package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Tool names, as the model sees them.
const (
	ReadFileName  = "read_file"
	WriteFileName = "write_file"
	ListDirName   = "list_dir"
)

// Tool is the file tool group over one tree.
type Tool struct {
	tree *fsx.Tree
	// images says whether read_file may return an image part. When false the
	// tool refuses an image by name: a model that cannot take image input must
	// not be handed one, and must not be handed mojibake either.
	images bool
	// root is the default for a path the caller omits. fsx has no notion of a
	// root — it resolves against handles — so the host supplies the directory it
	// wants "the workspace" to mean, and the tools say so in their schemas.
	root string
}

// Option configures the file tool group.
type Option func(*Tool)

// WithImages lets read_file return an image part. Enable it only when the model
// declares image input; otherwise the tool refuses images with a reason.
func WithImages(enabled bool) Option {
	return func(t *Tool) { t.images = enabled }
}

// New creates the file tools over tree. root becomes the default path; pass ""
// to make every path explicit.
func New(tree *fsx.Tree, root string, opts ...Option) (*Tool, error) {
	if tree == nil {
		return nil, errdefs.Validationf("files: an fsx tree is required")
	}
	if root != "" && !filepath.IsAbs(root) {
		return nil, errdefs.Validationf("files: default root %q must be absolute", root)
	}
	group := &Tool{tree: tree, root: filepath.Clean(root)}
	for _, opt := range opts {
		opt(group)
	}
	return group, nil
}

// Tools returns the tools sharing this tree.
func (t *Tool) Tools() []tool.Tool {
	return []tool.Tool{
		&readFileTool{tree: t.tree, root: t.root, images: t.images},
		&writeFileTool{tree: t.tree, root: t.root},
		&listDirTool{tree: t.tree, root: t.root},
		&grepTool{tree: t.tree, root: t.root},
		&globTool{tree: t.tree, root: t.root},
		&editFileTool{tree: t.tree, root: t.root},
	}
}

// absolute validates one model-supplied path.
//
// Relative paths are rejected: fsx resolves against a root handle, so a relative
// path would need a working directory this tool does not have. The old
// workspace-relative convention is deliberately gone.
func absolute(toolName, path, fallback string) (string, error) {
	if path == "" {
		path = fallback
	}
	if path == "" {
		return "", errdefs.Validationf("%s: path is required", toolName)
	}
	if !filepath.IsAbs(path) {
		return "", errdefs.Validationf("%s: path %q must be absolute", toolName, path)
	}
	return filepath.Clean(path), nil
}

// Tool names for the search tools.
const (
	GrepName = "grep"
	GlobName = "glob"
)

// errStopWalk ends a tree walk early without being an error: a cap was reached
// and the result says so.
var errStopWalk = errors.New("files: stop walk")

// validationf is the group's error shape: every tool names itself.
func validationf(format string, args ...any) error {
	return errdefs.Validationf(format, args...)
}

// property is one schema field.
type property struct {
	kind string
	desc string
	// def, min and max are optional JSON Schema constraints: a default and a
	// range say the same thing as prose, in a form a provider can enforce.
	def any
	min *int
	max *int
}

func num(v int) *int { return &v }

// definition builds one tool schema. json.Marshal sorts map keys, so the schema
// is deterministic without an ordering pass.
func definition(name, description string, properties map[string]property, required ...string) message.ToolDefinition {
	props := make(map[string]any, len(properties))
	for key, prop := range properties {
		entry := map[string]any{"type": prop.kind}
		if prop.desc != "" {
			entry["description"] = prop.desc
		}
		if prop.def != nil {
			entry["default"] = prop.def
		}
		if prop.min != nil {
			entry["minimum"] = *prop.min
		}
		if prop.max != nil {
			entry["maximum"] = *prop.max
		}
		props[key] = entry
	}
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		panic(fmt.Sprintf("files: schema for %s: %v", name, err))
	}
	return message.ToolDefinition{Name: name, Description: description, InputSchema: raw}
}

// result encodes one tool answer.
func result(toolName string, value any) (message.Content, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return message.Content{}, errdefs.Internalf("%s: encode result: %v", toolName, err)
	}
	return message.NewTextContent(string(payload)), nil
}
