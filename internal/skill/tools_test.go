package skill

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestSkillCallTool_DocumentSkill(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	docSkill := "---\nname: weather\ndescription: Get weather via wttr.in\n---\n# Weather\n\n```bash\ncurl wttr.in/London\n```"
	_ = ws.Write(ctx, "skills/weather/SKILL.md", []byte(docSkill))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	executor := &SkillExecutor{store: store}
	tool := &SkillTool{Store: store, Executor: executor}

	result, err := tool.Execute(ctx, `{"action": "call", "name": "weather"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result != docSkill {
		t.Fatalf("expected SKILL.md content, got %q", result)
	}
}

func TestSkillSearchTool(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	skill1 := "---\nname: analyzer\ndescription: Data analysis tool\ntags: [python]\nentry: main.py\n---\n"
	_ = ws.Write(ctx, "skills/analyzer/SKILL.md", []byte(skill1))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	tool := &SkillTool{Store: store}
	def := tool.Definition()
	if def.Name != "skill" {
		t.Fatalf("expected skill, got %q", def.Name)
	}

	result, err := tool.Execute(ctx, `{"action": "search", "query": "data"}`)
	if err != nil {
		t.Fatal(err)
	}

	var items []map[string]any
	if err := json.Unmarshal([]byte(result), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("expected results")
	}
}

func TestSkillInfoTool(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: analyzer\ndescription: test\nentry: main.py\n---\n# Docs"
	_ = ws.Write(ctx, "skills/analyzer/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	tool := &SkillTool{Store: store}
	result, err := tool.Execute(ctx, `{"action": "info", "name": "analyzer"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result == "" {
		t.Fatal("expected readme content")
	}
}

func TestSkillInfoTool_NotFound(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(context.Background())

	tool := &SkillTool{Store: store}
	_, err := tool.Execute(context.Background(), `{"action": "info", "name": "nonexistent"}`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSkillSearchTool_GatingOutput(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: gated\ndescription: Gated search test\nrequires:\n  bins: [__gating_search_missing__]\nentry: run.sh\n---\n"
	_ = ws.Write(ctx, "skills/gated/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	tool := &SkillTool{Store: store}
	result, err := tool.Execute(ctx, `{"action": "search", "query": "gated"}`)
	if err != nil {
		t.Fatal(err)
	}

	var items []map[string]any
	if err := json.Unmarshal([]byte(result), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 result, got %d", len(items))
	}

	avail, _ := items[0]["available"].(bool)
	if avail {
		t.Fatal("gated skill should not be available")
	}
	deps, ok := items[0]["missing_deps"].([]any)
	if !ok || len(deps) == 0 {
		t.Fatal("gated skill should have missing_deps")
	}
}

func TestSkillInfoTool_GatingWarning(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: warned\ndescription: Warned skill\nrequires:\n  bins: [__gating_info_missing__]\n---\n# Warned Skill Docs"
	_ = ws.Write(ctx, "skills/warned/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	tool := &SkillTool{Store: store}
	result, err := tool.Execute(ctx, `{"action": "info", "name": "warned"}`)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) < 10 || result[:9] != "[WARNING]" {
		t.Fatalf("expected [WARNING] prefix, got %q", result[:min(len(result), 50)])
	}
	if !containsStr(result, "# Warned Skill Docs") {
		t.Fatal("warning should be prepended, not replace content")
	}
}

func TestSkillInfoTool_AvailableNoWarning(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: ok-skill\ndescription: OK\n---\n# OK Docs"
	_ = ws.Write(ctx, "skills/ok-skill/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	tool := &SkillTool{Store: store}
	result, err := tool.Execute(ctx, `{"action": "info", "name": "ok-skill"}`)
	if err != nil {
		t.Fatal(err)
	}

	if containsStr(result, "[WARNING]") {
		t.Fatal("available skill should not have WARNING")
	}
}

func TestSkillCallTool_DocSkill_NoBlock(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	docSkill := "---\nname: gated-doc\ndescription: Doc skill with gating\nrequires:\n  bins: [__gating_doc_missing__]\n---\n# Usage\n\n```bash\ncurl example.com\n```"
	_ = ws.Write(ctx, "skills/gated-doc/SKILL.md", []byte(docSkill))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	executor := &SkillExecutor{store: store}
	tool := &SkillTool{Store: store, Executor: executor}

	result, err := tool.Execute(ctx, `{"action": "call", "name": "gated-doc"}`)
	if err != nil {
		t.Fatalf("document skill should not be blocked by gating: %v", err)
	}
	if !containsStr(result, "missing binaries") {
		t.Fatal("document skill should have gating warning prepended")
	}
	if !containsStr(result, "# Usage") {
		t.Fatal("document skill content should be present")
	}
}

func TestSkillCallTool_ExecSkill_Blocked(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	execSkill := "---\nname: gated-exec\ndescription: Exec skill\nrequires:\n  bins: [__gating_exec_missing__]\nentry: main.py\n---\n"
	_ = ws.Write(ctx, "skills/gated-exec/SKILL.md", []byte(execSkill))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	executor := &SkillExecutor{store: store}
	tool := &SkillTool{Store: store, Executor: executor}

	_, err := tool.Execute(ctx, `{"action": "call", "name": "gated-exec"}`)
	if err == nil {
		t.Fatal("executable skill with missing deps should be blocked")
	}
	if !containsStr(err.Error(), "not available") {
		t.Fatalf("error should mention not available, got %q", err.Error())
	}
}

func TestSkillCallTool_DocSkill_Available_NoWarning(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	docSkill := "---\nname: available-doc\ndescription: Available doc\n---\n# Available Docs"
	_ = ws.Write(ctx, "skills/available-doc/SKILL.md", []byte(docSkill))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	executor := &SkillExecutor{store: store}
	tool := &SkillTool{Store: store, Executor: executor}

	result, err := tool.Execute(ctx, `{"action": "call", "name": "available-doc"}`)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(result, "missing") {
		t.Fatal("available doc skill should not have warning")
	}
}

func TestSkillSearchTool_DisabledFiltered(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	_ = ws.Write(ctx, "skills/active/SKILL.md", []byte("---\nname: active\ndescription: Active skill\n---\n"))
	_ = ws.Write(ctx, "skills/hidden/SKILL.md", []byte("---\nname: hidden\ndescription: Hidden skill\n---\n"))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	disabled := false
	store.SetGlobalConfig(config.SkillsConfig{
		Entries: map[string]config.SkillEntryConfig{
			"hidden": {Enabled: &disabled},
		},
	})

	tool := &SkillTool{Store: store}
	result, err := tool.Execute(ctx, `{"action": "search", "query": "skill"}`)
	if err != nil {
		t.Fatal(err)
	}

	var items []map[string]any
	_ = json.Unmarshal([]byte(result), &items)
	for _, item := range items {
		if item["name"] == "hidden" {
			t.Fatal("disabled skill should not appear in search results")
		}
	}
}

func TestSkillCallTool_DisabledBlocked(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	_ = ws.Write(ctx, "skills/blocked/SKILL.md", []byte("---\nname: blocked\ndescription: Blocked doc\n---\n# Blocked"))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	disabled := false
	store.SetGlobalConfig(config.SkillsConfig{
		Entries: map[string]config.SkillEntryConfig{
			"blocked": {Enabled: &disabled},
		},
	})

	executor := &SkillExecutor{store: store}
	tool := &SkillTool{Store: store, Executor: executor}

	_, err := tool.Execute(ctx, `{"action": "call", "name": "blocked"}`)
	if err == nil {
		t.Fatal("calling disabled skill should return error")
	}
	if !containsStr(err.Error(), "disabled") {
		t.Fatalf("error should mention disabled, got %q", err.Error())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
