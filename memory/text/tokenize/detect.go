package tokenize

// Detect returns a [CJKBigram] tokenizer if sampleText contains any
// CJK character, otherwise a [Simple] tokenizer.
//
// The detection is intentionally cheap and lossy: it scans for the
// first CJK rune and stops. Callers that need a stronger language
// guess (e.g. distinguishing Japanese from Chinese, or Korean from
// either) should plug in their own classifier and instantiate the
// tokenizer directly.
func Detect(sampleText string) Tokenizer {
	for _, r := range sampleText {
		if IsCJK(r) {
			return &CJKBigram{}
		}
	}
	return &Simple{}
}
