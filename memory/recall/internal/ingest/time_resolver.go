package ingest

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/text/timex"
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

	MetaValidFromSource = "valid_from_source"
	MetaValidFromText   = "valid_from_text"
	MetaValidFromTimex  = "valid_from_timex"
	MetaValidFromKind   = "valid_from_kind"
	MetaValidFromPrec   = "valid_from_precision"
)

const (
	ValidFromSourceContentExplicit = "content_explicit"
	ValidFromSourceContentRelative = "content_relative"
	ValidFromSourceTimeFallback    = "source_time_fallback"
)

type passthroughTimeResolver struct{}

var _ port.TimeResolver = passthroughTimeResolver{}

// Resolve implements port.TimeResolver.
func (passthroughTimeResolver) Resolve(f domain.TemporalFact, now time.Time) domain.TemporalFact {
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	if f.ValidFrom == nil {
		if expr, ok := parseExpressionFromMeta(f.Metadata, MetaValidFromAt, time.Time{}, false); ok {
			tt := expressionValidFrom(expr)
			f.ValidFrom = &tt
			setValidToFromExpressionRange(&f, expr)
			preserveValidFromExpression(f.Metadata, expr)
			preserveValidFromText(f.Metadata)
			delete(f.Metadata, MetaValidFromAt)
			delete(f.Metadata, MetaValidFromHint)
		} else if expr, ok := parseExpressionFromMeta(f.Metadata, MetaValidFromHint, now, true); ok {
			tt := expressionValidFrom(expr)
			f.ValidFrom = &tt
			setValidToFromExpressionRange(&f, expr)
			preserveValidFromExpression(f.Metadata, expr)
			preserveValidFromText(f.Metadata)
			delete(f.Metadata, MetaValidFromHint)
		}
	}
	if f.ValidTo == nil {
		if expr, ok := parseExpressionFromMeta(f.Metadata, MetaValidToAt, time.Time{}, false); ok {
			tt := expressionValidFrom(expr)
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToAt)
			delete(f.Metadata, MetaValidToHint)
		} else if expr, ok := parseExpressionFromMeta(f.Metadata, MetaValidToHint, now, true); ok {
			tt := expressionValidFrom(expr)
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToHint)
		}
	}
	return f
}

func setValidToFromExpressionRange(f *domain.TemporalFact, expr *timex.Expression) {
	if f == nil || expr == nil || !expr.HasRange || expr.End.IsZero() || hasValidToMeta(f.Metadata) || f.ValidTo != nil {
		return
	}
	tt := expr.End.UTC()
	f.ValidTo = &tt
}

func hasValidToMeta(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	_, hasAt := meta[MetaValidToAt]
	_, hasHint := meta[MetaValidToHint]
	return hasAt || hasHint
}

func preserveValidFromText(meta map[string]any) {
	if len(meta) == 0 {
		return
	}
	if _, ok := meta[MetaValidFromText]; ok {
		return
	}
	if raw, ok := meta[MetaValidFromHint]; ok {
		meta[MetaValidFromText] = raw
	}
}

func preserveValidFromExpression(meta map[string]any, expr *timex.Expression) {
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
}

func parseExpressionFromMeta(meta map[string]any, key string, now time.Time, allowRelative bool) (*timex.Expression, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	raw, ok := meta[key]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case time.Time:
		if v.IsZero() {
			return nil, false
		}
		return expressionFromTime(v), true
	case string:
		return parseTimeExpression(strings.TrimSpace(v), now, allowRelative)
	}
	return nil, false
}

func parseTimeExpression(in string, now time.Time, allowRelative bool) (*timex.Expression, bool) {
	raw := strings.Trim(strings.TrimSpace(in), `"'.,;:!?()[]{} `)
	if raw == "" {
		return nil, false
	}
	if expr, ok := parseExactTimestampExpression(raw); ok {
		return expr, true
	}
	expr, err := timex.Extract(raw, now)
	if err != nil || expr == nil || expr.Time.IsZero() {
		return nil, false
	}
	if !isValidFromExpression(expr) {
		return nil, false
	}
	if expr.Relative && !allowRelative {
		return nil, false
	}
	return expr, true
}

func isValidFromExpression(expr *timex.Expression) bool {
	if expr == nil {
		return false
	}
	switch expr.Kind {
	case timex.ExpressionKindDate, timex.ExpressionKindDateRange:
		return true
	case timex.ExpressionKindDuration, timex.ExpressionKindSet:
		return false
	default:
		return !expr.Time.IsZero()
	}
}

func parseExactTimestampExpression(raw string) (*timex.Expression, bool) {
	match, err := (timex.RegexParser{}).Parse(raw, time.Time{})
	if err != nil || match == nil {
		return nil, false
	}
	if strings.TrimSpace(match.Text) != raw || !strings.Contains(match.Text, ":") {
		return nil, false
	}
	return expressionFromTime(match.Time), true
}

func expressionFromTime(t time.Time) *timex.Expression {
	t = t.UTC()
	return &timex.Expression{
		Match:  timex.Match{Time: t},
		Source: timex.MatchSourceCalendar,
		Kind:   timex.ExpressionKindDate,
		Timex:  t.Format(time.RFC3339Nano),
	}
}

func expressionValidFrom(expr *timex.Expression) time.Time {
	if expr == nil {
		return time.Time{}
	}
	if expr.HasRange && !expr.Start.IsZero() {
		return expr.Start.UTC()
	}
	return expr.Time.UTC()
}

func parseTimeHint(in string, now time.Time, allowRelative bool) (time.Time, bool) {
	expr, ok := parseTimeExpression(in, now, allowRelative)
	if !ok {
		return time.Time{}, false
	}
	return expressionValidFrom(expr), true
}

func calendarPrecisionString(p timex.CalendarPrecision) string {
	switch p {
	case timex.CalendarPrecisionDay:
		return "day"
	case timex.CalendarPrecisionWeek:
		return "week"
	case timex.CalendarPrecisionWeekend:
		return "weekend"
	case timex.CalendarPrecisionMonth:
		return "month"
	case timex.CalendarPrecisionYear:
		return "year"
	default:
		return ""
	}
}
