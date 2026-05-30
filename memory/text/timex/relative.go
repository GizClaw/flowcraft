package timex

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/text/phrase"
)

// RelativeMatch is a lexical relative-time phrase found in free text.
type RelativeMatch struct {
	Text  string
	Index int
}

type relativeResolution struct {
	Time         time.Time
	Precision    CalendarPrecision
	HasPrecision bool
	Start        time.Time
	End          time.Time
	HasRange     bool
}

// IsRelativePhrase reports whether raw looks like a relative time expression
// rather than an absolute calendar date. It is intentionally lexical: callers
// should still use a Parser to resolve the expression to a timestamp.
func IsRelativePhrase(raw string) bool {
	return FindRelativePhrase(raw) != nil
}

// FindRelativePhrase returns a lightweight lexical match for common relative
// time expressions. Use Extract when callers need the phrase resolved against
// an anchor timestamp.
func FindRelativePhrase(text string) *RelativeMatch {
	m := phrase.New(text)
	if rel := findDirectionalRelativePhrase(text); rel != nil {
		return rel
	}
	for _, rel := range lexicalRelativePhrases {
		parts := strings.Fields(rel.text)
		if len(parts) > 0 && m.ContainsPhrase(parts...) {
			return relativeMatch(m, rel.text)
		}
		if m.ContainsLiteral(rel.text) {
			return relativeMatch(m, rel.text)
		}
	}
	for _, raw := range []string{
		"from now",
	} {
		parts := strings.Fields(raw)
		if m.ContainsPhrase(parts...) {
			return relativeMatch(m, raw)
		}
	}
	for _, unit := range []string{"day", "week", "weekend", "month", "year"} {
		if m.ContainsPhrase("next", unit) {
			return relativeMatch(m, "next "+unit)
		}
		if m.ContainsPhrase("last", unit) {
			return relativeMatch(m, "last "+unit)
		}
		if m.ContainsPhrase("this", unit) {
			return relativeMatch(m, "this "+unit)
		}
	}
	for _, weekday := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
		for _, prefix := range []string{"next", "last", "this"} {
			if m.ContainsPhrase(prefix, weekday) {
				return relativeMatch(m, prefix+" "+weekday)
			}
		}
	}
	if rel := findCountedRelativePhrase(text); rel != nil {
		return rel
	}
	if m.Contains("ago") {
		return relativeMatch(m, "ago")
	}
	for _, raw := range []string{"前", "后"} {
		if m.ContainsLiteral(raw) {
			return relativeMatch(m, raw)
		}
	}
	return nil
}

func relativeMatch(m phrase.Matcher, raw string) *RelativeMatch {
	return &RelativeMatch{Text: raw, Index: m.IndexLiteral(raw)}
}

func matchCountedRelative(text string) (countedRelativeMatch, bool) {
	lower := strings.ToLower(text)
	for _, rule := range countedRelativeRules {
		loc := rule.pattern.FindStringSubmatchIndex(lower)
		if loc == nil {
			continue
		}
		count, unit, ok := countedRuleValues(lower, loc, rule)
		if !ok {
			continue
		}
		direction := rule.direction
		if rule.directionGroup > 0 {
			rawDirection, ok := submatchText(lower, loc, rule.directionGroup)
			if !ok {
				continue
			}
			direction = -1
			if rawDirection == "后" {
				direction = 1
			}
		}
		return countedRelativeMatch{
			text:      lower[loc[0]:loc[1]],
			index:     loc[0],
			count:     count,
			unit:      unit,
			direction: direction,
		}, true
	}
	return countedRelativeMatch{}, false
}

func countedRuleValues(text string, loc []int, rule countedRelativeRule) (string, string, bool) {
	for i, countGroup := range rule.countGroups {
		if i >= len(rule.unitGroups) {
			break
		}
		count, ok := submatchText(text, loc, countGroup)
		if !ok || count == "" {
			continue
		}
		unit, ok := submatchText(text, loc, rule.unitGroups[i])
		if !ok || unit == "" {
			continue
		}
		return count, unit, true
	}
	return "", "", false
}

func matchDirectionalRelative(text string) (directionalRelativeMatch, bool) {
	lower := strings.ToLower(text)
	loc := relativeDirectionalRule.pattern.FindStringSubmatchIndex(lower)
	if loc == nil {
		return directionalRelativeMatch{}, false
	}
	direction, ok := submatchText(lower, loc, relativeDirectionalRule.directionGroup)
	if !ok {
		return directionalRelativeMatch{}, false
	}
	unit, ok := submatchText(lower, loc, relativeDirectionalRule.unitGroup)
	if !ok {
		return directionalRelativeMatch{}, false
	}
	return directionalRelativeMatch{
		text:      lower[loc[0]:loc[1]],
		index:     loc[0],
		direction: direction,
		unit:      unit,
	}, true
}

func submatchText(text string, loc []int, group int) (string, bool) {
	i := group * 2
	if i+1 >= len(loc) || loc[i] < 0 || loc[i+1] < 0 {
		return "", false
	}
	return text[loc[i]:loc[i+1]], true
}

func findDirectionalRelativePhrase(text string) *RelativeMatch {
	match, ok := matchDirectionalRelative(text)
	if !ok {
		return nil
	}
	return &RelativeMatch{Text: match.text, Index: match.index}
}

func findCountedRelativePhrase(text string) *RelativeMatch {
	match, ok := matchCountedRelative(text)
	if !ok {
		return nil
	}
	return &RelativeMatch{Text: match.text, Index: match.index}
}

func resolveLexicalRelative(raw string, anchor time.Time) (relativeResolution, bool) {
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}
	base := startOfDay(anchor)
	if res, ok := resolveCountedRelative(raw, base); ok {
		return res, true
	}
	if res, ok := resolveDirectionalRelative(raw, base); ok {
		return res, true
	}
	needle := normalizeRelativeText(raw)
	for _, rel := range lexicalRelativePhrases {
		if normalizeRelativeText(rel.text) != needle {
			continue
		}
		if rel.now {
			return relativeResolution{Time: anchor}, true
		}
		t := base.AddDate(rel.years, rel.months, rel.days)
		return resolutionWithPrecision(t, precisionForRelativeText(rel.text)), true
	}
	return relativeResolution{}, false
}

func resolveCountedRelative(raw string, base time.Time) (relativeResolution, bool) {
	match, ok := matchCountedRelative(raw)
	if !ok {
		return relativeResolution{}, false
	}
	return resolveCountedParts(base, match.count, match.unit, match.direction)
}

func resolveCountedParts(base time.Time, count, unit string, direction int) (relativeResolution, bool) {
	n, ok := parseRelativeCount(strings.ToLower(count))
	if !ok || n <= 0 {
		return relativeResolution{}, false
	}
	normalizedUnit, ok := normalizeRelativeUnit(unit)
	if !ok {
		return relativeResolution{}, false
	}
	return countedResolution(base, n, normalizedUnit, direction)
}

func resolveDirectionalRelative(raw string, base time.Time) (relativeResolution, bool) {
	match, ok := matchDirectionalRelative(raw)
	if !ok {
		return relativeResolution{}, false
	}
	dir, unit := match.direction, match.unit
	switch unit {
	case "day":
		return resolutionWithPrecision(base.AddDate(0, 0, directionalOffset(dir)), CalendarPrecisionDay), true
	case "week":
		return resolutionWithPrecision(base.AddDate(0, 0, 7*directionalOffset(dir)), CalendarPrecisionWeek), true
	case "weekend":
		return resolutionWithPrecision(directionalWeekendStart(base, dir), CalendarPrecisionWeekend), true
	case "month":
		return resolutionWithPrecision(base.AddDate(0, directionalOffset(dir), 0), CalendarPrecisionMonth), true
	case "year":
		return resolutionWithPrecision(base.AddDate(directionalOffset(dir), 0, 0), CalendarPrecisionYear), true
	default:
		if wd, ok := parseWeekday(unit); ok {
			return resolutionWithPrecision(directionalWeekday(base, dir, wd), CalendarPrecisionDay), true
		}
		return relativeResolution{}, false
	}
}

func directionalOffset(dir string) int {
	switch dir {
	case "next":
		return 1
	case "last":
		return -1
	default:
		return 0
	}
}

func countedResolution(base time.Time, n int, unit string, direction int) (relativeResolution, bool) {
	switch unit {
	case "day":
		return resolutionWithPrecision(base.AddDate(0, 0, direction*n), CalendarPrecisionDay), true
	case "week":
		return resolutionWithPrecision(base.AddDate(0, 0, direction*7*n), CalendarPrecisionWeek), true
	case "weekend":
		if direction < 0 {
			return resolutionWithPrecision(weekendStartAgo(base, n), CalendarPrecisionWeekend), true
		}
		return resolutionWithPrecision(weekendStartFromNow(base, n), CalendarPrecisionWeekend), true
	case "month":
		return resolutionWithPrecision(base.AddDate(0, direction*n, 0), CalendarPrecisionMonth), true
	case "year":
		return resolutionWithPrecision(base.AddDate(direction*n, 0, 0), CalendarPrecisionYear), true
	default:
		return relativeResolution{}, false
	}
}

func weekendStartAgo(base time.Time, n int) time.Time {
	daysSinceSaturday := (int(base.Weekday()) - int(time.Saturday) + 7) % 7
	start := base.AddDate(0, 0, -daysSinceSaturday)
	if base.Weekday() == time.Saturday || base.Weekday() == time.Sunday {
		start = start.AddDate(0, 0, -7)
	}
	return start.AddDate(0, 0, -7*(n-1))
}

func weekendStartFromNow(base time.Time, n int) time.Time {
	start := weekendStartThisWeek(base)
	if !base.Before(start) {
		start = start.AddDate(0, 0, 7)
	}
	return start.AddDate(0, 0, 7*(n-1))
}

func directionalWeekendStart(base time.Time, dir string) time.Time {
	this := weekendStartThisWeek(base)
	switch dir {
	case "next":
		if base.Before(this) {
			return this
		}
		return this.AddDate(0, 0, 7)
	case "last":
		if base.Weekday() == time.Saturday || base.Weekday() == time.Sunday {
			return this.AddDate(0, 0, -7)
		}
		return this.AddDate(0, 0, -7)
	default:
		return this
	}
}

func weekendStartThisWeek(base time.Time) time.Time {
	daysUntilSaturday := (int(time.Saturday) - int(base.Weekday()) + 7) % 7
	if base.Weekday() == time.Sunday {
		daysUntilSaturday = -1
	}
	return base.AddDate(0, 0, daysUntilSaturday)
}

func directionalWeekday(base time.Time, dir string, target time.Weekday) time.Time {
	delta := (int(target) - int(base.Weekday()) + 7) % 7
	switch dir {
	case "next":
		if delta == 0 {
			delta = 7
		}
	case "last":
		if delta == 0 {
			delta = -7
		} else {
			delta -= 7
		}
	}
	return base.AddDate(0, 0, delta)
}

func parseWeekday(raw string) (time.Weekday, bool) {
	switch raw {
	case "sunday":
		return time.Sunday, true
	case "monday":
		return time.Monday, true
	case "tuesday":
		return time.Tuesday, true
	case "wednesday":
		return time.Wednesday, true
	case "thursday":
		return time.Thursday, true
	case "friday":
		return time.Friday, true
	case "saturday":
		return time.Saturday, true
	default:
		return time.Sunday, false
	}
}

func precisionForRelativeText(raw string) CalendarPrecision {
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "year") || strings.Contains(lower, "año") || strings.Contains(lower, "année") || strings.Contains(lower, "jahr") || strings.Contains(lower, "ano") || strings.Contains(lower, "год") || strings.Contains(lower, "年"):
		return CalendarPrecisionYear
	case strings.Contains(lower, "month") || strings.Contains(lower, "mes") || strings.Contains(lower, "mois") || strings.Contains(lower, "monat") || strings.Contains(lower, "mês") || strings.Contains(lower, "месяц") || strings.Contains(lower, "月"):
		return CalendarPrecisionMonth
	case strings.Contains(lower, "weekend"):
		return CalendarPrecisionWeekend
	case strings.Contains(lower, "week") || strings.Contains(lower, "semana") || strings.Contains(lower, "semaine") || strings.Contains(lower, "woche") || strings.Contains(lower, "недел") || strings.Contains(lower, "周"):
		return CalendarPrecisionWeek
	default:
		return CalendarPrecisionDay
	}
}

func resolutionWithPrecision(t time.Time, precision CalendarPrecision) relativeResolution {
	start, end, hasRange := rangeForPrecision(t, precision)
	return relativeResolution{
		Time:         t,
		Precision:    precision,
		HasPrecision: true,
		Start:        start,
		End:          end,
		HasRange:     hasRange,
	}
}

func normalizeRelativeText(raw string) string {
	return strings.ToLower(strings.Join(strings.Fields(raw), " "))
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func startOfWeek(t time.Time) time.Time {
	base := startOfDay(t)
	daysSinceMonday := (int(base.Weekday()) - int(time.Monday) + 7) % 7
	return base.AddDate(0, 0, -daysSinceMonday)
}
