package plugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"golang.org/x/mod/semver"
)

// versionPattern requires the full major.minor.patch form (with
// optional prerelease/build suffixes) so constraint parsing never has
// to guess at missing components.
var versionPattern = regexp.MustCompile(
	`^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// normalizeVersion canonicalizes a bare or v-prefixed full semver
// ("1.2.0" or "v1.2.0") into canonical "v1.2.0" form.
func normalizeVersion(version string) (string, error) {
	text := strings.TrimSpace(version)
	if !versionPattern.MatchString(text) {
		return "", errdefs.Validationf(
			"invalid semantic version %q (want major.minor.patch)", version)
	}
	canonical := semver.Canonical("v" + strings.TrimPrefix(text, "v"))
	if !semver.IsValid(canonical) {
		return "", errdefs.Validationf("invalid semantic version %q", version)
	}
	return canonical, nil
}

// constraint is a parsed semver range: comma-separated AND conditions
// supporting exact versions, caret (^) and comparison operators
// (>=, >, <=, <). Caret follows npm semantics.
type constraint struct {
	conditions []condition
}

type conditionKind uint8

const (
	condExact conditionKind = iota
	condCaret
	condGE
	condGT
	condLE
	condLT
)

type condition struct {
	kind    conditionKind
	version string // canonical "v1.2.3"
}

// parseConstraint parses a comma-separated semver range.
func parseConstraint(raw string) (constraint, error) {
	var c constraint
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return c, errdefs.Validationf("empty constraint in %q", raw)
		}
		cond, err := parseCondition(part)
		if err != nil {
			return c, err
		}
		c.conditions = append(c.conditions, cond)
	}
	return c, nil
}

func parseCondition(part string) (condition, error) {
	switch {
	case strings.HasPrefix(part, ">="):
		return comparisonCondition(condGE, strings.TrimSpace(strings.TrimPrefix(part, ">=")))
	case strings.HasPrefix(part, "<="):
		return comparisonCondition(condLE, strings.TrimSpace(strings.TrimPrefix(part, "<=")))
	case strings.HasPrefix(part, ">"):
		return comparisonCondition(condGT, strings.TrimSpace(strings.TrimPrefix(part, ">")))
	case strings.HasPrefix(part, "<"):
		return comparisonCondition(condLT, strings.TrimSpace(strings.TrimPrefix(part, "<")))
	case strings.HasPrefix(part, "^"):
		return comparisonCondition(condCaret, strings.TrimSpace(strings.TrimPrefix(part, "^")))
	default:
		return comparisonCondition(condExact, part)
	}
}

func comparisonCondition(kind conditionKind, version string) (condition, error) {
	normalized, err := normalizeVersion(version)
	if err != nil {
		return condition{}, err
	}
	return condition{kind: kind, version: normalized}, nil
}

// Match reports whether version (bare or v-prefixed) satisfies every
// condition.
func (c constraint) Match(version string) bool {
	normalized, err := normalizeVersion(version)
	if err != nil {
		return false
	}
	for _, cond := range c.conditions {
		if !cond.match(normalized) {
			return false
		}
	}
	return true
}

func (c condition) match(version string) bool {
	cmp := semver.Compare(version, c.version)
	switch c.kind {
	case condExact:
		return cmp == 0
	case condCaret:
		return cmp >= 0 && semver.Compare(version, caretUpper(c.version)) < 0
	case condGE:
		return cmp >= 0
	case condGT:
		return cmp > 0
	case condLE:
		return cmp <= 0
	case condLT:
		return cmp < 0
	default:
		return false
	}
}

// caretUpper returns the exclusive upper bound of a caret range:
// ^1.2.3 → <2.0.0, ^0.2.3 → <0.3.0, ^0.0.3 → <0.0.4.
func caretUpper(version string) string {
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	major, minor, patch := parts[0], parts[1], parts[2]
	switch {
	case major != "0":
		return fmt.Sprintf("v%d.0.0", mustAtoi(major)+1)
	case minor != "0":
		return fmt.Sprintf("v0.%d.0", mustAtoi(minor)+1)
	default:
		return fmt.Sprintf("v0.0.%d", mustAtoi(patch)+1)
	}
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// namedConstraint pairs a plugin name with an optional version
// constraint. Entries use "name" or "name@constraint" form.
type namedConstraint struct {
	name       string
	constraint constraint
}

// matches reports whether the given plugin version satisfies the
// constraint. An unconstrained entry matches any version.
func (n namedConstraint) matches(version string) bool {
	if n.constraint.conditions == nil {
		return true
	}
	return n.constraint.Match(version)
}

// parseNamedConstraint parses "name" or "name@constraint".
func parseNamedConstraint(raw string) (namedConstraint, error) {
	name, constraintRaw, _ := strings.Cut(strings.TrimSpace(raw), "@")
	if err := validateName(name); err != nil {
		return namedConstraint{}, err
	}
	n := namedConstraint{name: name}
	if constraintRaw == "" {
		return n, nil
	}
	c, err := parseConstraint(constraintRaw)
	if err != nil {
		return namedConstraint{}, err
	}
	n.constraint = c
	return n, nil
}
