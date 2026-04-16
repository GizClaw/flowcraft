package llm

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"
)

// RoundConfig configures the LLM call parameters for one round.
// Board I/O (system prompt injection, output routing, etc.) is the caller's responsibility.
type RoundConfig struct {
	Model       string   `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens   int64    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	JSONMode    bool     `json:"json_mode,omitempty" yaml:"json_mode,omitempty"`
	Thinking    bool     `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	ToolNames   []string `json:"tool_names,omitempty" yaml:"tool_names,omitempty"`
}

// CoerceMapForStruct uses reflection on T's json tags to coerce string values
// in m to the numeric/bool types expected by the target struct fields. This
// allows JSON round-trip (Marshal → Unmarshal) to succeed when map values
// arrive as strings (e.g. from ${board.temperature} template resolution).
//
// isDeferred, when non-nil, reports whether a string value is a deferred
// reference (e.g. a template variable) that will be resolved later. Such
// values are removed from non-string fields so that json.Unmarshal sees the
// zero value instead of an invalid string. The caller should supply the
// resolver's own ContainsRef function to keep detection in sync.
//
// The input map is not modified; a shallow clone is returned.
func CoerceMapForStruct[T any](m map[string]any, isDeferred func(string) bool) map[string]any {
	if m == nil {
		return nil
	}
	var zero T
	t := reflect.TypeOf(zero)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return m
	}

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

// RoundConfigFromMap parses RoundConfig from a generic map via JSON round-trip.
// isDeferred is passed through to CoerceMapForStruct; see its documentation.
func RoundConfigFromMap(m map[string]any, isDeferred func(string) bool) (RoundConfig, error) {
	var cfg RoundConfig
	if m == nil {
		return cfg, nil
	}
	m = CoerceMapForStruct[RoundConfig](m, isDeferred)
	data, err := json.Marshal(m)
	if err != nil {
		return cfg, fmt.Errorf("llm: marshal config map: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("llm: unmarshal config: %w", err)
	}
	return cfg, nil
}
