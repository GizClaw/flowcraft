package skill

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	doc := "---\nname: alpha\ndescription: does the alpha thing\n---\n# Alpha\n\nBody text.\n"
	result, err := Parse("/skills/alpha/SKILL.md", []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if result.Metadata.Name != "alpha" || result.Metadata.Description != "does the alpha thing" {
		t.Errorf("metadata = %+v", result.Metadata)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", result.Warnings)
	}
}

// A third-party skill without a usable name still loads, under its directory
// name, with a warning rather than a rejection.
func TestParseFallsBackToDirectoryName(t *testing.T) {
	doc := "---\ndescription: no name here\n---\nBody.\n"
	result, err := Parse("/skills/my-skill/SKILL.md", []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if result.Metadata.Name != "my-skill" {
		t.Errorf("name = %q, want the directory name", result.Metadata.Name)
	}
	if len(result.Warnings) == 0 {
		t.Error("fallback produced no warning")
	}
}

func TestParseRejectsMalformedDocuments(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":      "just markdown\n",
		"unterminated":        "---\nname: alpha\n",
		"empty body":          "---\nname: alpha\ndescription: x\n---\n\n",
		"missing description": "---\nname: alpha\n---\nBody.\n",
	}
	for name, doc := range cases {
		if _, err := Parse("/skills/alpha/SKILL.md", []byte(doc)); err == nil {
			t.Errorf("%s: Parse = nil error, want a rejection", name)
		}
	}
}

// A BOM is trimmed before the delimiter check: editors write one often enough
// that refusing the file would drop a usable skill.
func TestParseTrimsBOM(t *testing.T) {
	doc := "\ufeff---\nname: alpha\ndescription: with bom\n---\nBody.\n"
	if _, err := Parse("/skills/alpha/SKILL.md", []byte(doc)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestParseTruncatesDescription(t *testing.T) {
	doc := "---\nname: alpha\ndescription: " + strings.Repeat("x", maxDescriptionLen+50) + "\n---\nBody.\n"
	result, err := Parse("/skills/alpha/SKILL.md", []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len([]rune(result.Metadata.Description)); got != maxDescriptionLen {
		t.Errorf("description is %d runes, want %d", got, maxDescriptionLen)
	}
}
