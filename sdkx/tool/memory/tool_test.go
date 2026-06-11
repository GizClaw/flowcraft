package memory_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	memorytool "github.com/GizClaw/flowcraft/sdkx/tool/memory"
)

func newTool(t *testing.T) (*memorytool.Tool, *workspace.MemWorkspace) {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	return memorytool.New(ws), ws
}

func exec(t *testing.T, tool *memorytool.Tool, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), string(raw))
	if err != nil {
		t.Fatalf("Execute(%v): %v", args, err)
	}
	return out
}

func execErr(t *testing.T, tool *memorytool.Tool, args map[string]any) error {
	t.Helper()
	raw, _ := json.Marshal(args)
	_, err := tool.Execute(context.Background(), string(raw))
	if err == nil {
		t.Fatalf("Execute(%v): want error, got nil", args)
	}
	return err
}

func TestDefinition(t *testing.T) {
	tool, _ := newTool(t)
	def := tool.Definition()
	if def.Name != "memory" {
		t.Fatalf("name = %q, want memory", def.Name)
	}
}

func TestPathPrefixEnforced(t *testing.T) {
	tool, _ := newTool(t)
	err := execErr(t, tool, map[string]any{"command": "view", "path": "/etc/passwd"})
	if !strings.Contains(err.Error(), memorytool.PathPrefix) {
		t.Errorf("err = %v, want PathPrefix mention", err)
	}
}

func TestCreateAndView(t *testing.T) {
	tool, ws := newTool(t)
	exec(t, tool, map[string]any{
		"command":   "create",
		"path":      "/memories/notes/a.txt",
		"file_text": "hello\nworld\n",
	})

	// Verify the write landed under memories/ (not at workspace root)
	// so the Memory Tool subtree coexists with other workspace consumers.
	data, err := ws.Read(context.Background(), "memories/notes/a.txt")
	if err != nil {
		t.Fatalf("expected file at memories/notes/a.txt: %v", err)
	}
	if string(data) != "hello\nworld\n" {
		t.Errorf("workspace content = %q", data)
	}

	out := exec(t, tool, map[string]any{
		"command": "view",
		"path":    "/memories/notes/a.txt",
	})
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Errorf("view output missing content: %s", out)
	}
}

func TestSubtreeIsolation(t *testing.T) {
	tool, ws := newTool(t)

	// Pre-seed peer subtrees that other subsystems would own; the
	// Memory Tool must not see or touch them.
	if err := ws.Write(context.Background(), "retrieval/facts.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(context.Background(), "views/chunks.bin", []byte("x")); err != nil {
		t.Fatal(err)
	}

	exec(t, tool, map[string]any{"command": "create", "path": "/memories/note.md", "file_text": "hi"})

	out := exec(t, tool, map[string]any{"command": "view", "path": "/memories"})
	if strings.Contains(out, "retrieval") || strings.Contains(out, "views") {
		t.Errorf("memory view leaked sibling subtree: %s", out)
	}
	if !strings.Contains(out, "note.md") {
		t.Errorf("memory view missing own file: %s", out)
	}

	// Peer subtree must remain untouched.
	if exists, _ := ws.Exists(context.Background(), "retrieval/facts.json"); !exists {
		t.Error("peer subtree retrieval/facts.json was disturbed")
	}
}

func TestViewMemoriesRoot(t *testing.T) {
	tool, _ := newTool(t)
	// Empty memories/ root should still be viewable; List on a
	// non-existent dir is tolerated and returns empty entries.
	out := exec(t, tool, map[string]any{"command": "view", "path": "/memories"})
	if !strings.Contains(out, `"entries":[]`) {
		t.Errorf("empty memories root view = %s", out)
	}
}

func TestViewWithRange(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{
		"command":   "create",
		"path":      "/memories/a.txt",
		"file_text": "l1\nl2\nl3\nl4\nl5\n",
	})
	out := exec(t, tool, map[string]any{
		"command":    "view",
		"path":       "/memories/a.txt",
		"view_range": []int{2, 4},
	})
	if !strings.Contains(out, "l2") || !strings.Contains(out, "l4") {
		t.Errorf("range view missing l2/l4: %s", out)
	}
	if strings.Contains(out, "l1") || strings.Contains(out, "l5") {
		t.Errorf("range view leaked outside range: %s", out)
	}
}

func TestViewDir(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{"command": "create", "path": "/memories/a.txt", "file_text": "a"})
	exec(t, tool, map[string]any{"command": "create", "path": "/memories/sub/b.txt", "file_text": "b"})

	out := exec(t, tool, map[string]any{"command": "view", "path": "/memories"})
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "sub/") {
		t.Errorf("dir view missing entries: %s", out)
	}
}

func TestStrReplace(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{"command": "create", "path": "/memories/x.txt", "file_text": "hello world"})

	exec(t, tool, map[string]any{
		"command": "str_replace",
		"path":    "/memories/x.txt",
		"old_str": "world",
		"new_str": "anthropic",
	})
	out := exec(t, tool, map[string]any{"command": "view", "path": "/memories/x.txt"})
	if !strings.Contains(out, "anthropic") {
		t.Errorf("str_replace not applied: %s", out)
	}
}

func TestStrReplaceMultipleMatches(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{"command": "create", "path": "/memories/x.txt", "file_text": "a a a"})
	err := execErr(t, tool, map[string]any{
		"command": "str_replace",
		"path":    "/memories/x.txt",
		"old_str": "a",
		"new_str": "b",
	})
	if !strings.Contains(err.Error(), "matches 3 times") {
		t.Errorf("expected ambiguity error, got: %v", err)
	}
}

func TestStrReplaceNotFound(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{"command": "create", "path": "/memories/x.txt", "file_text": "abc"})
	err := execErr(t, tool, map[string]any{
		"command": "str_replace",
		"path":    "/memories/x.txt",
		"old_str": "xyz",
		"new_str": "q",
	})
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want 'not found'", err)
	}
}

func TestInsert(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{
		"command":   "create",
		"path":      "/memories/x.txt",
		"file_text": "l1\nl2\nl3\n",
	})
	exec(t, tool, map[string]any{
		"command":     "insert",
		"path":        "/memories/x.txt",
		"insert_line": 1,
		"insert_text": "INJECTED",
	})
	out := exec(t, tool, map[string]any{"command": "view", "path": "/memories/x.txt"})
	idxL1 := strings.Index(out, "l1")
	idxInj := strings.Index(out, "INJECTED")
	idxL2 := strings.Index(out, "l2")
	if !(idxL1 < idxInj && idxInj < idxL2) {
		t.Errorf("insert order wrong: %s", out)
	}
}

func TestDelete(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{"command": "create", "path": "/memories/x.txt", "file_text": "a"})
	exec(t, tool, map[string]any{"command": "delete", "path": "/memories/x.txt"})
	err := execErr(t, tool, map[string]any{"command": "view", "path": "/memories/x.txt"})
	if err == nil {
		t.Errorf("expected view error after delete")
	}
}

func TestDeleteRoot(t *testing.T) {
	tool, _ := newTool(t)
	err := execErr(t, tool, map[string]any{"command": "delete", "path": "/memories"})
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("err = %v, want refuse-to-delete-root", err)
	}
}

func TestRename(t *testing.T) {
	tool, _ := newTool(t)
	exec(t, tool, map[string]any{"command": "create", "path": "/memories/a.txt", "file_text": "X"})
	exec(t, tool, map[string]any{
		"command":  "rename",
		"old_path": "/memories/a.txt",
		"new_path": "/memories/b.txt",
	})
	out := exec(t, tool, map[string]any{"command": "view", "path": "/memories/b.txt"})
	if !strings.Contains(out, "X") {
		t.Errorf("rename target missing content: %s", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	tool, _ := newTool(t)
	err := execErr(t, tool, map[string]any{"command": "nope", "path": "/memories/x"})
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("err = %v, want unknown command", err)
	}
}

func TestMaxViewBytesTruncates(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	tool := memorytool.New(ws, memorytool.WithMaxViewBytes(8))
	exec(t, tool, map[string]any{
		"command":   "create",
		"path":      "/memories/big.txt",
		"file_text": "0123456789ABCDEF",
	})
	out := exec(t, tool, map[string]any{"command": "view", "path": "/memories/big.txt"})
	if !strings.Contains(out, `"truncated":true`) {
		t.Errorf("expected truncated=true, got: %s", out)
	}
}

func TestPathTraversalDenied(t *testing.T) {
	tool, _ := newTool(t)
	err := execErr(t, tool, map[string]any{
		"command": "view",
		"path":    "/memories/../etc/passwd",
	})
	if !strings.Contains(err.Error(), "traversal") {
		t.Errorf("err = %v, want traversal denied", err)
	}
}
