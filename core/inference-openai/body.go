package openai

import (
	"fmt"
	"slices"
	"strings"

	"github.com/goccy/go-json"
	"github.com/openai/openai-go/v3/option"
	"github.com/tidwall/sjson"
)

// bodyOptions turns the request's injected raw fields into a request-body
// override. Without them the typed request goes out untouched, exactly as the SDK
// built it, so the escape hatch costs nothing on the common path.
//
// The merge is one marshal plus one in-place splice per key. Decoding the body
// into a map and re-encoding it would allocate an entry per field and copy the
// whole body twice; a raw splice touches only the bytes it replaces, which matters
// because the same path serves prompts measured in kilobytes.
func bodyOptions(params any, extra map[string]json.RawMessage) ([]option.RequestOption, error) {
	if len(extra) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	// Sorted so the same request always produces the same bytes: the body is
	// what a recording proxy and a request digest compare.
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	options := &sjson.Options{Optimistic: false}
	for _, key := range keys {
		body, err = sjson.SetRawBytesOptions(body, jsonSetPath(key), extra[key], options)
		if err != nil {
			return nil, fmt.Errorf("openai: inject json_set field %q: %w", key, err)
		}
	}
	return []option.RequestOption{option.WithRequestBody("application/json", body)}, nil
}

// jsonSetPath escapes the two characters sjson reads as path syntax, so an
// injected field name is always one literal top-level key. A provider field whose
// name contains a dot (a namespaced vendor knob) must not silently become a nested
// object.
func jsonSetPath(key string) string {
	if !strings.ContainsAny(key, `\.`) {
		return key
	}
	var path strings.Builder
	path.Grow(len(key) + 2)
	for _, char := range key {
		if char == '\\' || char == '.' {
			path.WriteByte('\\')
		}
		path.WriteRune(char)
	}
	return path.String()
}
