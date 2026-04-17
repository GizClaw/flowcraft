package skill

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestStartWatching_LocalWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	skillsDir := filepath.Join(dir, "skills")
	_ = os.MkdirAll(skillsDir, 0o755)

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	w, err := store.StartWatching(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w == nil {
		t.Fatal("expected watcher for LocalWorkspace")
	}
	defer w.Stop()

	// Create a new skill directory and SKILL.md
	newSkillDir := filepath.Join(skillsDir, "new-skill")
	_ = os.MkdirAll(newSkillDir, 0o755)
	skillContent := "---\nname: new-skill\ndescription: A new skill\nentry: run.py\n---\n# New Skill"
	_ = os.WriteFile(filepath.Join(newSkillDir, "SKILL.md"), []byte(skillContent), 0o644)

	// Wait for debounce + rebuild
	time.Sleep(1500 * time.Millisecond)

	meta, ok := store.Get("new-skill")
	if !ok {
		t.Fatal("expected new-skill to be indexed after hot reload")
	}
	if meta.Description != "A new skill" {
		t.Fatalf("unexpected description: %q", meta.Description)
	}
}

func TestStartWatching_MemWorkspace(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewSkillStore(ws, "skills")

	w, err := store.StartWatching(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w != nil {
		t.Fatal("expected nil watcher for MemWorkspace (no Root())")
	}
}

func TestStartWatching_UpdateSkill(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	skillsDir := filepath.Join(dir, "skills", "updater")
	_ = os.MkdirAll(skillsDir, 0o755)
	original := "---\nname: updater\ndescription: Original\nentry: main.py\n---\n"
	_ = os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(original), 0o644)

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	meta, ok := store.Get("updater")
	if !ok || meta.Description != "Original" {
		t.Fatal("initial index failed")
	}

	w, err := store.StartWatching(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	// Update the SKILL.md
	updated := "---\nname: updater\ndescription: Updated version\nentry: main.py\n---\n"
	_ = os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(updated), 0o644)

	time.Sleep(1500 * time.Millisecond)

	meta, ok = store.Get("updater")
	if !ok {
		t.Fatal("expected updater after hot reload")
	}
	if meta.Description != "Updated version" {
		t.Fatalf("expected updated description, got %q", meta.Description)
	}
}

func TestStartWatching_DeleteSkill(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	skillsDir := filepath.Join(dir, "skills", "deleteme")
	_ = os.MkdirAll(skillsDir, 0o755)
	content := "---\nname: deleteme\ndescription: Will be deleted\nentry: run.sh\n---\n"
	_ = os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(content), 0o644)

	store := NewSkillStore(ws, "skills")
	_ = store.BuildIndex(ctx)

	_, ok := store.Get("deleteme")
	if !ok {
		t.Fatal("expected deleteme in index")
	}

	w, err := store.StartWatching(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	// Delete the SKILL.md
	_ = os.Remove(filepath.Join(skillsDir, "SKILL.md"))

	time.Sleep(1500 * time.Millisecond)

	_, ok = store.Get("deleteme")
	if ok {
		t.Fatal("expected deleteme to be removed after hot reload")
	}
}

func TestWatcher_StopIdempotent(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}

	_ = os.MkdirAll(filepath.Join(dir, "skills"), 0o755)
	store := NewSkillStore(ws, "skills")

	w, err := store.StartWatching(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w == nil {
		t.Fatal("expected watcher")
	}

	w.Stop()
	// Second stop should not panic
}
