package files

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func seed(t *testing.T, tree interface {
	Write(context.Context, string, []byte) error
}, files map[string]string) string {
	t.Helper()
	var root string
	for name, body := range files {
		root = filepath.Dir(filepath.Dir(name))
		if err := tree.Write(context.Background(), name, []byte(body)); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	return root
}

func TestGrepFindsLinesWithNumbers(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	target := filepath.Join(root, "a.txt")
	if err := tree.Write(ctx, target, []byte("alpha\nbeta\nALPHA again\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	grep := find(t, fileTools, GrepName)

	text, err := call(t, grep, map[string]any{"pattern": "alpha", "path": root})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := decode(t, text)
	matches := out["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("matches = %v, want one (case-sensitive by default)", matches)
	}
	first := matches[0].(map[string]any)
	if first["line_number"].(float64) != 1 || first["line"] != "alpha" {
		t.Errorf("match = %v", first)
	}

	text, err = call(t, grep, map[string]any{"pattern": "alpha", "path": root, "case_insensitive": true})
	if err != nil {
		t.Fatalf("grep ci: %v", err)
	}
	if got := len(decode(t, text)["matches"].([]any)); got != 2 {
		t.Errorf("case-insensitive matches = %d, want 2", got)
	}
}

func TestGrepSkipsHiddenAndReportsBinary(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	for name, body := range map[string]string{
		".secret.txt": "needle\n",
		"visible.txt": "needle\n",
	} {
		if err := tree.Write(ctx, filepath.Join(root, name), []byte(body)); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	binary := filepath.Join(root, "blob.bin")
	if err := tree.Write(ctx, binary, []byte("needle\x00binary")); err != nil {
		t.Fatalf("Write binary: %v", err)
	}

	grep := find(t, fileTools, GrepName)
	text, err := call(t, grep, map[string]any{"pattern": "needle", "path": root})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := decode(t, text)
	matches := out["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("matches = %v, want only the visible file", matches)
	}
	if got := out["binary_matches"].([]any); len(got) != 1 || got[0] != binary {
		t.Errorf("binary matches = %v, want [%s]", got, binary)
	}
}

func TestGrepTruncatesAtMaxMatches(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	if err := tree.Write(ctx, filepath.Join(root, "many.txt"), []byte("hit\nhit\nhit\nhit\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	text, err := call(t, find(t, fileTools, GrepName), map[string]any{
		"pattern": "hit", "path": root, "max_matches": 2,
	})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := decode(t, text)
	if got := len(out["matches"].([]any)); got != 2 {
		t.Errorf("matches = %d, want the cap of 2", got)
	}
	if out["truncated"] != true {
		t.Error("truncation not reported")
	}
}

func TestGlobMatchesAcrossSegments(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	for _, name := range []string{"top.txt", "sub/deep/nested.txt", "sub/other.go"} {
		if err := tree.Write(ctx, filepath.Join(root, name), []byte("x")); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	glob := find(t, fileTools, GlobName)

	text, err := call(t, glob, map[string]any{"pattern": "**/*.txt", "path": root})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	matches := decode(t, text)["matches"].([]any)
	if len(matches) != 2 {
		t.Fatalf("**/*.txt matches = %v, want two", matches)
	}

	text, err = call(t, glob, map[string]any{"pattern": "*.txt", "path": root})
	if err != nil {
		t.Fatalf("glob top: %v", err)
	}
	matches = decode(t, text)["matches"].([]any)
	if len(matches) != 1 || !strings.HasSuffix(matches[0].(string), "top.txt") {
		t.Errorf("*.txt matches = %v, want only top.txt", matches)
	}
}

// Line numbers come from the shared splitLines: a trailing newline must not add
// a phantom line, and the scan must not re-count the file per line.
func TestGrepLineNumbersIgnoreTrailingNewline(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	if err := tree.Write(context.Background(), filepath.Join(root, "two.txt"), []byte("a\nb\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	text, err := call(t, find(t, fileTools, GrepName), map[string]any{"pattern": "b", "path": root})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	got := decode(t, text)["matches"].([]any)[0].(map[string]any)["line_number"].(float64)
	if got != 2 {
		t.Errorf("line_number = %v, want 2", got)
	}
}

// .git follows include_hidden like every other hidden entry, as the description
// promises.
func TestGrepGitDirFollowsIncludeHidden(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	if err := tree.Write(ctx, filepath.Join(root, ".git", "config"), []byte("needle\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	grep := find(t, fileTools, GrepName)

	text, err := call(t, grep, map[string]any{"pattern": "needle", "path": root})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if got := len(decode(t, text)["matches"].([]any)); got != 0 {
		t.Errorf("hidden .git searched without include_hidden: %d matches", got)
	}
	text, err = call(t, grep, map[string]any{"pattern": "needle", "path": root, "include_hidden": true})
	if err != nil {
		t.Fatalf("grep include_hidden: %v", err)
	}
	if got := len(decode(t, text)["matches"].([]any)); got != 1 {
		t.Errorf("include_hidden searched .git: %d matches, want 1", got)
	}
}

// A malformed pattern is rejected, not silently matched against nothing.
func TestGlobRejectsInvalidPattern(t *testing.T) {
	fileTools, tree := group(t)
	if _, err := call(t, find(t, fileTools, GlobName), map[string]any{
		"pattern": "[", "path": tree.Writable()[0],
	}); err == nil {
		t.Fatal("invalid pattern accepted")
	}
}

func TestGlobIncludesHiddenWhenAsked(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	if err := tree.Write(context.Background(), filepath.Join(root, ".hidden", "x.txt"), []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	glob := find(t, fileTools, GlobName)

	text, err := call(t, glob, map[string]any{"pattern": "**/*.txt", "path": root})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if got := len(decode(t, text)["matches"].([]any)); got != 0 {
		t.Errorf("hidden file matched without include_hidden: %d", got)
	}
	text, err = call(t, glob, map[string]any{"pattern": "**/*.txt", "path": root, "include_hidden": true})
	if err != nil {
		t.Fatalf("glob include_hidden: %v", err)
	}
	if got := len(decode(t, text)["matches"].([]any)); got != 1 {
		t.Errorf("include_hidden matched %d files, want 1", got)
	}
}
