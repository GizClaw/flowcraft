package timex

import (
	"slices"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/text/phrase"
)

// RelativeMatch is a lexical relative-time phrase found in free text.
type RelativeMatch struct {
	Text  string
	Index int
}

type lexicalRelativePhrase struct {
	text   string
	days   int
	months int
	years  int
	now    bool
}

var lexicalRelativePhrases = []lexicalRelativePhrase{
	{text: "now", now: true},
	{text: "today"}, {text: "tomorrow", days: 1}, {text: "yesterday", days: -1},
	{text: "next week", days: 7}, {text: "last week", days: -7}, {text: "this week"},
	{text: "next month", months: 1}, {text: "last month", months: -1}, {text: "this month"},
	{text: "next year", years: 1}, {text: "last year", years: -1}, {text: "this year"},
	{text: "hoy"}, {text: "mañana", days: 1}, {text: "manana", days: 1}, {text: "ayer", days: -1},
	{text: "la próxima semana", days: 7}, {text: "la proxima semana", days: 7}, {text: "la semana pasada", days: -7},
	{text: "el próximo mes", months: 1}, {text: "el proximo mes", months: 1}, {text: "el mes pasado", months: -1},
	{text: "el próximo año", years: 1}, {text: "el proximo año", years: 1}, {text: "el año pasado", years: -1},
	{text: "aujourd hui"}, {text: "demain", days: 1}, {text: "hier", days: -1},
	{text: "la semaine prochaine", days: 7}, {text: "la semaine dernière", days: -7}, {text: "la semaine derniere", days: -7},
	{text: "le mois prochain", months: 1}, {text: "le mois dernier", months: -1},
	{text: "l année prochaine", years: 1}, {text: "l annee prochaine", years: 1}, {text: "l année dernière", years: -1}, {text: "l annee derniere", years: -1},
	{text: "heute"}, {text: "morgen", days: 1}, {text: "gestern", days: -1},
	{text: "nächste woche", days: 7}, {text: "nachste woche", days: 7}, {text: "letzte woche", days: -7},
	{text: "nächster monat", months: 1}, {text: "nachster monat", months: 1}, {text: "letzter monat", months: -1},
	{text: "nächstes jahr", years: 1}, {text: "nachstes jahr", years: 1}, {text: "letztes jahr", years: -1},
	{text: "hoje"}, {text: "amanhã", days: 1}, {text: "amanha", days: 1}, {text: "ontem", days: -1},
	{text: "próxima semana", days: 7}, {text: "proxima semana", days: 7}, {text: "semana passada", days: -7},
	{text: "próximo mês", months: 1}, {text: "proximo mes", months: 1}, {text: "mês passado", months: -1}, {text: "mes passado", months: -1},
	{text: "próximo ano", years: 1}, {text: "proximo ano", years: 1}, {text: "ano passado", years: -1},
	{text: "vandaag"}, {text: "morgen", days: 1}, {text: "gisteren", days: -1},
	{text: "volgende week", days: 7}, {text: "vorige week", days: -7},
	{text: "volgende maand", months: 1}, {text: "vorige maand", months: -1},
	{text: "volgend jaar", years: 1}, {text: "vorig jaar", years: -1},
	{text: "сегодня"}, {text: "завтра", days: 1}, {text: "вчера", days: -1},
	{text: "на следующей неделе", days: 7}, {text: "на прошлой неделе", days: -7},
	{text: "в следующем месяце", months: 1}, {text: "в прошлом месяце", months: -1},
	{text: "в следующем году", years: 1}, {text: "в прошлом году", years: -1},
	{text: "今天"}, {text: "明天", days: 1}, {text: "昨天", days: -1}, {text: "后天", days: 2}, {text: "前天", days: -2},
	{text: "下周", days: 7}, {text: "上周", days: -7},
	{text: "下个月", months: 1}, {text: "上个月", months: -1},
	{text: "明年", years: 1}, {text: "去年", years: -1},
}

// IsRelativePhrase reports whether raw looks like a relative time expression
// rather than an absolute calendar date. It is intentionally lexical: callers
// should still use a Parser to resolve the expression to a timestamp.
func IsRelativePhrase(raw string) bool {
	return FindRelativePhrase(raw) != nil
}

// FindRelativePhrase returns a lightweight lexical match for common relative
// time expressions. It complements natural-language parsers: callers that need
// a resolved timestamp should still run a Parser such as adapter/when.
func FindRelativePhrase(text string) *RelativeMatch {
	m := phrase.New(text)
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
	for _, unit := range []string{"day", "week", "month", "year"} {
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
	if m.Contains("ago") {
		return relativeMatch(m, "ago")
	}
	if m.StartsWithPhrase("in") {
		if slices.ContainsFunc([]string{"day", "week", "month", "year"}, m.Contains) {
			return relativeMatch(m, "in")
		}
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

func resolveLexicalRelative(raw string, anchor time.Time) (time.Time, bool) {
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}
	base := startOfDay(anchor)
	needle := normalizeRelativeText(raw)
	for _, rel := range lexicalRelativePhrases {
		if normalizeRelativeText(rel.text) != needle {
			continue
		}
		if rel.now {
			return anchor, true
		}
		return base.AddDate(rel.years, rel.months, rel.days), true
	}
	return time.Time{}, false
}

func normalizeRelativeText(raw string) string {
	return strings.ToLower(strings.Join(strings.Fields(raw), " "))
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
