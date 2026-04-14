package variable

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// refPattern matches ${scope.name} references.
var refPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// Resolver resolves ${scope.name} variable references in strings.
// It is not safe for concurrent use; use Clone to create independent
// copies for parallel branches.
type Resolver struct {
	scopes map[string]map[string]any
}

// NewResolver creates a new Resolver.
func NewResolver() *Resolver {
	return &Resolver{scopes: make(map[string]map[string]any)}
}

// AddScope registers a named scope with its values.
func (r *Resolver) AddScope(name string, values map[string]any) {
	r.scopes[name] = values
}

// Clone creates an independent deep copy of this Resolver.
// All nested maps and slices are recursively copied so that
// parallel branches cannot mutate each other's state.
func (r *Resolver) Clone() *Resolver {
	cp := &Resolver{scopes: make(map[string]map[string]any, len(r.scopes))}
	for k, v := range r.scopes {
		cp.scopes[k] = deepCopyMap(v)
	}
	return cp
}

func deepCopyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = deepCopyValue(v)
	}
	return cp
}

func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return deepCopyMap(val)
	case []any:
		cp := make([]any, len(val))
		for i, elem := range val {
			cp[i] = deepCopyValue(elem)
		}
		return cp
	default:
		return v
	}
}

// Resolve replaces all ${scope.name} references in template with actual values.
// References that cannot be resolved are left unchanged.
func (r *Resolver) Resolve(template string) string {
	result := refPattern.ReplaceAllStringFunc(template, func(match string) string {
		ref := strings.TrimSpace(match[2 : len(match)-1]) // strip ${ and }
		val := r.lookup(ref)
		if val == nil {
			return match
		}
		switch v := val.(type) {
		case string:
			return v
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Sprintf("%v", v)
			}
			return string(b)
		}
	})
	return result
}

// ResolveMap resolves all string values in a config map.
// Non-string values are passed through unchanged.
//
// When a string value is exactly a single ${scope.name} reference (no
// surrounding text), the resolved value preserves the original type from
// the scope (e.g. float64, int, bool). This allows board variables to
// inject typed config values such as temperature.
func (r *Resolver) ResolveMap(config map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(config))
	for k, v := range config {
		switch val := v.(type) {
		case string:
			if typed, ok := r.resolveTyped(val); ok {
				result[k] = typed
			} else {
				result[k] = r.Resolve(val)
			}
		default:
			result[k] = v
		}
	}
	return result, nil
}

// resolveTyped checks if the entire string is a single ${scope.name}
// reference and, if so, returns the raw (typed) value from the scope
// instead of a stringified version. Returns (nil, false) otherwise.
func (r *Resolver) resolveTyped(s string) (any, bool) {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "${") || !strings.HasSuffix(trimmed, "}") {
		return nil, false
	}
	matches := refPattern.FindAllStringIndex(trimmed, -1)
	if len(matches) != 1 || matches[0][0] != 0 || matches[0][1] != len(trimmed) {
		return nil, false
	}
	ref := strings.TrimSpace(trimmed[2 : len(trimmed)-1])
	val := r.lookup(ref)
	if val == nil {
		return nil, false
	}
	return val, true
}

// lookup resolves a dotted path like "input.topic" or "board.query".
func (r *Resolver) lookup(ref string) any {
	parts := strings.SplitN(ref, ".", 2)
	if len(parts) != 2 {
		return nil
	}
	scope, name := parts[0], parts[1]
	vals, ok := r.scopes[scope]
	if !ok {
		return nil
	}
	return resolveNested(vals, name)
}

// resolveNested navigates a dotted path through nested maps.
func resolveNested(m map[string]any, path string) any {
	parts := strings.Split(path, ".")
	var current any = m

	for _, part := range parts {
		switch c := current.(type) {
		case map[string]any:
			val, ok := c[part]
			if !ok {
				return nil
			}
			current = val
		default:
			return nil
		}
	}
	return current
}

// ExtractRefs returns all ${scope.name} references found in a string.
func ExtractRefs(text string) []string {
	matches := refPattern.FindAllStringSubmatch(text, -1)
	refs := make([]string, 0, len(matches))
	for _, match := range matches {
		refs = append(refs, strings.TrimSpace(match[1]))
	}
	return refs
}
