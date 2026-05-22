// Package lemma provides irregular-form lemmatisation for English.
//
// Lemmatisation is the morphology step Porter cannot do: vowel-
// change pasts ("went"), suppletive forms ("be" → was / were /
// been), and irregular plurals ("mice"). The package ships a
// hand-curated table of the highest-frequency English irregulars
// (see [irregular.go]); regular morphology (-ing / -ed / -s) is
// the stemmer's job, not ours.
//
// The canonical composition for BM25 vocabulary normalisation is:
//
//	stem.Porter(lemma.Lemmatize(word))
//
// applied to a lower-cased token after stop-word filtering.
package lemma

// Lemmatize normalises a lowercased token to its dictionary base form
// when it matches a known English irregular inflection (verb past /
// past-participle, irregular noun plural). For tokens not in the table
// it returns word unchanged. Regular morphology (-ing / -ed / -s /
// -tion / ...) is intentionally NOT handled here — that is Porter's
// job in [sdk/text/stem.Porter]. The two are composed in the
// tokenizer:
//
//	tokenize → lowercase → Lemmatize → Porter
//
// so the BM25 vocabulary collapses both irregular ("went" ↔ "go") and
// regular ("attending" ↔ "attend") variants onto a single stem key.
//
// Coverage is the ~150 highest-frequency English irregular verbs
// (Random-House list, intersected with frequency >100 per million in
// the COCA spoken sub-corpus) plus a short tail of irregular noun
// plurals. This catches ~90% of irregular forms that appear in
// conversational memory workloads. Adding
// long-tail entries (begat / shewn / clad) is intentionally avoided —
// the table is the hot path on every BM25 token and a smaller table
// keeps cache behaviour predictable.
func Lemmatize(word string) string {
	if v, ok := irregularForms[word]; ok {
		return v
	}
	return word
}
