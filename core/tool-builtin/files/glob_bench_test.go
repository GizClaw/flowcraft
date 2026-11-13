package files

import (
	"path"
	"strings"
	"testing"
)

// referenceMatchSegments is the implementation matchPath replaced, kept here as
// the specification: the in-place walk must agree with it on every shape a
// pattern can take, including the ones the tool tests never reach.
func referenceMatchSegments(pattern, segments []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(segments); i++ {
				if referenceMatchSegments(pattern[1:], segments[i:]) {
					return true
				}
			}
			return false
		}
		if len(segments) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], segments[0]); err != nil || !ok {
			return false
		}
		pattern, segments = pattern[1:], segments[1:]
	}
	return len(segments) == 0
}

func TestMatchPathAgreesWithTheSplitImplementation(t *testing.T) {
	paths := []string{
		"a.go",
		"a.go/b.go",
		"internal/a.go",
		"internal/deep/a.go",
		"internal/deep/nested/b_test.go",
		"internal/deep/nested/b.txt",
		"x/y/z/w.go",
		"x",
	}
	patterns := []string{
		"*",
		"*.go",
		"**",
		"**/*.go",
		"internal/**",
		"internal/**/*.go",
		"internal/*/*.go",
		"**/x/**/w.go",
		"x/y/z/w.go",
		"internal/deep/nested/*.txt",
		"nomatch/*.go",
	}
	for _, raw := range patterns {
		pattern := strings.Split(strings.Trim(raw, "/"), "/")
		for _, rel := range paths {
			want := referenceMatchSegments(pattern, strings.Split(strings.Trim(rel, "/"), "/"))
			if got := matchPath(pattern, rel); got != want {
				t.Errorf("matchPath(%q, %q) = %v, reference says %v", raw, rel, got, want)
			}
		}
	}
}

// TestMatchPathLeavesNoSeparators: a trailing or doubled separator is not a
// segment, so a pattern that matches the path must not gain an empty one.
func TestMatchPathLeavesNoSeparators(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		{"*.go", "a.go", true},
		{"**/*.go", "internal//a.go", true},
		{"**", "a/b/c", true},
		{"a/**", "a", true},
		{"a/*", "a", false},
	}
	for _, tc := range cases {
		pattern := strings.Split(tc.pattern, "/")
		if got := matchPath(pattern, tc.rel); got != tc.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", tc.pattern, tc.rel, got, tc.want)
		}
	}
}

// BenchmarkMatchPath is the per-visited-file cost of matching, which is what a
// glob over a large tree pays once per entry. The split variant is measured
// beside it so the saving is visible rather than asserted.
func BenchmarkMatchPath(b *testing.B) {
	pattern := strings.Split("internal/**/nested/*.go", "/")
	rel := "internal/deep/nested/b_test.go"
	b.Run("in place", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !matchPath(pattern, rel) {
				b.Fatal("no match")
			}
		}
	})
	b.Run("split per entry", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !referenceMatchSegments(pattern, strings.Split(strings.Trim(rel, "/"), "/")) {
				b.Fatal("no match")
			}
		}
	})
}
