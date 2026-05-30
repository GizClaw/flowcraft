package knowledge

import (
	"github.com/GizClaw/flowcraft/memory/text/bm25"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// Knowledge re-exports a minimal slice of [sdk/text] under the
// knowledge namespace so existing callers that historically reached
// for these symbols through the (now-deprecated) sdk/textsearch
// facade keep working unchanged. The aliases are the documented
// public surface; new code should import sdk/text/{tokenize,bm25}
// directly.

// Tokenizer aliases [tokenize.Tokenizer].
type Tokenizer = tokenize.Tokenizer

// SimpleTokenizer aliases [tokenize.Simple].
type SimpleTokenizer = tokenize.Simple

// CJKTokenizer aliases [tokenize.CJKBigram].
type CJKTokenizer = tokenize.CJKBigram

// CorpusStats aliases [bm25.CorpusStats].
type CorpusStats = bm25.CorpusStats

// DetectTokenizer aliases [tokenize.Detect].
var DetectTokenizer = tokenize.Detect

// NewCorpusStats aliases [bm25.NewCorpus].
var NewCorpusStats = bm25.NewCorpus

// ExtractKeywords aliases [bm25.ExtractKeywords].
var ExtractKeywords = bm25.ExtractKeywords

// ScoreText aliases [bm25.ScoreText].
var ScoreText = bm25.ScoreText
