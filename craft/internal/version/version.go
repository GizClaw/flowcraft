// Package version compares the dotted numeric versions craft uses for
// definitions, manifests and host compatibility floors.
package version

import (
	"strconv"
	"strings"
)

// Valid reports whether s is a dotted numeric version with an optional
// prerelease suffix.
func Valid(s string) bool {
	text := strings.TrimSpace(s)
	if text == "" {
		return false
	}
	main, _, _ := strings.Cut(text, "-")
	for _, part := range strings.Split(main, ".") {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

// Compare orders two dotted numeric versions with an optional
// prerelease; a release sorts above its prereleases. Invalid inputs
// fall back to a plain string comparison.
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
		return strings.Compare(av.pre, bv.pre)
	}
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
	main, pre, _ := strings.Cut(text, "-")
	parts := strings.Split(main, ".")
	out := parsed{core: make([]int, 0, len(parts)), pre: pre}
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return parsed{}, false
		}
		out.core = append(out.core, n)
	}
	return out, true
}
