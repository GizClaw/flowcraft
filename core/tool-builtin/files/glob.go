package files

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

const maxGlobMatches = 1000

// maxGlobScanned bounds the walk itself. A match cap alone does not: a pattern
// that matches nothing still visits the whole tree, and ** on a deep tree can
// visit far more entries than it can ever return. A variable so tests can lower
// it without building a quarter-million-entry tree.
var maxGlobScanned = 200000

type globTool struct {
	tree *fsx.Tree
	root string
}

var _ tool.Tool = (*globTool)(nil)

func (t *globTool) Definition() message.ToolDefinition {
	return definition(
		GlobName,
		"Find files matching a glob pattern under a directory. Supports * (one "+
			"segment), ** (any number of segments), ? and [class]. Hidden entries "+
			"and .git are skipped unless include_hidden is true. Returns JSON "+
			"{matches, truncated} with absolute paths.",
		map[string]property{
			"pattern":        {kind: "string", desc: "Glob applied under path, e.g. **/*.go."},
			"path":           {kind: "string", desc: "Absolute base directory; defaults to the configured root."},
			"include_hidden": {kind: "boolean", desc: "Include hidden entries and .git.", def: false},
		},
		"pattern",
	)
}

func (t *globTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

func (t *globTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		Pattern       string `json:"pattern"`
		Path          string `json:"path"`
		IncludeHidden bool   `json:"include_hidden"`
	}
	if err := tool.DecodeArgs(GlobName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	if strings.TrimSpace(params.Pattern) == "" {
		return message.Content{}, errdefs.Validationf("%s: pattern is required", GlobName)
	}
	if err := validateGlob(params.Pattern); err != nil {
		return message.Content{}, errdefs.Validationf("%s: invalid pattern: %v", GlobName, err)
	}
	root, err := absolute(GlobName, params.Path, t.root)
	if err != nil {
		return message.Content{}, err
	}

	// Compile the pattern once: splitting it per visited entry was work the walk
	// repeated for every file in the tree.
	pattern := strings.Split(strings.Trim(params.Pattern, "/"), "/")
	matches := []string{}
	truncated := false
	scanned := 0
	err = t.tree.Walk(ctx, root, func(candidate string, entry fs.DirEntry) error {
		if scanned >= maxGlobScanned {
			truncated = true
			return errStopWalk
		}
		scanned++
		name := entry.Name()
		if entry.IsDir() {
			if !params.IncludeHidden && (name == alwaysSkipDir || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		rel, ok := relTo(root, candidate)
		if !ok {
			return nil
		}
		if !matchSegments(pattern, strings.Split(strings.Trim(rel, "/"), "/")) {
			return nil
		}
		if len(matches) >= maxGlobMatches {
			truncated = true
			return errStopWalk
		}
		matches = append(matches, candidate)
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return message.Content{}, err
	}
	return result(GlobName, map[string]any{"matches": matches, "truncated": truncated})
}

// relTo returns the slash-separated path under root. filepath.Rel per entry
// allocates and re-cleans both paths; the walk already knows the prefix.
func relTo(root, path string) (string, bool) {
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return filepath.ToSlash(path[len(prefix):]), true
}

func matchSegments(pattern, segments []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(segments); i++ {
				if matchSegments(pattern[1:], segments[i:]) {
					return true
				}
			}
			return false
		}
		if len(segments) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], segments[0]); err != nil || !ok {
			return false
		}
		pattern, segments = pattern[1:], segments[1:]
	}
	return len(segments) == 0
}

// validateGlob rejects a pattern a segment matcher cannot compile.
//
// path.Match reports a malformed class only when it is applied, and the inner
// matcher treats that error as "no match" — so without this check an invalid
// pattern would silently find nothing and look like an empty directory.
func validateGlob(pattern string) error {
	for _, segment := range strings.Split(strings.Trim(pattern, "/"), "/") {
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return err
		}
	}
	return nil
}
