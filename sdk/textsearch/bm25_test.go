package textsearch

import (
	"strings"
	"testing"
)

func TestCorpusStats_AddRemove(t *testing.T) {
	cs := NewCorpusStats()
	tokens1 := []string{"hello", "world", "go"}
	tokens2 := []string{"hello", "test"}

	cs.AddDocument(tokens1)
	if cs.DocCount != 1 {
		t.Fatalf("expected 1 doc, got %d", cs.DocCount)
	}

	cs.AddDocument(tokens2)
	if cs.DocCount != 2 {
		t.Fatalf("expected 2 docs, got %d", cs.DocCount)
	}
	if cs.DocFreq["hello"] != 2 {
		t.Fatalf("expected hello df=2, got %d", cs.DocFreq["hello"])
	}

	cs.RemoveDocument(tokens1)
	if cs.DocCount != 1 {
		t.Fatalf("expected 1 doc after remove, got %d", cs.DocCount)
	}
	if cs.DocFreq["hello"] != 1 {
		t.Fatalf("expected hello df=1 after remove, got %d", cs.DocFreq["hello"])
	}
	if cs.DocFreq["world"] != 0 {
		t.Fatalf("expected world df=0, got %d", cs.DocFreq["world"])
	}
}

func TestExtractKeywords(t *testing.T) {
	tok := &SimpleTokenizer{}
	kws := ExtractKeywords("Hello world hello test", tok)
	seen := make(map[string]int)
	for _, kw := range kws {
		seen[kw]++
	}
	for _, kw := range kws {
		if seen[kw] > 1 {
			t.Fatalf("keyword %q appears %d times (not deduped)", kw, seen[kw])
		}
	}
	if len(kws) == 0 {
		t.Fatal("expected non-empty keywords")
	}
}

func TestBM25_ScoreOptions(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()
	cs.AddDocument(tok.Tokenize("go programming"))
	cs.AddDocument(tok.Tokenize("python"))
	doc := tok.Tokenize("go go programming")
	kw := []string{"go", "programming"}
	sDefault := BM25(doc, kw, cs)
	sHighK1 := BM25(doc, kw, cs, WithK1(3.0))
	if sDefault == sHighK1 {
		t.Fatalf("expected different scores for different k1: default=%f high=%f", sDefault, sHighK1)
	}
}

func TestBM25_Basic(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()
	cs.AddDocument(tok.Tokenize("Go programming language"))
	cs.AddDocument(tok.Tokenize("Python scripting"))

	score := ScoreText("Go programming language is great", ExtractKeywords("go programming", tok), cs, tok)
	if score <= 0 {
		t.Fatalf("expected positive score, got %f", score)
	}
}

func TestBM25_NoMatch(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()
	cs.AddDocument(tok.Tokenize("Go programming"))

	score := ScoreText("Go programming", ExtractKeywords("python", tok), cs, tok)
	if score != 0 {
		t.Fatalf("expected 0, got %f", score)
	}
}

func TestBM25_NilCorpus(t *testing.T) {
	tok := &SimpleTokenizer{}
	score := ScoreText("test", []string{"test"}, nil, tok)
	if score != 0 {
		t.Fatalf("expected 0 for nil corpus, got %f", score)
	}
}

func TestBM25_RankingCorrectness(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()

	doc1 := "Go programming language concurrency goroutines channels"
	doc2 := "Python scripting language dynamic typing"
	doc3 := "Go Go Go programming programming language language concurrency concurrency"

	cs.AddDocument(tok.Tokenize(doc1))
	cs.AddDocument(tok.Tokenize(doc2))
	cs.AddDocument(tok.Tokenize(doc3))

	keywords := ExtractKeywords("Go programming", tok)

	score1 := ScoreText(doc1, keywords, cs, tok)
	score3 := ScoreText(doc3, keywords, cs, tok)
	scorePy := ScoreText(doc2, keywords, cs, tok)

	if score1 <= 0 {
		t.Fatalf("expected positive score for Go doc, got %f", score1)
	}
	if score3 <= 0 {
		t.Fatalf("expected positive score for Go-heavy doc, got %f", score3)
	}
	if scorePy >= score1 {
		t.Fatalf("Python doc (%f) should score lower than Go doc (%f) for 'Go programming'", scorePy, score1)
	}
	if score3 <= score1 {
		t.Fatalf("doc with higher TF for query terms (%f) should score higher than doc1 (%f)", score3, score1)
	}
}

func TestBM25_LengthNormalization(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()

	short := "Go concurrency patterns"
	long := "Go concurrency patterns " + strings.Repeat("filler text padding words ", 50)

	cs.AddDocument(tok.Tokenize(short))
	cs.AddDocument(tok.Tokenize(long))

	keywords := ExtractKeywords("Go concurrency", tok)
	scoreShort := ScoreText(short, keywords, cs, tok)
	scoreLong := ScoreText(long, keywords, cs, tok)

	if scoreShort <= scoreLong {
		t.Errorf("short doc should score higher due to length normalization (b=0.75): short=%f, long=%f",
			scoreShort, scoreLong)
	}
}

func TestStemming_RecallImprovement(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()
	docText := "programming languages support concurrent computing"
	cs.AddDocument(tok.Tokenize(docText))

	score := ScoreText(docText, ExtractKeywords("programs language", tok), cs, tok)
	if score <= 0 {
		t.Fatalf("stemming should enable 'programs' to match 'programming', got score %f", score)
	}

	if Stem("programs") != Stem("programming") {
		t.Fatalf("expected Stem('programs')=%q == Stem('programming')=%q", Stem("programs"), Stem("programming"))
	}
	if Stem("languages") != Stem("language") {
		t.Fatalf("expected Stem('languages')=%q == Stem('language')=%q", Stem("languages"), Stem("language"))
	}

	scoreNoMatch := ScoreText(docText, ExtractKeywords("database storage", tok), cs, tok)
	if scoreNoMatch != 0 {
		t.Fatalf("expected 0 score for unrelated query, got %f", scoreNoMatch)
	}
}
