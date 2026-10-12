package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSkill creates one SKILL.md under root/<name> with the given
// frontmatter and returns its path.
func writeSkill(t *testing.T, root, name, frontmatter string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	body := "---\n" + frontmatter + "---\n\n# Instructions\nDo the thing.\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// canonical returns the symlink-resolved form of path: discovery
// records canonical paths, so tests compare against this form.
func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
