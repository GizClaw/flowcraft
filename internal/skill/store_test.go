package skill

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func setupSkillStore(t *testing.T) (*SkillStore, workspace.Workspace) {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	skill1 := "---\nname: analyzer\ndescription: Data analysis tool\ntags: [python, data]\nentry: main.py\n---\n# Analyzer"
	skill2 := "---\nname: formatter\ndescription: Code formatting\ntags: [go, lint]\nentry: format.sh\n---\n# Formatter"

	_ = ws.Write(ctx, "skills/analyzer/SKILL.md", []byte(skill1))
	_ = ws.Write(ctx, "skills/formatter/SKILL.md", []byte(skill2))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)
	return store, ws
}

func TestSkillStore_BuildIndex(t *testing.T) {
	store, _ := setupSkillStore(t)

	meta, ok := store.Get("analyzer")
	if !ok {
		t.Fatal("expected analyzer skill")
	}
	if meta.Description != "Data analysis tool" {
		t.Fatalf("unexpected description: %q", meta.Description)
	}

	_, ok = store.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestSkillStore_Search(t *testing.T) {
	store, _ := setupSkillStore(t)

	results := store.Search("data analysis", nil)
	if len(results) == 0 {
		t.Fatal("expected search results")
	}

	found := false
	for _, r := range results {
		if r.Name == "analyzer" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected to find 'analyzer'")
	}
}

func TestSkillStore_Search_BM25Ranking(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	_ = ws.Write(ctx, "skills/pg-backup/SKILL.md", []byte(
		"---\nname: pg-backup\ndescription: PostgreSQL database backup and restore tool\ntags: [postgres, database, backup]\n---\n"))
	_ = ws.Write(ctx, "skills/log-viewer/SKILL.md", []byte(
		"---\nname: log-viewer\ndescription: View and search application log files\ntags: [logs, search]\n---\n"))
	_ = ws.Write(ctx, "skills/db-migrate/SKILL.md", []byte(
		"---\nname: db-migrate\ndescription: Database schema migration runner\ntags: [database, migration]\n---\n"))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	results := store.Search("database backup", nil)
	if len(results) == 0 {
		t.Fatal("expected results for 'database backup'")
	}
	if results[0].Name != "pg-backup" {
		t.Fatalf("pg-backup should rank first, got %q", results[0].Name)
	}

	// log-viewer should not match "database backup"
	for _, r := range results {
		if r.Name == "log-viewer" {
			t.Fatal("log-viewer should not match 'database backup'")
		}
	}
}

func TestSkillStore_SearchWithWhitelist(t *testing.T) {
	store, _ := setupSkillStore(t)

	results := store.Search("analysis", []string{"formatter"})
	for _, r := range results {
		if r.Name == "analyzer" {
			t.Fatal("analyzer should be filtered by whitelist")
		}
	}
}

func TestSkillStore_List(t *testing.T) {
	store, _ := setupSkillStore(t)

	all := store.List(nil)
	if len(all) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(all))
	}

	filtered := store.List([]string{"analyzer"})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered, got %d", len(filtered))
	}
}

func TestSkillStore_BuildIndex_DetectEntry(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	noEntry := "---\nname: evolver\ndescription: Self-evolution engine\ntags: [meta, ai]\n---\n# Evolver"
	_ = ws.Write(ctx, "skills/evolver/SKILL.md", []byte(noEntry))
	_ = ws.Write(ctx, "skills/evolver/index.js", []byte("console.log('hi')"))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	meta, ok := store.Get("evolver")
	if !ok {
		t.Fatal("expected evolver skill to be indexed with auto-detected entry")
	}
	if meta.Entry != "index.js" {
		t.Fatalf("expected auto-detected entry 'index.js', got %q", meta.Entry)
	}
}

func TestSkillStore_BuildIndex_DocumentSkill(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	docSkill := "---\nname: weather\ndescription: Get current weather via wttr.in\n---\n# Weather\n\n```bash\ncurl wttr.in/London\n```"
	_ = ws.Write(ctx, "skills/weather/SKILL.md", []byte(docSkill))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	meta, ok := store.Get("weather")
	if !ok {
		t.Fatal("document-based skill should be indexed even without entry point")
	}
	if meta.Entry != "" {
		t.Fatalf("expected empty entry for document skill, got %q", meta.Entry)
	}
}

func TestSkillStore_SyncBuiltins(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	builtinFS := fstest.MapFS{
		"weather/SKILL.md": &fstest.MapFile{
			Data: []byte("---\nname: weather\ndescription: Get weather\n---\n# Weather"),
		},
	}

	store := NewSkillStore(ws, "skills")
	store.SetBuiltinFS(builtinFS)
	_ = store.BuildIndex(ctx)

	meta, ok := store.Get("weather")
	if !ok {
		t.Fatal("builtin skill should be indexed")
	}
	if meta.Description != "Get weather" {
		t.Fatalf("unexpected description: %q", meta.Description)
	}
}

func TestSkillStore_SyncBuiltins_NoOverwrite(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	_ = ws.Write(ctx, "skills/weather/SKILL.md", []byte(
		"---\nname: weather\ndescription: User customized\n---\n# Custom"))

	builtinFS := fstest.MapFS{
		"weather/SKILL.md": &fstest.MapFile{
			Data: []byte("---\nname: weather\ndescription: Builtin version\n---\n# Builtin"),
		},
	}

	store := NewSkillStore(ws, "skills")
	store.SetBuiltinFS(builtinFS)
	_ = store.BuildIndex(ctx)

	meta, ok := store.Get("weather")
	if !ok {
		t.Fatal("skill should be indexed")
	}
	if meta.Description != "User customized" {
		t.Fatalf("builtin should not overwrite existing, got %q", meta.Description)
	}
}

func TestSkillStore_GetReadme(t *testing.T) {
	store, _ := setupSkillStore(t)

	readme, ok := store.GetReadme("analyzer")
	if !ok {
		t.Fatal("expected readme")
	}
	if readme == "" {
		t.Fatal("readme should not be empty")
	}
}

func TestSkillStore_BuildIndex_GatingAvailable(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: simple\ndescription: No deps\nentry: main.py\n---\n"
	_ = ws.Write(ctx, "skills/simple/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	meta, ok := store.Get("simple")
	if !ok {
		t.Fatal("expected skill to be indexed")
	}
	if meta.Gating == nil {
		t.Fatal("expected non-nil Gating")
	}
	if !meta.Gating.Available {
		t.Fatal("skill with no requires should be available")
	}
}

func TestSkillStore_BuildIndex_GatingWithRequires(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: needs-deps\ndescription: Needs deps\nrequires:\n  bins: [__nonexistent_gating_test_bin__]\n---\n"
	_ = ws.Write(ctx, "skills/needs-deps/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	meta, ok := store.Get("needs-deps")
	if !ok {
		t.Fatal("skill with missing bins should still be indexed")
	}
	if meta.Gating == nil {
		t.Fatal("expected non-nil Gating")
	}
	if meta.Gating.Available {
		t.Fatal("should be unavailable with missing bin")
	}
	if len(meta.Gating.MissingBins) != 1 {
		t.Fatalf("expected 1 missing bin, got %d", len(meta.Gating.MissingBins))
	}
}

func TestSkillStore_BuildIndex_GatingOSSkip(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: os-skip\ndescription: Wrong OS\nrequires:\n  os: [__unsupported_os__]\n---\n"
	_ = ws.Write(ctx, "skills/os-skip/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	_, ok := store.Get("os-skip")
	if ok {
		t.Fatal("skill with unsupported OS should NOT be indexed")
	}
}

func TestSkillStore_Get_DeepCopyGating(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: test\ndescription: test\nrequires:\n  bins: [__nonexistent_deep_copy_bin__]\n---\n"
	_ = ws.Write(ctx, "skills/test/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	meta1, _ := store.Get("test")
	meta2, _ := store.Get("test")

	if meta1.Gating == nil || meta2.Gating == nil {
		t.Fatal("both copies should have Gating")
	}

	meta1.Gating.Available = !meta1.Gating.Available
	if meta1.Gating.Available == meta2.Gating.Available {
		t.Fatal("deep copy failed: modifying one copy affected the other")
	}
}

func TestSkillStore_Get_DeepCopyRequires(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: req-copy\ndescription: test\nrequires:\n  bins: [curl]\n  env: [KEY]\n---\n"
	_ = ws.Write(ctx, "skills/req-copy/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	meta1, _ := store.Get("req-copy")
	meta2, _ := store.Get("req-copy")

	meta1.Requires.Bins[0] = "wget"
	if meta2.Requires.Bins[0] == "wget" {
		t.Fatal("Requires.Bins should be deep-copied")
	}
}

func TestSkillStore_List_DeepCopyGating(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: list-test\ndescription: test\nrequires:\n  bins: [__missing__]\n---\n"
	_ = ws.Write(ctx, "skills/list-test/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	list1 := store.List(nil)
	list2 := store.List(nil)

	if len(list1) != 1 || len(list2) != 1 {
		t.Fatalf("expected 1 skill each, got %d and %d", len(list1), len(list2))
	}

	list1[0].Gating.Available = true
	if list2[0].Gating.Available {
		t.Fatal("List should return deep copies")
	}
}

func TestSkillStore_IsEnabled(t *testing.T) {
	store, _ := setupSkillStore(t)

	if !store.IsEnabled("analyzer") {
		t.Fatal("skill without config should be enabled by default")
	}

	disabled := false
	store.SetGlobalConfig(config.SkillsConfig{
		Entries: map[string]config.SkillEntryConfig{
			"analyzer": {Enabled: &disabled},
		},
	})

	if store.IsEnabled("analyzer") {
		t.Fatal("skill should be disabled after config")
	}
	if !store.IsEnabled("formatter") {
		t.Fatal("unconfigured skill should remain enabled")
	}
}

func TestSkillStore_ResolveEnv(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: img\ndescription: Image gen\nprimary_env: OPENAI_API_KEY\nentry: main.py\n---\n"
	_ = ws.Write(ctx, "skills/img/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	store.SetGlobalConfig(config.SkillsConfig{
		Entries: map[string]config.SkillEntryConfig{
			"img": {
				APIKey: "sk-abc",
				Env:    map[string]string{"MODEL": "dall-e-3"},
			},
		},
	})

	env := store.ResolveEnv("img")
	if env == nil {
		t.Fatal("expected env map")
	}
	if env["OPENAI_API_KEY"] != "sk-abc" {
		t.Fatalf("expected APIKey mapped to PrimaryEnv, got %q", env["OPENAI_API_KEY"])
	}
	if env["MODEL"] != "dall-e-3" {
		t.Fatalf("expected MODEL 'dall-e-3', got %q", env["MODEL"])
	}
}

func TestSkillStore_ResolveEnv_NoConfig(t *testing.T) {
	store, _ := setupSkillStore(t)
	env := store.ResolveEnv("analyzer")
	if env != nil {
		t.Fatal("no config should return nil env")
	}
}

func TestSkillStore_BuiltinSkills(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	builtinFS := fstest.MapFS{
		"github/SKILL.md":       {Data: []byte("---\nname: github\ndescription: GitHub CLI\ntags: [github]\n---\n# GitHub")},
		"summarize/SKILL.md":    {Data: []byte("---\nname: summarize\ndescription: Summarize text\ntags: [text]\n---\n# Summarize")},
		"coding-agent/SKILL.md": {Data: []byte("---\nname: coding-agent\ndescription: Coding best practices\ntags: [coding]\n---\n# Coding")},
	}

	store := NewSkillStore(ws, "skills")
	store.SetBuiltinFS(builtinFS)
	if err := store.BuildIndex(ctx); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"github", "summarize", "coding-agent"} {
		meta, ok := store.Get(name)
		if !ok {
			t.Fatalf("builtin skill %q should be indexed", name)
		}
		if !meta.Builtin {
			t.Fatalf("skill %q should be marked as builtin", name)
		}
		if !store.IsBuiltin(name) {
			t.Fatalf("IsBuiltin(%q) should be true", name)
		}
	}

	results := store.Search("github", nil)
	found := false
	for _, r := range results {
		if r.Name == "github" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("github should appear in search results")
	}
}

func TestLockfile_SetGetRemove(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	lf := NewLockfile(ws, "skills")
	lf.Set("my-skill", &SkillSource{GitURL: "https://github.com/example/my-skill.git", Commit: "abc1234"})
	if err := lf.Save(ctx); err != nil {
		t.Fatal(err)
	}

	lf2 := NewLockfile(ws, "skills")
	if err := lf2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	src := lf2.Get("my-skill")
	if src == nil {
		t.Fatal("expected lockfile entry")
	}
	if src.GitURL != "https://github.com/example/my-skill.git" {
		t.Fatalf("unexpected git_url %q", src.GitURL)
	}
	if src.Commit != "abc1234" {
		t.Fatalf("unexpected commit %q", src.Commit)
	}
	if src.InstalledAt == "" {
		t.Fatal("installed_at should be auto-set")
	}

	lf2.Remove("my-skill")
	if lf2.Get("my-skill") != nil {
		t.Fatal("entry should be removed")
	}
}

func TestLockfile_Load_MissingFile(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	lf := NewLockfile(ws, "skills")
	if err := lf.Load(context.Background()); err != nil {
		t.Fatalf("missing lockfile should not error: %v", err)
	}
	if lf.Get("anything") != nil {
		t.Fatal("should have no entries")
	}
}

func TestSkillStore_GetSource(t *testing.T) {
	store, _ := setupSkillStore(t)
	if store.GetSource("analyzer") != nil {
		t.Fatal("non-git skill should have no source")
	}
}

func TestSkillStore_Search_DeepCopyGating(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	content := "---\nname: search-test\ndescription: searchable\nrequires:\n  bins: [__missing__]\n---\n"
	_ = ws.Write(ctx, "skills/search-test/SKILL.md", []byte(content))

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	res1 := store.Search("searchable", nil)
	res2 := store.Search("searchable", nil)

	if len(res1) != 1 || len(res2) != 1 {
		t.Fatalf("expected 1 result each, got %d and %d", len(res1), len(res2))
	}

	res1[0].Gating.Available = true
	if res2[0].Gating.Available {
		t.Fatal("Search should return deep copies")
	}
}
