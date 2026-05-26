package timex

import (
	"regexp"
	"strconv"
	"strings"
)

// DurationMatch is a duration expression found in free text.
type DurationMatch struct {
	Text  string
	Index int
	Timex string
}

var durationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bfor\s+(\d+|one|two|three|four|five|six|seven|eight|nine|ten|a|an)\s+(days?|weeks?|weekends?|months?|years?)\b`),
	regexp.MustCompile(`\bduring\s+(\d+|one|two|three|four|five|six|seven|eight|nine|ten|a|an)\s+(days?|weeks?|weekends?|months?|years?)\b`),
	regexp.MustCompile(`\b(?:for\s+the\s+past|over\s+the\s+past|over|past)\s+(\d+|one|two|three|four|five|six|seven|eight|nine|ten|a|an)\s+(days?|weeks?|weekends?|months?|years?)\b`),
	regexp.MustCompile(`\bdurante\s+(\d+|un|una|uno|dos|tres|cuatro|cinco|seis|siete|ocho|nueve|diez)\s+(fines?\s+de\s+semana|d[ií]as?|semanas?|mes(?:es)?|a[ñn]os?)\b`),
	regexp.MustCompile(`\bpendant\s+(\d+|un|une|deux|trois|quatre|cinq|six|sept|huit|neuf|dix)\s+(week-?ends?|jours?|semaines?|mois|ans?|ann[ée]es?)\b`),
	regexp.MustCompile(`(?:持续|历时|为期)([0-9]+|[一二两三四五六七八九十百千万零〇]+)个?(天|日|周末|周|星期|礼拜|月|年)`),
}

// FindDurationPhrase returns the first duration expression in text.
func FindDurationPhrase(text string) *DurationMatch {
	lower := strings.ToLower(text)
	for _, re := range durationPatterns {
		loc := re.FindStringSubmatchIndex(lower)
		if len(loc) < 6 {
			continue
		}
		count := lower[loc[2]:loc[3]]
		unit := lower[loc[4]:loc[5]]
		timex, ok := durationTimex(count, unit)
		if !ok {
			continue
		}
		return &DurationMatch{
			Text:  lower[loc[0]:loc[1]],
			Index: loc[0],
			Timex: timex,
		}
	}
	return nil
}

func durationTimex(count, unit string) (string, bool) {
	n, ok := parseRelativeCount(strings.ToLower(count))
	if !ok || n <= 0 {
		return "", false
	}
	normalizedUnit, ok := normalizeRelativeUnit(unit)
	if !ok {
		return "", false
	}
	suffix := ""
	switch normalizedUnit {
	case "day":
		suffix = "D"
	case "week":
		suffix = "W"
	case "weekend":
		// A weekend duration spans two calendar days.
		suffix = "D"
		n *= 2
	case "month":
		suffix = "M"
	case "year":
		suffix = "Y"
	default:
		return "", false
	}
	return "P" + strconv.Itoa(n) + suffix, true
}
