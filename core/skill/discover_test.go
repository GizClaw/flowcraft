package skill

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
)

func writeSkill(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SkillFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "alpha"), "---\nname: alpha\ndescription: first\n---\nBody.\n")
	writeSkill(t, filepath.Join(root, "nested", "beta"), "---\nname: beta\ndescription: second\n---\nBody.\n")
	// Hidden trees are skipped, and a broken skill is reported without hiding
	// the rest.
	writeSkill(t, filepath.Join(root, ".hidden", "gamma"), "---\nname: gamma\ndescription: hidden\n---\nBody.\n")
	writeSkill(t, filepath.Join(root, "broken"), "not frontmatter\n")
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	tree, err := fsx.Open()
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	out := Discover(context.Background(), tree, []string{root, root})

	if len(out.Skills) != 2 {
		t.Fatalf("found %d skills, want 2: %+v", len(out.Skills), out.Skills)
	}
	if out.Skills[0].Name != "alpha" || out.Skills[1].Name != "beta" {
		t.Errorf("skills = %v, want alpha then beta", []string{out.Skills[0].Name, out.Skills[1].Name})
	}
	if len(out.Diagnostics) != 1 || out.Diagnostics[0].Warning {
		t.Errorf("diagnostics = %+v, want one error for the broken skill", out.Diagnostics)
	}
	if len(out.Roots) != 1 {
		t.Errorf("roots = %v, want the root listed once", out.Roots)
	}
}

func TestDiscoverMissingRootIsEmpty(t *testing.T) {
	tree, err := fsx.Open()
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	out := Discover(context.Background(), tree, []string{filepath.Join(t.TempDir(), "absent")})
	if len(out.Skills) != 0 || len(out.Diagnostics) != 0 {
		t.Errorf("outcome = %+v, want an empty pass", out)
	}
}
