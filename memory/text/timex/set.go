package timex

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SetMatch is a recurring time expression found in free text.
type SetMatch struct {
	Text      string
	Index     int
	Timex     string
	Precision CalendarPrecision
}

var (
	englishSetRE           = regexp.MustCompile(`\b(?:every|each)\s+(day|week|weekend|month|year|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)
	englishEveryOtherSetRE = regexp.MustCompile(`\bevery\s+other\s+(day|week|weekend|month|year|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)
	englishFrequencySetRE  = regexp.MustCompile(`\b(once|twice|\d+|one|two|three|four|five|six|seven|eight|nine|ten)\s+a\s+(day|week|month|year)\b`)
	spanishSetRE           = regexp.MustCompile(`\bcada\s+(d[ií]a|semana|fin\s+de\s+semana|mes|a[ñn]o|lunes|martes|mi[ée]rcoles|jueves|viernes|s[áa]bado|domingo)\b`)
	frenchSetRE            = regexp.MustCompile(`\bchaque\s+(jour|semaine|week-?end|mois|an|ann[ée]e|lundi|mardi|mercredi|jeudi|vendredi|samedi|dimanche)\b`)
	chineseSetRE           = regexp.MustCompile(`每(天|日|周末|周|星期|礼拜|月|年)`)
)

// FindSetPhrase returns the first recurring time expression in text.
func FindSetPhrase(text string) *SetMatch {
	lower := strings.ToLower(text)
	if loc := englishEveryOtherSetRE.FindStringSubmatchIndex(lower); loc != nil {
		unit := lower[loc[2]:loc[3]]
		timex, precision, ok := setTimex(unit)
		if ok {
			if timex == "P1D" {
				timex = "P2D"
			} else if timex == "P1W" || strings.HasPrefix(timex, "XXXX-WXX-") {
				timex = "P2W"
				precision = CalendarPrecisionWeek
			} else if timex == "P1M" {
				timex = "P2M"
			} else if timex == "P1Y" {
				timex = "P2Y"
			}
			return &SetMatch{Text: lower[loc[0]:loc[1]], Index: loc[0], Timex: timex, Precision: precision}
		}
	}
	if loc := englishFrequencySetRE.FindStringSubmatchIndex(lower); loc != nil {
		unit := lower[loc[4]:loc[5]]
		timex, precision, ok := setTimex(unit)
		if ok {
			return &SetMatch{Text: lower[loc[0]:loc[1]], Index: loc[0], Timex: timex, Precision: precision}
		}
	}
	for _, re := range []*regexp.Regexp{englishSetRE, spanishSetRE, frenchSetRE, chineseSetRE} {
		loc := re.FindStringSubmatchIndex(lower)
		if len(loc) < 4 {
			continue
		}
		unit := lower[loc[2]:loc[3]]
		timex, precision, ok := setTimex(unit)
		if !ok {
			continue
		}
		return &SetMatch{
			Text:      lower[loc[0]:loc[1]],
			Index:     loc[0],
			Timex:     timex,
			Precision: precision,
		}
	}
	return nil
}

func setTimex(unit string) (string, CalendarPrecision, bool) {
	normalizedUnit, ok := normalizeRelativeUnit(unit)
	if ok {
		switch normalizedUnit {
		case "day":
			return "P1D", CalendarPrecisionDay, true
		case "week":
			return "P1W", CalendarPrecisionWeek, true
		case "weekend":
			return "XXXX-WXX-WE", CalendarPrecisionWeekend, true
		case "month":
			return "P1M", CalendarPrecisionMonth, true
		case "year":
			return "P1Y", CalendarPrecisionYear, true
		}
	}
	if wd, ok := parseLocalizedWeekday(unit); ok {
		return weekdaySetTimex(wd), CalendarPrecisionDay, true
	}
	return "", CalendarPrecisionDay, false
}

func parseLocalizedWeekday(raw string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sunday", "domingo", "dimanche":
		return time.Sunday, true
	case "monday", "lunes", "lundi":
		return time.Monday, true
	case "tuesday", "martes", "mardi":
		return time.Tuesday, true
	case "wednesday", "miércoles", "miercoles", "mercredi":
		return time.Wednesday, true
	case "thursday", "jueves", "jeudi":
		return time.Thursday, true
	case "friday", "viernes", "vendredi":
		return time.Friday, true
	case "saturday", "sábado", "sabado", "samedi":
		return time.Saturday, true
	default:
		return time.Sunday, false
	}
}

func weekdaySetTimex(wd time.Weekday) string {
	if wd == time.Sunday {
		return "XXXX-WXX-7"
	}
	return "XXXX-WXX-" + strconv.Itoa(int(wd))
}
