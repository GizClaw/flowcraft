package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// PathPrefix is the mandatory path prefix every Anthropic Memory
// Tool path must carry. The runtime strips it before delegating to
// the underlying [workspace.Workspace], whose paths are relative to
// its root.
const PathPrefix = "/memories"

// MaxViewBytes caps the bytes returned by a single view of a file.
// Anthropic's reference client truncates large files; we apply the
// same guard to keep tool output bounded. Configure via [Option].
const MaxViewBytes = 64 * 1024

// Tool is the single Anthropic Memory Tool exposed to LLMs. The
// command verb is dispatched on the "command" argument; see
// [https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/memory-tool]
// for the upstream spec.
type Tool struct {
	ws       workspace.Workspace
	maxBytes int64
}

// Option configures [Tool].
type Option func(*Tool)

// WithMaxViewBytes overrides the default truncation cap for view.
func WithMaxViewBytes(n int64) Option {
	return func(t *Tool) {
		if n > 0 {
			t.maxBytes = n
		}
	}
}

// New returns a Memory Tool backed by ws. Paths emitted by the
// model must begin with [PathPrefix]; the prefix is stripped
// before forwarding to ws.
func New(ws workspace.Workspace, opts ...Option) *Tool {
	t := &Tool{ws: ws, maxBytes: MaxViewBytes}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Definition implements [tool.Tool].
func (t *Tool) Definition() model.ToolDefinition {
	return tool.DefineSchema(
		"memory",
		"Persistent file-tree memory. Issue commands to read and edit files under "+PathPrefix+"/. "+
			"Use 'view' to inspect, 'create' to write, 'str_replace' to edit, 'insert' to splice, 'delete' to remove, 'rename' to move.",
		tool.EnumProperty("command", "string",
			"The memory operation to perform.",
			"view", "create", "str_replace", "insert", "delete", "rename"),
		tool.Property("path", "string",
			"Target path. Must begin with "+PathPrefix+"/. Required for view, create, str_replace, insert, delete."),
		tool.Property("file_text", "string",
			"Full file contents (create only)."),
		tool.Property("old_str", "string",
			"Substring to replace (str_replace only). Must match exactly once."),
		tool.Property("new_str", "string",
			"Replacement string (str_replace only)."),
		tool.Property("insert_line", "integer",
			"0-based line index after which insert_text is spliced (insert only). 0 inserts before the first line."),
		tool.Property("insert_text", "string",
			"Text to splice into the file (insert only). A trailing newline is appended if missing."),
		tool.Property("old_path", "string",
			"Source path for rename. Must begin with "+PathPrefix+"/."),
		tool.Property("new_path", "string",
			"Destination path for rename. Must begin with "+PathPrefix+"/."),
		tool.ArrayProperty("view_range",
			"Optional 2-element [start, end] 1-based inclusive line range for view. Use -1 for end-of-file.",
			map[string]any{"type": "integer"}),
	).Required("command").Build()
}

// Metadata implements [tool.ToolMetadata]. Every command except
// view mutates the underlying store, and bandwidth is dominated
// by file size rather than call rate, so we declare side-effects
// without claiming a specific RateLimit.
func (t *Tool) Metadata() tool.ToolMeta {
	return tool.ToolMeta{MutatesState: true}
}

type args struct {
	Command    string `json:"command"`
	Path       string `json:"path"`
	FileText   string `json:"file_text"`
	OldStr     string `json:"old_str"`
	NewStr     string `json:"new_str"`
	InsertLine *int   `json:"insert_line"`
	InsertText string `json:"insert_text"`
	OldPath    string `json:"old_path"`
	NewPath    string `json:"new_path"`
	ViewRange  []int  `json:"view_range"`
}

// Execute implements [tool.Tool].
func (t *Tool) Execute(ctx context.Context, arguments string) (string, error) {
	var a args
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", errdefs.Validationf("memory: parse args: %v", err)
	}
	switch a.Command {
	case "view":
		return t.view(ctx, a)
	case "create":
		return t.create(ctx, a)
	case "str_replace":
		return t.strReplace(ctx, a)
	case "insert":
		return t.insert(ctx, a)
	case "delete":
		return t.del(ctx, a)
	case "rename":
		return t.rename(ctx, a)
	default:
		return "", errdefs.Validationf("memory: unknown command %q", a.Command)
	}
}

// stripPrefix validates and strips PathPrefix, returning the
// workspace-relative path. An empty result is the workspace root.
func stripPrefix(p string) (string, error) {
	if p == "" {
		return "", errdefs.Validationf("memory: path is required")
	}
	if !strings.HasPrefix(p, PathPrefix) {
		return "", errdefs.Validationf("memory: path %q must begin with %s", p, PathPrefix)
	}
	rel := strings.TrimPrefix(p, PathPrefix)
	rel = strings.TrimPrefix(rel, "/")
	if strings.Contains(rel, "..") {
		return "", errdefs.Validationf("memory: path traversal denied")
	}
	return rel, nil
}

func (t *Tool) view(ctx context.Context, a args) (string, error) {
	rel, err := stripPrefix(a.Path)
	if err != nil {
		return "", err
	}

	// Empty rel == list workspace root; otherwise probe Stat to
	// distinguish file from directory.
	if rel == "" {
		return t.viewDir(ctx, "")
	}
	info, err := t.ws.Stat(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("memory.view: %w", err)
	}
	if info.IsDir() {
		return t.viewDir(ctx, rel)
	}
	return t.viewFile(ctx, rel, a.ViewRange)
}

func (t *Tool) viewDir(ctx context.Context, rel string) (string, error) {
	entries, err := t.ws.List(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("memory.view: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)

	display := PathPrefix + "/" + rel
	display = strings.TrimSuffix(display, "/")
	if rel == "" {
		display = PathPrefix + "/"
	} else {
		display += "/"
	}
	out := map[string]any{
		"path":    display,
		"entries": names,
	}
	return marshal(out), nil
}

func (t *Tool) viewFile(ctx context.Context, rel string, viewRange []int) (string, error) {
	data, err := t.ws.Read(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("memory.view: %w", err)
	}
	truncated := false
	if int64(len(data)) > t.maxBytes {
		data = data[:t.maxBytes]
		truncated = true
	}
	lines := strings.Split(string(data), "\n")
	start, end := 1, len(lines)
	if len(viewRange) == 2 {
		start = viewRange[0]
		end = viewRange[1]
		if start < 1 {
			start = 1
		}
		if end == -1 || end > len(lines) {
			end = len(lines)
		}
		if start > end {
			return "", errdefs.Validationf("memory.view: invalid view_range %v", viewRange)
		}
	}

	var b strings.Builder
	for i := start - 1; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	out := map[string]any{
		"path":      PathPrefix + "/" + rel,
		"content":   b.String(),
		"truncated": truncated,
	}
	return marshal(out), nil
}

func (t *Tool) create(ctx context.Context, a args) (string, error) {
	rel, err := stripPrefix(a.Path)
	if err != nil {
		return "", err
	}
	if rel == "" {
		return "", errdefs.Validationf("memory.create: cannot create at root")
	}
	if err := t.ws.Write(ctx, rel, []byte(a.FileText)); err != nil {
		return "", fmt.Errorf("memory.create: %w", err)
	}
	return marshal(map[string]any{
		"path":  a.Path,
		"bytes": len(a.FileText),
	}), nil
}

func (t *Tool) strReplace(ctx context.Context, a args) (string, error) {
	rel, err := stripPrefix(a.Path)
	if err != nil {
		return "", err
	}
	if a.OldStr == "" {
		return "", errdefs.Validationf("memory.str_replace: old_str is required")
	}
	data, err := t.ws.Read(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("memory.str_replace: %w", err)
	}
	body := string(data)
	count := strings.Count(body, a.OldStr)
	if count == 0 {
		return "", errdefs.NotFoundf("memory.str_replace: old_str not found in %s", a.Path)
	}
	if count > 1 {
		return "", errdefs.Conflictf("memory.str_replace: old_str matches %d times in %s; tighten the snippet", count, a.Path)
	}
	updated := strings.Replace(body, a.OldStr, a.NewStr, 1)
	if err := t.ws.Write(ctx, rel, []byte(updated)); err != nil {
		return "", fmt.Errorf("memory.str_replace: %w", err)
	}
	return marshal(map[string]any{"path": a.Path, "replaced": 1}), nil
}

func (t *Tool) insert(ctx context.Context, a args) (string, error) {
	rel, err := stripPrefix(a.Path)
	if err != nil {
		return "", err
	}
	if a.InsertLine == nil {
		return "", errdefs.Validationf("memory.insert: insert_line is required")
	}
	at := *a.InsertLine
	data, err := t.ws.Read(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("memory.insert: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if at < 0 || at > len(lines) {
		return "", errdefs.Validationf("memory.insert: insert_line %d out of range [0,%d]", at, len(lines))
	}
	insertText := a.InsertText
	if !strings.HasSuffix(insertText, "\n") {
		insertText += "\n"
	}
	newLines := strings.Split(strings.TrimSuffix(insertText, "\n"), "\n")
	out := make([]string, 0, len(lines)+len(newLines))
	out = append(out, lines[:at]...)
	out = append(out, newLines...)
	out = append(out, lines[at:]...)
	if err := t.ws.Write(ctx, rel, []byte(strings.Join(out, "\n"))); err != nil {
		return "", fmt.Errorf("memory.insert: %w", err)
	}
	return marshal(map[string]any{"path": a.Path, "inserted": len(newLines)}), nil
}

func (t *Tool) del(ctx context.Context, a args) (string, error) {
	rel, err := stripPrefix(a.Path)
	if err != nil {
		return "", err
	}
	if rel == "" {
		return "", errdefs.Validationf("memory.delete: refusing to delete root")
	}
	info, err := t.ws.Stat(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("memory.delete: %w", err)
	}
	if info.IsDir() {
		if err := t.ws.RemoveAll(ctx, rel); err != nil {
			return "", fmt.Errorf("memory.delete: %w", err)
		}
	} else {
		if err := t.ws.Delete(ctx, rel); err != nil {
			return "", fmt.Errorf("memory.delete: %w", err)
		}
	}
	return marshal(map[string]any{"path": a.Path, "deleted": true}), nil
}

func (t *Tool) rename(ctx context.Context, a args) (string, error) {
	src, err := stripPrefix(a.OldPath)
	if err != nil {
		return "", fmt.Errorf("memory.rename old_path: %w", err)
	}
	dst, err := stripPrefix(a.NewPath)
	if err != nil {
		return "", fmt.Errorf("memory.rename new_path: %w", err)
	}
	if src == "" || dst == "" {
		return "", errdefs.Validationf("memory.rename: cannot rename root")
	}
	if err := t.ws.Rename(ctx, src, dst); err != nil {
		return "", fmt.Errorf("memory.rename: %w", err)
	}
	return marshal(map[string]any{
		"old_path": a.OldPath,
		"new_path": a.NewPath,
	}), nil
}

func marshal(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

// Compile-time interface checks.
var (
	_ tool.Tool         = (*Tool)(nil)
	_ tool.ToolMetadata = (*Tool)(nil)
)
