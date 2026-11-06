package files

import (
	"context"
	"errors"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// maxWriteBytes bounds one write; a variable so tests can exercise the cap
// without allocating the real limit.
var maxWriteBytes = 8 << 20

type writeFileTool struct {
	tree *fsx.Tree
	root string
}

var _ tool.Tool = (*writeFileTool)(nil)

func (t *writeFileTool) Definition() message.ToolDefinition {
	return definition(
		WriteFileName,
		"Write content to a file, creating parent directories as needed and "+
			"overwriting existing content. The path must be absolute and inside "+
			"a writable root; anything else is refused by the kernel. Returns "+
			"JSON {file_path, bytes}.",
		map[string]property{
			"file_path": {kind: "string", desc: "Absolute path of the file to write; defaults to the configured root."},
			"content":   {kind: "string", desc: "The full new file content (default empty)."},
		},
		"file_path",
	)
}

func (t *writeFileTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (t *writeFileTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := tool.DecodeArgs(WriteFileName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	path, err := absolute(WriteFileName, params.FilePath, t.root)
	if err != nil {
		return message.Content{}, err
	}
	if len(params.Content) > maxWriteBytes {
		return message.Content{}, errdefs.Validationf(
			"%s: content is %d bytes and the limit is %d; write it in parts, or generate the file with a command",
			WriteFileName, len(params.Content), maxWriteBytes)
	}
	if err := t.tree.Write(ctx, path, []byte(params.Content)); err != nil {
		if errors.Is(err, fsx.ErrDenied) {
			// %w keeps fsx.ErrDenied reachable for a programmatic caller; the
			// listed roots are what a model needs to correct itself.
			return message.Content{}, errdefs.Forbiddenf(
				"%s: %s is outside the writable roots (%s): %w",
				WriteFileName, path, strings.Join(t.tree.Writable(), ", "), err)
		}
		return message.Content{}, err
	}
	return result(WriteFileName, map[string]any{
		"file_path": path,
		"bytes":     len(params.Content),
	})
}
