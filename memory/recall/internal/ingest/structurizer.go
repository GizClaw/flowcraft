package ingest

import (
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/memory/text/timex"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// NopStructurizer leaves facts unchanged. Default for ingest test
// paths that supply fully-formed facts directly.
type NopStructurizer struct{}

var _ port.Structurizer = NopStructurizer{}

// Structurize implements port.Structurizer.
func (NopStructurizer) Structurize(f domain.TemporalFact, _ port.IngestInput) domain.TemporalFact {
	return f
}

// DiffStructurizerCoverage returns a one-fact coverage delta by
// comparing the structurizer's input and output. It only counts a
// field as "filled" when the structurizer flipped it from empty to
// non-empty — caller-supplied facts that already had the field set
// do not inflate the counters. The result feeds the canonical
// diagnostic.StructurizerCoverage tally.
func DiffStructurizerCoverage(before, after domain.TemporalFact) diagnostic.StructurizerCoverage {
	cov := diagnostic.StructurizerCoverage{TotalFactsSeen: 1}
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
	EntityExtractor port.EntityExtractor

	// TimeParser turns natural-language time expressions inside
	// fact.Content into raw + parsed time metadata consumed by
	// TimeResolver. Nil uses the core timex.Extract grammar.
	// Callers can still plug in a timex.Parser implementation for
	// broader natural-language coverage.
	TimeParser timex.Parser
}

var _ port.Structurizer = DefaultStructurizer{}

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
//  5. Fill time metadata from content when it carries a time phrase,
//     otherwise from the supporting turn's typed Time. TimeResolver
//     later normalises the metadata into ValidFrom.
//
// Confidence is left to the SalienceScorer's DefaultConfidence so
// we don't compete with that contract; the slim LLM schema does not
// emit one and 0.5 is the canonical floor.
func (s DefaultStructurizer) Structurize(f domain.TemporalFact, input port.IngestInput) domain.TemporalFact {
	turn := resolveSupportingTurn(f, input.Turns)

	// Only default Kind when it is unset. Semantic kind classification belongs
	// to the extractor contract, not Go-side keyword heuristics.
	if f.Kind == "" {
		f.Kind = domain.KindNote
	}

	if len(f.Entities) == 0 {
		extractor := s.EntityExtractor
		if extractor == nil {
			extractor = RuleBasedEntityExtractor{}
		}
		f.Entities = extractor.ExtractEntities(f.Content, input.KnownEntities)
	}

	subjectSuppressed, _ := f.Metadata[domain.MetaSubjectSuppressed].(bool)
	if f.Subject == "" && !subjectSuppressed {
		if turn != nil && turn.Speaker != "" {
			f.Subject = turn.Speaker
		} else if len(f.Entities) > 0 {
			f.Subject = f.Entities[0]
		}
	}

	_, hasHint := f.Metadata[MetaValidFromHint]
	_, hasParsedTime := f.Metadata[MetaValidFromAt]
	if !hasHint && !hasParsedTime && f.ValidFrom == nil {
		hint := inferEvidenceRelativeValidFromHint(turn, f)
		if hint.Raw == "" && hint.At.IsZero() && hint.Expr == nil {
			hint = inferValidFromHint(s.TimeParser, turn, f.Content)
		}
		if hint.Raw != "" || !hint.At.IsZero() || hint.Expr != nil {
			if f.Metadata == nil {
				f.Metadata = map[string]any{}
			}
			if hint.Raw != "" {
				f.Metadata[MetaValidFromHint] = hint.Raw
				f.Metadata[MetaValidFromText] = hint.Raw
			}
			if !hint.At.IsZero() {
				f.Metadata[MetaValidFromAt] = hint.At.UTC().Format(time.RFC3339Nano)
			}
			if hint.Expr != nil {
				writeValidFromExpressionMetadata(f.Metadata, hint.Expr)
			}
			if hint.Source != "" {
				f.Metadata[MetaValidFromSource] = hint.Source
			}
		}
	}

	return f
}

func inferEvidenceRelativeValidFromHint(turn *port.TurnContext, f domain.TemporalFact) parsedTimeHint {
	if turn == nil || turn.Time.IsZero() {
		return parsedTimeHint{}
	}
	switch f.Kind {
	case domain.KindEvent, domain.KindPlan:
	default:
		return parsedTimeHint{}
	}
	for _, text := range evidenceTimeTexts(f) {
		if hint := parseRelativeEvidenceTime(text, turn.Time); hint.Raw != "" || !hint.At.IsZero() || hint.Expr != nil {
			return hint
		}
	}
	return parsedTimeHint{}
}

func evidenceTimeTexts(f domain.TemporalFact) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 1+len(f.EvidenceRefs))
	add := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		key := strings.ToLower(strings.Join(strings.Fields(text), " "))
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, text)
	}
	for _, ref := range f.EvidenceRefs {
		add(ref.Text)
	}
	add(f.EvidenceText)
	return out
}

func parseRelativeEvidenceTime(text string, anchor time.Time) parsedTimeHint {
	rel := timex.FindRelativePhrase(text)
	if rel == nil {
		return parsedTimeHint{}
	}
	expr, err := timex.Extract(rel.Text, anchor)
	if err != nil || expr == nil || !expr.Relative || !isValidFromExpression(expr) {
		return parsedTimeHint{}
	}
	return parsedTimeHint{
		Raw:    rel.Text,
		At:     expressionValidFrom(expr),
		Source: ValidFromSourceContentRelative,
		Expr:   expr,
	}
}

// resolveSupportingTurn picks the first port.TurnContext referenced by
// any of the fact's EvidenceRefs. The LLM cites multiple turns when
// a fact spans a back-and-forth; we use the first as the primary
// temporal / speaker anchor, matching how the answer LLM later
// quotes it.
func resolveSupportingTurn(f domain.TemporalFact, turns []port.TurnContext) *port.TurnContext {
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
func extractEntities(content string, known []port.EntitySnapshot) []string {
	seen := make(map[string]struct{})
	var out []string

	add := func(s string) {
		cleaned := cleanExtractedEntity(s)
		if isInvalidExtractedEntityAnchor(cleaned) {
			return
		}
		c := strings.ToLower(strings.TrimSpace(cleaned))
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
	// tokenize.SplitProperNouns preserves apostrophes / hyphens
	// inside tokens so compound names ("O'Brien", "Jean-Luc")
	// survive as single tokens — plain tokenize.SplitWords would
	// fragment them into useless capitalised letter fragments.
	for _, tok := range tokenize.SplitProperNouns(content) {
		if words.IsInvalidEntityAnchorToken(tok) {
			continue
		}
		if isTitleCased(tok) {
			add(tok)
		}
	}
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

// inferValidFromHint first scans content for explicit time phrases
// and falls back to the typed Time on the supporting turn
// with a two-tier parser cascade:
//
//  1. timex.RegexParser — strict ISO 8601 / US slash dates only.
//     This wins on any structured date because looser NL parsers
//     ("05-07" → May 7th of current year, "2024-05-07" → just
//     "05-07" because they index off MM-DD shorthand) regress on
//     full ISO dates.
//  2. timex.Extract with the optional caller-supplied timex.Parser
//     — handles relative phrases ("yesterday", "next Tuesday") and
//     natural-language expressions the regex baseline does not recognise.
//
// When the supporting turn has a timestamp, relative phrases in
// content are resolved against that turn and surfaced as an absolute
// timestamp. This keeps historical replay stable: "4 years ago" in a
// 2023 conversation should not resolve against the wall clock of a
// 2026 eval run. Without a turn timestamp, the raw substring is
// preserved and TimeResolver resolves it against the ingest clock.
type parsedTimeHint struct {
	Raw    string
	At     time.Time
	Source string
	Expr   *timex.Expression
}

func inferValidFromHint(parser timex.Parser, turn *port.TurnContext, content string) parsedTimeHint {
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
	if content != "" {
		// Tier 1: strict ISO baseline. timex.RegexParser is stateless
		// and pinned to ISO 8601 + US-slash shapes, so it never bites
		// off shorter substrings the way looser NL parsers do.
		if m, err := (timex.RegexParser{}).Parse(content, anchor); err == nil && m != nil {
			expr, _ := timex.Extract(m.Text, anchor)
			if isExactTimestampText(m.Text) {
				expr = expressionFromTime(m.Time)
			}
			return parsedTimeHint{Raw: m.Text, At: m.Time, Source: ValidFromSourceContentExplicit, Expr: expr}
		}
		// Tier 2: NL + lexical fallback. Only consulted when the strict
		// baseline missed, so the wider net only fires on truly natural
		// language time expressions. Use timex.Extract instead of calling
		// parser.Parse directly so short lexical phrases such as "last year",
		// "last month", and "next month" are still lifted when the NL parser
		// declines them.
		var parsers []timex.Parser
		if parser != nil {
			parsers = append(parsers, parser)
		}
		if expr, err := timex.Extract(content, anchor, parsers...); err == nil && isValidFromExpression(expr) {
			source := validFromSourceForContentHint(expr.Text)
			if turn != nil && !turn.Time.IsZero() && !expr.Time.IsZero() {
				return parsedTimeHint{Raw: expr.Text, At: expressionValidFrom(expr), Source: source, Expr: expr}
			}
			return parsedTimeHint{Raw: expr.Text, Source: source, Expr: expr}
		}
	}
	if turn != nil && !turn.Time.IsZero() {
		expr := expressionFromTime(turn.Time)
		return parsedTimeHint{
			Raw:    turn.Time.UTC().Format(time.RFC3339Nano),
			At:     turn.Time,
			Source: ValidFromSourceTimeFallback,
			Expr:   expr,
		}
	}
	return parsedTimeHint{}
}

func writeValidFromExpressionMetadata(meta map[string]any, expr *timex.Expression) {
	if meta == nil || expr == nil {
		return
	}
	if expr.Timex != "" {
		meta[MetaValidFromTimex] = expr.Timex
	}
	if expr.Kind != "" {
		meta[MetaValidFromKind] = string(expr.Kind)
	}
	if expr.HasPrecision {
		meta[MetaValidFromPrec] = calendarPrecisionString(expr.Precision)
	}
	if expr.HasRange && !expr.End.IsZero() {
		meta[MetaValidToAt] = expr.End.UTC().Format(time.RFC3339Nano)
	}
}

func isExactTimestampText(raw string) bool {
	return strings.Contains(raw, ":")
}

func validFromSourceForContentHint(raw string) string {
	if timex.IsRelativePhrase(raw) {
		return ValidFromSourceContentRelative
	}
	return ValidFromSourceContentExplicit
}
