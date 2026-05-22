package knowledge

import "github.com/GizClaw/flowcraft/sdk/textsearch"

// Type and function aliases re-exported from sdk/textsearch for backward
// compatibility. Internal code and tests can use these without changing
// import paths.
type (
	Tokenizer       = textsearch.Tokenizer
	SimpleTokenizer = textsearch.SimpleTokenizer
	CJKTokenizer    = textsearch.CJKTokenizer
	CorpusStats     = textsearch.CorpusStats
)

var (
	DetectTokenizer = textsearch.DetectTokenizer
	NewCorpusStats  = textsearch.NewCorpusStats
	ExtractKeywords = textsearch.ExtractKeywords
	ScoreText       = textsearch.ScoreText
)
