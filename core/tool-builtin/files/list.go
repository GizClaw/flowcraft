package files

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

const (
	defaultListDepth = 4
	maxListDepth     = 32
	maxListEntries   = 10_000
	alwaysSkipDir    = ".git"
)

type listDirTool struct {
	tree *fsx.Tree
	root string
}

var _ tool.Tool = (*listDirTool)(nil)

func (t *listDirTool) Definition() message.ToolDefinition {
	return definition(
		ListDirName,
		"List files and directories under a directory. The path must be "+
			"absolute and name a directory. Hidden entries and .git are skipped "+
			"unless include_hidden is true. Returns JSON "+
			"{path, entries:[{path, type, size}], truncated}.",
		map[string]property{
			"path":           {kind: "string", desc: "Absolute path of the directory to list; defaults to the configured root."},
			"recursive":      {kind: "boolean", desc: "Recurse into subdirectories.", def: false},
			"include_hidden": {kind: "boolean", desc: "Include hidden entries.", def: false},
			"max_depth":      {kind: "integer", desc: "Recursion depth below path.", def: defaultListDepth, min: num(0), max: num(maxListDepth)},
		},
	)
}

func (t *listDirTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

type dirEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

func (t *listDirTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		Path          string `json:"path"`
		Recursive     bool   `json:"recursive"`
		IncludeHidden bool   `json:"include_hidden"`
		MaxDepth      int    `json:"max_depth"`
	}
	if err := tool.DecodeArgs(ListDirName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	root, err := absolute(ListDirName, params.Path, t.root)
	if err != nil {
		return message.Content{}, err
	}

	if params.MaxDepth < 0 {
		return message.Content{}, errdefs.Validationf("%s: max_depth must not be negative", ListDirName)
	}
	maxDepth := params.MaxDepth
	if maxDepth <= 0 || !params.Recursive {
		maxDepth = 0
	}
	if params.Recursive && params.MaxDepth == 0 {
		maxDepth = defaultListDepth
	}
	if maxDepth > maxListDepth {
		maxDepth = maxListDepth
	}

	info, err := t.tree.Stat(ctx, root)
	if err != nil {
		return message.Content{}, err
	}
	if !info.IsDir() {
		return message.Content{}, errdefs.Validationf(
			"%s: path %q is a file, not a directory", ListDirName, root)
	}

	entries := make([]dirEntry, 0)
	truncated := false
	if err := t.collect(ctx, root, params.IncludeHidden, maxDepth, 0, &entries, &truncated); err != nil {
		return message.Content{}, err
	}

	return result(ListDirName, map[string]any{
		"path":      root,
		"entries":   entries,
		"truncated": truncated,
	})
}

// collect appends one directory's children, then recurses while depth allows.
func (t *listDirTool) collect(
	ctx context.Context,
	dir string,
	includeHidden bool,
	maxDepth, depth int,
	out *[]dirEntry,
	truncated *bool,
) error {
	children, err := t.tree.List(ctx, dir)
	if err != nil {
		return err
	}
	for _, entry := range children {
		name := entry.Name()
		if !includeHidden && (name == alwaysSkipDir || strings.HasPrefix(name, ".")) {
			continue
		}
		if len(*out) >= maxListEntries {
			*truncated = true
			return nil
		}
		full := filepath.Join(dir, name)
		kind := "file"
		var size int64
		if entry.IsDir() {
			kind = "dir"
		} else if info, err := entry.Info(); err == nil {
			size = info.Size()
		}
		*out = append(*out, dirEntry{Path: full, Type: kind, Size: size})
		if entry.IsDir() && depth < maxDepth {
			if err := t.collect(ctx, full, includeHidden, maxDepth, depth+1, out, truncated); err != nil {
				return err
			}
			if *truncated {
				return nil
			}
		}
	}
	return nil
}
