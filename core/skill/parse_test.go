package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseFileFallbackAndWarnings(t *testing.T) {
	root := t.TempDir()
	// Invalid name (uppercase + underscore): falls back to the
	// slugified directory name and keeps loading.
	dir := filepath.Join(root, "My_Skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(
		"---\nname: My_Skill\ndescription: bad name\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile with invalid name should not fail: %v", err)
	}
	if res.Metadata.Name != "my-skill" {
		t.Fatalf("fallback name = %q, want my-skill", res.Metadata.Name)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("invalid name should produce a warning")
	}

	// A valid name that differs from its directory is accepted with a
	// warning.
	mismatch := writeSkill(t, root, "mismatch",
		"name: something-else\ndescription: tolerated name mismatch\n")
	res, err = ParseFile(mismatch)
	if err != nil {
		t.Fatalf("ParseFile(name mismatch) = %v", err)
	}
	if res.Metadata.Name != "something-else" || len(res.Warnings) == 0 {
		t.Fatalf("name mismatch = %+v / %v, want accepted with a warning",
			res.Metadata, res.Warnings)
	}

	// Missing description is fatal.
	bad := writeSkill(t, root, "nod", "name: nod\n")
	if _, err := ParseFile(bad); err == nil {
		t.Fatal("missing description should fail")
	}

	// Missing frontmatter is fatal.
	noFm := filepath.Join(root, "nofm")
	if err := os.MkdirAll(noFm, 0o755); err != nil {
		t.Fatal(err)
	}
	noFmPath := filepath.Join(noFm, "SKILL.md")
	if err := os.WriteFile(noFmPath, []byte("no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(noFmPath); err == nil {
		t.Fatal("missing frontmatter should fail")
	}

	// An empty body is fatal.
	empty := writeSkill(t, root, "empty-body", "name: empty-body\ndescription: d\n")
	if err := os.WriteFile(empty, []byte(
		"---\nname: empty-body\ndescription: d\n---\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(empty); err == nil {
		t.Fatal("empty body should fail")
	}

	// A missing file is fatal.
	if _, err := ParseFile(filepath.Join(root, "absent", "SKILL.md")); err == nil {
		t.Fatal("missing file should fail")
	}
}

// TestParseToleratesExtensionFields pins the lenient decode: a
// third-party skill carries fields the standard does not define, and
// the non-standard metadata.short-description is read through its
// hyphenated key.
func TestParseToleratesExtensionFields(t *testing.T) {
	root := t.TempDir()
	path := writeSkill(t, root, "extended",
		"name: extended\ndescription: carries extensions\n"+
			"license: MIT\nversion: 1.2.3\nmetadata:\n"+
			"  short-description: short form\n  owner: someone\n")
	res, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile(extensions) = %v, want tolerated", err)
	}
	if res.Metadata.ShortDescription != "short form" {
		t.Fatalf("short-description = %q, want short form",
			res.Metadata.ShortDescription)
	}
}

func TestParseLimits(t *testing.T) {
	root := t.TempDir()

	// A too-long name falls back to the directory name with a warning.
	long := strings.Repeat("a", maxNameLen+1)
	path := writeSkill(t, root, "long-name-skill",
		"name: "+long+"\ndescription: d\n")
	res, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile(long name) = %v", err)
	}
	if res.Metadata.Name != "long-name-skill" || len(res.Warnings) == 0 {
		t.Fatalf("long name = %q / %v, want fallback with a warning",
			res.Metadata.Name, res.Warnings)
	}

	// A too-long description is truncated to the rune limit (CJK
	// included: the cut is by characters, not bytes).
	desc := strings.Repeat("描", maxDescriptionLen*2)
	path = writeSkill(t, root, "long-desc",
		"name: long-desc\ndescription: "+desc+"\n")
	res, err = ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile(long description) = %v", err)
	}
	if got := utf8.RuneCountInString(res.Metadata.Description); got != maxDescriptionLen {
		t.Fatalf("description length = %d runes, want %d",
			got, maxDescriptionLen)
	}
}

func TestOversizedSkillRejected(t *testing.T) {
	big := strings.Repeat("x", maxSkillFileBytes+1)
	if _, err := parseBytes("big/SKILL.md", []byte(
		"---\nname: big\ndescription: d\n---\n\n"+big)); err == nil {
		t.Fatal("oversized SKILL.md accepted by parseBytes")
	}

	root := t.TempDir()
	path := writeSkill(t, root, "big", "name: big\ndescription: d\n")
	if err := os.Truncate(path, maxSkillFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(path); err == nil {
		t.Fatal("oversized SKILL.md accepted by ParseFile")
	}
}

// TestParseToleratesLeadingBOM pins the lenient edge: editors write a
// UTF-8 BOM often enough that refusing the file would drop a usable
// skill, so the byte-order mark is trimmed before the frontmatter
// delimiter is checked. The body readers share the split.
func TestParseToleratesLeadingBOM(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bom-skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(
		"\ufeff---\nname: bom-skill\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile(BOM) = %v, want tolerated", err)
	}
	if res.Metadata.Name != "bom-skill" {
		t.Fatalf("name = %q, want bom-skill", res.Metadata.Name)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", res.Warnings)
	}

	svc := NewService(context.Background(), Options{Roots: []string{root}})
	_, body, err := svc.ReadFull("bom-skill")
	if err != nil || !strings.Contains(body, "body") {
		t.Fatalf("ReadFull(BOM) = %q, %v", body, err)
	}
}

// TestParseNamelessSkillInNonASCIIDirectory documents the one fallback
// dead end: a missing name in a directory that carries no ASCII to
// slugify (文档, say) leaves nothing usable, so the file is reported as
// an error diagnostic and skipped rather than loading under an
// invented name.
func TestParseNamelessSkillInNonASCIIDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "文档")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(
		"---\ndescription: d\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ParseFile(path)
	if err == nil {
		t.Fatal("nameless skill in a non-ASCII directory must not load")
	}
	if !strings.Contains(err.Error(), "not usable as a fallback") {
		t.Fatalf("err = %v, want the fallback note", err)
	}
}
