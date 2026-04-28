package llmnode

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"
)

// ConfigFromMap parses a Config from a generic map via JSON round-trip.
//
// String values that originate from board template references (e.g.
// "${board.temperature}") are not valid JSON for non-string fields. The
// isDeferred predicate (typically variable.ContainsRef) is consulted to
// decide what to do with such values:
//   - When isDeferred is nil, an unparseable string returns an error.
//   - When isDeferred reports true, the entry is dropped so the field
//     receives its zero value at build time and gets re-parsed at execute
//     time once the resolver substitutes the real value.
//
// The input map is never mutated; coercion happens on a shallow clone.
func ConfigFromMap(m map[string]any, isDeferred func(string) bool) (Config, error) {
	var cfg Config
	if m == nil {
		return cfg, nil
	}
	m = coerceMapForConfig(m, isDeferred)
	data, err := json.Marshal(m)
	if err != nil {
		return cfg, fmt.Errorf("llmnode: marshal config map: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("llmnode: unmarshal config: %w", err)
	}
	return cfg, nil
}

// coerceMapForConfig walks Config's json tags and converts string entries
// to the numeric / bool types the struct expects. This lets JSON
// round-trip succeed when map values arrive as strings (typically from
// template resolution: "0.7" instead of 0.7).
func coerceMapForConfig(m map[string]any, isDeferred func(string) bool) map[string]any {
	if m == nil {
		return nil
	}
	t := reflect.TypeOf(Config{})
	result := maps.Clone(m)
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		key, _, _ := strings.Cut(tag, ",")
		if key == "" {
			continue
		}
		val, ok := result[key]
		if !ok {
			continue
		}
		str, ok := val.(string)
		if !ok {
			continue
		}

		target := field.Type
		if target.Kind() == reflect.Ptr {
			target = target.Elem()
		}
		if target.Kind() == reflect.String {
			continue
		}
		if coerced, ok := coerceString(str, target.Kind()); ok {
			result[key] = coerced
		} else if isDeferred != nil && isDeferred(str) {
			delete(result, key)
		}
	}
	return result
}

func coerceString(s string, kind reflect.Kind) (any, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	switch kind {
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, false
		}
		return f, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return i, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return u, true
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, false
		}
		return b, true
	default:
		return nil, false
	}
}
