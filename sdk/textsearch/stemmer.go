package textsearch

import "strings"

// Stem applies a simplified Porter stemmer to the given word.
func Stem(word string) string {
	if len(word) < 3 {
		return word
	}

	w := word

	// Step 1a
	if s, ok := hasSuffix(w, "sses"); ok {
		w = s + "ss"
	} else if s, ok := hasSuffix(w, "ies"); ok {
		w = s + "i"
	} else if !strings.HasSuffix(w, "ss") {
		if s, ok := hasSuffix(w, "s"); ok {
			w = s
		}
	}

	// Step 1b
	step1bExtra := false
	if s, ok := hasSuffix(w, "eed"); ok {
		if measure(s) > 0 {
			w = s + "ee"
		}
	} else if s, ok := hasSuffix(w, "ed"); ok {
		if hasVowel(s) {
			w = s
			step1bExtra = true
		}
	} else if s, ok := hasSuffix(w, "ing"); ok {
		if hasVowel(s) {
			w = s
			step1bExtra = true
		}
	}

	if step1bExtra {
		if strings.HasSuffix(w, "at") || strings.HasSuffix(w, "bl") || strings.HasSuffix(w, "iz") {
			w += "e"
		} else if endsDoubleConsonant(w) && !strings.HasSuffix(w, "l") && !strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "z") {
			w = w[:len(w)-1]
		} else if measure(w) == 1 && endsCVC(w) {
			w += "e"
		}
	}

	// Step 1c
	if s, ok := hasSuffix(w, "y"); ok {
		if hasVowel(s) {
			w = s + "i"
		}
	}

	// Step 2
	step2Map := []struct {
		suffix, replacement string
	}{
		{"ational", "ate"},
		{"tional", "tion"},
		{"enci", "ence"},
		{"anci", "ance"},
		{"izer", "ize"},
		{"abli", "able"},
		{"alli", "al"},
		{"entli", "ent"},
		{"eli", "e"},
		{"ousli", "ous"},
		{"ization", "ize"},
		{"ation", "ate"},
		{"ator", "ate"},
		{"alism", "al"},
		{"iveness", "ive"},
		{"fulness", "ful"},
		{"ousness", "ous"},
		{"aliti", "al"},
		{"iviti", "ive"},
		{"biliti", "ble"},
	}
	for _, rule := range step2Map {
		if s, ok := hasSuffix(w, rule.suffix); ok {
			if measure(s) > 0 {
				w = s + rule.replacement
			}
			break
		}
	}

	// Step 3
	step3Map := []struct {
		suffix, replacement string
	}{
		{"icate", "ic"},
		{"ative", ""},
		{"alize", "al"},
		{"iciti", "ic"},
		{"ical", "ic"},
		{"ful", ""},
		{"ness", ""},
	}
	for _, rule := range step3Map {
		if s, ok := hasSuffix(w, rule.suffix); ok {
			if measure(s) > 0 {
				w = s + rule.replacement
			}
			break
		}
	}

	// Step 4
	step4Suffixes := []string{
		"al", "ance", "ence", "er", "ic", "able", "ible", "ant",
		"ement", "ment", "ent", "ion", "ou", "ism", "ate", "iti",
		"ous", "ive", "ize",
	}
	for _, suffix := range step4Suffixes {
		if s, ok := hasSuffix(w, suffix); ok {
			if suffix == "ion" {
				if measure(s) > 1 && len(s) > 0 && (s[len(s)-1] == 's' || s[len(s)-1] == 't') {
					w = s
				}
			} else if measure(s) > 1 {
				w = s
			}
			break
		}
	}

	// Step 5a
	if s, ok := hasSuffix(w, "e"); ok {
		if measure(s) > 1 {
			w = s
		} else if measure(s) == 1 && !endsCVC(s) {
			w = s
		}
	}

	// Step 5b
	if measure(w) > 1 && endsDoubleConsonant(w) && w[len(w)-1] == 'l' {
		w = w[:len(w)-1]
	}

	return w
}

func isConsonant(word string, i int) bool {
	if i < 0 || i >= len(word) {
		return false
	}
	switch word[i] {
	case 'a', 'e', 'i', 'o', 'u':
		return false
	case 'y':
		if i == 0 {
			return true
		}
		return !isConsonant(word, i-1)
	}
	return true
}

func measure(word string) int {
	n := len(word)
	if n == 0 {
		return 0
	}
	i := 0
	for i < n && isConsonant(word, i) {
		i++
	}

	m := 0
	for i < n {
		for i < n && !isConsonant(word, i) {
			i++
		}
		if i >= n {
			break
		}
		m++
		for i < n && isConsonant(word, i) {
			i++
		}
	}
	return m
}

func hasVowel(word string) bool {
	for i := range len(word) {
		if !isConsonant(word, i) {
			return true
		}
	}
	return false
}

func endsDoubleConsonant(word string) bool {
	n := len(word)
	if n < 2 {
		return false
	}
	return word[n-1] == word[n-2] && isConsonant(word, n-1)
}

func endsCVC(word string) bool {
	n := len(word)
	if n < 3 {
		return false
	}
	if !isConsonant(word, n-1) || isConsonant(word, n-2) || !isConsonant(word, n-3) {
		return false
	}
	c := word[n-1]
	return c != 'w' && c != 'x' && c != 'y'
}

func hasSuffix(word, suffix string) (string, bool) {
	if strings.HasSuffix(word, suffix) {
		return word[:len(word)-len(suffix)], true
	}
	return "", false
}
