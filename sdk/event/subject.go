package event

import (
	"errors"
	"fmt"
	"strings"
)

// Subject is a dot-delimited routing key, e.g.:
//
//	graph.run.r1.start
//	graph.run.r1.node.n1.complete
//	kanban.board.b1.update
//
// Segments are separated by '.'. A segment must not contain '.', '*' or '>'
// and must not be empty.
type Subject string

// Pattern is a Subject matcher using NATS-style wildcards:
//
//   - matches exactly one segment
//     >  matches one or more trailing segments (must be the last segment)
//
// Examples:
//
//	graph.run.r1.>             every event for run r1
//	graph.run.*.node.*.start   every node start across runs
//	kanban.>                   every kanban event
//
// Pattern matching is case-sensitive.
type Pattern string

const (
	subjectSep      = "."
	wildcardOne     = "*"
	wildcardTrail   = ">"
	subjectMaxBytes = 1024
)

// ErrInvalidSubject indicates a malformed Subject literal.
var ErrInvalidSubject = errors.New("event: invalid subject")

// ErrInvalidPattern indicates a malformed Pattern literal.
var ErrInvalidPattern = errors.New("event: invalid pattern")

// Validate reports whether s is a well-formed subject literal.
//
// A subject must:
//   - be non-empty;
//   - not exceed subjectMaxBytes;
//   - have no leading, trailing or consecutive '.' separators;
//   - have no segment containing '*' or '>' (those are pattern-only).
func (s Subject) Validate() error {
	if s == "" {
		return fmt.Errorf("%w: empty", ErrInvalidSubject)
	}
	if len(s) > subjectMaxBytes {
		return fmt.Errorf("%w: %d bytes exceeds max %d", ErrInvalidSubject, len(s), subjectMaxBytes)
	}
	str := string(s)
	if strings.HasPrefix(str, subjectSep) || strings.HasSuffix(str, subjectSep) {
		return fmt.Errorf("%w: leading or trailing dot in %q", ErrInvalidSubject, str)
	}
	for i, seg := range strings.Split(str, subjectSep) {
		if seg == "" {
			return fmt.Errorf("%w: empty segment at index %d in %q", ErrInvalidSubject, i, str)
		}
		if strings.ContainsAny(seg, "*>") {
			return fmt.Errorf("%w: wildcard %q present at segment %d (use Pattern, not Subject)", ErrInvalidSubject, seg, i)
		}
	}
	return nil
}

// Validate reports whether p is a well-formed pattern literal.
//
// A pattern must:
//   - be non-empty;
//   - not exceed subjectMaxBytes;
//   - have no leading, trailing or consecutive '.' separators;
//   - have each '*' / '>' segment occupy a whole segment;
//   - have at most one '>' segment, and only as the last segment.
func (p Pattern) Validate() error {
	if p == "" {
		return fmt.Errorf("%w: empty", ErrInvalidPattern)
	}
	if len(p) > subjectMaxBytes {
		return fmt.Errorf("%w: %d bytes exceeds max %d", ErrInvalidPattern, len(p), subjectMaxBytes)
	}
	str := string(p)
	if strings.HasPrefix(str, subjectSep) || strings.HasSuffix(str, subjectSep) {
		return fmt.Errorf("%w: leading or trailing dot in %q", ErrInvalidPattern, str)
	}
	segs := strings.Split(str, subjectSep)
	for i, seg := range segs {
		if seg == "" {
			return fmt.Errorf("%w: empty segment at index %d in %q", ErrInvalidPattern, i, str)
		}
		if seg == wildcardTrail {
			if i != len(segs)-1 {
				return fmt.Errorf("%w: '>' must be the last segment in %q", ErrInvalidPattern, str)
			}
			continue
		}
		if seg == wildcardOne {
			continue
		}
		if strings.ContainsAny(seg, "*>") {
			return fmt.Errorf("%w: wildcard must occupy a whole segment, got %q", ErrInvalidPattern, seg)
		}
	}
	return nil
}

// Matches reports whether s satisfies pattern p.
//
// Matching is segment-wise:
//   - literal segments must compare byte-for-byte equal;
//   - '*' matches any single segment;
//   - '>' matches one or more trailing segments (must be the last pattern
//     segment; Validate enforces this).
//
// An empty pattern matches nothing. An empty subject matches nothing.
// Matches does not validate p; callers that accept untrusted input should
// call p.Validate() first (Bus implementations are required to).
//
// Matches splits both p and s on every call. Hot paths inside the package
// (MemoryBus.Publish) use matchSegs directly with pre-split inputs to
// avoid the per-call allocations.
func (p Pattern) Matches(s Subject) bool {
	if p == "" || s == "" {
		return false
	}
	return matchSegs(splitSubject(string(p)), splitSubject(string(s)))
}

// splitSubject returns the segments of s. Centralised so MemoryBus and the
// public Matches share one allocation strategy.
func splitSubject(s string) []string {
	return strings.Split(s, subjectSep)
}

// matchSegs is the segment-level matcher shared by Pattern.Matches and the
// MemoryBus hot path. Both inputs are non-empty.
//
// Well-formed pattern segments satisfy Pattern.Validate: at most one '>'
// segment, only at the tail. To stay defined for malformed input that
// reaches Pattern.Matches directly (Bus implementations validate first),
// a '>' that is not the last pattern segment is treated as a literal
// segment — i.e. it only matches a subject segment that is also '>'.
// Subject.Validate rejects '>' inside a subject, so under normal Bus
// usage that branch is unreachable; the literal fallback exists only to
// keep the standalone Pattern.Matches helper total.
func matchSegs(pSegs, sSegs []string) bool {
	last := len(pSegs) - 1
	for i, pSeg := range pSegs {
		if pSeg == wildcardTrail && i == last {
			return len(sSegs) >= i+1
		}
		if i >= len(sSegs) {
			return false
		}
		if pSeg == wildcardOne {
			continue
		}
		// Non-tail '>' falls through to literal compare (defensive); same
		// goes for any other byte sequence — Validate is the front gate.
		if pSeg != sSegs[i] {
			return false
		}
	}
	return len(pSegs) == len(sSegs)
}
