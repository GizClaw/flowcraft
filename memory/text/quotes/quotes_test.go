package quotes_test

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/quotes"
)

// TestExtractSpans covers the headline contract — pairing across
// every recognised quote glyph and dropping unclosed / empty spans.
func TestExtractSpans(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"no_quotes", "Alice met Bob", nil},
		{"ascii_single", `She said "hello"`, []string{"hello"}},
		{"ascii_two", `She said "hi" then "bye"`, []string{"hi", "bye"}},
		{"smart_quotes", "She said \u201chello\u201d", []string{"hello"}},
		{"cjk_corner", "「东京」 is a city", []string{"东京"}},
		{"mixed_glyphs", "He said \u201chi\u201d and 「bye」", []string{"hi", "bye"}},
		{"unclosed_dropped", `He said "hi`, nil},
		{"empty_span_dropped", `say "" again`, nil},
		// Pairing is non-nesting: the inner quote closes the outer,
		// so the third quote re-opens and the fourth closes —
		// surfacing two adjacent spans, NOT a nested one.
		{"non_nesting", `a"b"c"d"e`, []string{"b", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := quotes.ExtractSpans(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractSpans(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
