package stopword

// cjkStopChars is the canonical CJK function-character baseline.
// Each entry is a single-rune Chinese particle or pronoun whose
// frequency in conversational text makes it a poor BM25 signal:
//
//   - structural particles ("的", "了", "在", "是")
//   - pronouns ("我", "你", "他/她/它", "们")
//   - modal endings ("吧", "吗", "呢", "啊")
//
// The 50-character set was tuned against the conversational
// memory workloads (Chinese subset). Callers needing a stricter
// or larger set should layer their own table on top and consult
// IsCJKChar only as a baseline; the package intentionally exposes
// no Extend hook for CJK because production deployments doing
// Chinese search typically need a proper segmenter, not a richer
// stop-rune list.
var cjkStopChars = map[rune]bool{
	'的': true, '了': true, '在': true, '是': true, '我': true,
	'有': true, '和': true, '就': true, '不': true, '人': true,
	'都': true, '一': true, '个': true, '上': true, '也': true,
	'很': true, '到': true, '说': true, '要': true, '去': true,
	'你': true, '会': true, '着': true, '没': true, '看': true,
	'好': true, '自': true, '这': true, '他': true, '她': true,
	'它': true, '们': true, '那': true, '被': true, '从': true,
	'把': true, '让': true, '给': true, '向': true, '吧': true,
	'吗': true, '呢': true, '啊': true, '哦': true, '嗯': true,
	'呀': true, '啦': true, '哈': true, '嘛': true, '么': true,
}

// IsCJKChar reports whether r is in the package's CJK stop-character
// baseline. Used by [sdk/text/tokenize.CJKBigram] to skip
// semantically empty runes during bigram emission.
func IsCJKChar(r rune) bool {
	return cjkStopChars[r]
}
