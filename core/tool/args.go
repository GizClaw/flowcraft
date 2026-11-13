package tool

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/goccy/go-json"
)

// Decode parses one tool-call argument object into out, a pointer to a
// struct whose json tags name the canonical arguments.
//
// aliases maps an accepted alternative key to its canonical name; the
// alias applies only when the canonical key is absent, so the schema
// stays authoritative and a payload carrying both keys keeps the
// canonical one. Unknown keys are rejected by name with the accepted
// argument list: silently ignoring them turns a typo into a confusing
// "required argument missing" or, worse, into a default value.
func DecodeArgs(tool, arguments string, aliases map[string]string, out any) error {
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
	dec := json.NewDecoder(strings.NewReader(string(normalized)))
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
