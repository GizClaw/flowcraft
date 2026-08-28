package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Board reference syntax
//
// Strings may embed ${board.<path>} references, where <path> is a
// dot-separated path into the board vars (${board.user.name} reads var
// "user" and then its "name" field). Nested lookup walks maps with
// string keys (map[string]string, map[string]int, ...) and exported
// struct fields, dereferencing pointers along the way. An exact
// variable name is tried first, so a var literally named "user.name"
// wins over nested lookup.
// An optional default follows a colon: ${board.limit:3}. A reference
// standing alone in a string keeps the variable's typed value (a list
// stays a list); when embedded in a longer string the value is
// interpolated as text. Referencing a missing variable is a validation
// error unless a default is given. When a reference stands alone, a
// default that is a valid JSON literal keeps its type
// (${board.limit:3} yields a number); otherwise it stays text. Defaults
// are raw text and may not nest further references. Prefix a reference
// with a backslash (\${board.x}) to emit it literally; within a
// default, \} and \\ produce "}" and "\".

// BoardRefMarker is the literal prefix of every board reference.
const BoardRefMarker = "${board."

// BoardRefPrefix is the scheme-style name of the board reference
// namespace ("${board.<path>}"), shared by every layer that resolves
// references against the agent board at execution time (graph node
// configs, the script bridge's board.resolve, ...).
const BoardRefPrefix = "board"

var boardIdentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// boardRefPattern matches reference syntax for static analysis
// (ContainsBoardRef / ExtractBoardRefs). Runtime expansion uses the
// scanner in resolveString instead, which also handles escapes.
var boardRefPattern = regexp.MustCompile(`\$\{board\.([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)(?::((?:[^}\\]|\\.)*))?\}`)

// ContainsBoardRef reports whether v — a decoded JSON value or a plain
// string — contains at least one live (unescaped) "${board.*}"
// reference.
func ContainsBoardRef(v any) bool {
	return len(findBoardRefs(v)) > 0
}

// ExtractBoardRefs returns the sorted, deduplicated board paths
// referenced anywhere inside v. Escaped references and defaults are
// not included.
func ExtractBoardRefs(v any) []string {
	seen := map[string]bool{}
	for _, r := range findBoardRefs(v) {
		seen[r] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func findBoardRefs(v any) []string {
	switch t := v.(type) {
	case string:
		return findStringRefs(t)
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, findBoardRefs(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range t {
			out = append(out, findBoardRefs(e)...)
		}
		return out
	default:
		return nil
	}
}

// findStringRefs returns the paths of all live (unescaped) references
// in s. The regex does not know about escapes, so matches preceded by
// an odd run of backslashes are skipped.
func findStringRefs(s string) []string {
	idxs := boardRefPattern.FindAllStringSubmatchIndex(s, -1)
	var out []string
	for _, m := range idxs {
		if backslashed(s, m[0]) {
			continue
		}
		out = append(out, s[m[2]:m[3]])
	}
	return out
}

// ResolveString expands board references in s. When s is exactly one
// reference, the variable's typed value (or typed default) is
// returned; otherwise the interpolated string is returned. Missing
// references fail with a validation error unless a default is given.
func (b *Board) ResolveString(s string) (any, error) {
	return b.resolveString(s)
}

func (b *Board) resolveString(s string) (any, error) {
	if !strings.Contains(s, BoardRefMarker) {
		return s, nil
	}
	var sb strings.Builder
	pos := 0
	for i := 0; i < len(s); {
		idx := strings.Index(s[i:], BoardRefMarker)
		if idx < 0 {
			break
		}
		idx += i
		end := refEnd(s, idx+len(BoardRefMarker))
		if backslashed(s, idx) {
			// Escaped reference: emit the literal text, dropping the
			// escaping backslash. Unterminated markers are fine here.
			if end < 0 {
				end = len(s)
			}
			sb.WriteString(s[pos : idx-1])
			sb.WriteString(s[idx:end])
			pos = end
			i = end
			continue
		}
		if end < 0 {
			return nil, errdefs.Validationf(
				"malformed board reference in %q (escape literal text with \\%s)", s, BoardRefMarker)
		}
		// A single reference spanning the whole string keeps the
		// variable's typed value.
		if idx == 0 && end == len(s) {
			body := s[len(BoardRefMarker) : end-1]
			if strings.Contains(body, BoardRefMarker) {
				return nil, errdefs.Validationf(
					"nested board references are not supported: %q", s)
			}
			path, def, hasDef, ok := parseBoardRef(body)
			if !ok {
				return nil, errdefs.Validationf(
					"malformed board reference in %q (escape literal text with \\%s)", s, BoardRefMarker)
			}
			if v, found := b.lookupBoardRef(path); found {
				return v, nil
			}
			if hasDef {
				return typedBoardDefault(def), nil
			}
			return nil, boardRefMissingError(path)
		}
		body := s[idx+len(BoardRefMarker) : end-1]
		if strings.Contains(body, BoardRefMarker) {
			return nil, errdefs.Validationf(
				"nested board references are not supported: %q", s)
		}
		path, def, hasDef, ok := parseBoardRef(body)
		if !ok {
			return nil, errdefs.Validationf(
				"malformed board reference in %q (escape literal text with \\%s)", s, BoardRefMarker)
		}
		if v, found := b.lookupBoardRef(path); found {
			sb.WriteString(s[pos:idx])
			sb.WriteString(stringifyBoardValue(v))
		} else if hasDef {
			sb.WriteString(s[pos:idx])
			sb.WriteString(def)
		} else {
			return nil, boardRefMissingError(path)
		}
		pos = end
		i = end
	}
	sb.WriteString(s[pos:])
	return sb.String(), nil
}

// Resolve expands board references throughout v, a decoded JSON value
// (strings, []any, map[string]any, or leaf scalars). References inside
// object keys are rejected: config keys are part of the node type's
// contract and must stay constant.
func (b *Board) Resolve(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return b.resolveString(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := b.Resolve(e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			if boardRefPattern.MatchString(k) {
				return nil, errdefs.Validationf(
					"board references are not supported in object keys: %q", k)
			}
			r, err := b.Resolve(e)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// lookupBoardRef resolves a reference path. The exact variable name is
// tried first; otherwise the path is walked segment by segment through
// nested maps (any map type with string keys) and exported struct
// fields.
func (b *Board) lookupBoardRef(path string) (any, bool) {
	if v, ok := b.GetVar(path); ok {
		return v, true
	}
	segs := strings.Split(path, ".")
	if len(segs) == 1 {
		return nil, false
	}
	v, ok := b.GetVar(segs[0])
	if !ok {
		return nil, false
	}
	for _, seg := range segs[1:] {
		v, ok = lookupBoardSegment(v, seg)
		if !ok {
			return nil, false
		}
	}
	return v, true
}

// lookupBoardSegment resolves one path segment against a value.
// Reflection keeps the lookup open to any map with a string key type
// (map[string]string, map[string]int, named map types) instead of
// special-casing every combination; pointers are dereferenced and
// exported struct fields are readable by Go field name.
func lookupBoardSegment(v any, seg string) (any, bool) {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil, false
	}
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		ev := rv.MapIndex(reflect.ValueOf(seg).Convert(rv.Type().Key()))
		if !ev.IsValid() {
			return nil, false
		}
		return ev.Interface(), true
	case reflect.Struct:
		f := rv.FieldByName(seg)
		if !f.IsValid() || !f.CanInterface() {
			return nil, false
		}
		return f.Interface(), true
	default:
		return nil, false
	}
}

func parseBoardRef(body string) (path, def string, hasDef bool, ok bool) {
	path, def, hasDef = strings.Cut(body, ":")
	if !validBoardPath(path) {
		return "", "", false, false
	}
	if hasDef {
		def = unescapeBoardDefault(def)
	}
	return path, def, hasDef, true
}

func validBoardPath(p string) bool {
	if p == "" {
		return false
	}
	for seg := range strings.SplitSeq(p, ".") {
		if !boardIdentPattern.MatchString(seg) {
			return false
		}
	}
	return true
}

// refEnd returns the index just past the closing brace of the
// reference starting at from, or -1 when unterminated. A \} inside the
// body is part of a default, not the terminator.
func refEnd(s string, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == '}' && !backslashed(s, i) {
			return i + 1
		}
	}
	return -1
}

// backslashed reports whether s[i] is preceded by an odd number of
// backslashes, i.e. escaped.
func backslashed(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// unescapeBoardDefault unescapes \} and \\ inside a default value.
func unescapeBoardDefault(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '}' || s[i+1] == '\\') {
			sb.WriteByte(s[i+1])
			i++
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// typedBoardDefault turns a default into a typed value when it is a
// valid JSON literal; otherwise it stays a string. This lets
// ${board.limit:3} fill an integer config field while
// ${board.who:anon} stays text.
func typedBoardDefault(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return raw
}

// stringifyBoardValue renders an interpolated value as text: strings
// verbatim, JSON-like values as compact JSON, everything else via %v.
func stringifyBoardValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

func boardRefMissingError(path string) error {
	return errdefs.Validationf(
		"board reference ${board.%s} does not resolve: %q is not set", path, path)
}
