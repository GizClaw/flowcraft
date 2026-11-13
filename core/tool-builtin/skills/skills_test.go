package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/skill"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

func writeSkill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, skill.SkillFile), []byte(doc), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func catalog(t *testing.T) *Tool {
	t.Helper()
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "alpha"), "alpha", "handles alpha payloads", "Alpha body.\n")
	writeSkill(t, filepath.Join(root, "beta"), "beta", "handles beta payloads", "Beta body.\n")

	tree, err := fsx.Open()
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	discovered := skill.Discover(context.Background(), tree, []string{root})
	if len(discovered.Skills) != 2 {
		t.Fatalf("discovered %d skills, want 2: %+v", len(discovered.Skills), discovered.Diagnostics)
	}
	index, err := skill.NewIndex(discovered.Skills)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	group, err := New(tree, index)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return group
}

func find(t *testing.T, group *Tool, name string) tool.Tool {
	t.Helper()
	for _, candidate := range group.Tools() {
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
		t.Fatalf("marshal: %v", err)
	}
	content, err := candidate.Execute(context.Background(), string(payload))
	if err != nil {
		return "", err
	}
	return content.Text(), nil
}

func TestSearchRanksAndLists(t *testing.T) {
	group := catalog(t)
	search := find(t, group, SearchName)

	text, err := call(t, search, map[string]any{"query": "beta payloads"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(text), &hits); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	if len(hits) == 0 || hits[0]["name"] != "beta" {
		t.Errorf("search hits = %v, want beta first", hits)
	}
	if hits[0]["score"].(float64) <= 0 {
		t.Errorf("ranked hit carries no score: %v", hits[0])
	}

	text, err = call(t, search, map[string]any{"query": ""})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	hits = nil
	if err := json.Unmarshal([]byte(text), &hits); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	if len(hits) != 2 || hits[0]["name"] != "alpha" || hits[1]["name"] != "beta" {
		t.Errorf("catalog listing = %v, want alpha then beta", hits)
	}
}

// Loading returns the whole document, not just its metadata: the body is what
// the model needs to follow the skill.
func TestReadReturnsTheBody(t *testing.T) {
	group := catalog(t)
	text, err := call(t, find(t, group, ReadName), map[string]any{"name": "alpha"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"# Skill: alpha", "SKILL.md", "Alpha body."} {
		if !strings.Contains(text, want) {
			t.Errorf("read result misses %q:\n%s", want, text)
		}
	}
}

func TestReadUnknownSkillIsRejected(t *testing.T) {
	group := catalog(t)
	if _, err := call(t, find(t, group, ReadName), map[string]any{"name": "absent"}); err == nil {
		t.Fatal("unknown skill accepted")
	}
}

func TestReadRequiresName(t *testing.T) {
	group := catalog(t)
	if _, err := call(t, find(t, group, ReadName), map[string]any{}); err == nil {
		t.Fatal("read without a name accepted")
	}
}

// A search limit is bounded: the cap protects the model's context, not the
// catalog.
func TestSearchClampsLimit(t *testing.T) {
	group := catalog(t)
	previous := maxSearchLimit
	maxSearchLimit = 1
	t.Cleanup(func() { maxSearchLimit = previous })

	text, err := call(t, find(t, group, SearchName), map[string]any{"query": "", "limit": 100})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(text), &hits); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	if len(hits) != 1 {
		t.Errorf("hits = %d, want the clamped 1", len(hits))
	}
}
