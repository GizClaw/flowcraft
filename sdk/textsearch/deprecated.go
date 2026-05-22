// Package textsearch is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/text and its focused
// sub-packages instead.
// This compatibility package will be removed in v0.5.0.
package textsearch

import (
	"github.com/GizClaw/flowcraft/memory/text/bm25"
	"github.com/GizClaw/flowcraft/memory/text/lemma"
	"github.com/GizClaw/flowcraft/memory/text/stem"
	"github.com/GizClaw/flowcraft/memory/text/stopword"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

type (
	CJKTokenizer    = tokenize.CJKBigram
	CorpusStats     = bm25.CorpusStats
	ScoreOption     = bm25.ScoreOption
	SimpleTokenizer = tokenize.Simple
	Tokenizer       = tokenize.Tokenizer
)

var (
	BM25            = bm25.Score
	DetectTokenizer = tokenize.Detect
	ExtractKeywords = bm25.ExtractKeywords
	IsCJK           = tokenize.IsCJK
	IsCJKStopChar   = stopword.IsCJKChar
	IsStopWord      = stopword.IsEnglish
	Lemmatize       = lemma.Lemmatize
	NewCorpusStats  = bm25.NewCorpus
	ScoreText       = bm25.ScoreText
	Stem            = stem.Porter
	WithB           = bm25.WithB
	WithK1          = bm25.WithK1
)
