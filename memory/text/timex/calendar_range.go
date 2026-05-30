package timex

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type calendarRangeMatch struct {
	Text      string
	Index     int
	Start     time.Time
	End       time.Time
	Precision CalendarPrecision
	Timex     string
}

var (
	quarterRE          = regexp.MustCompile(`\b(?:q([1-4])|quarter\s+([1-4])|([1-4])(?:st|nd|rd|th)\s+quarter)\s+(?:of\s+)?(\d{4})\b`)
	yearQuarterRE      = regexp.MustCompile(`\b(\d{4})\s+q([1-4])\b`)
	partMonthRE        = regexp.MustCompile(`\b(early|mid|late)[-\s]+(` + calendarMonthPattern + `)(?:\s+(\d{4}))?\b`)
	partYearRE         = regexp.MustCompile(`\b(early|mid|late)-?(\d{4})\b`)
	seasonYearRE       = regexp.MustCompile(`\b(spring|summer|fall|autumn|winter)\s+(\d{4})\b`)
	relativeSeasonRE   = regexp.MustCompile(`\b(last|this|next)\s+(spring|summer|fall|autumn|winter)\b`)
	betweenMonthsRE    = regexp.MustCompile(`\bbetween\s+(` + calendarMonthPattern + `)\s+and\s+(` + calendarMonthPattern + `)\s+(\d{4})\b`)
	betweenISODateRE   = regexp.MustCompile(`\bbetween\s+(\d{4}-\d{2}-\d{2})\s+and\s+(\d{4}-\d{2}-\d{2})\b`)
	relativeToAnchorRE = regexp.MustCompile(`\b(\d+|one|two|three|four|five|six|seven|eight|nine|ten|[一二两三四五六七八九十百零〇]+)\s+(days?|weeks?|weekends?|months?|years?|天|日|周末|周|星期|礼拜|月|年)\s+(before|after)\s+(.+)$`)
	openRangeRE        = regexp.MustCompile(`\b(since|until|through|throughout)\s+(.+)$`)
)

func findCalendarRange(text string, anchor time.Time) *calendarRangeMatch {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	if match := matchQuarter(text, lower); match != nil {
		return match
	}
	if match := matchPartMonth(text, lower, anchor); match != nil {
		return match
	}
	if match := matchPartYear(text, lower); match != nil {
		return match
	}
	if match := matchSeason(text, lower, anchor); match != nil {
		return match
	}
	if match := matchBetween(text, lower); match != nil {
		return match
	}
	if match := matchRelativeToAnchor(text, lower); match != nil {
		return match
	}
	if match := matchOpenRange(text, lower, anchor); match != nil {
		return match
	}
	return nil
}

func matchQuarter(text, lower string) *calendarRangeMatch {
	if loc := quarterRE.FindStringSubmatchIndex(lower); loc != nil {
		q, _ := firstIntGroup(lower, loc, 1, 2, 3)
		year, _ := strconv.Atoi(lower[loc[8]:loc[9]])
		return quarterMatch(text, loc, year, q)
	}
	if loc := yearQuarterRE.FindStringSubmatchIndex(lower); loc != nil {
		year, _ := strconv.Atoi(lower[loc[2]:loc[3]])
		q, _ := strconv.Atoi(lower[loc[4]:loc[5]])
		return quarterMatch(text, loc, year, q)
	}
	return nil
}

func quarterMatch(text string, loc []int, year, quarter int) *calendarRangeMatch {
	if year < 1900 || year > 2100 || quarter < 1 || quarter > 4 {
		return nil
	}
	startMonth := time.Month((quarter-1)*3 + 1)
	start := time.Date(year, startMonth, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 3, 0)
	return &calendarRangeMatch{
		Text:      calendarRangeText(text, loc),
		Index:     loc[0],
		Start:     start,
		End:       end,
		Precision: CalendarPrecisionMonth,
		Timex:     fmt.Sprintf("%04d-Q%d", year, quarter),
	}
}

func matchPartMonth(text, lower string, anchor time.Time) *calendarRangeMatch {
	loc := partMonthRE.FindStringSubmatchIndex(lower)
	if loc == nil {
		return nil
	}
	part := lower[loc[2]:loc[3]]
	month := calendarMonthNumber(lower[loc[4]:loc[5]])
	if month == 0 {
		return nil
	}
	year := anchor.UTC().Year()
	if loc[6] >= 0 {
		year, _ = strconv.Atoi(lower[loc[6]:loc[7]])
	}
	monthStart := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)
	start, end := partOfRange(monthStart, nextMonth, part)
	return &calendarRangeMatch{
		Text:      calendarRangeText(text, loc),
		Index:     loc[0],
		Start:     start,
		End:       end,
		Precision: CalendarPrecisionDay,
		Timex:     fmt.Sprintf("(%s,%s)", start.Format("2006-01-02"), end.Format("2006-01-02")),
	}
}

func matchPartYear(text, lower string) *calendarRangeMatch {
	loc := partYearRE.FindStringSubmatchIndex(lower)
	if loc == nil {
		return nil
	}
	part := lower[loc[2]:loc[3]]
	year, _ := strconv.Atoi(lower[loc[4]:loc[5]])
	start, end := partOfRange(
		time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC),
		part,
	)
	return &calendarRangeMatch{
		Text:      calendarRangeText(text, loc),
		Index:     loc[0],
		Start:     start,
		End:       end,
		Precision: CalendarPrecisionMonth,
		Timex:     fmt.Sprintf("(%s,%s)", start.Format("2006-01"), end.Format("2006-01")),
	}
}

func matchSeason(text, lower string, anchor time.Time) *calendarRangeMatch {
	if loc := seasonYearRE.FindStringSubmatchIndex(lower); loc != nil {
		season := lower[loc[2]:loc[3]]
		year, _ := strconv.Atoi(lower[loc[4]:loc[5]])
		return seasonMatch(text, loc, season, year)
	}
	if loc := relativeSeasonRE.FindStringSubmatchIndex(lower); loc != nil {
		dir := lower[loc[2]:loc[3]]
		season := lower[loc[4]:loc[5]]
		year := anchor.UTC().Year()
		if dir == "last" {
			year--
		} else if dir == "next" {
			year++
		}
		return seasonMatch(text, loc, season, year)
	}
	return nil
}

func seasonMatch(text string, loc []int, season string, year int) *calendarRangeMatch {
	start, end, ok := seasonRange(season, year)
	if !ok {
		return nil
	}
	return &calendarRangeMatch{
		Text:      calendarRangeText(text, loc),
		Index:     loc[0],
		Start:     start,
		End:       end,
		Precision: CalendarPrecisionMonth,
		Timex:     fmt.Sprintf("(%s,%s)", start.Format("2006-01"), end.Format("2006-01")),
	}
}

func matchBetween(text, lower string) *calendarRangeMatch {
	if loc := betweenMonthsRE.FindStringSubmatchIndex(lower); loc != nil {
		startMonth := calendarMonthNumber(lower[loc[2]:loc[3]])
		endMonth := calendarMonthNumber(lower[loc[4]:loc[5]])
		year, _ := strconv.Atoi(lower[loc[6]:loc[7]])
		if startMonth == 0 || endMonth == 0 {
			return nil
		}
		start := time.Date(year, startMonth, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(year, endMonth, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		return &calendarRangeMatch{Text: calendarRangeText(text, loc), Index: loc[0], Start: start, End: end, Precision: CalendarPrecisionMonth, Timex: fmt.Sprintf("(%s,%s)", start.Format("2006-01"), end.Format("2006-01"))}
	}
	if loc := betweenISODateRE.FindStringSubmatchIndex(lower); loc != nil {
		start, err1 := time.Parse("2006-01-02", lower[loc[2]:loc[3]])
		end, err2 := time.Parse("2006-01-02", lower[loc[4]:loc[5]])
		if err1 != nil || err2 != nil {
			return nil
		}
		end = end.AddDate(0, 0, 1)
		return &calendarRangeMatch{Text: calendarRangeText(text, loc), Index: loc[0], Start: start, End: end, Precision: CalendarPrecisionDay, Timex: fmt.Sprintf("(%s,%s)", start.Format("2006-01-02"), end.Format("2006-01-02"))}
	}
	return nil
}

func matchRelativeToAnchor(text, lower string) *calendarRangeMatch {
	loc := relativeToAnchorRE.FindStringSubmatchIndex(lower)
	if loc == nil {
		return nil
	}
	count := lower[loc[2]:loc[3]]
	unit := lower[loc[4]:loc[5]]
	dir := lower[loc[6]:loc[7]]
	tail := strings.TrimSpace(lower[loc[8]:loc[9]])
	anchor := ParseCalendar(tail)
	if anchor == nil {
		return nil
	}
	n, ok := parseRelativeCount(count)
	if !ok {
		return nil
	}
	normalizedUnit, ok := normalizeRelativeUnit(unit)
	if !ok {
		return nil
	}
	direction := -1
	if dir == "after" {
		direction = 1
	}
	res, ok := countedResolution(startOfDay(anchor.Time), n, normalizedUnit, direction)
	if !ok {
		return nil
	}
	return &calendarRangeMatch{Text: calendarRangeText(text, loc), Index: loc[0], Start: res.Start, End: res.End, Precision: res.Precision, Timex: timexForPrecision(res.Start, res.Precision)}
}

func matchOpenRange(text, lower string, anchor time.Time) *calendarRangeMatch {
	loc := openRangeRE.FindStringSubmatchIndex(lower)
	if loc == nil {
		return nil
	}
	op := lower[loc[2]:loc[3]]
	tail := strings.TrimSpace(lower[loc[4]:loc[5]])
	expr, err := Extract(tail, anchor)
	if err != nil || expr == nil || !expr.HasRange {
		return nil
	}
	anchorDay := startOfDay(anchor.UTC())
	start := expr.Start.UTC()
	var end time.Time
	if op == "since" {
		end = anchorDay.AddDate(0, 0, 1)
	} else if op == "throughout" {
		end = expr.End.UTC()
	} else {
		start = anchorDay
		end = expr.Start.UTC()
		if !end.After(start) {
			end = expr.End.UTC()
		}
	}
	return &calendarRangeMatch{Text: calendarRangeText(text, loc), Index: loc[0], Start: start, End: end, Precision: expr.Precision, Timex: fmt.Sprintf("(%s,%s)", start.Format("2006-01-02"), end.Format("2006-01-02"))}
}

func calendarRangeText(text string, loc []int) string {
	return strings.TrimRight(strings.TrimSpace(text[loc[0]:loc[1]]), `"'.,;:!?()[]{} `)
}

func firstIntGroup(text string, loc []int, groups ...int) (int, bool) {
	for _, group := range groups {
		i := group * 2
		if i+1 < len(loc) && loc[i] >= 0 && loc[i+1] >= 0 {
			n, err := strconv.Atoi(text[loc[i]:loc[i+1]])
			return n, err == nil
		}
	}
	return 0, false
}

func partOfRange(start, end time.Time, part string) (time.Time, time.Time) {
	days := int(end.Sub(start).Hours() / 24)
	firstEnd := start.AddDate(0, 0, days/3)
	midEnd := start.AddDate(0, 0, (2*days)/3)
	switch part {
	case "early":
		return start, firstEnd
	case "mid":
		return firstEnd, midEnd
	default:
		return midEnd, end
	}
}

func seasonRange(season string, year int) (time.Time, time.Time, bool) {
	switch season {
	case "spring":
		return time.Date(year, time.March, 1, 0, 0, 0, 0, time.UTC), time.Date(year, time.June, 1, 0, 0, 0, 0, time.UTC), true
	case "summer":
		return time.Date(year, time.June, 1, 0, 0, 0, 0, time.UTC), time.Date(year, time.September, 1, 0, 0, 0, 0, time.UTC), true
	case "fall", "autumn":
		return time.Date(year, time.September, 1, 0, 0, 0, 0, time.UTC), time.Date(year, time.December, 1, 0, 0, 0, 0, time.UTC), true
	case "winter":
		return time.Date(year, time.December, 1, 0, 0, 0, 0, time.UTC), time.Date(year+1, time.March, 1, 0, 0, 0, 0, time.UTC), true
	default:
		return time.Time{}, time.Time{}, false
	}
}
