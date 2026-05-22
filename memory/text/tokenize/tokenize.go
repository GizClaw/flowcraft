// Package tokenize provides tokenizers for the sdk/text family.
//
// A Tokenizer splits raw text into searchable tokens — typically
// lower-cased, morphology-folded units used as BM25 vocabulary keys.
// The package ships three concrete tokenizers covering the common
// retrieval cases:
//
//   - [Simple]: ASCII / Latin text. Splits on Unicode letter / digit
//     boundaries, lower-cases, filters English stop words and tokens
//     shorter than 2 characters, then folds each survivor through
//     lemma.Lemmatize + stem.Porter so irregular ("went"/"go") and
//     regular ("attending"/"attend") forms collapse to one key.
//
//   - [CJKBigram]: Mixed-script text containing Han / Hangul /
//     Kana. Emits unigrams + bigrams over each CJK run and falls
//     back to [Simple] for ASCII runs. Cheap and dependency-free,
//     suitable as a default until a proper segmenter (gse, jieba)
//     is plugged in via an adapter sub-package.
//
//   - [Detect]: Cheap script sniffer that picks Simple or
//     CJKBigram based on the first CJK rune it sees. Use when the
//     caller does not know the language up front.
//
// [SplitWords] is a complementary helper for callers that need
// raw word boundaries WITHOUT case folding or stop-word filtering
// — primarily NER and named-entity hint extraction. It is
// intentionally NOT a Tokenizer because BM25 callers should reach
// for Simple / CJKBigram instead.
package tokenize

// Tokenizer splits text into searchable tokens. Implementations
// must be safe for concurrent use; the SDK assumes a single shared
// Tokenizer instance per retrieval backend.
type Tokenizer interface {
	Tokenize(text string) []string
}
