package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// boardRefPattern matches "${board.<name>}" references inside node
// config strings. <name> is a board variable name.
//
// References are resolved per invocation, immediately before the typed
// config decode: a node can consume values written by an upstream node
// in the same run, and nothing is baked into shared registration
// state.
var boardRefPattern = regexp.MustCompile(`\$\{board\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// ContainsRef reports whether v — a decoded JSON value or a plain
// string — contains at least one "${board.*}" reference.
func ContainsRef(v any) bool {
	return len(findRefs(v)) > 0
}

// ExtractRefs returns the sorted, deduplicated board variable names
// referenced anywhere inside v. Useful for tooling and for
// documentation generation off a [GraphDefinition].
func ExtractRefs(v any) []string {
	seen := map[string]bool{}
	for _, r := range findRefs(v) {
		seen[r] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func findRefs(v any) []string {
	switch t := v.(type) {
	case string:
		var out []string
		for _, m := range boardRefPattern.FindAllStringSubmatch(t, -1) {
			out = append(out, m[1])
		}
		return out
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, findRefs(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range t {
			out = append(out, findRefs(e)...)
		}
		return out
	default:
		return nil
	}
}

// resolveRefs substitutes board references throughout v, a decoded
// JSON value.
//
// A string consisting of exactly one reference is replaced by the
// variable's typed value (a list stays a list, a map stays a map). A
// reference embedded in a longer string is interpolated with fmt's %v
// verb. References to missing variables are left literally in place so
// misconfigurations surface at decode time instead of silently
// zeroing.
func resolveRefs(v any, board *agent.Board) any {
	switch t := v.(type) {
	case string:
		return resolveStringRefs(t, board)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = resolveRefs(e, board)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = resolveRefs(e, board)
		}
		return out
	default:
		return v
	}
}

func resolveStringRefs(s string, board *agent.Board) any {
	if m := boardRefPattern.FindStringSubmatch(s); m != nil && m[0] == s {
		if val, ok := board.GetVar(m[1]); ok {
			return val
		}
		return s
	}
	return boardRefPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := boardRefPattern.FindStringSubmatch(match)[1]
		if val, ok := board.GetVar(name); ok {
			return fmt.Sprintf("%v", val)
		}
		return match
	})
}

// resolveConfig resolves board references inside a raw JSON node
// config, returning the rewritten raw JSON for the typed decode.
// Configs without the reference marker are returned unchanged.
func resolveConfig(raw json.RawMessage, board *agent.Board) (json.RawMessage, error) {
	if len(raw) == 0 || !bytes.Contains(raw, []byte("${board.")) {
		return raw, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errdefs.Validationf("node config is not valid JSON: %v", err)
	}
	out, err := json.Marshal(resolveRefs(v, board))
	if err != nil {
		return nil, errdefs.Internalf("failed to re-encode resolved node config: %v", err)
	}
	return out, nil
}
