package bbh

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/registry"
)

func TestGojiebaAnalyzer(t *testing.T) {
	idx := openInternalIndex(t, t.TempDir(), WithConfig(Config{
		Bleve: BleveConfig{
			Analyzer: "gojieba",
			Gojieba:  GojiebaConfig{Mode: "search"},
		},
	}))
	if err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{
		ID:        "doc",
		Content:   "小明硕士毕业于中国科学院计算所",
		Timestamp: time.Unix(1_700_000_000, 0).UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	assertSearchCount(t, idx, "中国科学院", 1)
	assertSearchCount(t, idx, "计算所", 1)
}

func TestGojiebaHelpersAndAnalyzerErrors(t *testing.T) {
	if _, err := newGojiebaTokenizer(GojiebaConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := newGojiebaTokenizer(GojiebaConfig{DictPath: filepath.Join(t.TempDir(), "missing.dict")}); err == nil {
		t.Fatal("missing gojieba dict should fail")
	}
	if got := byteIndexFrom("小明abc小明", "小明", -5); got != 0 {
		t.Fatalf("byteIndexFrom negative = %d", got)
	}
	if got := byteIndexFrom("小明abc小明", "小明", 1); got <= 0 {
		t.Fatalf("byteIndexFrom later = %d", got)
	}
	if got := byteIndexFrom("abc", "a", 10); got != -1 {
		t.Fatalf("byteIndexFrom overflow = %d", got)
	}
	words := makeWordsBySearch("a中a", []string{"a", "中", "missing", ""})
	if len(words) != 3 || words[0].Start != 0 || words[1].Start != 1 {
		t.Fatalf("words = %+v", words)
	}
	if args := gojiebaDictArgs(GojiebaConfig{DictPath: "d"}); len(args) != 5 || args[0] != "d" {
		t.Fatalf("dict args = %#v", args)
	}
	if _, err := newGojiebaTokenizer(GojiebaConfig{Mode: "bad"}); err == nil {
		t.Fatal("bad gojieba mode should fail")
	}
	defaultTok, err := newGojiebaTokenizer(GojiebaConfig{Mode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if stream := defaultTok.Tokenize([]byte("小明abc")); len(stream) == 0 {
		t.Fatal("default tokenizer returned no tokens")
	}
	hmm := false
	tok, err := gojiebaTokenizerConstructor(map[string]interface{}{"mode": "all", "hmm": hmm}, &registry.Cache{})
	if err != nil {
		t.Fatal(err)
	}
	if stream := tok.Tokenize([]byte("小明abc")); len(stream) == 0 {
		t.Fatal("gojieba tokenizer returned no tokens")
	}
	m := mapping.NewIndexMapping()
	if err := configureGojiebaAnalyzer(m, GojiebaConfig{Mode: "search", HMM: &hmm}); err != nil {
		t.Fatal(err)
	}
	if err := configureGojiebaAnalyzer(m, GojiebaConfig{}); err == nil {
		t.Fatal("duplicate tokenizer/analyzer should fail")
	}
}
