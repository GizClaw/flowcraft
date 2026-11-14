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

// editFileTool replaces an exact run of text in an existing file.
//
// It exists because the alternative — write_file with the whole edited document —
// costs the model a full re-emission of the file, and a long rewrite is where
// unrelated changes sneak in. An exact replacement makes the intent reviewable:
// this text, and nothing else, becomes that text.
type editFileTool struct {
	tree *fsx.Tree
	root string
}

var _ tool.Tool = (*editFileTool)(nil)

func (t *editFileTool) Definition() message.ToolDefinition {
	return definition(
		EditName,
		"Replace an exact string in an existing file. old_string must appear "+
			"exactly once unless replace_all is true, so include enough surrounding "+
			"text to identify the spot. Returns JSON {file_path, replaced}. Use "+
			WriteFileName+" to create a file or replace it entirely.",
		map[string]property{
			"file_path":  {kind: "string", desc: "Absolute path of the file to edit; defaults to the configured root."},
			"old_string": {kind: "string", desc: "Text to replace, matched exactly (including indentation and line endings)."},
			"new_string": {kind: "string", desc: "Replacement text; empty deletes the match."},
			"replace_all": {kind: "boolean", def: false,
				desc: "Replace every occurrence instead of requiring a unique match."},
		},
		"file_path", "old_string", "new_string",
	)
}

func (t *editFileTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (t *editFileTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := tool.DecodeArgs(EditName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	if params.OldString == "" {
		return message.Content{}, errdefs.Validationf(
			"%s: old_string is required; use %s to create an empty file", EditName, WriteFileName)
	}
	path, err := absolute(EditName, params.FilePath, t.root)
	if err != nil {
		return message.Content{}, err
	}

	current, err := t.tree.Read(ctx, path)
	if err != nil {
		return message.Content{}, err
	}
	text := string(current)

	count := strings.Count(text, params.OldString)
	switch {
	case count == 0:
		return message.Content{}, errdefs.Validationf(
			"%s: old_string does not appear in %s; read the file and copy the text exactly",
			EditName, path)
	case count > 1 && !params.ReplaceAll:
		return message.Content{}, errdefs.Validationf(
			"%s: old_string appears %d times in %s; include more surrounding text, or set replace_all",
			EditName, count, path)
	}

	replaced := count
	if !params.ReplaceAll {
		replaced = 1
	}
	updated := strings.Replace(text, params.OldString, params.NewString, replaced)
	if len(updated) > maxWriteBytes {
		return message.Content{}, errdefs.Validationf(
			"%s: the edited file would be %d bytes and the limit is %d",
			EditName, len(updated), maxWriteBytes)
	}
	if err := t.tree.Write(ctx, path, []byte(updated)); err != nil {
		if errors.Is(err, fsx.ErrDenied) {
			return message.Content{}, errdefs.Forbiddenf(
				"%s: %s is outside the writable roots (%s): %w",
				EditName, path, strings.Join(t.tree.Writable(), ", "), err)
		}
		return message.Content{}, err
	}
	return result(EditName, map[string]any{
		"file_path": path,
		"replaced":  replaced,
	})
}

// EditName is the tool name the model sees.
const EditName = "edit"
