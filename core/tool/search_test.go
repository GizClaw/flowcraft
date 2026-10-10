package tool

import (
	"fmt"
	"slices"
	"testing"
)

// The kernel is shared (core/utils/bm25); the catalog's own contract is
// what it hands the kernel and how it keys the results.

func TestCatalogTermsDropSingleRuneASCII(t *testing.T) {
	got := catalogTerms("Read a file 1 世界")
	want := []string{"read", "file", "世", "世界", "界"}
	if !slices.Equal(got, want) {
		t.Fatalf("catalogTerms = %v, want %v", got, want)
	}
}

func catalogDocs() []searchDoc {
	return []searchDoc{
		{name: "read_file", description: "Read a file from the workspace"},
		{name: "write_file", description: "Write a file into the workspace"},
		{name: "webfetch", description: "Fetch a URL over HTTP"},
		{name: "清理会话", description: "清理当前会话的历史记录"},
	}
}

func hitNames(hits []SearchHit) []string {
	names := make([]string, 0, len(hits))
	for _, hit := range hits {
		names = append(names, hit.Name)
	}
	return names
}

// TestSearchIgnoresFillerWordsInQueries pins what the catalog's two-rune
// minimum buys. Without it a bare "a" is indexed and prefix-expands to
// every term starting with "a", so "read a file" dragged webfetch into the
// hit set — and every hit is loaded and takes a discovery-pool slot, which
// is a tool the model was using pushed out for a round.
func TestSearchIgnoresFillerWordsInQueries(t *testing.T) {
	hits, err := bm25Search(catalogDocs(), "read a file", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if names := hitNames(hits); !slices.Contains(names, "read_file") || slices.Contains(names, "webfetch") {
		t.Fatalf("bm25Search(read a file) = %v, want read_file and no webfetch", names)
	}
	for _, query := range []string{"a", "1", "a 1"} {
		hits, err := bm25Search(catalogDocs(), query, 0)
		if err != nil {
			t.Fatalf("bm25Search(%q): %v", query, err)
		}
		if len(hits) != 0 {
			t.Fatalf("bm25Search(%q) = %v, want no hits", query, hitNames(hits))
		}
	}
}

func TestSearchPrefixHitsSurfaceNames(t *testing.T) {
	hits, err := bm25Search(catalogDocs(), "webf", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "webfetch" {
		t.Fatalf("bm25Search(webf) = %v, want [webfetch]", hitNames(hits))
	}
}

// TestSearchMatchesCJKQueries covers the path the catalog did not have
// before the kernel: a Chinese query matches the run of characters it
// mentions and any fragment of it, without a segmentation dictionary.
func TestSearchMatchesCJKQueries(t *testing.T) {
	for _, query := range []string{"清理会话", "会话", "清理", "历史记录"} {
		hits, err := bm25Search(catalogDocs(), query, 0)
		if err != nil {
			t.Fatalf("bm25Search(%q): %v", query, err)
		}
		if len(hits) == 0 || hits[0].Name != "清理会话" {
			t.Fatalf("bm25Search(%q) = %v, want 清理会话 first", query, hitNames(hits))
		}
	}
}

func TestSearchNameOutranksText(t *testing.T) {
	hits, err := bm25Search([]searchDoc{
		{name: "alpha", description: "unrelated wording"},
		{name: "beta", description: "alpha in the description"},
	}, "alpha", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if names := hitNames(hits); len(names) != 2 || names[0] != "alpha" {
		t.Fatalf("bm25Search(alpha) = %v, want the name hit first", names)
	}
}

// TestSearchKeepsCallerOrder pins that ranking never reorders the slice a
// caller passed in: the kernel resolves ties by input order, and callers
// reuse cached definition slices.
func TestSearchKeepsCallerOrder(t *testing.T) {
	docs := catalogDocs()
	before := slices.Clone(docs)
	if _, err := bm25Search(docs, "file", 0); err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if !slices.Equal(docs, before) {
		t.Fatalf("bm25Search reordered its input: %v", docs)
	}
}

func TestSearchDefaultLimit(t *testing.T) {
	docs := make([]searchDoc, 0, 20)
	for i := range 20 {
		docs = append(docs, searchDoc{
			name:        fmt.Sprintf("tool_%02d", i),
			description: "shared capability",
		})
	}
	hits, err := bm25Search(docs, "capability", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if len(hits) != defaultSearchLimit {
		t.Fatalf("bm25Search with no limit = %d hits, want %d", len(hits), defaultSearchLimit)
	}
	if hits[0].Name != "tool_00" {
		t.Fatalf("bm25Search tie-break = %v, want the first name", hitNames(hits))
	}
}

func TestSearchEmptyQueryAndCatalog(t *testing.T) {
	for _, query := range []string{"", "   ", "!!!"} {
		hits, err := bm25Search(catalogDocs(), query, 0)
		if err != nil || len(hits) != 0 {
			t.Fatalf("bm25Search(%q) = %v, %v, want no hits", query, hits, err)
		}
	}
	if hits, err := bm25Search(nil, "read", 0); err != nil || len(hits) != 0 {
		t.Fatalf("bm25Search on an empty catalog = %v, %v, want no hits", hits, err)
	}
}
