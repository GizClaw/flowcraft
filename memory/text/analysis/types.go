// Package analysis provides structured text analysis pipelines for retrieval.
package analysis

// Mode tells an analyzer whether text is being prepared for indexing or query
// parsing. Analyzers may use it to make query-time expansion choices.
type Mode int

const (
	ModeIndex Mode = iota
	ModeQuery
)

// TokenType classifies a token's source shape.
type TokenType string

const (
	Word        TokenType = "word"
	Number      TokenType = "number"
	Ideographic TokenType = "ideographic"
	Hangul      TokenType = "hangul"
	Kana        TokenType = "kana"
	Email       TokenType = "email"
	URL         TokenType = "url"
	Code        TokenType = "code"
	Shingle     TokenType = "shingle"
	Synonym     TokenType = "synonym"
)

// Token is the structured unit produced by analyzers. Offsets are byte offsets
// into the original Go string, so callers can slice without rune conversion.
type Token struct {
	Term           string
	Original       string
	Start          int
	End            int
	Position       int
	PositionIncr   int
	PositionLength int
	Type           TokenType
	Keyword        bool
}

// Options carries per-call analysis choices.
type Options struct {
	Mode       Mode
	Language   string
	KeepStops  bool
	NeedOffset bool
	NeedType   bool
}

// Analyzer turns raw text into structured tokens.
type Analyzer interface {
	Analyze(text string, opts Options) []Token
}
