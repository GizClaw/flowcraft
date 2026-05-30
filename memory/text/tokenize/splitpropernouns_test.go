package tokenize_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// TestSplitProperNouns_KeepsCompoundNames is the headline contract:
// apostrophes and hyphens INSIDE a token are preserved so names like
// "O'Brien" and "Jean-Luc" survive as single tokens. SplitWords
// would split them on the inner punctuation, which is exactly the
// case this helper was added to handle.
func TestSplitProperNouns_KeepsCompoundNames(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "Alice", []string{"Alice"}},
		{"apostrophe", "O'Brien met Jean-Luc.", []string{"O'Brien", "met", "Jean-Luc"}},
		{"trim_edge_apos", "'Alice'", []string{"Alice"}},
		{"trim_edge_hyphen", "-Alice-", []string{"Alice"}},
		{"only_punct", "!@#$", nil},
		{"only_apostrophes", "'-'", nil},
		{"cjk_unchanged", "你好,世界", []string{"你好", "世界"}},
		{"mixed_punct_split", "hello, world!", []string{"hello", "world"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenize.SplitProperNouns(tc.in)
			if !sliceEqual(got, tc.want) {
				t.Errorf("SplitProperNouns(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
