package compiler

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// TimeResolver normalizes ObservedAt / ValidFrom / ValidTo. It:
//
//   - fills ObservedAt with the supplied now when missing;
//   - if Metadata carries a controlled relative-time hint key
//     (MetaValidFromHint / MetaValidToHint) and the corresponding
//     canonical field is nil, attempts to parse the hint as one of
//     the supported English relative-time tokens. Unrecognised
//     hints are ignored (left as nil) so half-parsed times never
//     bleed into the canonical model.
//
// PR-4 deliberately handles only an English minimal subset; richer
// locale-aware parsing lands in a follow-up phase.
type TimeResolver interface {
	Resolve(f model.TemporalFact, now time.Time) model.TemporalFact
}

// Metadata keys consulted by the default TimeResolver. Callers
// (typically the LLM extractor) write structured relative-time
// strings into these keys; the resolver reads-and-strips them so
// the canonical fact stays clean.
const (
	MetaValidFromHint = "valid_from_hint"
	MetaValidToHint   = "valid_to_hint"
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
}

type passthroughTimeResolver struct{}

// Resolve implements TimeResolver.
func (passthroughTimeResolver) Resolve(f model.TemporalFact, now time.Time) model.TemporalFact {
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	if f.ValidFrom == nil {
		if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidFromHint, now); ok {
			tt := t
			f.ValidFrom = &tt
			delete(f.Metadata, MetaValidFromHint)
		}
	}
	if f.ValidTo == nil {
		if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidToHint, now); ok {
			tt := t
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToHint)
		}
	}
	return f
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
// subset spelled out in SupportedRelativeTimes. Inputs are
// trimmed + lower-cased; whitespace between tokens collapses to
// single spaces; unrecognised strings return ok=false.
func parseRelativeEnglish(in string, now time.Time) (time.Time, bool) {
	token := strings.ToLower(strings.Join(strings.Fields(in), " "))
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
	return time.Time{}, false
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
