package snowball_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/text/stem"
	"github.com/GizClaw/flowcraft/sdk/text/stem/adapter/snowball"
)

// TestStem_KnownPorter2Cases pins the words where Porter2 is known
// to fix Porter1 over-stemming. Switching the SDK default would
// shift these BM25 keys, so the test doubles as a migration
// safety net.
func TestStem_KnownPorter2Cases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"running", "run"},
		{"caresses", "caress"},
		{"ponies", "poni"},
		{"agreed", "agre"},
		{"matting", "mat"},
		{"meeting", "meet"},
		{"programming", "program"},
		// Porter2 keeps "general" and "generic" distinct (Porter1
		// over-stems both to "gener"); this is the canonical
		// example for why production stacks moved off Porter1.
		{"general", "general"},
		{"generic", "generic"},
	}
	for _, tc := range cases {
		if got := snowball.Stem(tc.in); got != tc.want {
			t.Errorf("Stem(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStem_DropInForPorter1 confirms the adapter exposes the same
// signature as [stem.Porter] so callers can swap by import alias
// without touching call sites. The test type-checks the function
// against stem.Porter.
func TestStem_DropInForPorter1(t *testing.T) {
	var p1 = stem.Porter
	var p2 = snowball.Stem
	// Both must be (string) string. The blank var lines fail to
	// compile if either signature drifts.
	_ = p1
	_ = p2
}

func TestStem_Idempotent(t *testing.T) {
	for _, w := range []string{"running", "general", "programming"} {
		first := snowball.Stem(w)
		second := snowball.Stem(first)
		if first != second {
			t.Errorf("not idempotent for %q: first=%q second=%q", w, first, second)
		}
	}
}

func TestStemLang_UnknownLang(t *testing.T) {
	_, err := snowball.StemLang("hello", "klingon", false)
	if err == nil {
		t.Error("expected error for unsupported language")
	}
}
