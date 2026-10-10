package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverRootsAndDuplicates(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeSkill(t, rootA, "alpha", "name: alpha\ndescription: alpha skill in root A\n")
	writeSkill(t, rootB, "beta", "name: beta\ndescription: beta skill in root B\n")
	// Duplicate name across roots: both paths are kept, and ByName
	// resolves the first one in the (name, path) sort order.
	dupA := canonical(t, writeSkill(t, rootA, "dup",
		"name: dup\ndescription: dup in root A\n"))
	dupB := canonical(t, writeSkill(t, rootB, "dup",
		"name: dup\ndescription: dup in root B\n"))

	out := Discover(context.Background(), []string{rootA, rootB})
	if len(out.Skills) != 4 {
		t.Fatalf("Discover() = %d skills, want 4: %+v", len(out.Skills), out.Skills)
	}
	if len(out.Diagnostics) != 0 {
		t.Fatalf("Discover() diagnostics = %+v, want none", out.Diagnostics)
	}
	if len(out.Roots) != 2 || len(out.ScanRoots) != 2 {
		t.Fatalf("roots = %v / scan roots = %v, want both roots",
			out.Roots, out.ScanRoots)
	}

	svc := NewService(context.Background(),
		Options{Roots: []string{rootA, rootB}})
	got, ok := svc.ByName("alpha")
	if !ok || !strings.HasPrefix(got.Path, canonical(t, rootA)) {
		t.Fatalf("ByName(alpha) = %+v, want a path under root A", got)
	}
	dup, ok := svc.ByName("dup")
	wantDup := dupA
	if dupB < dupA {
		wantDup = dupB
	}
	if !ok || dup.Path != wantDup {
		t.Fatalf("ByName(dup) = %q, want %q", dup.Path, wantDup)
	}
	list := svc.List()
	if len(list) != 4 {
		t.Fatalf("List() = %d skills, want 4", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Fatalf("List() not sorted by name: %+v", list)
		}
	}
}

func TestDiscoverMissingRootIsSilentlyEmpty(t *testing.T) {
	out := Discover(context.Background(),
		[]string{filepath.Join(t.TempDir(), "does-not-exist")})
	if len(out.Skills) != 0 || len(out.Diagnostics) != 0 {
		t.Fatalf("missing root = %+v, want silently empty", out)
	}
}

func TestAbsoluteRootsSkipsEmptyAndAbsolutizes(t *testing.T) {
	got := absoluteRoots([]string{"", filepath.Join("some", "rel")})
	if len(got) != 1 {
		t.Fatalf("absoluteRoots = %v, want one entry", got)
	}
	if !filepath.IsAbs(got[0]) {
		t.Fatalf("absoluteRoots entry = %q, want absolute", got[0])
	}
}

// TestDiscoveryDiagnosticsMarkAcceptedWarnings separates the two
// diagnostic kinds discovery produces: a tolerated shape issue (a name
// that differs from its directory) is a warning, while a file that
// cannot load stays an error.
func TestDiscoveryDiagnosticsMarkAcceptedWarnings(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "mismatch",
		"name: something-else\ndescription: tolerated name mismatch\n")
	writeSkill(t, root, "broken", "name: broken\n")

	svc := NewService(context.Background(), Options{Roots: []string{root}})
	var sawWarning, sawError bool
	for _, e := range svc.Diagnostics() {
		switch {
		case strings.Contains(e.Path, "mismatch"):
			if !e.Warning {
				t.Errorf("tolerated name mismatch must be a warning: %+v", e)
			}
			sawWarning = true
		case strings.Contains(e.Path, "broken"):
			if e.Warning {
				t.Errorf("unparsable SKILL.md must stay an error: %+v", e)
			}
			sawError = true
		}
	}
	if !sawWarning || !sawError {
		t.Fatalf("diagnostics warning=%v error=%v: %+v",
			sawWarning, sawError, svc.Diagnostics())
	}
}

func TestHiddenEntriesSkipped(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "visible", "name: visible\ndescription: visible skill\n")
	hidden := filepath.Join(root, ".hidden")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "SKILL.md"),
		[]byte("---\nname: hidden\ndescription: hidden\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(context.Background(), Options{Roots: []string{root}})
	if _, ok := svc.ByName("visible"); !ok {
		t.Fatalf("visible skill missing: %+v", svc.List())
	}
	if _, ok := svc.ByName("hidden"); ok {
		t.Fatalf("hidden skill must be skipped: %+v", svc.List())
	}
}

func TestFollowSymlinks(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	// The symlink target lives inside another configured root, so
	// following it stays in-bounds.
	real := filepath.Join(rootB, "linked")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, real, "linked-skill",
		"name: linked-skill\ndescription: reached via symlink\n")
	if err := os.Symlink(real, filepath.Join(rootA, "linked")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	svc := NewService(context.Background(), Options{Roots: []string{rootA, rootB}})
	// The skill is reachable through root A's symlink and directly
	// under root B: it is collected once, under its canonical path.
	var found []Metadata
	for _, sk := range svc.List() {
		if sk.Name == "linked-skill" {
			found = append(found, sk)
		}
	}
	if len(found) != 1 {
		t.Fatalf("linked-skill discovered %d times (%+v), want once",
			len(found), found)
	}
	_, body, err := svc.ReadFull("linked-skill")
	if err != nil || !strings.Contains(body, "Instructions") {
		t.Fatalf("ReadFull(linked-skill) = %q, %v", body, err)
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, outside, "leak",
		"name: leak\ndescription: must not be reachable\n")
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file symlink pointing at an out-of-root SKILL.md.
	if err := os.Symlink(filepath.Join(outside, "leak", "SKILL.md"),
		filepath.Join(root, "SKILL.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// A directory symlink pointing at an out-of-root skill tree.
	if err := os.Symlink(filepath.Join(outside, "leak"),
		filepath.Join(root, "leak-dir")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	out := Discover(context.Background(), []string{root})
	for _, sk := range out.Skills {
		if sk.Name == "leak" {
			t.Fatalf("escaping symlink skill discovered: %+v", out.Skills)
		}
	}
	if len(out.Diagnostics) == 0 {
		t.Fatal("escaping symlinks must be recorded as errors")
	}
	svc := NewService(context.Background(), Options{Roots: []string{root}})
	if _, _, err := svc.ReadFull("leak"); err == nil {
		t.Fatal("ReadFull must not resolve an escaping skill")
	}
}
