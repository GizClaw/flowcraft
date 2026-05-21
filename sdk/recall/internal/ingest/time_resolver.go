package ingest

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Metadata keys consulted by the default port.TimeResolver. Callers
// (typically the Structurizer or LLM extractor) write either raw time
// hints (*Hint) or already parsed absolute timestamps (*At) into
// these keys; the resolver reads-and-strips them so the canonical fact
// stays clean.
const (
	MetaValidFromHint = "valid_from_hint"
	MetaValidToHint   = "valid_to_hint"
	MetaValidFromAt   = "valid_from_at"
	MetaValidToAt     = "valid_to_at"
)

// SupportedRelativeTimes lists the tokens the default
// TimeResolver understands. Useful for tests / extractor schemas.
var SupportedRelativeTimes = []string{
	"now",
	"today",
	"tomorrow",
	"yesterday",
	"next week",
	"last week",
	"next month",
	"last month",
	"next year",
	"last year",
	"<n> days ago",
	"<n> weeks ago",
	"<n> months ago",
	"<n> years ago",
	"in <n> days",
	"in <n> weeks",
	"in <n> months",
	"in <n> years",
}

type passthroughTimeResolver struct{}

var _ port.TimeResolver = passthroughTimeResolver{}

// Resolve implements port.TimeResolver.
func (passthroughTimeResolver) Resolve(f domain.TemporalFact, now time.Time) domain.TemporalFact {
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	if f.ValidFrom == nil {
		if t, ok := parseAbsoluteFromMeta(f.Metadata, MetaValidFromAt); ok {
			tt := t
			f.ValidFrom = &tt
			delete(f.Metadata, MetaValidFromAt)
			delete(f.Metadata, MetaValidFromHint)
		} else if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidFromHint, now); ok {
			tt := t
			f.ValidFrom = &tt
			delete(f.Metadata, MetaValidFromHint)
		}
	}
	if f.ValidTo == nil {
		if t, ok := parseAbsoluteFromMeta(f.Metadata, MetaValidToAt); ok {
			tt := t
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToAt)
			delete(f.Metadata, MetaValidToHint)
		} else if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidToHint, now); ok {
			tt := t
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToHint)
		}
	}
	return f
}

func parseAbsoluteFromMeta(meta map[string]any, key string) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	raw, ok := meta[key]
	if !ok {
		return time.Time{}, false
	}
	switch v := raw.(type) {
	case time.Time:
		if v.IsZero() {
			return time.Time{}, false
		}
		return v, true
	case string:
		if t, ok := parseAbsoluteTime(strings.TrimSpace(v)); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseRelativeFromMeta extracts and parses a relative-time hint
// from Metadata. Non-string or unrecognised values return ok=false
// so the canonical field stays nil. Recognised hints have their
// metadata entry consumed by the caller.
func parseRelativeFromMeta(meta map[string]any, key string, now time.Time) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	raw, ok := meta[key]
	if !ok {
		return time.Time{}, false
	}
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, false
	}
	return parseRelativeEnglish(s, now)
}

// parseRelativeEnglish handles the small English relative-time
// subset spelled out in SupportedRelativeTimes, plus a handful of
// absolute formats that the LLM extractor is asked to emit when a
// snippet states a calendar date or datetime. Inputs are
// trimmed + lower-cased for the relative table; absolute formats
// are parsed against the original (case-preserving) string so
// "January" / "Jan" survive. Unrecognised strings return ok=false.
func parseRelativeEnglish(in string, now time.Time) (time.Time, bool) {
	raw := strings.Trim(strings.TrimSpace(in), `"'.,;:!?()[]{} `)
	if raw == "" {
		return time.Time{}, false
	}
	if t, ok := parseAbsoluteTime(raw); ok {
		return t, true
	}
	token := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	switch token {
	case "now":
		return now, true
	case "today":
		return startOfDay(now), true
	case "tomorrow":
		return startOfDay(now).AddDate(0, 0, 1), true
	case "yesterday":
		return startOfDay(now).AddDate(0, 0, -1), true
	case "next week":
		return startOfDay(now).AddDate(0, 0, 7), true
	case "last week":
		return startOfDay(now).AddDate(0, 0, -7), true
	case "next month":
		return startOfDay(now).AddDate(0, 1, 0), true
	case "last month":
		return startOfDay(now).AddDate(0, -1, 0), true
	case "next year":
		return startOfDay(now).AddDate(1, 0, 0), true
	case "last year":
		return startOfDay(now).AddDate(-1, 0, 0), true
	}
	if t, ok := parseRelativeQuantity(token, now); ok {
		return t, true
	}
	return time.Time{}, false
}

var relativeQuantityRE = regexp.MustCompile(`^(?:(in) )?([a-z]+|\d+) (day|days|week|weeks|month|months|year|years)(?: (ago|from now))?$`)

func parseRelativeQuantity(token string, now time.Time) (time.Time, bool) {
	m := relativeQuantityRE.FindStringSubmatch(token)
	if m == nil {
		return time.Time{}, false
	}
	n, ok := parseSmallEnglishNumber(m[2])
	if !ok || n == 0 {
		return time.Time{}, false
	}
	direction := 1
	if m[4] == "ago" {
		direction = -1
	}
	unit := m[3]
	base := startOfDay(now)
	switch unit {
	case "day", "days":
		return base.AddDate(0, 0, direction*n), true
	case "week", "weeks":
		return base.AddDate(0, 0, direction*n*7), true
	case "month", "months":
		return base.AddDate(0, direction*n, 0), true
	case "year", "years":
		return base.AddDate(direction*n, 0, 0), true
	default:
		return time.Time{}, false
	}
}

func parseSmallEnglishNumber(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	switch s {
	case "a", "an", "one":
		return 1, true
	case "two":
		return 2, true
	case "three":
		return 3, true
	case "four":
		return 4, true
	case "five":
		return 5, true
	case "six":
		return 6, true
	case "seven":
		return 7, true
	case "eight":
		return 8, true
	case "nine":
		return 9, true
	case "ten":
		return 10, true
	case "eleven":
		return 11, true
	case "twelve":
		return 12, true
	default:
		return 0, false
	}
}

// absoluteTimeLayouts are the calendar shapes the resolver accepts
// alongside the relative token table. Order matters: more specific
// layouts come first so an RFC3339 timestamp is not mis-parsed as a
// bare date. The set is intentionally small — anything beyond this
// belongs to a locale-aware time library, not the canonical resolver.
var absoluteTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02",
	"January 2, 2006",
	"Jan 2, 2006",
	"2 January 2006",
	"2 Jan 2006",
}

// parseAbsoluteTime returns the first absolute-format match against
// the configured layouts. Returns ok=false when no layout parses.
func parseAbsoluteTime(raw string) (time.Time, bool) {
	for _, layout := range absoluteTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
