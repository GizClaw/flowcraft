package intent

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/text/quotes"
	"github.com/GizClaw/flowcraft/memory/text/stopword"
	"github.com/GizClaw/flowcraft/memory/text/timex"
	whenadp "github.com/GizClaw/flowcraft/memory/text/timex/adapter/when"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// intentStopwords extends the canonical English stopword set with
// recall-specific question / framing verbs ("meet"/"told"/"said")
// that are not entities even though they survive sdk/text's general
// stopword filter. The set is computed once at package init.
var intentStopwords = stopword.EnglishSet().Extend(
	"whom", "whose", "why", "done",
	"am", "having", "would", "could", "should", "might", "must",
	"again", "also", "just", "very", "too", "so", "yes",
	"mine", "yours", "hers", "ours", "theirs",
	"meet", "met", "meeting",
	"tell", "told", "say", "said", "know", "knew",
)

var queryTimeParser = newQueryTimeParser()

// RuleBased is the default deterministic query intent compiler.
type RuleBased struct{}

var _ port.IntentCompiler = RuleBased{}

// Default returns the rule-based compiler wired by recall.New.
func Default() port.IntentCompiler { return RuleBased{} }

// Compile merges explicit entities with rule-based extraction from Text.
func (RuleBased) Compile(_ context.Context, input port.IntentInput) (port.IntentResult, error) {
	entities := mergeEntities(input.Entities, extractEntitiesFromText(input.Text))
	out := port.IntentResult{
		Text:      input.Text,
		Subject:   input.Subject,
		Predicate: input.Predicate,
		Object:    input.Object,
		Kinds:     append([]domain.FactKind(nil), input.Kinds...),
		TimeRange: input.TimeRange,
		Entities:  entities,
	}
	if out.TimeRange.IsZero() {
		out.TimeRange = inferTimeRange(input.Text)
	}
	if len(out.Kinds) == 0 {
		out.Kinds = inferKinds(input.Text)
	}
	if out.Subject == "" && shouldInferSubject(input.Text) {
		out.Subject = inferSubject(input.Text, entities)
	}
	return out, nil
}

func newQueryTimeParser() timex.Parser {
	p, err := whenadp.New()
	if err == nil && p != nil {
		return p
	}
	return timex.RegexParser{}
}

func mergeEntities(explicit, extracted []string) []string {
	seen := make(map[string]struct{}, len(explicit)+len(extracted))
	add := func(s string) []string {
		s = normalizeEntityMention(s)
		if s == "" {
			return nil
		}
		if _, ok := seen[s]; ok {
			return nil
		}
		seen[s] = struct{}{}
		return []string{s}
	}
	var out []string
	for _, e := range explicit {
		out = append(out, add(e)...)
	}
	for _, e := range extracted {
		out = append(out, add(e)...)
	}
	return out
}

func normalizeEntityMention(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
	if len(s) < 2 {
		return ""
	}
	return strings.ToLower(s)
}

// extractEntitiesFromText is a conservative rule baseline: quoted spans,
// capitalized tokens, and CJK runs. Common question words are filtered
// (via intentStopwords) so "Who did Alice meet in Paris?" yields alice
// and paris, not who.
func extractEntitiesFromText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	set := map[string]struct{}{}
	add := func(s string) {
		s = normalizeEntityMention(s)
		if s == "" || intentStopwords.Contains(s) {
			return
		}
		set[s] = struct{}{}
	}
	for _, q := range quotes.ExtractSpans(text) {
		add(q)
	}
	// FieldsFunc keeps apostrophe / hyphen inside tokens so names
	// like "O'Brien" and "Jean-Luc" survive as single mentions —
	// tokenize.SplitWords splits on those, so we cannot use it
	// directly here.
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || (unicode.IsPunct(r) && r != '\'' && r != '-')
	})
	for i, w := range fields {
		runes := []rune(w)
		if len(runes) < 2 {
			continue
		}
		lower := strings.ToLower(w)
		if i == 0 && intentStopwords.Contains(lower) {
			continue
		}
		if unicode.IsUpper(runes[0]) && !intentStopwords.Contains(lower) {
			add(w)
		}
		if hasCJKRunes(w) && len(runes) >= 2 {
			add(w)
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func inferKinds(text string) []domain.FactKind {
	lower := strings.ToLower(text)
	if isTemporalQuestion(lower) {
		return []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan}
	}
	return nil
}

func inferSubject(text string, entities []string) string {
	if len(entities) == 0 {
		return ""
	}
	lower := strings.ToLower(text)
	best := ""
	bestIdx := len(lower) + 1
	for _, e := range entities {
		e = normalizeEntityMention(e)
		if e == "" {
			continue
		}
		idx := strings.Index(lower, e)
		if idx >= 0 && idx < bestIdx {
			best = e
			bestIdx = idx
		}
	}
	if best != "" {
		return best
	}
	return entities[0]
}

func shouldInferSubject(text string) bool {
	lower := strings.TrimSpace(strings.ToLower(text))
	if isTemporalQuestion(lower) {
		return false
	}
	return strings.HasSuffix(lower, "?") ||
		strings.HasPrefix(lower, "who ") ||
		strings.HasPrefix(lower, "what ") ||
		strings.HasPrefix(lower, "when ") ||
		strings.HasPrefix(lower, "where ") ||
		strings.HasPrefix(lower, "which ") ||
		strings.HasPrefix(lower, "how ") ||
		strings.Contains(lower, "'s ")
}

func isTemporalQuestion(lower string) bool {
	return hasAny(lower, "when", "what date", "which date", "how long", "how many days", "how many months", "how many years")
}

func inferTimeRange(text string) domain.TimeRange {
	text = strings.TrimSpace(text)
	if text == "" {
		return domain.TimeRange{}
	}
	if tr := inferRegexTimeRange(text); !tr.IsZero() {
		return tr
	}
	if queryTimeParser != nil {
		// Use a stable reference for absolute dates. Relative matches are
		// accepted only when their matched text also contains an explicit
		// calendar anchor, so wall-clock time cannot change default recall.
		m, err := queryTimeParser.Parse(text, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		if err == nil && m != nil && hasExplicitCalendarAnchor(m.Text) {
			return rangeFromParsedTime(m.Time, calendarPrecision(m.Text))
		}
	}
	return domain.TimeRange{}
}

func inferRegexTimeRange(text string) domain.TimeRange {
	if m, err := (timex.RegexParser{}).Parse(text, time.Time{}); err == nil && m != nil {
		return dayRange(m.Time)
	}
	lower := strings.ToLower(text)
	if loc := monthDayYearRE.FindStringSubmatchIndex(lower); loc != nil {
		month := monthNumber(lower[loc[2]:loc[3]])
		day, _ := strconv.Atoi(lower[loc[4]:loc[5]])
		year, _ := strconv.Atoi(lower[loc[6]:loc[7]])
		if t, ok := validDate(year, month, day); ok {
			return dayRange(t)
		}
	}
	if loc := dayMonthYearRE.FindStringSubmatchIndex(lower); loc != nil {
		day, _ := strconv.Atoi(lower[loc[2]:loc[3]])
		month := monthNumber(lower[loc[4]:loc[5]])
		year, _ := strconv.Atoi(lower[loc[6]:loc[7]])
		if t, ok := validDate(year, month, day); ok {
			return dayRange(t)
		}
	}
	if loc := monthYearRE.FindStringSubmatchIndex(lower); loc != nil {
		month := monthNumber(lower[loc[2]:loc[3]])
		year, _ := strconv.Atoi(lower[loc[4]:loc[5]])
		if month >= time.January && month <= time.December {
			return monthRange(year, month)
		}
	}
	if loc := anchoredYearRE.FindStringSubmatchIndex(lower); loc != nil {
		year, _ := strconv.Atoi(lower[loc[2]:loc[3]])
		if year >= 1900 && year <= 2100 {
			return yearRange(year)
		}
	}
	return domain.TimeRange{}
}

func rangeFromParsedTime(t time.Time, precision calendarPrecisionKind) domain.TimeRange {
	t = t.UTC()
	switch precision {
	case calendarPrecisionYear:
		return yearRange(t.Year())
	case calendarPrecisionMonth:
		return monthRange(t.Year(), t.Month())
	default:
		return dayRange(t)
	}
}

func dayRange(t time.Time) domain.TimeRange {
	from := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return domain.TimeRange{From: from, To: from.AddDate(0, 0, 1)}
}

func monthRange(year int, month time.Month) domain.TimeRange {
	from := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return domain.TimeRange{From: from, To: from.AddDate(0, 1, 0)}
}

func yearRange(year int) domain.TimeRange {
	from := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	return domain.TimeRange{From: from, To: from.AddDate(1, 0, 0)}
}

func validDate(year int, month time.Month, day int) (time.Time, bool) {
	if year < 1900 || year > 2100 || month < time.January || month > time.December || day < 1 || day > 31 {
		return time.Time{}, false
	}
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if t.Year() != year || t.Month() != month || t.Day() != day {
		return time.Time{}, false
	}
	return t, true
}

type calendarPrecisionKind int

const (
	calendarPrecisionDay calendarPrecisionKind = iota
	calendarPrecisionMonth
	calendarPrecisionYear
)

func calendarPrecision(raw string) calendarPrecisionKind {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" {
		return calendarPrecisionDay
	}
	if monthDayYearRE.MatchString(lower) || dayMonthYearRE.MatchString(lower) || isoDateLikeRE.MatchString(lower) || usSlashLikeRE.MatchString(lower) {
		return calendarPrecisionDay
	}
	if monthYearRE.MatchString(lower) {
		return calendarPrecisionMonth
	}
	if anchoredYearRE.MatchString("in "+lower) || yearOnlyRE.MatchString(lower) {
		return calendarPrecisionYear
	}
	return calendarPrecisionDay
}

func hasExplicitCalendarAnchor(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return monthDayYearRE.MatchString(lower) ||
		dayMonthYearRE.MatchString(lower) ||
		monthYearRE.MatchString(lower) ||
		isoDateLikeRE.MatchString(lower) ||
		usSlashLikeRE.MatchString(lower) ||
		yearOnlyRE.MatchString(lower)
}

func monthNumber(raw string) time.Month {
	switch strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".") {
	case "jan", "january":
		return time.January
	case "feb", "february":
		return time.February
	case "mar", "march":
		return time.March
	case "apr", "april":
		return time.April
	case "may":
		return time.May
	case "jun", "june":
		return time.June
	case "jul", "july":
		return time.July
	case "aug", "august":
		return time.August
	case "sep", "sept", "september":
		return time.September
	case "oct", "october":
		return time.October
	case "nov", "november":
		return time.November
	case "dec", "december":
		return time.December
	default:
		return 0
	}
}

func hasAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func hasCJKRunes(s string) bool {
	for _, r := range s {
		if tokenize.IsCJK(r) {
			return true
		}
	}
	return false
}

const (
	monthPattern = `(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t|tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`
	ordPattern   = `(?:st|nd|rd|th)?`
)

var (
	monthDayYearRE = regexp.MustCompile(`\b(` + monthPattern + `)\s+(\d{1,2})` + ordPattern + `,?\s+(\d{4})\b`)
	dayMonthYearRE = regexp.MustCompile(`\b(\d{1,2})` + ordPattern + `\s+(` + monthPattern + `),?\s+(\d{4})\b`)
	monthYearRE    = regexp.MustCompile(`\b(` + monthPattern + `)\s+(\d{4})\b`)
	anchoredYearRE = regexp.MustCompile(`\b(?:in|during|throughout|around|by|before|after|since|from)\s+(\d{4})\b`)
	yearOnlyRE     = regexp.MustCompile(`^\d{4}$`)
	isoDateLikeRE  = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	usSlashLikeRE  = regexp.MustCompile(`\b\d{1,2}/\d{1,2}/\d{4}\b`)
)
