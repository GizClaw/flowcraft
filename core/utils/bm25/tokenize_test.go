package bm25

import (
	"slices"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single word", "resume", []string{"resume"}},
		{"words", "switch sandbox mode", []string{"switch", "sandbox", "mode"}},
		{"punctuation splits", "clean-up", []string{"clean", "up"}},
		{"underscores split", "web_fetch v2", []string{"web", "fetch", "v2"}},
		{"case folded", "WebFetch", []string{"webfetch"}},
		{"cjk singles and bigrams", "清理会话", []string{"清", "清理", "理", "理会", "会", "会话", "话"}},
		{"cjk bigrams do not bridge punctuation", "清, 洁", []string{"清", "洁"}},
		{"cjk does not bridge latin", "a清b", []string{"a", "清", "b"}},
		{"empty", "", nil},
		{"symbols only", "!!!", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Tokenize(c.in); !slices.Equal(got, c.want) {
				t.Errorf("Tokenize(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
