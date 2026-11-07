package files

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

const (
	maxGrepFileBytes = 1 << 20
	// maxGrepMatchedBytes bounds the returned text. max_matches bounds the count,
	// but a hundred matched lines of 200 KB each is 20 MB of answer. A variable so
	// a test can lower it without writing megabytes.
)

var maxGrepMatchedBytes = 4 << 20

const (
	binarySniffBytes   = 8 << 10
	defaultGrepMatches = 100
	maxGrepMatches     = 1000
	maxGrepFiles       = 5000
)

type grepTool struct {
	tree *fsx.Tree
	root string
}

var _ tool.Tool = (*grepTool)(nil)

func (t *grepTool) Definition() message.ToolDefinition {
	return definition(
		GrepName,
		"Search files for lines matching a pattern. path may name a directory "+
			"(searched recursively) or a single file. Hidden entries and .git are "+
			"skipped unless include_hidden is true; files larger than 1 MiB are "+
			"skipped and counted; a binary file that matches is reported by path "+
			"only. Returns JSON {matches:[{path, line_number, line}], truncated, "+
			"skipped_large, binary_matches}.",
		map[string]property{
			"pattern":          {kind: "string", desc: "Regular expression, or a literal when fixed_strings is true."},
			"path":             {kind: "string", desc: "Absolute directory, or a single file; defaults to the configured root."},
			"case_insensitive": {kind: "boolean", desc: "Case-insensitive match.", def: false},
			"fixed_strings":    {kind: "boolean", desc: "Treat pattern as a literal.", def: false},
			"max_matches":      {kind: "integer", desc: "Return at most this many matches.", def: defaultGrepMatches, min: num(1), max: num(maxGrepMatches)},
			"include_hidden":   {kind: "boolean", desc: "Include hidden files and directories.", def: false},
		},
		"pattern",
	)
}

func (t *grepTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

type grepMatch struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	Line       string `json:"line"`
}

func (t *grepTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		CaseInsensitive bool   `json:"case_insensitive"`
		FixedStrings    bool   `json:"fixed_strings"`
		MaxMatches      int    `json:"max_matches"`
		IncludeHidden   bool   `json:"include_hidden"`
	}
	if err := tool.DecodeArgs(GrepName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	if strings.TrimSpace(params.Pattern) == "" {
		return message.Content{}, errdefs.Validationf("%s: pattern is required", GrepName)
	}
	root, err := absolute(GrepName, params.Path, t.root)
	if err != nil {
		return message.Content{}, err
	}

	expr := params.Pattern
	if params.FixedStrings {
		expr = regexp.QuoteMeta(expr)
	}
	if params.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return message.Content{}, errdefs.Validationf("%s: invalid pattern: %v", GrepName, err)
	}

	maxMatches := params.MaxMatches
	if maxMatches <= 0 {
		maxMatches = defaultGrepMatches
	}
	if maxMatches > maxGrepMatches {
		maxMatches = maxGrepMatches
	}

	scan := &grepScan{re: re, maxMatches: maxMatches}
	info, err := t.tree.Stat(ctx, root)
	if err != nil {
		return message.Content{}, err
	}
	if info.IsDir() {
		err = t.walk(ctx, root, params.IncludeHidden, scan)
	} else {
		err = scan.file(ctx, t.tree, root, info)
	}
	if err != nil && !errors.Is(err, errStopWalk) {
		return message.Content{}, err
	}
	if len(scan.matches) > maxMatches {
		scan.matches = scan.matches[:maxMatches]
	}
	if scan.matches == nil {
		scan.matches = []grepMatch{}
	}
	if scan.binaryMatches == nil {
		scan.binaryMatches = []string{}
	}
	return result(GrepName, map[string]any{
		"matches":        scan.matches,
		"truncated":      scan.truncated,
		"skipped_large":  scan.skippedLarge,
		"binary_matches": scan.binaryMatches,
	})
}

// walk searches a directory tree, pruning .git and hidden directories before
// descending, and stopping once a cap is reached.
func (t *grepTool) walk(ctx context.Context, root string, includeHidden bool, scan *grepScan) error {
	return t.tree.Walk(ctx, root, func(path string, entry fs.DirEntry) error {
		name := entry.Name()
		if entry.IsDir() {
			if !includeHidden && (name == alwaysSkipDir || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !includeHidden && strings.HasPrefix(name, ".") {
			return nil
		}
		if scan.files >= maxGrepFiles {
			scan.truncated = true
			return errStopWalk
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		return scan.file(ctx, t.tree, path, info)
	})
}

// grepScan accumulates the hits of one call.
// nextLine returns the bytes of one line starting at at, the offset after it, and
// whether a line was there. A trailing CR is stripped so CRLF files match the
// same way splitLines made them match, and a final newline does not open an
// empty line.
func nextLine(data []byte, at int) ([]byte, int, bool) {
	if at >= len(data) {
		return nil, at, false
	}
	end := at
	for end < len(data) && data[end] != '\n' {
		end++
	}
	line := data[at:end]
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if end < len(data) {
		end++
	}
	return line, end, true
}

type grepScan struct {
	re            *regexp.Regexp
	maxMatches    int
	matches       []grepMatch
	binaryMatches []string
	truncated     bool
	skippedLarge  int
	files         int
	matchedBytes  int
}

func (s *grepScan) file(ctx context.Context, tree *fsx.Tree, path string, info fs.FileInfo) error {
	if info.Size() > maxGrepFileBytes {
		s.skippedLarge++
		return nil
	}
	data, err := tree.Read(ctx, path)
	if err != nil {
		// An unreadable file is not a reason to fail the search.
		return nil
	}
	s.files++
	s.scanData(path, data)
	if len(s.matches) > s.maxMatches {
		s.truncated = true
		return errStopWalk
	}
	return nil
}

// scanData classifies one file's bytes: binary content is matched whole and
// reported by path; text is scanned line by line.
func (s *grepScan) scanData(path string, data []byte) {
	head := data[:min(len(data), binarySniffBytes)]
	if bytes.IndexByte(head, 0) >= 0 {
		if s.re.Match(data) {
			s.binaryMatches = append(s.binaryMatches, path)
		}
		return
	}
	// Scan the raw bytes: splitLines would convert the file to one string and
	// then copy every line, so a newline-dense file amplified to a multiple of
	// its size before a single match was known. Only matched lines become strings.
	lineNo := 0
	for at := 0; ; {
		line, next, ok := nextLine(data, at)
		if !ok {
			return
		}
		at = next
		lineNo++
		if !s.re.Match(line) {
			continue
		}
		s.matches = append(s.matches, grepMatch{Path: path, LineNumber: lineNo, Line: string(line)})
		s.matchedBytes += len(line)
		if s.matchedBytes > maxGrepMatchedBytes {
			s.truncated = true
			return
		}
		if len(s.matches) > s.maxMatches {
			return
		}
	}
}
