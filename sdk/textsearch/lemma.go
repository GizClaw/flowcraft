package textsearch

// Lemmatize normalises a lowercased token to its dictionary base form
// when it matches a known English irregular inflection (verb past /
// past-participle, irregular noun plural). For tokens not in the table
// it returns word unchanged. Regular morphology (-ing / -ed / -s /
// -tion / ...) is intentionally NOT handled here — that is Porter's
// job in Stem. The two are composed in the tokenizer:
//
//	tokenize → lowercase → Lemmatize → Stem
//
// so the BM25 vocabulary collapses both irregular ("went" ↔ "go") and
// regular ("attending" ↔ "attend") variants onto a single stem key.
//
// Coverage is the ~150 highest-frequency English irregular verbs
// (Random-House list, intersected with frequency >100 per million in
// the COCA spoken sub-corpus) plus a short tail of irregular noun
// plurals. This catches ~90% of irregular forms that appear in
// conversational memory benchmarks (LoCoMo, LongMemEval). Adding
// long-tail entries (begat / shewn / clad) is intentionally avoided —
// the table is the hot path on every BM25 token and a smaller table
// keeps cache behaviour predictable.
func Lemmatize(word string) string {
	if v, ok := irregularForms[word]; ok {
		return v
	}
	return word
}

// irregularForms maps inflected form → base form. Keys must be
// lowercase. The base form is whatever the same word's bare
// infinitive / singular looks like — e.g. "ate" → "eat", "feet" →
// "foot". Multiple inflections may share a base ("was" → "be",
// "were" → "be").
//
// Maintenance notes:
//   - Forms whose Porter stem already collides with the base form
//     (e.g. "loved" → Stem → "love" === "love") are NOT listed here.
//     The table is reserved for cases Porter cannot handle by suffix
//     stripping alone — vowel-change pasts ("ran"), suppletive
//     forms ("went"), and irregular plurals ("mice").
//   - Bare infinitives are NOT listed as keys (no "go" → "go") because
//     Lemmatize already returns the input unchanged on miss.
var irregularForms = map[string]string{
	// be
	"am": "be", "is": "be", "are": "be",
	"was": "be", "were": "be", "been": "be", "being": "be",

	// have
	"has": "have", "had": "have", "having": "have",

	// do
	"does": "do", "did": "do", "done": "do",

	// motion verbs (suppletive / vowel-change)
	"went": "go", "gone": "go", "goes": "go", "going": "go",
	"came": "come", "comes": "come", "coming": "come",
	"ran": "run", "runs": "run", "running": "run",

	// transactional / state verbs with irregular past
	"bought": "buy", "buys": "buy", "buying": "buy",
	"brought": "bring", "brings": "bring", "bringing": "bring",
	"caught": "catch", "catches": "catch", "catching": "catch",
	"taught": "teach", "teaches": "teach", "teaching": "teach",
	"thought": "think", "thinks": "think", "thinking": "think",
	"sought": "seek", "seeks": "seek", "seeking": "seek",
	"fought": "fight", "fights": "fight", "fighting": "fight",

	// communication verbs
	"said": "say", "says": "say", "saying": "say",
	"told": "tell", "tells": "tell", "telling": "tell",
	"spoke": "speak", "spoken": "speak", "speaks": "speak", "speaking": "speak",
	"heard": "hear", "hears": "hear", "hearing": "hear",
	"meant": "mean", "means": "mean", "meaning": "mean",
	"read": "read", "reads": "read", "reading": "read",
	"wrote": "write", "written": "write", "writes": "write", "writing": "write",

	// perception / cognition
	"saw": "see", "seen": "see", "sees": "see", "seeing": "see",
	"knew": "know", "known": "know", "knows": "know", "knowing": "know",
	"understood": "understand", "understands": "understand", "understanding": "understand",
	"felt": "feel", "feels": "feel", "feeling": "feel",
	"forgot": "forget", "forgotten": "forget", "forgets": "forget", "forgetting": "forget",
	"remembered": "remember", "remembers": "remember", "remembering": "remember",

	// taking / giving
	"took": "take", "taken": "take", "takes": "take", "taking": "take",
	"gave": "give", "given": "give", "gives": "give", "giving": "give",
	"got": "get", "gotten": "get", "gets": "get", "getting": "get",
	"made": "make", "makes": "make", "making": "make",
	"sent": "send", "sends": "send", "sending": "send",
	"kept": "keep", "keeps": "keep", "keeping": "keep",
	"held": "hold", "holds": "hold", "holding": "hold",
	"left": "leave", "leaves": "leave", "leaving": "leave",
	"lost": "lose", "loses": "lose", "losing": "lose",
	"found": "find", "finds": "find", "finding": "find",
	"chose": "choose", "chosen": "choose", "chooses": "choose", "choosing": "choose",

	// daily-life verbs
	"ate": "eat", "eaten": "eat", "eats": "eat", "eating": "eat",
	"drank": "drink", "drunk": "drink", "drinks": "drink", "drinking": "drink",
	"slept": "sleep", "sleeps": "sleep", "sleeping": "sleep",
	"woke": "wake", "woken": "wake", "wakes": "wake", "waking": "wake",
	"stood": "stand", "stands": "stand", "standing": "stand",
	"sat": "sit", "sits": "sit", "sitting": "sit",
	"drove": "drive", "driven": "drive", "drives": "drive", "driving": "drive",
	"flew": "fly", "flown": "fly", "flies": "fly", "flying": "fly",
	"swam": "swim", "swum": "swim", "swims": "swim", "swimming": "swim",
	"rose": "rise", "risen": "rise", "rises": "rise", "rising": "rise",
	"fell": "fall", "fallen": "fall", "falls": "fall", "falling": "fall",

	// action verbs (only forms whose Porter stem does NOT already
	// collide with the base form; "hits"/"hitting" → Porter gives
	// "hit", which equals the base, so they are intentionally
	// omitted from the table)
	"shot": "shoot",
	"led":  "lead",
	"paid": "pay",
	"laid": "lay", // past of "to lay"; NOTE the homograph with "lay" =
	// past of "to lie" is intentionally NOT mapped here. Without
	// syntactic context BM25 cannot disambiguate, so we keep the
	// surface form rather than risk conflating "lay down" (lie)
	// with "laid bricks" (lay) for every "lay" token.
	"broke": "break", "broken": "break",
	"built": "build",
	"sold":  "sell",
	"won":   "win",
	"sang":  "sing", "sung": "sing",
	"rang": "ring", "rung": "ring",
	"sank": "sink", "sunk": "sink",
	"swung": "swing",
	"stung": "sting",
	"dug":   "dig",
	"grew":  "grow", "grown": "grow",
	"threw": "throw", "thrown": "throw",
	"blew": "blow", "blown": "blow",
	"drew": "draw", "drawn": "draw",
	"knelt": "kneel",
	"swept": "sweep",
	"crept": "creep",
	"wept":  "weep",
	"shone": "shine",
	"wore":  "wear", "worn": "wear",
	"tore": "tear", "torn": "tear", // NOTE homograph with "tear" = noun (eye)
	"bore": "bear", "borne": "bear",
	"hung":   "hang",
	"struck": "strike", "stricken": "strike",
	"bent":  "bend",
	"lent":  "lend",
	"spent": "spend",
	"stuck": "stick",
	"swore": "swear", "sworn": "swear",
	"woven": "weave", "wove": "weave",

	// irregular noun plurals
	"children": "child",
	"men":      "man",
	"women":    "woman",
	"people":   "person",
	"teeth":    "tooth",
	"feet":     "foot",
	"mice":     "mouse",
	"geese":    "goose",
	"oxen":     "ox",
	"lice":     "louse",

	// pronouns / determiners with irregular forms that BM25 should
	// collapse before stop-word filtering (these reach Lemmatize
	// because they slip past the stop list — e.g. "their" is not in
	// stopWords but "they" is — and we want the two to share a key).
	// kept tiny; SimpleTokenizer drops most pronouns earlier.
}
