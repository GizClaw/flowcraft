package main

// The bm25 corpus. Every list here is an input; the outcomes are computed by
// bm25contract.go, so nothing in this file has to be kept in sync with core by
// hand -- if core's tokenizer or scoring moves, the dump moves with it and the
// gate goes red on the port's side.

// bm25TokenizeSamples are whole strings, chosen for the decisions the tokenizer
// makes: word runs, CJK bigrams, digits, symbols, and the characters where Go
// and Rust could disagree.
var bm25TokenizeSamples = []string{
	"",
	" ",
	"WebFetch",
	"webfetch the URL and return its text",
	"  ..hello--world.. ",
	"CamelCase42",
	"snake_case_name",
	"kebab-case-name",
	"v1.2.3",
	"100% pure",
	"读取工具",
	"从工作区读取文件",
	"日本語",
	"한국어 텍스트",
	"混合 mixed 文本 with latin",
	"读a取",
	"读 取",
	"读读取",
	"東京タワーへ行く",
	"アイウエオ",
	"ｱｲｳｴｵ",        // halfwidth katakana
	"Ａｂｃ１２３",       // fullwidth latin and digits
	"e\u0301clair", // combining acute accent
	"İstanbul",     // dotted capital I: a multi-character lowercase in Rust
	"ẞtraße",       // capital sharp s
	"ΟΔΟΣ",         // Greek final sigma rules
	"МИР",          // Cyrillic
	"٣٤٥",          // Arabic-Indic digits (Nd)
	"३४५",          // Devanagari digits (Nd)
	"½ Ⅻ ²",        // No and Nl: Rust says numeric, Go says not a digit
	"👩‍💻 emoji",    // emoji and a zero-width joiner
	"a\u00a0b",     // non-breaking space
	"tab\there\nnewline",
	"tool=read_file&limit=3",
	"读取：文件",
	"々", // Han iteration mark
	"ー", // katakana prolonged sound mark
}

// bm25ScriptCodes are the single code points whose classification decides how a
// string splits. They are the boundaries of every range the port spells out,
// plus characters it deliberately leaves out -- the fast way to catch a range
// that is one code point too wide.
var bm25ScriptCodes = []int{
	0x0041,  // A, letter
	0x0061,  // a
	0x0030,  // 0, digit
	0x00DF,  // ß
	0x0130,  // İ
	0x00BD,  // ½, No
	0x216B,  // Ⅻ, Nl
	0x00B2,  // ², No
	0x0301,  // combining acute, Mn
	0x0663,  // ٣ Arabic-Indic digit
	0x0969,  // ३ Devanagari digit
	0x03B1,  // α Greek
	0x0410,  // А Cyrillic
	0x200D,  // zero-width joiner
	0x1F469, // 👩 emoji
	// Han
	0x2E7F, 0x2E80, 0x2E99, 0x2E9A, 0x2E9B, 0x2EF3, 0x2F00, 0x2FD5,
	0x3005, 0x3006, 0x3007, 0x3021, 0x3029, 0x3038, 0x303B,
	0x3400, 0x4DBF, 0x4DC0, 0x4E00, 0x9FFF, 0xA000,
	0xF900, 0xFA6D, 0xFA6E, 0xFA70, 0xFAD9,
	0x16FE2, 0x16FE3, 0x16FF0, 0x16FF1,
	0x20000, 0x2A6DF, 0x2A700, 0x2B739, 0x2B740, 0x2B81D,
	0x2B820, 0x2CEA1, 0x2CEB0, 0x2EBE0, 0x2EBF0, 0x2EE5D,
	0x30000, 0x3134A, 0x31350, 0x323AF,
	// Hiragana
	0x3040, 0x3041, 0x3096, 0x3097, 0x309D, 0x309F, 0x30A0,
	0x1B001, 0x1B11F, 0x1B132, 0x1B150, 0x1B152,
	// Katakana
	0x30A1, 0x30FA, 0x30FB, 0x30FD, 0x30FF, 0x3100,
	0x31F0, 0x31FF, 0x3200, 0x32D0, 0x32FE, 0x3300, 0x3357,
	0xFF65, 0xFF66, 0xFF6F, 0xFF70, 0xFF71, 0xFF9D, 0xFF9E,
	0x1AFF0, 0x1AFF3, 0x1AFF5, 0x1AFFE, 0x1B000, 0x1B120, 0x1B122, 0x1B155, 0x1B164, 0x1B167,
	// Hangul
	0x10FF, 0x1100, 0x11FF, 0x3131, 0x318E, 0x318F,
	0xA960, 0xA97C, 0xA97D, 0xAC00, 0xD7A3, 0xD7A4, 0xD7B0, 0xD7C6, 0xD7CB, 0xD7FB, 0xD7FC,
	0xFFA0, 0xFFBE, 0xFFC2, 0xFFC7, 0xFFCA, 0xFFCF, 0xFFD2, 0xFFD7, 0xFFDA, 0xFFDC,
	// Parenthesized and circled Hangul, and the tone marks: blocks Hangul owns
	// that are neither syllables nor jamo.
	0x302E, 0x302F, 0x3030, 0x3200, 0x321E, 0x321F, 0x3260, 0x327E, 0x327F,
}

type sampleDoc struct {
	id   string
	name string
	text string
}

// bm25SearchSample is one Search / SearchTerms call. `terms` selects
// SearchTerms; a nil slice means the row is a Search.
type bm25SearchSample struct {
	docs         []sampleDoc
	query        string
	terms        []string
	k1           float64
	b            float64
	prefixWeight float64
	limit        int
}

const (
	bm25K1 = 1.2
	bm25B  = 0.75
	bm25PW = 0.5
)

var bm25ToolDocs = []sampleDoc{
	{"webfetch", "webfetch", "fetch a URL and return its text"},
	{"websearch", "websearch", "search the web for pages matching a query"},
	{"fileread", "fileread", "read a file from the workspace"},
	{"filewrite", "filewrite", "write or overwrite a file in the workspace"},
	{"bookmark", "bookmark", "save a resume position; resume reading later"},
	{"resume", "resume", "resume the last run"},
	{"empty", "", ""},
	{"textonly", "", "webfetch is mentioned here but not indexed as a name"},
}

var bm25CJKDocs = []sampleDoc{
	{"read", "读取工具", "从工作区读取文件"},
	{"write", "写入工具", "向工作区写入文件"},
	{"search", "搜索工具", "在工作区搜索文件名"},
}

var bm25SearchSamples = []bm25SearchSample{
	{docs: bm25ToolDocs, query: "webfetch", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "webf", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "WEbf", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "web", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 3},
	{docs: bm25ToolDocs, query: "web", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 1},
	{docs: bm25ToolDocs, query: "web", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "resume", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "resume resume", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "file write read", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "!!!", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "absent", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "e", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	// The option grid: every knob at both ends of its range.
	{docs: bm25ToolDocs, query: "web", k1: bm25K1, b: bm25B, prefixWeight: 0, limit: 0},
	{docs: bm25ToolDocs, query: "web", k1: bm25K1, b: bm25B, prefixWeight: 1, limit: 0},
	{docs: bm25ToolDocs, query: "web", k1: 2, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "web", k1: 0.5, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "web", k1: bm25K1, b: 0, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, query: "web", k1: bm25K1, b: 1, prefixWeight: bm25PW, limit: 0},
	// Ties keep the caller's document order.
	{
		docs: []sampleDoc{
			{"b", "same", "same"}, {"a", "same", "same"}, {"c", "same", "same"},
		},
		query: "same", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0,
	},
	// One document, one term, so the score is arithmetic a reader can check.
	{docs: []sampleDoc{{"one", "alpha", "alpha beta"}}, query: "alpha", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	// CJK.
	{docs: bm25CJKDocs, query: "读取", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25CJKDocs, query: "文件", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25CJKDocs, query: "工具", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25CJKDocs, query: "搜索 文件", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	// Empty corpus.
	{docs: nil, query: "anything", k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	// Duplicate ids are a refusal, reported through the constructor.
	{
		docs:  []sampleDoc{{"x", "a", "b"}, {"x", "c", "d"}},
		query: "a",
		k1:    bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0,
	},
}

// bm25TermSearchSamples drive SearchTerms, where the caller did the tokenizing.
var bm25TermSearchSamples = []bm25SearchSample{
	{docs: bm25ToolDocs, terms: []string{"web"}, k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, terms: []string{"web", "web"}, k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, terms: []string{}, k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, terms: []string{"", "file"}, k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
	{docs: bm25ToolDocs, terms: []string{"WEB"}, k1: bm25K1, b: bm25B, prefixWeight: bm25PW, limit: 0},
}

type bm25ConfigSample struct {
	k1           float64
	b            float64
	prefixWeight float64
}

var bm25ConfigSamples = []bm25ConfigSample{
	{bm25K1, bm25B, bm25PW},
	{0, bm25B, bm25PW},
	{-1, bm25B, bm25PW},
	{bm25K1, -0.1, bm25PW},
	{bm25K1, 1.1, bm25PW},
	{bm25K1, bm25B, -0.1},
	{bm25K1, bm25B, 1.1},
	{bm25K1, 0, 0},
	{bm25K1, 1, 1},
}

type bm25FrequencySample struct {
	id    string
	name  map[string]int
	text  map[string]int
	query string
}

var bm25FrequencySamples = []bm25FrequencySample{
	{id: "ok", name: map[string]int{"tool": 1, "read": 2}, text: map[string]int{"file": 1}, query: "read"},
	{id: "empty-term", name: map[string]int{"": 1}, query: "x"},
	{id: "zero", name: map[string]int{"tool": 0}, query: "x"},
	{id: "negative", text: map[string]int{"tool": -3}, query: "x"},
	{id: "big", name: map[string]int{"tool": 1 << 20}, query: "tool"},
	{id: "nil-fields", query: "x"},
}
