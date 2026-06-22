package analysis

import (
	"strings"
	"sync"
	"unicode"

	bleveanalysis "github.com/blevesearch/bleve/v2/analysis"
	blevecjk "github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	"github.com/blevesearch/bleve/v2/analysis/lang/en"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/es"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/fr"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/ru"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/token/porter"
	"github.com/blevesearch/bleve/v2/analysis/token/stop"
	bleveunicode "github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/registry"
)

var (
	englishStopOnce sync.Once
	englishStopMap  bleveanalysis.TokenMap
	analyzerCache   sync.Map
)

// SimpleAnalyzer is the default English / Latin analyzer.
type SimpleAnalyzer struct{}

// Analyze implements Analyzer.
func (a *SimpleAnalyzer) Analyze(text string, opts Options) []Token {
	lang := normalizeLanguage(opts.Language)
	if lang != "" && lang != "en" && !opts.KeepStops {
		if analyzer, ok := cachedRegisteredAnalyzer(lang); ok {
			return convertBleveTokens(text, analyzer.Analyze([]byte(text)))
		}
	}
	return analyzeWithBleve(text, englishAnalyzer(opts.KeepStops))
}

// NewMultilingual returns a Bleve-backed analyzer that auto-selects CJK for
// CJK text and otherwise uses Bleve's English analyzer defaults.
func NewMultilingual() Analyzer {
	return multilingualAnalyzer{}
}

type multilingualAnalyzer struct{}

// Analyze implements Analyzer.
func (multilingualAnalyzer) Analyze(text string, opts Options) []Token {
	if opts.Language != "" {
		return (&SimpleAnalyzer{}).Analyze(text, opts)
	}
	return Detect(text).Analyze(text, opts)
}

// CJKBigramAnalyzer handles mixed CJK / Latin text.
type CJKBigramAnalyzer struct{}

// Analyze implements Analyzer.
func (a *CJKBigramAnalyzer) Analyze(text string, opts Options) []Token {
	return analyzeWithBleve(text, cjkAnalyzer(opts.KeepStops))
}

// Detect returns a CJKBigramAnalyzer if sampleText contains any CJK character,
// otherwise a SimpleAnalyzer.
func Detect(sampleText string) Analyzer {
	if containsCJK(sampleText) {
		return &CJKBigramAnalyzer{}
	}
	return &SimpleAnalyzer{}
}

func analyzeWithBleve(text string, analyzer bleveanalysis.Analyzer) []Token {
	if analyzer == nil {
		return nil
	}
	return convertBleveTokens(text, analyzer.Analyze([]byte(text)))
}

func englishAnalyzer(keepStops bool) bleveanalysis.Analyzer {
	key := "en"
	if keepStops {
		key = "en_keep_stops"
	}
	if cached, ok := analyzerCache.Load(key); ok {
		return cached.(bleveanalysis.Analyzer)
	}

	filters := []bleveanalysis.TokenFilter{
		en.NewPossessiveFilter(),
		lowercase.NewLowerCaseFilter(),
	}
	if !keepStops {
		filters = append(filters, stop.NewStopTokensFilter(englishStops()))
	}
	filters = append(filters, porter.NewPorterStemmer())
	analyzer := &bleveanalysis.DefaultAnalyzer{
		Tokenizer:    bleveunicode.NewUnicodeTokenizer(),
		TokenFilters: filters,
	}
	actual, _ := analyzerCache.LoadOrStore(key, analyzer)
	return actual.(bleveanalysis.Analyzer)
}

func cjkAnalyzer(keepStops bool) bleveanalysis.Analyzer {
	key := "cjk"
	if keepStops {
		key = "cjk_keep_stops"
	}
	if cached, ok := analyzerCache.Load(key); ok {
		return cached.(bleveanalysis.Analyzer)
	}

	filters := []bleveanalysis.TokenFilter{
		blevecjk.NewCJKWidthFilter(),
		lowercase.NewLowerCaseFilter(),
		blevecjk.NewCJKBigramFilter(true),
	}
	if !keepStops {
		filters = append(filters, stop.NewStopTokensFilter(englishStops()))
	}
	filters = append(filters, porter.NewPorterStemmer())
	analyzer := &bleveanalysis.DefaultAnalyzer{
		Tokenizer:    bleveunicode.NewUnicodeTokenizer(),
		TokenFilters: filters,
	}
	actual, _ := analyzerCache.LoadOrStore(key, analyzer)
	return actual.(bleveanalysis.Analyzer)
}

func englishStops() bleveanalysis.TokenMap {
	englishStopOnce.Do(func() {
		m := bleveanalysis.NewTokenMap()
		_ = m.LoadBytes(en.EnglishStopWords)
		englishStopMap = m
	})
	return englishStopMap
}

func cachedRegisteredAnalyzer(name string) (bleveanalysis.Analyzer, bool) {
	key := "registered:" + name
	if cached, ok := analyzerCache.Load(key); ok {
		return cached.(bleveanalysis.Analyzer), true
	}
	analyzer, err := registry.NewCache().AnalyzerNamed(name)
	if err != nil {
		return nil, false
	}
	actual, _ := analyzerCache.LoadOrStore(key, analyzer)
	return actual.(bleveanalysis.Analyzer), true
}

func normalizeLanguage(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "en", "eng", "english":
		return "en"
	case "es", "spa", "spanish":
		return "es"
	case "fr", "fra", "fre", "french":
		return "fr"
	case "ru", "rus", "russian":
		return "ru"
	default:
		return ""
	}
}

func convertBleveTokens(text string, in bleveanalysis.TokenStream) []Token {
	if len(in) == 0 {
		return nil
	}
	out := make([]Token, 0, len(in))
	for _, token := range in {
		if token == nil || len(token.Term) == 0 {
			continue
		}
		term := string(token.Term)
		original := term
		if token.Start >= 0 && token.End >= token.Start && token.End <= len(text) {
			original = text[token.Start:token.End]
		}
		out = append(out, Token{
			Term:           term,
			Original:       original,
			Start:          token.Start,
			End:            token.End,
			Position:       token.Position - 1,
			PositionLength: positionLength(token),
			Type:           tokenType(token.Type, term),
			Keyword:        token.KeyWord,
		})
	}
	recomputePositionIncrements(out)
	return out
}

func positionLength(token *bleveanalysis.Token) int {
	switch token.Type {
	case bleveanalysis.Double, bleveanalysis.Shingle:
		return 2
	default:
		return 1
	}
}

func tokenType(typ bleveanalysis.TokenType, term string) TokenType {
	switch typ {
	case bleveanalysis.Numeric:
		return Number
	case bleveanalysis.Ideographic, bleveanalysis.Single:
		return cjkTermType(term)
	case bleveanalysis.Double, bleveanalysis.Shingle:
		return Shingle
	default:
		if allDigits(term) {
			return Number
		}
		if containsCJK(term) {
			return cjkTermType(term)
		}
		return Word
	}
}

func allDigits(term string) bool {
	if term == "" {
		return false
	}
	for _, r := range term {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func cjkTermType(term string) TokenType {
	for _, r := range term {
		switch {
		case unicode.Is(unicode.Han, r):
			return Ideographic
		case unicode.Is(unicode.Hangul, r):
			return Hangul
		case unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hiragana, r):
			return Kana
		}
	}
	return Word
}

// IsCJK reports whether r is Han, Hangul, Katakana, or Hiragana.
func IsCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hiragana, r)
}

func containsCJK(text string) bool {
	return strings.IndexFunc(text, IsCJK) >= 0
}
