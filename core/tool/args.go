package tool

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/goccy/go-json"
)

// DecodeArgs parses one tool-call argument object into out, a pointer to a
// struct whose json tags name the canonical arguments.
//
// aliases maps an accepted alternative key to its canonical name; the
// alias applies only when the canonical key is absent, so the schema
// stays authoritative and a payload carrying both keys keeps the
// canonical one. Unknown keys are rejected by name with the accepted
// argument list: silently ignoring them turns a typo into a confusing
// "required argument missing" or, worse, into a default value.
//
// An argument object a model got right is decoded once, straight into out. The
// diagnostic machinery — the accepted-key reflection, the object re-encode that
// applies aliases, the second decode — only runs when that first decode fails,
// because the error is the only thing that needs it. Every tool the builtins
// ship decodes through here, and write_file carries a whole file body, so the
// difference is the cost of two extra passes over the model's payload.
//
// The fast path is strict about the same things the slow one was: one value,
// nothing after it. Decoder.Decode reads a single value and stops, which would
// accept {"a":1}junk or two concatenated objects where the old first pass
// (json.Unmarshal over the whole payload) rejected them, so the trailing read
// below restores that guarantee for the cost of one token.
func DecodeArgs(tool, arguments string, aliases map[string]string, out any) error {
	if len(aliases) == 0 {
		if trimmed := strings.TrimSpace(arguments); trimmed != "" && trimmed[0] == '{' {
			dec := json.NewDecoder(strings.NewReader(trimmed))
			dec.DisallowUnknownFields()
			if err := dec.Decode(out); err == nil {
				// A second Decode must report io.EOF: anything else is trailing
				// data, which the old implementation refused.
				var trailing any
				if err := dec.Decode(&trailing); errors.Is(err, io.EOF) {
					return nil
				}
			}
			// Any failure is re-derived below so the caller sees the same
			// message it always has, with the accepted keys named and aliases
			// applied. Paying for that on the error path is the point.
			//
			// Two deviations are deliberate, both asserted in args_test.go.
			//
			// A target struct with no json-tagged field used to be refused
			// before any decoding; it now decodes an empty object successfully.
			// Every non-empty object is still rejected by
			// DisallowUnknownFields, and no builtin tool declares such a
			// struct.
			//
			// A key that differs from the json tag only in case is accepted
			// here and refused by the diagnostic path, because
			// DisallowUnknownFields matches fields case-insensitively (the
			// encoding/json rule) while that path compares raw keys against the
			// tags literally. The value still lands in the right field, and
			// narrowing it would mean rebuilding the accepted-key set on the hot
			// path — the reflection this change removed.
		}
	}
	return decodeArgsSlow(tool, arguments, aliases, out)
}

// decodeArgsSlow is the diagnostic implementation of [DecodeArgs]: it names
// unknown keys, maps aliases, and rejects the non-object shapes some providers
// fall back to. It runs only for arguments that could not be decoded as-is.
func decodeArgsSlow(tool, arguments string, aliases map[string]string, out any) error {
	fields, err := acceptedFields(out)
	if err != nil {
		return errdefs.Internalf("%s: %v", tool, err)
	}
	raw, err := decodeObject(tool, arguments)
	if err != nil {
		return err
	}
	unknown := make([]string, 0)
	for key := range raw {
		if _, ok := fields[key]; ok {
			continue
		}
		canonical, ok := aliases[key]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		if _, present := raw[canonical]; !present {
			raw[canonical] = raw[key]
		}
		delete(raw, key)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		noun := "argument"
		if len(unknown) > 1 {
			noun = "arguments"
		}
		return errdefs.Validationf(
			"%s: unknown %s %s; accepted arguments: %s",
			tool, noun, quoteAll(unknown), acceptedList(fields, aliases))
	}
	normalized, err := json.Marshal(raw)
	if err != nil {
		return errdefs.Internalf("%s: encode arguments: %v", tool, err)
	}
	dec := json.NewDecoder(bytes.NewReader(normalized))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errdefs.Validationf("%s: parse arguments: %v", tool, err)
	}
	return nil
}

// decodeObject parses arguments as a JSON object and rejects the
// non-object shapes some providers fall back to (a bare string, an
// array) with a message that says what was expected.
func decodeObject(tool, arguments string) (map[string]json.RawMessage, error) {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return nil, errdefs.Validationf(
			"%s: parse arguments: empty argument object, want {...}", tool)
	}
	if trimmed[0] != '{' {
		return nil, errdefs.Validationf(
			"%s: parse arguments: want a JSON object, got %s",
			tool, truncate(trimmed, 48))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, errdefs.Validationf("%s: parse arguments: %v", tool, err)
	}
	return raw, nil
}

// acceptedFields collects the json keys out accepts: the top-level
// exported fields of the struct pointer the tool decodes into.
func acceptedFields(out any) (map[string]struct{}, error) {
	rt := reflect.TypeOf(out)
	if rt == nil || rt.Kind() != reflect.Pointer ||
		rt.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("args: out must be a pointer to a struct")
	}
	rt = rt.Elem()
	fields := make(map[string]struct{}, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = struct{}{}
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("args: out has no json fields")
	}
	return fields, nil
}

// acceptedList renders the accepted arguments for an error message,
// naming the aliases of each canonical key so the model can map its
// habit onto the schema.
func acceptedList(fields map[string]struct{}, aliases map[string]string) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		var alts []string
		for alias, canonical := range aliases {
			if canonical == name {
				alts = append(alts, alias)
			}
		}
		if len(alts) == 0 {
			continue
		}
		sort.Strings(alts)
		names[i] = fmt.Sprintf("%s (alias: %s)", name, strings.Join(alts, ", "))
	}
	return strings.Join(names, ", ")
}

func quoteAll(keys []string) string {
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = strconv.Quote(key)
	}
	return strings.Join(quoted, ", ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
