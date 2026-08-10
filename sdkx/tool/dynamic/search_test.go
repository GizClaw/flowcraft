package dynamic

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	got := tokenize("Read file from GitHub repository 你好世界")
	want := []string{"read", "file", "from", "github", "repository", "你好世界"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenize = %v, want %v", got, want)
	}
}

func TestBm25Search_RanksExactMatchFirst(t *testing.T) {
	docs := []searchDoc{
		{name: "fs__read_file", text: "fs__read_file read file contents from disk"},
		{name: "git__search", text: "git__search search code in repositories"},
	}
	hits := bm25Search(docs, "read file", 10)
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (only the matching doc scores)", len(hits))
	}
	if hits[0].Name != "fs__read_file" {
		t.Errorf("top hit = %q, want fs__read_file", hits[0].Name)
	}
}

func TestBm25Search_EmptyQueryAndLimit(t *testing.T) {
	docs := []searchDoc{{name: "a", text: "a alpha"}, {name: "b", text: "b beta"}}
	if hits := bm25Search(docs, "", 10); len(hits) != 0 {
		t.Errorf("empty query returned hits: %v", hits)
	}
	if hits := bm25Search(docs, "alpha beta", 1); len(hits) != 1 {
		t.Errorf("limit 1 returned %d hits", len(hits))
	}
	if hits := bm25Search(docs, "alpha", 0); len(hits) > defaultSearchLimit {
		t.Errorf("default limit not applied: %d hits", len(hits))
	}
}

func TestBm25Search_TieBreaksByName(t *testing.T) {
	docs := []searchDoc{
		{name: "b_tool", text: "b_tool zap"},
		{name: "a_tool", text: "a_tool zap"},
	}
	hits := bm25Search(docs, "zap", 10)
	if hits[0].Name != "a_tool" {
		t.Errorf("tie-break order = %v, want a_tool first", hits[0].Name)
	}
}

func TestBm25Search_Deterministic(t *testing.T) {
	docs := []searchDoc{
		{name: "one", text: "one alpha beta"},
		{name: "two", text: "two beta gamma"},
	}
	first := bm25Search(docs, "beta", 10)
	second := bm25Search(docs, "beta", 10)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("search is not deterministic: %v vs %v", first, second)
	}
}
