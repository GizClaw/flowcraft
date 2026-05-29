package normalize_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/normalize"
)

// TestNFC_CollapsesDecomposedComposed pins the headline contract:
// the pre-composed "é" (U+00E9) and the decomposed
// "e\u0301" (e + COMBINING ACUTE ACCENT) collapse onto the same byte
// sequence after NFC. This is the equality invariant downstream
// hashing relies on.
func TestNFC_CollapsesDecomposedComposed(t *testing.T) {
	composed := "Pokémon"
	decomposed := "Pokémon" // contains decomposed e + COMBINING ACUTE
	if normalize.NFC(composed) != normalize.NFC(decomposed) {
		t.Fatalf("NFC must collapse composed and decomposed forms")
	}
}

func TestNFC_EmptyInput(t *testing.T) {
	if got := normalize.NFC(""); got != "" {
		t.Errorf("NFC(\"\") = %q, want \"\"", got)
	}
}

// TestCollapseSpaces covers the canonical user-visible form: NFC +
// collapse internal whitespace + trim edges.
func TestCollapseSpaces(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"trim_edges", "  hello  ", "hello"},
		{"collapse_internal", "hello   world", "hello world"},
		{"tabs_and_newlines", "hello\t\nworld", "hello world"},
		{"single_word", "alice", "alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.CollapseSpaces(tc.in); got != tc.want {
				t.Errorf("CollapseSpaces(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReplaceNonAlnumWithSpace pins the predicate-folding behaviour:
// punctuation collapses to space, letters and digits survive. Run
// lengths are NOT collapsed here — callers compose with
// CollapseSpaces.
func TestReplaceNonAlnumWithSpace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"alnum_only", "abc123", "abc123"},
		{"hyphen", "favorite-color", "favorite color"},
		{"slash", "favorite/color", "favorite color"},
		{"mixed_punct", "a.b,c-d", "a b c d"},
		{"unicode_letter_kept", "Pokémon", "Pokémon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.ReplaceNonAlnumWithSpace(tc.in); got != tc.want {
				t.Errorf("ReplaceNonAlnumWithSpace(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReplaceNonAlnum_ThenCollapse covers the canonical composition:
// the two helpers together produce predicate identifiers that
// collapse mixed punctuation onto a single canonical form.
func TestReplaceNonAlnum_ThenCollapse(t *testing.T) {
	got := normalize.CollapseSpaces(normalize.ReplaceNonAlnumWithSpace("favorite-color"))
	if got != "favorite color" {
		t.Errorf("compose got %q, want %q", got, "favorite color")
	}
}

func TestNumericHelpers(t *testing.T) {
	if !normalize.IsDigitString("007") {
		t.Fatal("expected digit string")
	}
	if normalize.IsDigitString("7a") || normalize.IsDigitString("") {
		t.Fatal("expected non-digit strings to be rejected")
	}
	if got := normalize.TrimLeadingASCIIZeros("00042"); got != "42" {
		t.Fatalf("TrimLeadingASCIIZeros = %q", got)
	}
	if got := normalize.TrimLeadingASCIIZeros("000"); got != "0" {
		t.Fatalf("all-zero normalization = %q", got)
	}
}

func TestReplaceStandaloneFold(t *testing.T) {
	got := normalize.ReplaceStandaloneFold("Avery said I have a bike; mine is blue.", "I have", "Avery has")
	if got != "Avery said Avery has a bike; mine is blue." {
		t.Fatalf("ReplaceStandaloneFold phrase = %q", got)
	}
	got = normalize.ReplaceStandaloneFold("This timing is minefield-safe.", "mine", "Avery's")
	if got != "This timing is minefield-safe." {
		t.Fatalf("ReplaceStandaloneFold should respect token boundaries, got %q", got)
	}
}
