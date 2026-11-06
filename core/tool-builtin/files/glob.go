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

	matches := []string{}
	truncated := false
	err = t.tree.Walk(ctx, root, func(candidate string, entry fs.DirEntry) error {
		name := entry.Name()
		if entry.IsDir() {
			if !params.IncludeHidden && (name == alwaysSkipDir || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, candidate)
		if relErr != nil {
			return nil
		}
		if !matchGlob(params.Pattern, filepath.ToSlash(rel)) {
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

// matchGlob matches one slash-separated pattern against a slash-separated
// relative path. ** spans any number of segments; everything else is path.Match
// within one segment.
func matchGlob(pattern, rel string) bool {
	return matchSegments(
		strings.Split(strings.Trim(pattern, "/"), "/"),
		strings.Split(strings.Trim(rel, "/"), "/"),
	)
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
