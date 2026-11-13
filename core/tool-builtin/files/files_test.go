package files

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

func group(t *testing.T) (*Tool, *fsx.Tree) {
	t.Helper()
	root := t.TempDir()
	tree, err := fsx.Open(fsx.WithWritable(root))
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	fileTools, err := New(tree, tree.Writable()[0])
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return fileTools, tree
}

func find(t *testing.T, fileTools *Tool, name string) tool.Tool {
	t.Helper()
	for _, candidate := range fileTools.Tools() {
		if candidate.Definition().Name == name {
			return candidate
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func call(t *testing.T, candidate tool.Tool, arguments map[string]any) (string, error) {
	t.Helper()
	payload, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	content, err := candidate.Execute(context.Background(), string(payload))
	if err != nil {
		return "", err
	}
	return content.Text(), nil
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode result %q: %v", text, err)
	}
	return out
}

// A read returns one window and tells the caller where to continue.
func TestReadWindow(t *testing.T) {
	fileTools, tree := group(t)
	target := filepath.Join(tree.Writable()[0], "doc.txt")
	if err := tree.Write(context.Background(), target, []byte("l1\nl2\nl3\nl4\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	read := find(t, fileTools, ReadFileName)

	text, err := call(t, read, map[string]any{"file_path": target, "offset": 2, "limit": 2})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := decode(t, text)
	if out["content"] != "l2\nl3" {
		t.Errorf("content = %q, want %q", out["content"], "l2\nl3")
	}
	if out["total_lines"].(float64) != 4 {
		t.Errorf("total_lines = %v, want 4", out["total_lines"])
	}
	if out["is_truncated"] != true || out["next_offset"].(float64) != 4 {
		t.Errorf("window did not report continuation: %v", out)
	}
}

func TestReadRejectsRelativePath(t *testing.T) {
	fileTools, _ := group(t)
	if _, err := call(t, find(t, fileTools, ReadFileName), map[string]any{"file_path": "doc.txt"}); err == nil {
		t.Fatal("relative path accepted, want a rejection")
	}
}

func TestWriteReportsBytes(t *testing.T) {
	fileTools, tree := group(t)
	target := filepath.Join(tree.Writable()[0], "nested", "a.txt")

	text, err := call(t, find(t, fileTools, WriteFileName), map[string]any{
		"file_path": target,
		"content":   "hello",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if out := decode(t, text); out["bytes"].(float64) != 5 {
		t.Errorf("bytes = %v, want 5", out["bytes"])
	}
	got, err := tree.Read(context.Background(), target)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back = %q, %v", got, err)
	}
}

// The writable boundary is the kernel's: a path outside it is refused by fsx,
// not by a string check in the tool.
func TestWriteOutsideWritableRootIsDenied(t *testing.T) {
	fileTools, _ := group(t)
	outside := filepath.Join(t.TempDir(), "escape.txt")
	_, err := call(t, find(t, fileTools, WriteFileName), map[string]any{
		"file_path": outside,
		"content":   "x",
	})
	if !errors.Is(err, fsx.ErrDenied) {
		t.Errorf("write outside the root = %v, want fsx.ErrDenied", err)
	}
}

func TestListDirSkipsHiddenAndRespectsDepth(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	for _, name := range []string{"a.txt", ".hidden.txt", "sub/b.txt", "sub/.git/c"} {
		if err := tree.Write(ctx, filepath.Join(root, name), []byte("x")); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	list := find(t, fileTools, ListDirName)

	text, err := call(t, list, map[string]any{"path": root})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := entryNames(t, decode(t, text))
	if got := relative(t, root, names); got != "a.txt,sub" {
		t.Errorf("non-recursive entries = %v, want [a.txt sub]", got)
	}

	text, err = call(t, list, map[string]any{"path": root, "recursive": true, "max_depth": 1})
	if err != nil {
		t.Fatalf("list recursive: %v", err)
	}
	names = entryNames(t, decode(t, text))
	if got := relative(t, root, names); got != "a.txt,sub,sub/b.txt" {
		t.Errorf("recursive entries = %v", got)
	}
}

// relative renders entries relative to root, slash-separated.
func relative(t *testing.T, root string, paths []string) string {
	t.Helper()
	rel := make([]string, 0, len(paths))
	for _, path := range paths {
		trimmed, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("Rel(%q, %q): %v", root, path, err)
		}
		rel = append(rel, filepath.ToSlash(trimmed))
	}
	return strings.Join(rel, ",")
}

func entryNames(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, _ := out["entries"].([]any) // an empty list may be absent or []

	root := filepath.Dir(filepath.Dir(t.TempDir())) // unused; kept for clarity
	_ = root
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		entry := item.(map[string]any)
		names = append(names, entry["path"].(string))
	}
	return names
}

// fsx.Walk hands the callback's error to filepath.WalkDir, so returning
// fs.SkipDir prunes a subtree — the property list_dir and the grep/glob tools
// rely on.
func TestWalkHonoursSkipDir(t *testing.T) {
	_, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	for _, name := range []string{"keep/a.txt", "skip/b.txt"} {
		if err := tree.Write(ctx, filepath.Join(root, name), []byte("x")); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}

	var seen []string
	err := tree.Walk(ctx, root, func(path string, entry fs.DirEntry) error {
		if entry.IsDir() && entry.Name() == "skip" {
			return fs.SkipDir
		}
		seen = append(seen, path)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, path := range seen {
		if strings.Contains(path, string(filepath.Separator)+"skip") {
			t.Errorf("SkipDir did not prune the subtree; saw %q", path)
		}
	}
}

// .git follows include_hidden in list_dir as well; the three tree-walking tools
// agree on one rule.
func TestListGitDirFollowsIncludeHidden(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	if err := tree.Write(context.Background(), filepath.Join(root, ".git", "config"), []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	list := find(t, fileTools, ListDirName)

	text, err := call(t, list, map[string]any{"path": root})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if names := entryNames(t, decode(t, text)); len(names) != 0 {
		t.Errorf("hidden .git listed without include_hidden: %v", names)
	}
	text, err = call(t, list, map[string]any{"path": root, "include_hidden": true})
	if err != nil {
		t.Fatalf("list include_hidden: %v", err)
	}
	if names := entryNames(t, decode(t, text)); len(names) != 1 {
		t.Errorf("include_hidden listed %v, want .git", names)
	}
}

// The content cap is checked before the write, so an oversized payload never
// reaches the disk.
func TestWriteRejectsOversizedContent(t *testing.T) {
	fileTools, tree := group(t)
	previous := maxWriteBytes
	maxWriteBytes = 4
	t.Cleanup(func() { maxWriteBytes = previous })

	_, err := call(t, find(t, fileTools, WriteFileName), map[string]any{
		"file_path": filepath.Join(tree.Writable()[0], "big.txt"),
		"content":   "12345",
	})
	if err == nil {
		t.Fatal("oversized content accepted")
	}
	if _, statErr := tree.Stat(context.Background(), filepath.Join(tree.Writable()[0], "big.txt")); statErr == nil {
		t.Error("rejected write still created the file")
	}
}

// A denial names the roots the deployment declared: "outside every writable
// root" alone leaves the model guessing where it may write.
func TestWriteDenialListsWritableRoots(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	_, err := call(t, find(t, fileTools, WriteFileName), map[string]any{
		"file_path": filepath.Join(t.TempDir(), "escape.txt"),
		"content":   "x",
	})
	if err == nil {
		t.Fatal("write outside the root accepted")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("denial %q does not name the writable root %q", err, root)
	}
}

// A directory path may be omitted: the group's configured root is the default,
// and the schema says so.
func TestDirectoryPathsDefaultToRoot(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	if err := tree.Write(context.Background(), filepath.Join(root, "a.txt"), []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	text, err := call(t, find(t, fileTools, ListDirName), map[string]any{})
	if err != nil {
		t.Fatalf("list without a path: %v", err)
	}
	if names := entryNames(t, decode(t, text)); len(names) != 1 {
		t.Errorf("list defaulted to the root and found %v", names)
	}

	text, err = call(t, find(t, fileTools, GrepName), map[string]any{"pattern": "x"})
	if err != nil {
		t.Fatalf("grep without a path: %v", err)
	}
	if got := len(decode(t, text)["matches"].([]any)); got != 1 {
		t.Errorf("grep defaulted to the root and found %d matches", got)
	}
}

func TestListRejectsNegativeDepth(t *testing.T) {
	fileTools, _ := group(t)
	if _, err := call(t, find(t, fileTools, ListDirName), map[string]any{
		"recursive": true, "max_depth": -1,
	}); err == nil {
		t.Fatal("negative max_depth accepted")
	}
}

// The schema states its constraints instead of leaving them to prose.
func TestSchemasCarryConstraints(t *testing.T) {
	fileTools, _ := group(t)
	// write_file has no numeric parameter, so it has no constraint to state;
	// the others do, and prose is not a substitute for a schema keyword.
	for _, candidate := range fileTools.Tools() {
		if candidate.Definition().Name == WriteFileName {
			continue
		}
		schema := string(candidate.Definition().InputSchema)
		if !strings.Contains(schema, `"default"`) && !strings.Contains(schema, `"minimum"`) {
			t.Errorf("%s schema has no default or minimum: %s", candidate.Definition().Name, schema)
		}
	}
}
