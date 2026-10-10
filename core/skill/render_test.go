package skill

import (
	"context"
	"strings"
	"testing"
)

// TestRenderSectionGolden pins the exact per-turn section format: the
// header names the paired tools, each row is "- name: description
// (file: path)", and the section carries no trailing newline.
func TestRenderSectionGolden(t *testing.T) {
	sk := Metadata{
		Name:        "review",
		Description: "review code and docs",
		Path:        "/skills/review/SKILL.md",
	}
	want := "## Skills\n" +
		"Skills relevant to this turn. To use one, search with skill_search " +
		"and load its full instructions with skill_read. The user can also " +
		"activate a skill by mentioning $name.\n" +
		"- review: review code and docs (file: /skills/review/SKILL.md)"
	if got := RenderSection([]Metadata{sk}); got != want {
		t.Fatalf("RenderSection() = %q, want %q", got, want)
	}
}

func TestRenderSectionEmpty(t *testing.T) {
	if got := RenderSection(nil); got != "" {
		t.Fatalf("RenderSection(nil) = %q, want empty", got)
	}
	if got := RenderSection([]Metadata{}); got != "" {
		t.Fatalf("RenderSection(empty) = %q, want empty", got)
	}
}

func TestRenderSectionCarriesNoBody(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "review", "name: review\ndescription: review code\n")
	svc := NewService(context.Background(), Options{Roots: []string{root}})
	list := svc.List()
	if len(list) != 1 {
		t.Fatalf("List() = %+v, want one skill", list)
	}
	sec := RenderSection(list)
	if !strings.Contains(sec, "review") ||
		!strings.Contains(sec, list[0].Path) ||
		strings.Contains(sec, "Do the thing.") {
		t.Fatalf("RenderSection = %q, want metadata only (no body)", sec)
	}
}
