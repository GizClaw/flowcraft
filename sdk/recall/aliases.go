package recall

import "strings"

// PredicateAliases maps locale-specific or model-drift predicate strings
// to the canonical entry from DefaultPredicates. Lookup is performed
// after lowercase + trim. Without aliases, an extractor that emits
// "lives in" / "居住地" / "lives-in" for what is semantically
// `lives_in` would scatter across distinct slot keys and the slot
// supersede channel would never collapse them.
//
// The default table covers the high-frequency EN/CN surface forms
// observed in production extractor outputs (CN: "住"/"住所"/"老家"
// /"故乡" → lives_in, "供职"/"就职" → works_at, "出生" → birthday,
// "爱人"/"伴侣" → spouse, …; EN: "based_in" → lives_in,
// "married_to" → spouse, "born_on" → birthday, …). It is NOT
// exhaustive — domain-specific synonyms (e.g. medical
// "primary_care_physician" → doctor) should be layered on via
// [WithPredicateAlias] which takes precedence over this table.
// Keys MUST already be lowercased and trimmed; values SHOULD match
// a canonical predicate listed in [DefaultPredicates].
var PredicateAliases = map[string]string{
	// lives_in
	"lives in":   "lives_in",
	"lives-in":   "lives_in",
	"livesin":    "lives_in",
	"resides_in": "lives_in",
	"resides in": "lives_in",
	"resides":    "lives_in",
	"based in":   "lives_in",
	"based_in":   "lives_in",
	"location":   "lives_in",
	"city":       "lives_in",
	"hometown":   "lives_in",
	"住":          "lives_in",
	"在":          "lives_in",
	"住址":         "lives_in",
	"住所":         "lives_in",
	"居住地":        "lives_in",
	"居住":         "lives_in",
	"住在":         "lives_in",
	"现居":         "lives_in",
	"老家":         "lives_in",
	"故乡":         "lives_in",
	"家乡":         "lives_in",
	"所在地":        "lives_in",
	"所在城市":       "lives_in",

	// works_at
	"works at":  "works_at",
	"works-at":  "works_at",
	"works for": "works_at",
	"works_for": "works_at",
	"employed":  "works_at",
	"employer":  "works_at",
	"company":   "works_at",
	"工作单位":      "works_at",
	"工作":        "works_at",
	"在职":        "works_at",
	"任职":        "works_at",
	"任职于":       "works_at",
	"供职":        "works_at",
	"供职于":       "works_at",
	"就职":        "works_at",
	"就职于":       "works_at",
	"单位":        "works_at",
	"公司":        "works_at",

	// occupation
	"job":        "occupation",
	"profession": "occupation",
	"role":       "occupation",
	"title":      "occupation",
	"职业":         "occupation",
	"职位":         "occupation",
	"工种":         "occupation",
	"岗位":         "occupation",

	// birthday
	"dob":           "birthday",
	"date_of_birth": "birthday",
	"date of birth": "birthday",
	"birth_date":    "birthday",
	"birth date":    "birthday",
	"birthdate":     "birthday",
	"born":          "birthday",
	"born_on":       "birthday",
	"生日":            "birthday",
	"出生":            "birthday",
	"出生日期":          "birthday",
	"生辰":            "birthday",

	// language
	"languages":       "language",
	"native_language": "language",
	"native language": "language",
	"speaks":          "language",
	"语言":              "language",
	"母语":              "language",
	"使用语言":            "language",

	// spouse
	"wife":              "spouse",
	"husband":           "spouse",
	"partner":           "spouse",
	"married_to":        "spouse",
	"married to":        "spouse",
	"significant other": "spouse",
	"配偶":                "spouse",
	"老婆":                "spouse",
	"老公":                "spouse",
	"妻子":                "spouse",
	"丈夫":                "spouse",
	"爱人":                "spouse",
	"对象":                "spouse",
	"伴侣":                "spouse",

	// child / parent / pet
	"children": "child",
	"kid":      "child",
	"kids":     "child",
	"son":      "child",
	"daughter": "child",
	"孩子":       "child",
	"小孩":       "child",
	"儿子":       "child",
	"女儿":       "child",
	"父母":       "parent",
	"father":   "parent",
	"mother":   "parent",
	"mom":      "parent",
	"dad":      "parent",
	"妈妈":       "parent",
	"爸爸":       "parent",
	"母亲":       "parent",
	"父亲":       "parent",
	"宠物":       "pet",
	"pets":     "pet",
	"猫":        "pet",
	"狗":        "pet",
}

// SubjectAliases normalizes the most common subject surface forms
// extractors emit in mixed-language conversations. As with
// PredicateAliases, keys are pre-trimmed lowercase strings; values are
// the canonical subject form. Composite subjects (e.g. "pet:Lucky")
// pass through unchanged.
var SubjectAliases = map[string]string{
	"i":        "user",
	"me":       "user",
	"the user": "user",
	"用户":       "user",
	"我":        "user",
}

// normalizePredicate applies PredicateAliases (after lowercase + trim)
// followed by per-instance overrides supplied via WithPredicateAlias.
// Returns "" when the input is empty so the caller can short-circuit
// and skip slot metadata writing.
func normalizePredicate(p string, overrides map[string]string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return ""
	}
	if v, ok := overrides[p]; ok {
		return v
	}
	if v, ok := PredicateAliases[p]; ok {
		return v
	}
	return p
}

// normalizeSubject mirrors normalizePredicate for the subject field.
// Composite subjects (containing ':' or '.') are passed through as-is
// — only the bare token is rewritten — so "pet:Lucky" remains
// distinguishable from "pet:Max".
//
// As with normalizePredicate, the bare-token branch returns the
// LOWERCASE form even when no alias matches, so that two extractor
// outputs differing only in case ("Alice" vs "alice", "Pet" vs
// "pet") collapse onto the same slot_key. This trades proper-noun
// casing in metadata for slot supersede stability — the slot channel
// is exact-string and would otherwise treat the two as separate
// slots. The original surface form is still preserved on
// ExtractedFact.Content; only the slot metadata sees the
// canonicalised value.
func normalizeSubject(s string, overrides map[string]string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	low := strings.ToLower(s)
	if strings.ContainsAny(low, ":.") {
		return s
	}
	if v, ok := overrides[low]; ok {
		return v
	}
	if v, ok := SubjectAliases[low]; ok {
		return v
	}
	return low
}
