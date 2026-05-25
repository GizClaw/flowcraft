package timex

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CalendarPrecision describes how much calendar information a text span
// carries. Callers can turn it into a day/month/year range without guessing.
type CalendarPrecision int

const (
	CalendarPrecisionDay CalendarPrecision = iota
	CalendarPrecisionMonth
	CalendarPrecisionYear
)

// CalendarMatch is a calendar expression found in free text.
type CalendarMatch struct {
	Time      time.Time
	Text      string
	Index     int
	Precision CalendarPrecision
}

// ParseCalendar recognises common absolute calendar expressions in free text.
// It is intentionally generic: recall-specific query intent can map the
// resulting precision to domain time ranges.
func ParseCalendar(text string) *CalendarMatch {
	if cal := ParseNumericCalendar(text); cal != nil {
		return cal
	}
	return parseCalendarWords(text)
}

// ParseNumericCalendar recognises locale-independent numeric calendar
// expressions such as ISO dates. It is separated from ParseCalendar so the
// Extract facade can keep deterministic numeric parsing ahead of natural
// language parsers while letting locale-aware parsers handle word dates first.
func ParseNumericCalendar(text string) *CalendarMatch {
	if text == "" {
		return nil
	}
	if m, err := (RegexParser{}).Parse(text, time.Time{}); err == nil && m != nil {
		return &CalendarMatch{Time: m.Time, Text: m.Text, Index: m.Index, Precision: CalendarPrecisionDay}
	}
	return nil
}

func parseCalendarWords(text string) *CalendarMatch {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	if loc := calendarMonthDayYearRE.FindStringSubmatchIndex(lower); loc != nil {
		month := calendarMonthNumber(lower[loc[2]:loc[3]])
		day, _ := strconv.Atoi(lower[loc[4]:loc[5]])
		year, _ := strconv.Atoi(lower[loc[6]:loc[7]])
		if t, ok := validCalendarDate(year, month, day); ok {
			return &CalendarMatch{Time: t, Text: text[loc[0]:loc[1]], Index: loc[0], Precision: CalendarPrecisionDay}
		}
	}
	if loc := calendarDayMonthYearRE.FindStringSubmatchIndex(lower); loc != nil {
		day, _ := strconv.Atoi(lower[loc[2]:loc[3]])
		month := calendarMonthNumber(lower[loc[4]:loc[5]])
		year, _ := strconv.Atoi(lower[loc[6]:loc[7]])
		if t, ok := validCalendarDate(year, month, day); ok {
			return &CalendarMatch{Time: t, Text: text[loc[0]:loc[1]], Index: loc[0], Precision: CalendarPrecisionDay}
		}
	}
	if loc := calendarMonthYearRE.FindStringSubmatchIndex(lower); loc != nil {
		month := calendarMonthNumber(lower[loc[2]:loc[3]])
		year, _ := strconv.Atoi(lower[loc[4]:loc[5]])
		if month >= time.January && month <= time.December {
			return &CalendarMatch{
				Time:      time.Date(year, month, 1, 0, 0, 0, 0, time.UTC),
				Text:      text[loc[0]:loc[1]],
				Index:     loc[0],
				Precision: CalendarPrecisionMonth,
			}
		}
	}
	if loc := calendarAnchoredYearRE.FindStringSubmatchIndex(lower); loc != nil {
		year, _ := strconv.Atoi(lower[loc[2]:loc[3]])
		if year >= 1900 && year <= 2100 {
			return &CalendarMatch{
				Time:      time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC),
				Text:      text[loc[0]:loc[1]],
				Index:     loc[0],
				Precision: CalendarPrecisionYear,
			}
		}
	}
	if calendarYearOnlyRE.MatchString(strings.TrimSpace(lower)) {
		year, _ := strconv.Atoi(strings.TrimSpace(lower))
		if year >= 1900 && year <= 2100 {
			return &CalendarMatch{
				Time:      time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC),
				Text:      strings.TrimSpace(text),
				Index:     strings.Index(text, strings.TrimSpace(text)),
				Precision: CalendarPrecisionYear,
			}
		}
	}
	return nil
}

// HasCalendarAnchor reports whether text carries an explicit calendar anchor.
func HasCalendarAnchor(text string) bool {
	return ParseCalendar(text) != nil
}

const (
	calendarMonthPattern = `(?:jan(?:uary|eiro)?|feb(?:ruary|rero|vereiro)?|mar(?:ch|zo|ço|co|s)?|maart|apr(?:il)?|may|jun(?:e|io|ho)?|jul(?:y|io|ho)?|aug(?:ust)?|sep(?:t|tember|tembre|tiembre|tembro)?|oct(?:ober|obre|ubre)?|out(?:ubro)?|nov(?:ember|embre|iembre|embro)?|dec(?:ember)?|ene(?:ro)?|abr(?:il)?|ago(?:sto)?|dic(?:iembre)?|dez(?:ember|embro)?|mai(?:o)?|févr(?:ier)?|fevr(?:ier)?|avr(?:il)?|juin|juil(?:let)?|août|aout|déc(?:embre)?|decembre|märz|marz|okt(?:ober)?|mrt|mei|juni|juli|augustus|январ[ья]|феврал[ья]|март[ае]?|апрел[ья]|ма[йя]|июн[ья]|июл[ья]|август[ае]?|сентябр[ья]|октябр[ья]|ноябр[ья]|декабр[ья])`
	calendarOrdPattern   = `(?:st|nd|rd|th|º|ª)?`
)

var (
	calendarMonthDayYearRE = regexp.MustCompile(`\b(` + calendarMonthPattern + `)\s+(\d{1,2})` + calendarOrdPattern + `,?\s+(\d{4})\b`)
	calendarDayMonthYearRE = regexp.MustCompile(`\b(\d{1,2})` + calendarOrdPattern + `\s+(` + calendarMonthPattern + `),?\s+(\d{4})\b`)
	calendarMonthYearRE    = regexp.MustCompile(`\b(` + calendarMonthPattern + `)\s+(\d{4})\b`)
	calendarAnchoredYearRE = regexp.MustCompile(`\b(?:in|during|throughout|around|by|before|after|since|from|en|durante|desde|hasta|avant|après|apres|pendant|depuis|im|in|während|wahrend|seit|bis|em|durante|desde|até|ate|в|во|до|после|с|около)\s+(\d{4})\b`)
	calendarYearOnlyRE     = regexp.MustCompile(`^\d{4}$`)
)

func calendarMonthNumber(raw string) time.Month {
	switch strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".") {
	case "jan", "january", "ene", "enero", "janeiro", "janvier", "januar", "januari", "январь", "января":
		return time.January
	case "feb", "february", "febrero", "fevereiro", "févr", "février", "fevr", "fevrier", "februar", "februari", "февраль", "февраля":
		return time.February
	case "mar", "march", "marzo", "março", "marco", "mars", "märz", "marz", "mrt", "maart", "март", "марта":
		return time.March
	case "apr", "april", "abr", "abril", "avr", "avril", "апрель", "апреля":
		return time.April
	case "may", "mai", "maio", "mei", "май", "мая":
		return time.May
	case "jun", "june", "junio", "junho", "juin", "juni", "июнь", "июня":
		return time.June
	case "jul", "july", "julio", "julho", "juil", "juillet", "juli", "июль", "июля":
		return time.July
	case "aug", "august", "ago", "agosto", "août", "aout", "augustus", "август", "августа":
		return time.August
	case "sep", "sept", "september", "septiembre", "setembro", "septembre", "сентябрь", "сентября":
		return time.September
	case "oct", "october", "octubre", "out", "outubro", "octobre", "okt", "oktober", "октябрь", "октября":
		return time.October
	case "nov", "november", "noviembre", "novembro", "novembre", "ноябрь", "ноября":
		return time.November
	case "dec", "december", "dic", "diciembre", "déc", "décembre", "decembre", "dez", "dezembro", "dezember", "декабрь", "декабря":
		return time.December
	default:
		return 0
	}
}

func validCalendarDate(year int, month time.Month, day int) (time.Time, bool) {
	if year < 1900 || year > 2100 || month < time.January || month > time.December || day < 1 || day > 31 {
		return time.Time{}, false
	}
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if t.Year() != year || t.Month() != month || t.Day() != day {
		return time.Time{}, false
	}
	return t, true
}
