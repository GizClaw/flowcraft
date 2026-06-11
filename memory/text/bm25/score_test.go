package bm25_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/bm25"
	snowball "github.com/GizClaw/flowcraft/memory/text/stem/adapter/snowball"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

func TestCorpusStats_AddRemove(t *testing.T) {
	cs := bm25.NewCorpus()
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

func TestBM25_ScoreOptions(t *testing.T) {
	tok := &tokenize.Simple{}
	cs := bm25.NewCorpus()
	cs.AddDocument(tok.Tokenize("go programming"))
	cs.AddDocument(tok.Tokenize("python"))
	doc := tok.Tokenize("go go programming")
	kw := []string{"go", "programming"}
	sDefault := bm25.Score(doc, kw, cs)
	sHighK1 := bm25.Score(doc, kw, cs, bm25.WithK1(3.0))
	if sDefault == sHighK1 {
		t.Fatalf("expected different scores for different k1: default=%f high=%f", sDefault, sHighK1)
	}
}

func TestBM25_Basic(t *testing.T) {
	tok := &tokenize.Simple{}
	cs := bm25.NewCorpus()
	cs.AddDocument(tok.Tokenize("Go programming language"))
	cs.AddDocument(tok.Tokenize("Python scripting"))

	score := bm25.Score(tok.Tokenize("Go programming language is great"), tokenize.ExtractKeywords("go programming", tok), cs)
	if score <= 0 {
		t.Fatalf("expected positive score, got %f", score)
	}
}

func TestBM25_NoMatch(t *testing.T) {
	tok := &tokenize.Simple{}
	cs := bm25.NewCorpus()
	cs.AddDocument(tok.Tokenize("Go programming"))

	score := bm25.Score(tok.Tokenize("Go programming"), tokenize.ExtractKeywords("python", tok), cs)
	if score != 0 {
		t.Fatalf("expected 0, got %f", score)
	}
}

func TestBM25_NilCorpus(t *testing.T) {
	tok := &tokenize.Simple{}
	score := bm25.Score(tok.Tokenize("test"), []string{"test"}, nil)
	if score != 0 {
		t.Fatalf("expected 0 for nil corpus, got %f", score)
	}
}

func TestBM25_RankingCorrectness(t *testing.T) {
	tok := &tokenize.Simple{}
	cs := bm25.NewCorpus()

	doc1 := "Go programming language concurrency goroutines channels"
	doc2 := "Python scripting language dynamic typing"
	doc3 := "Go Go Go programming programming language language concurrency concurrency"

	cs.AddDocument(tok.Tokenize(doc1))
	cs.AddDocument(tok.Tokenize(doc2))
	cs.AddDocument(tok.Tokenize(doc3))

	keywords := tokenize.ExtractKeywords("Go programming", tok)

	score1 := bm25.Score(tok.Tokenize(doc1), keywords, cs)
	score3 := bm25.Score(tok.Tokenize(doc3), keywords, cs)
	scorePy := bm25.Score(tok.Tokenize(doc2), keywords, cs)

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
	tok := &tokenize.Simple{}
	cs := bm25.NewCorpus()

	short := "Go concurrency patterns"
	long := "Go concurrency patterns " + strings.Repeat("filler text padding words ", 50)

	cs.AddDocument(tok.Tokenize(short))
	cs.AddDocument(tok.Tokenize(long))

	keywords := tokenize.ExtractKeywords("Go concurrency", tok)
	scoreShort := bm25.Score(tok.Tokenize(short), keywords, cs)
	scoreLong := bm25.Score(tok.Tokenize(long), keywords, cs)

	if scoreShort <= scoreLong {
		t.Errorf("short doc should score higher due to length normalization (b=0.75): short=%f, long=%f",
			scoreShort, scoreLong)
	}
}

func TestStemming_RecallImprovement(t *testing.T) {
	tok := &tokenize.Simple{}
	cs := bm25.NewCorpus()
	docText := "programming languages support concurrent computing"
	cs.AddDocument(tok.Tokenize(docText))

	score := bm25.Score(tok.Tokenize(docText), tokenize.ExtractKeywords("programs language", tok), cs)
	if score <= 0 {
		t.Fatalf("stemming should enable 'programs' to match 'programming', got score %f", score)
	}

	if snowball.Stem("programs") != snowball.Stem("programming") {
		t.Fatalf("expected snowball.Stem('programs')=%q == snowball.Stem('programming')=%q", snowball.Stem("programs"), snowball.Stem("programming"))
	}
	if snowball.Stem("languages") != snowball.Stem("language") {
		t.Fatalf("expected snowball.Stem('languages')=%q == snowball.Stem('language')=%q", snowball.Stem("languages"), snowball.Stem("language"))
	}

	scoreNoMatch := bm25.Score(tok.Tokenize(docText), tokenize.ExtractKeywords("database storage", tok), cs)
	if scoreNoMatch != 0 {
		t.Fatalf("expected 0 score for unrelated query, got %f", scoreNoMatch)
	}
}
