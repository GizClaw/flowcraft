package stopword

// multilingualStopWords is a compact conversational baseline for languages
// commonly seen by recall literal feature extraction. It intentionally complements the
// English tokenizer baseline instead of replacing it.
var multilingualStopWords = newWordTable(
	// Spanish
	"el", "la", "los", "las", "un", "una",
	"unos", "unas", "de", "del", "a", "al",
	"en", "con", "por", "para", "y", "o",
	"que", "qué", "quien", "quién", "cuando", "cuándo",
	"donde", "dónde", "como", "cómo", "mi", "mis",
	"tu", "tus", "su", "sus", "es", "son", "fue",

	// French
	"le", "la", "les", "un", "une", "des",
	"du", "de", "d", "à", "a", "au", "aux",
	"en", "avec", "pour", "par", "et", "ou",
	"qui", "quoi", "quand", "où", "ou", "comment",
	"mon", "ma", "mes", "ton", "ta", "tes",
	"son", "sa", "ses", "est", "sont", "était", "etait",

	// German
	"der", "die", "das", "den", "dem", "des",
	"ein", "eine", "einer", "eines", "und", "oder",
	"zu", "im", "in", "am", "an", "mit", "für", "fur",
	"von", "wer", "was", "wann", "wo", "wie",
	"mein", "meine", "dein", "deine", "sein", "seine",
	"ist", "sind", "war", "waren",

	// Portuguese
	"o", "a", "os", "as", "um", "uma",
	"de", "do", "da", "dos", "das", "em", "no", "na",
	"com", "por", "para", "e", "ou", "que",
	"quem", "quando", "onde", "como", "meu", "minha",
	"seu", "sua", "é", "são", "sao", "foi",

	// Dutch
	"de", "het", "een", "en", "of", "in",
	"op", "met", "voor", "van", "te", "naar",
	"wie", "wat", "wanneer", "waar", "hoe",
	"mijn", "jouw", "zijn", "haar", "is", "was",

	// Russian
	"и", "или", "в", "во", "на", "с", "со",
	"к", "ко", "для", "по", "от", "до", "из",
	"кто", "что", "когда", "где", "как", "мой",
	"моя", "мои", "твой", "его", "ее", "её", "это",
)

// IsMultilingual reports whether word is a high-frequency function word in the
// package's compact multilingual baseline.
func IsMultilingual(word string) bool {
	return multilingualStopWords[normalizeLookup(word)]
}

// MultilingualSet returns a writable set containing the English baseline and
// the compact multilingual baseline.
func MultilingualSet() Set {
	s := EnglishSet()
	return s.Union(MultilingualOnlySet())
}

// MultilingualOnlySet returns a writable set containing only the compact
// non-English multilingual baseline.
func MultilingualOnlySet() Set {
	s := NewSet()
	for w := range multilingualStopWords {
		s[w] = struct{}{}
	}
	return s.Union(CJKSet())
}
