package skill

import "testing"

func TestParseSkillMeta_Valid(t *testing.T) {
	content := `---
name: data-analysis
description: Analyze data using pandas
tags: [python, analysis]
entry: main.py
---
# Data Analysis Skill
Detailed docs here.`

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "data-analysis" {
		t.Fatalf("expected data-analysis, got %q", meta.Name)
	}
	if meta.Description != "Analyze data using pandas" {
		t.Fatalf("unexpected description: %q", meta.Description)
	}
	if len(meta.Tags) != 2 || meta.Tags[0] != "python" || meta.Tags[1] != "analysis" {
		t.Fatalf("unexpected tags: %v", meta.Tags)
	}
	if meta.Entry != "main.py" {
		t.Fatalf("expected main.py, got %q", meta.Entry)
	}
}

func TestParseSkillMeta_MissingFrontmatter(t *testing.T) {
	_, err := ParseSkillMeta("# No frontmatter")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseSkillMeta_MissingName(t *testing.T) {
	content := "---\ndescription: test\nentry: main.py\n---\n"
	_, err := ParseSkillMeta(content)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestParseSkillMeta_MissingEntry(t *testing.T) {
	content := "---\nname: test\ndescription: test\n---\n"
	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Entry != "" {
		t.Fatalf("expected empty entry, got %q", meta.Entry)
	}
}

func TestParseSkillMeta_QuotedDescription(t *testing.T) {
	content := `---
name: weather
description: "Get current weather and forecasts via wttr.in. No API key needed."
---
# Weather`

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "Get current weather and forecasts via wttr.in. No API key needed." {
		t.Fatalf("expected unquoted description, got %q", meta.Description)
	}
}

func TestParseSkillMeta_DescriptionWithColon(t *testing.T) {
	content := `---
name: weather
description: "Get weather. Use when: user asks about temperature."
homepage: https://wttr.in/:help
---
# Weather`

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "Get weather. Use when: user asks about temperature." {
		t.Fatalf("unexpected description: %q", meta.Description)
	}
	if meta.Homepage != "https://wttr.in/:help" {
		t.Fatalf("unexpected homepage: %q", meta.Homepage)
	}
}

func TestParseSkillMeta_OpenClawFormat(t *testing.T) {
	content := `---
name: xurl
description: A CLI tool for making authenticated requests to the X API.
metadata:
 {
 "openclaw":
 {
 "emoji": "X",
 "requires": { "bins": ["xurl"] }
 }
 }
---
# xurl`

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "xurl" {
		t.Fatalf("expected xurl, got %q", meta.Name)
	}
	if meta.Description != "A CLI tool for making authenticated requests to the X API." {
		t.Fatalf("unexpected description: %q", meta.Description)
	}
	if meta.Entry != "" {
		t.Fatalf("expected empty entry, got %q", meta.Entry)
	}
}

func TestParseSkillMeta_OpenClawWeather(t *testing.T) {
	content := `---
name: weather
description: "Get current weather and forecasts via wttr.in or Open-Meteo. Use when: user asks about weather, temperature, or forecasts for any location. NOT for: historical weather data, severe weather alerts, or detailed meteorological analysis. No API key needed."
homepage: https://wttr.in/:help
metadata: { "openclaw": { "emoji": "🌤️", "requires": { "bins": ["curl"] } } }
---
# Weather Skill`

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "weather" {
		t.Fatalf("expected weather, got %q", meta.Name)
	}
	if !contains(meta.Description, "Get current weather") {
		t.Fatalf("unexpected description: %q", meta.Description)
	}
	if meta.Homepage != "https://wttr.in/:help" {
		t.Fatalf("unexpected homepage: %q", meta.Homepage)
	}
}

func TestParseSkillMeta_MultilineMetadata(t *testing.T) {
	content := `---
name: test-multi
description: Test skill with multiline metadata
metadata:
  openclaw:
    emoji: "🔧"
    requires:
      bins:
        - curl
        - jq
---
# Test`

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "test-multi" {
		t.Fatalf("expected test-multi, got %q", meta.Name)
	}
}

func TestParseSkillMeta_YAMLFallbackToLine(t *testing.T) {
	// Intentionally bad YAML but valid line format
	content := "---\nname: fallback-skill\ndescription: This works\n\t\tbadindent:\n---\n"
	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "fallback-skill" {
		t.Fatalf("expected fallback-skill, got %q", meta.Name)
	}
}

func TestParseTags(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"bracket", "[python, analysis, ml]", 3},
		{"json_array", `["python","analysis"]`, 2},
		{"empty_bracket", "[]", 0},
		{"empty_string", "", 0},
		{"plain", "python, analysis", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tags := parseTags(tt.input)
			if len(tags) != tt.want {
				t.Fatalf("expected %d tags, got %d: %v", tt.want, len(tags), tags)
			}
		})
	}
}

func TestUnquote(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{`"hello world"`, "hello world"},
		{`'single'`, "single"},
		{`no quotes`, "no quotes"},
		{`""`, ""},
		{`"`, `"`},
		{``, ``},
	}
	for _, tt := range tests {
		got := unquote(tt.input)
		if got != tt.want {
			t.Errorf("unquote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFullReadme(t *testing.T) {
	content := "---\nname: test\n---\n# Docs"
	if FullReadme(content) != content {
		t.Fatal("FullReadme should return full content")
	}
}

func TestExtractFrontmatter(t *testing.T) {
	content := "---\nname: test\ndescription: hello\n---\n# Body"
	block, err := extractFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(block, "name: test") {
		t.Fatalf("unexpected block: %q", block)
	}
}

func TestExtractFrontmatter_NoClosure(t *testing.T) {
	_, err := extractFrontmatter("---\nname: test\nno closing")
	if err == nil {
		t.Fatal("expected error for unclosed frontmatter")
	}
}

func TestMergeFields(t *testing.T) {
	yamlParsed := map[string]string{
		"name":        "test",
		"description": "desc",
		"metadata":    `{"openclaw":{"emoji":"X"}}`,
	}
	lineParsed := map[string]string{
		"name":     "test",
		"metadata": `{ "openclaw": { "emoji": "X" } }`,
	}

	merged := mergeFields(yamlParsed, lineParsed)
	if merged["metadata"] != lineParsed["metadata"] {
		t.Fatalf("JSON-like value should prefer line-parsed, got %q", merged["metadata"])
	}
	if merged["description"] != "desc" {
		t.Fatalf("non-JSON value should use YAML-parsed, got %q", merged["description"])
	}
}

func TestMergeFields_NilYAML(t *testing.T) {
	lineParsed := map[string]string{"name": "test"}
	merged := mergeFields(nil, lineParsed)
	if merged["name"] != "test" {
		t.Fatal("should fallback to line-parsed when YAML is nil")
	}
}

func TestParseRequires_OpenClawFormat(t *testing.T) {
	content := "---\nname: weather\ndescription: Get weather\nmetadata:\n  openclaw:\n    requires:\n      bins: [curl]\n---\n# Weather"

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Requires == nil {
		t.Fatal("expected non-nil Requires")
	}
	if len(meta.Requires.Bins) != 1 || meta.Requires.Bins[0] != "curl" {
		t.Fatalf("unexpected bins: %v", meta.Requires.Bins)
	}
}

func TestParseRequires_TopLevel(t *testing.T) {
	content := "---\nname: my-tool\ndescription: A tool\nrequires:\n  bins: [jq, python3]\n  env: [API_KEY]\n  os: [linux, darwin]\n---\n"

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Requires == nil {
		t.Fatal("expected non-nil Requires")
	}
	if len(meta.Requires.Bins) != 2 {
		t.Fatalf("expected 2 bins, got %d", len(meta.Requires.Bins))
	}
	if len(meta.Requires.Env) != 1 || meta.Requires.Env[0] != "API_KEY" {
		t.Fatalf("unexpected env: %v", meta.Requires.Env)
	}
	if len(meta.Requires.OS) != 2 {
		t.Fatalf("unexpected os: %v", meta.Requires.OS)
	}
}

func TestParseRequires_None(t *testing.T) {
	content := "---\nname: simple\ndescription: No deps\n---\n"
	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Requires != nil {
		t.Fatalf("expected nil Requires, got %+v", meta.Requires)
	}
}

func TestParseRequires_OpenClawPriority(t *testing.T) {
	content := "---\nname: dual\ndescription: Both formats\nrequires:\n  bins: [top-level]\nmetadata:\n  openclaw:\n    requires:\n      bins: [openclaw-bin]\n---\n"

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Requires == nil {
		t.Fatal("expected non-nil Requires")
	}
	if len(meta.Requires.Bins) != 1 || meta.Requires.Bins[0] != "openclaw-bin" {
		t.Fatalf("openclaw format should take priority, got bins=%v", meta.Requires.Bins)
	}
}

func TestParseRequires_AnyBins(t *testing.T) {
	content := "---\nname: tool\ndescription: test\nrequires:\n  any_bins: [curl, wget]\n---\n"

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Requires == nil {
		t.Fatal("expected non-nil Requires")
	}
	if len(meta.Requires.AnyBins) != 2 {
		t.Fatalf("expected 2 any_bins, got %d", len(meta.Requires.AnyBins))
	}
}

func TestParseRequires_EmptyRequires(t *testing.T) {
	content := "---\nname: tool\ndescription: test\nrequires:\n  bins: []\n---\n"

	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Requires != nil {
		t.Fatalf("all-empty requires should be nil, got %+v", meta.Requires)
	}
}

func TestToStringSlice(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want int
	}{
		{"nil", nil, 0},
		{"[]any", []any{"a", "b"}, 2},
		{"[]string", []string{"x"}, 1},
		{"int", 42, 0},
		{"mixed_types", []any{"a", 42, "b"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toStringSlice(tt.val)
			if len(got) != tt.want {
				t.Fatalf("expected %d, got %d: %v", tt.want, len(got), got)
			}
		})
	}
}

func TestCopySkillMeta(t *testing.T) {
	original := &SkillMeta{
		Name: "test",
		Tags: []string{"a", "b"},
		Requires: &SkillRequires{
			Bins: []string{"curl"},
			Env:  []string{"KEY"},
		},
		Gating: &SkillGating{
			Available:   false,
			MissingBins: []string{"curl"},
			MissingEnv:  []string{"KEY"},
		},
	}

	cp := copySkillMeta(original)

	cp.Tags[0] = "modified"
	if original.Tags[0] == "modified" {
		t.Fatal("Tags should be deep-copied")
	}

	cp.Requires.Bins[0] = "wget"
	if original.Requires.Bins[0] == "wget" {
		t.Fatal("Requires.Bins should be deep-copied")
	}

	cp.Gating.Available = true
	if original.Gating.Available {
		t.Fatal("Gating should be deep-copied")
	}

	cp.Gating.MissingBins[0] = "jq"
	if original.Gating.MissingBins[0] == "jq" {
		t.Fatal("Gating.MissingBins should be deep-copied")
	}
}

func TestCopySkillMeta_NilPointers(t *testing.T) {
	original := &SkillMeta{Name: "simple"}
	cp := copySkillMeta(original)
	if cp.Requires != nil || cp.Gating != nil {
		t.Fatal("nil pointer fields should remain nil after copy")
	}
}

func TestParseSkillMeta_PrimaryEnv(t *testing.T) {
	content := "---\nname: img\ndescription: Image gen\nprimary_env: OPENAI_API_KEY\n---\n"
	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PrimaryEnv != "OPENAI_API_KEY" {
		t.Fatalf("expected PrimaryEnv 'OPENAI_API_KEY', got %q", meta.PrimaryEnv)
	}
}

func TestParseSkillMeta_PrimaryEnv_OpenClaw(t *testing.T) {
	content := "---\nname: gemini\ndescription: Gemini CLI\nmetadata: {\"openclaw\":{\"primaryEnv\":\"GEMINI_API_KEY\"}}\n---\n"
	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PrimaryEnv != "GEMINI_API_KEY" {
		t.Fatalf("expected PrimaryEnv 'GEMINI_API_KEY' from openclaw metadata, got %q", meta.PrimaryEnv)
	}
}

func TestParseSkillMeta_NoPrimaryEnv(t *testing.T) {
	content := "---\nname: weather\ndescription: Get weather\n---\n"
	meta, err := ParseSkillMeta(content)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PrimaryEnv != "" {
		t.Fatalf("expected empty PrimaryEnv, got %q", meta.PrimaryEnv)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
