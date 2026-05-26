package timex

import (
	"strconv"
	"strings"
)

func parseRelativeCount(raw string) (int, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if n, err := strconv.Atoi(raw); err == nil {
		return n, true
	}
	if n, ok := parseChineseNumber(raw); ok {
		return n, true
	}
	switch raw {
	case "one", "a", "an", "un", "una", "uno", "une", "一":
		return 1, true
	case "two", "dos", "deux", "二", "两":
		return 2, true
	case "three", "tres", "trois", "三":
		return 3, true
	case "four", "cuatro", "quatre", "四":
		return 4, true
	case "five", "cinco", "cinq", "五":
		return 5, true
	case "six", "seis", "六":
		return 6, true
	case "seven", "siete", "sept", "七":
		return 7, true
	case "eight", "ocho", "huit", "八":
		return 8, true
	case "nine", "nueve", "neuf", "九":
		return 9, true
	case "ten", "diez", "dix", "十":
		return 10, true
	default:
		return 0, false
	}
}

func parseChineseNumber(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	total, section, digit, seen := 0, 0, 0, false
	for _, r := range raw {
		switch r {
		case '零', '〇':
			seen = true
			digit = 0
		case '一':
			seen = true
			digit = 1
		case '二', '两':
			seen = true
			digit = 2
		case '三':
			seen = true
			digit = 3
		case '四':
			seen = true
			digit = 4
		case '五':
			seen = true
			digit = 5
		case '六':
			seen = true
			digit = 6
		case '七':
			seen = true
			digit = 7
		case '八':
			seen = true
			digit = 8
		case '九':
			seen = true
			digit = 9
		case '十':
			seen = true
			if digit == 0 {
				digit = 1
			}
			section += digit * 10
			digit = 0
		case '百':
			seen = true
			if digit == 0 {
				digit = 1
			}
			section += digit * 100
			digit = 0
		case '千':
			seen = true
			if digit == 0 {
				digit = 1
			}
			section += digit * 1000
			digit = 0
		case '万':
			seen = true
			total += (section + digit) * 10000
			section, digit = 0, 0
		default:
			return 0, false
		}
	}
	if !seen {
		return 0, false
	}
	return total + section + digit, true
}

func normalizeRelativeUnit(raw string) (string, bool) {
	unit := strings.TrimSpace(strings.ToLower(raw))
	unit = strings.ReplaceAll(unit, "-", "")
	unit = strings.Join(strings.Fields(unit), " ")
	switch unit {
	case "days":
		unit = "day"
	case "weeks":
		unit = "week"
	case "weekends":
		unit = "weekend"
	case "months":
		unit = "month"
	case "years":
		unit = "year"
	case "días", "dias":
		unit = "dia"
	case "semanas":
		unit = "semana"
	case "meses":
		unit = "mes"
	case "años", "anos":
		unit = "año"
	case "jours":
		unit = "jour"
	case "semaines":
		unit = "semaine"
	case "années", "annees", "ans":
		unit = "année"
	}
	switch unit {
	case "day", "día", "dia", "jour", "天", "日":
		return "day", true
	case "week", "semana", "semaine", "周", "星期", "礼拜":
		return "week", true
	case "weekend", "fin de semana", "fines de semana", "周末":
		return "weekend", true
	case "month", "mes", "mese", "mois", "moi", "月":
		return "month", true
	case "year", "año", "ano", "an", "année", "年":
		return "year", true
	default:
		return "", false
	}
}
