package bm25

import (
	"testing"
)

func BenchmarkNew(b *testing.B) {
	docs := corpusDocs(500)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := New(docs); err != nil {
			b.Fatalf("New: %v", err)
		}
	}
}

func BenchmarkSearch(b *testing.B) {
	index, err := New(corpusDocs(500))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if results := index.Search("w42", 8); len(results) == 0 {
			b.Fatal("Search(w42) returned no results")
		}
	}
}
