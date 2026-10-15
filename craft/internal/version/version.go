// Package version compares the dotted numeric versions craft uses for
// definitions, manifests and host compatibility floors.
package version

import (
	"regexp"
	"strconv"
	"strings"
)

// prereleasePattern is the spelling a prerelease suffix may carry:
// dot-separated identifiers of letters, digits and dashes, none of them
// empty. "1.0.0-" is not a version with an empty suffix, it is a version
// that was cut short; a plugin manifest's own pattern refused it while
// this validator accepted it, so the two disagreed about which versions
// exist.
var prereleasePattern = regexp.MustCompile(`^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$`)

// Valid reports whether s is a dotted numeric version with an optional
// prerelease suffix: one or more numeric parts, and a suffix that is
// non-empty and spelled in the characters a prerelease may carry. It is
// the one grammar a version field is held to — definition floors, plugin
// manifests and the update rules all ask it.
func Valid(s string) bool {
	_, ok := parse(s)
	return ok
}

// Compare orders two dotted numeric versions with an optional
// prerelease; a release sorts above its prereleases, and prereleases
// compare identifier by identifier (numeric ones numerically, others
// lexically). Invalid inputs fall back to a plain string comparison.
func Compare(a, b string) int {
	av, aok := parse(a)
	bv, bok := parse(b)
	if !aok || !bok {
		return strings.Compare(a, b)
	}
	for i := 0; i < len(av.core) || i < len(bv.core); i++ {
		var ai, bi int
		if i < len(av.core) {
			ai = av.core[i]
		}
		if i < len(bv.core) {
			bi = bv.core[i]
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	switch {
	case av.pre == bv.pre:
		return 0
	case av.pre == "":
		return 1
	case bv.pre == "":
		return -1
	default:
		return comparePrerelease(av.pre, bv.pre)
	}
}

// comparePrerelease orders two prerelease suffixes by identifier, the
// way every version writer means them: identifiers are split on ".",
// numeric ones compare numerically and rank below alphanumeric ones,
// other identifiers compare lexically, and a list sorts below a longer
// list that starts with it. Comparing the raw strings instead puts
// alpha.10 below alpha.2 — the direction that decides whether an update
// is newer and whether a host satisfies a floor.
func comparePrerelease(a, b string) int {
	left := strings.Split(a, ".")
	right := strings.Split(b, ".")
	for i := 0; i < len(left) && i < len(right); i++ {
		if cmp := compareIdentifier(left[i], right[i]); cmp != 0 {
			return cmp
		}
	}
	switch {
	case len(left) == len(right):
		return 0
	case len(left) < len(right):
		return -1
	default:
		return 1
	}
}

// compareIdentifier orders one pair of prerelease identifiers.
func compareIdentifier(a, b string) int {
	left, leftNumeric := numericIdentifier(a)
	right, rightNumeric := numericIdentifier(b)
	switch {
	case leftNumeric && rightNumeric:
		switch {
		case left == right:
			return 0
		case left < right:
			return -1
		default:
			return 1
		}
	case leftNumeric:
		// Numeric identifiers have lower precedence than alphanumeric
		// ones.
		return -1
	case rightNumeric:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// numericIdentifier reports whether an identifier is a non-negative
// decimal number, and its value.
func numericIdentifier(id string) (int, bool) {
	if id == "" {
		return 0, false
	}
	value, err := strconv.Atoi(id)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

type parsed struct {
	core []int
	pre  string
}

func parse(s string) (parsed, bool) {
	text := strings.TrimSpace(s)
	if text == "" {
		return parsed{}, false
	}
	main, pre, hasPre := strings.Cut(text, "-")
	if hasPre && !prereleasePattern.MatchString(pre) {
		return parsed{}, false
	}
	parts := strings.Split(main, ".")
	out := parsed{core: make([]int, 0, len(parts)), pre: pre}
	for _, part := range parts {
		n, ok := numericPart(part)
		if !ok {
			return parsed{}, false
		}
		out.core = append(out.core, n)
	}
	return out, true
}

// numericPart reads one dot-separated core part: digits and nothing else,
// so a sign ("+1.0.0"), a letter ("1.0.x") or an empty part ("1..0") is
// not a version.
func numericPart(part string) (int, bool) {
	if part == "" {
		return 0, false
	}
	for _, r := range part {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(part)
	if err != nil {
		return 0, false
	}
	return n, true
}
