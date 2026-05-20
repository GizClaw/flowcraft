package compiler

import (
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/text/timex"
	whenadp "github.com/GizClaw/flowcraft/sdk/text/timex/adapter/when"
)

// defaultTimeParser is the process-wide fallback used when a
// DefaultStructurizer is constructed without an explicit
// TimeParser. We prefer the olebedev/when adapter over the
// zero-dep timex.RegexParser because when handles relative
// phrases ("yesterday", "next Tuesday") in addition to absolute
// dates — exactly the class of expressions an LLM-extracted fact
// inherits verbatim from the conversational turn.
//
// Construction loads the English + common rule set; failure
// (essentially impossible — when's rules are in-memory) degrades
// to the regex baseline so the structurizer never blocks on
// time parsing.
var defaultTimeParser = sync.OnceValue(func() timex.Parser {
	p, err := whenadp.New()
	if err != nil {
		return timex.RegexParser{}
	}
	return p
})

// Structurizer fills the structural fields the slim LLM extractor
// does not own (entities, subject/predicate/object, valid_from
// hints), and acts as a keyword-based fallback for Kind when the
// LLM left it empty (legacy schema responses).
//
// It runs after the Extractor and before Normalizer, so heuristic
// outputs still flow through the canonical hardening pipeline. The
// rule is conservative: only empty fields are filled. Caller-
// supplied facts (passthrough extractor) keep their authoritative
// shape; only the LLM-extracted slim facts get the lift.
//
// Why this exists: an earlier extractor design asked the LLM to
// fill a 14-field grid per fact (kind taxonomy, entity NER, SPO
// inference, temporal arithmetic, evidence selection, …). On
// smaller models that load manifested as fewer facts, vague
// categorization, and mis-labelled temporal info. The current split
// keeps the LLM responsible for the one classification it does
// well (Kind, picked from a closed 6-value enum) and delegates the
// rest — deterministic NER, SPO inference from the typed turn
// channel, absolute-date hints — to this stage.
type Structurizer interface {
	Structurize(f model.TemporalFact, input Input) model.TemporalFact
}

// NopStructurizer leaves facts unchanged. Default for compiler test
// paths that supply fully-formed facts directly.
type NopStructurizer struct{}

// Structurize implements Structurizer.
func (NopStructurizer) Structurize(f model.TemporalFact, _ Input) model.TemporalFact {
	return f
}

// StructurizerCoverage tallies how many times each sub-task of the
// Structurizer actually filled a previously-empty field on its
// way through the compiler.
//
// It is the observability surface that turns Structurizer from a
// black-box bundle into four independently measurable jobs:
//
//   - KindFilled        — facts that entered with Kind == "" and
//     left with a non-empty Kind. High value
//     means the LLM is leaning on the keyword
//     fallback for classification; low value
//     means the LLM's enum field is winning.
//   - EntitiesFilled    — facts that gained at least one entity
//     (either Title-Cased NER or KnownEntity
//     match). High value confirms the cross-
//     fact canonicalisation hint is paying off.
//   - SubjectFilled     — facts whose Subject was populated from
//     turn.Speaker / first entity. High value
//     tells us the typed Speaker channel is
//     load-bearing for SPO derivation.
//   - ValidFromHintFilled — facts that got an absolute-time hint
//     from turn.Time or content regex. High
//     value confirms timeline source is being
//     seeded; zero would mean the hint pipeline
//     is silently dead.
//
// TotalFactsSeen is the denominator every other counter rides on,
// so ratios stay meaningful when callers aggregate across runs.
type StructurizerCoverage struct {
	TotalFactsSeen      int
	KindFilled          int
	EntitiesFilled      int
	SubjectFilled       int
	ValidFromHintFilled int
}

// DiffStructurizerCoverage returns a one-fact coverage delta by
// comparing the structurizer's input and output. It only counts a
// field as "filled" when the structurizer flipped it from empty to
// non-empty — caller-supplied facts that already had the field set
// do not inflate the counters.
func DiffStructurizerCoverage(before, after model.TemporalFact) StructurizerCoverage {
	cov := StructurizerCoverage{TotalFactsSeen: 1}
	if before.Kind == "" && after.Kind != "" {
		cov.KindFilled = 1
	}
	if len(before.Entities) == 0 && len(after.Entities) > 0 {
		cov.EntitiesFilled = 1
	}
	if before.Subject == "" && after.Subject != "" {
		cov.SubjectFilled = 1
	}
	beforeHint, _ := before.Metadata[MetaValidFromHint].(string)
	afterHint, _ := after.Metadata[MetaValidFromHint].(string)
	if beforeHint == "" && afterHint != "" {
		cov.ValidFromHintFilled = 1
	}
	return cov
}

// Add merges another coverage tally into this one. Used by the
// compiler to fold per-fact deltas into a single per-Save total.
func (c *StructurizerCoverage) Add(other StructurizerCoverage) {
	c.TotalFactsSeen += other.TotalFactsSeen
	c.KindFilled += other.KindFilled
	c.EntitiesFilled += other.EntitiesFilled
	c.SubjectFilled += other.SubjectFilled
	c.ValidFromHintFilled += other.ValidFromHintFilled
}

// DefaultStructurizer is the production fill-in. It walks each fact
// once, filling only fields the LLM did not emit, and is safe to
// run on partial caller-supplied facts (already-set fields are
// preserved).
//
// EntityExtractor is the only sub-task exposed as a swappable
// interface — per-run diagnostics show it is the only Structurizer
// stage doing real semantic work (~100% fact coverage). Subject and
// ValidFrom inference are simple typed-channel lifts (a few lines
// each) and keeping them inline avoids interface noise for code that
// would always default to the same trivial implementation; if you
// need to swap them, wrap DefaultStructurizer in a custom
// Structurizer.
type DefaultStructurizer struct {
	// EntityExtractor mines entity mentions from fact.Content.
	// Nil falls back to RuleBasedEntityExtractor — the historical
	// rule-based behaviour. Production deployments can plug in
	// LLM- or spaCy-backed extractors for non-English content or
	// entity disambiguation without touching the rest of the
	// pipeline.
	EntityExtractor EntityExtractor

	// TimeParser turns natural-language time expressions inside
	// fact.Content into the MetaValidFromHint string consumed by
	// TimeResolver. Nil falls back to the olebedev/when adapter
	// (see [defaultTimeParser]) which handles both ISO dates and
	// English relative phrases. Callers needing CJK / multi-
	// language parsing can plug in any timex.Parser
	// implementation; the zero-dep timex.RegexParser is also a
	// valid choice when relative phrases are unwanted.
	TimeParser timex.Parser
}

// Structurize implements Structurizer.
//
// Order matters:
//
//  1. Resolve the supporting turn (by evidence_refs[].id). Once we
//     have it, Time / Speaker become typed sources for Subject and
//     valid_from arithmetic — no regex archaeology on prose.
//  2. Fill Kind by keyword vote against the content. Default note.
//  3. Fill Entities: extract Title-Cased tokens from content + add
//     any KnownEntities whose canonical / alias appears in content.
//     Lowercased + deduped to match the entity-projection contract.
//  4. Fill Subject from the supporting turn's Speaker; fall back to
//     the first entity. Object / Predicate stay empty unless the
//     LLM provided them.
//  5. Fill MetaValidFromHint from the supporting turn's typed Time
//     when present; otherwise grep an absolute date out of content.
//     TimeResolver later parses the hint into ValidFrom.
//
// Confidence is left to the SalienceScorer's DefaultConfidence so
// we don't compete with that contract; the slim LLM schema does not
// emit one and 0.5 is the canonical floor.
func (s DefaultStructurizer) Structurize(f model.TemporalFact, input Input) model.TemporalFact {
	turn := resolveSupportingTurn(f, input.Turns)

	// Only infer Kind when it is unset. A caller / extractor that
	// explicitly emitted a Kind value — even an invalid one — must
	// surface as an error from the compiler, not get silently
	// rewritten by the heuristic.
	if f.Kind == "" {
		f.Kind = inferKind()
	}

	if len(f.Entities) == 0 {
		extractor := s.EntityExtractor
		if extractor == nil {
			extractor = RuleBasedEntityExtractor{}
		}
		f.Entities = extractor.ExtractEntities(f.Content, input.KnownEntities)
	}

	if f.Subject == "" {
		if turn != nil && turn.Speaker != "" {
			f.Subject = turn.Speaker
		} else if len(f.Entities) > 0 {
			f.Subject = f.Entities[0]
		}
	}

	if _, hasHint := f.Metadata[MetaValidFromHint]; !hasHint && f.ValidFrom == nil {
		parser := s.TimeParser
		if parser == nil {
			parser = defaultTimeParser()
		}
		if hint := inferValidFromHint(parser, turn, f.Content); hint != "" {
			if f.Metadata == nil {
				f.Metadata = map[string]any{}
			}
			f.Metadata[MetaValidFromHint] = hint
		}
	}

	return f
}

// resolveSupportingTurn picks the first TurnContext referenced by
// any of the fact's EvidenceRefs. The LLM cites multiple turns when
// a fact spans a back-and-forth; we use the first as the primary
// temporal / speaker anchor, matching how the answer LLM later
// quotes it.
func resolveSupportingTurn(f model.TemporalFact, turns []TurnContext) *TurnContext {
	if len(turns) == 0 || len(f.EvidenceRefs) == 0 {
		return nil
	}
	byID := make(map[string]int, len(turns))
	for i, t := range turns {
		if t.ID != "" {
			byID[t.ID] = i
		}
		if t.EvidenceID != "" && t.EvidenceID != t.ID {
			byID[t.EvidenceID] = i
		}
	}
	for _, ref := range f.EvidenceRefs {
		if idx, ok := byID[ref.ID]; ok {
			return &turns[idx]
		}
		if idx, ok := byID[ref.MessageID]; ok {
			return &turns[idx]
		}
	}
	return nil
}

// inferKind is the Kind-fallback used when neither the caller nor
// the LLM extractor populated f.Kind. Per-run diagnostics on the
// production extractor (Route 2 schema with the kind enum) show
// this path fires on 0% of facts in practice — the LLM owns the
// classification. The earlier keyword-rule table was therefore
// removed: its only callers in real deployments would be legacy
// callers running the slim text-only schema or providers whose
// structured-output downgrade strips the kind field, and for those
// cases a stable KindNote default is both correct and predictable.
//
// We keep the function (instead of inlining KindNote) so future
// callers can swap in a smarter fallback without changing
// Structurize's call site, and so the godoc above makes the
// design intent legible to anyone wondering where the keyword
// table went.
func inferKind() model.FactKind {
	return model.KindNote
}

// extractEntities scans the content for capitalised tokens (likely
// proper nouns) and additionally folds in any KnownEntities whose
// canonical or alias surface form appears in the content. Output is
// lowercased + deduped to match canonicalEntity in the entity
// projection.
//
// Limits / trade-offs:
//   - Only Title-Cased ASCII tokens are extracted (mid-sentence
//     proper nouns). Sentence-start words are skipped to avoid
//     promoting "She" / "I" / generic openers. KnownEntities cover
//     the case where a sentence starts with a known speaker name.
//   - Multi-word names ("Tom Smith") are NOT joined heuristically;
//     KnownEntities seeds the join when the canonical projection
//     already has the multi-word form. This is a deliberate quality
//     ceiling: we accept "tom" + "smith" as two singletons in the
//     no-history case, and recover the joined form once at least
//     one prior fact established the canonical alias.
func extractEntities(content string, known []EntitySnapshot) []string {
	seen := make(map[string]struct{})
	var out []string

	add := func(s string) {
		c := strings.ToLower(strings.TrimSpace(s))
		if c == "" {
			return
		}
		if _, dup := seen[c]; dup {
			return
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}

	// Pass 1: KnownEntities. We do a case-insensitive substring
	// match so any prior canonical form / alias is rescued.
	lower := strings.ToLower(content)
	for _, e := range known {
		if e.Canonical != "" && strings.Contains(lower, strings.ToLower(e.Canonical)) {
			add(e.Canonical)
			continue
		}
		for _, alias := range e.Aliases {
			if alias == "" {
				continue
			}
			if strings.Contains(lower, strings.ToLower(alias)) {
				add(e.Canonical)
				break
			}
		}
	}

	// Pass 2: Title-Cased tokens (heuristic NER).
	for _, tok := range tokenizeForNER(content) {
		if isStructurizerStopword(strings.ToLower(tok)) {
			continue
		}
		if isTitleCased(tok) {
			add(tok)
		}
	}
	return out
}

// tokenizeForNER splits content on whitespace + ASCII punctuation
// but keeps internal apostrophes / hyphens so names like
// "O'Brien" / "Jean-Luc" survive as single tokens.
func tokenizeForNER(content string) []string {
	if content == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tok := strings.Trim(cur.String(), "'-")
		if tok != "" {
			out = append(out, tok)
		}
		cur.Reset()
	}
	for _, r := range content {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '-':
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// isTitleCased reports tokens whose first letter is upper-case and
// the rest is not all-upper. "Paris" / "MacBook" pass; "PARIS" /
// "i" do not — all-caps tokens are usually acronyms we'd over-pull
// (ATM, CEO) without a known-entity hint.
func isTitleCased(tok string) bool {
	if len(tok) < 2 {
		return false
	}
	runes := []rune(tok)
	if !unicode.IsUpper(runes[0]) {
		return false
	}
	allUpper := !slices.ContainsFunc(runes[1:], unicode.IsLower)
	return !allUpper
}

// structurizerStopwords is the closed list of sentence-start
// pronouns / openers we never want to treat as entities even when
// they're capitalised. This is intentionally tiny: KnownEntities
// catches everything else.
var structurizerStopwords = map[string]struct{}{
	"i": {}, "you": {}, "he": {}, "she": {}, "it": {}, "we": {}, "they": {},
	"the": {}, "a": {}, "an": {}, "this": {}, "that": {}, "these": {}, "those": {},
	"my": {}, "your": {}, "his": {}, "her": {}, "its": {}, "our": {}, "their": {},
	"and": {}, "or": {}, "but": {}, "so": {}, "yes": {}, "no": {}, "ok": {}, "okay": {},
}

func isStructurizerStopword(lower string) bool {
	_, ok := structurizerStopwords[lower]
	return ok
}

// inferValidFromHint prefers the typed Time on the supporting turn
// (verbatim RFC3339 string) and falls back to scanning content
// with a two-tier parser cascade:
//
//  1. timex.RegexParser — strict ISO 8601 / US slash dates only.
//     This wins on any structured date because looser NL parsers
//     ("05-07" → May 7th of current year, "2024-05-07" → just
//     "05-07" because they index off MM-DD shorthand) regress on
//     full ISO dates.
//  2. The caller-supplied timex.Parser (default: olebedev/when)
//     — handles relative phrases ("yesterday", "next Tuesday")
//     and natural-language expressions the regex baseline does
//     not recognise.
//
// The hint string is the raw substring the parser matched (e.g.
// "2024-05-07", "yesterday") — TimeResolver re-parses it against
// its own absolute-format + relative-token table which already
// covers both shapes. Surfacing the substring instead of the
// resolved time keeps Structurizer's contract simple: structurizer
// finds time mentions, TimeResolver grounds them, and the two
// stages stay independently testable.
func inferValidFromHint(parser timex.Parser, turn *TurnContext, content string) string {
	if turn != nil && !turn.Time.IsZero() {
		return turn.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if content == "" {
		return ""
	}
	// Anchor relative phrases ("yesterday") to the turn's time
	// when available so the resolved hint string still makes sense
	// downstream. Falling back to time.Now() is acceptable: the
	// TimeResolver re-parses the relative token against its own
	// `now`, so the structurizer's anchor is only used by absolute
	// parsers that need it for sanity.
	anchor := time.Now()
	if turn != nil && !turn.Time.IsZero() {
		anchor = turn.Time
	}
	// Tier 1: strict ISO baseline. timex.RegexParser is stateless
	// and pinned to ISO 8601 + US-slash shapes, so it never bites
	// off shorter substrings the way looser NL parsers do.
	if m, err := (timex.RegexParser{}).Parse(content, anchor); err == nil && m != nil {
		return m.Text
	}
	// Tier 2: NL fallback. Only consulted when the strict baseline
	// missed, so the wider net only fires on truly natural-language
	// time expressions.
	if parser == nil {
		return ""
	}
	if m, err := parser.Parse(content, anchor); err == nil && m != nil {
		return m.Text
	}
	return ""
}
