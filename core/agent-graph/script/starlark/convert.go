package starlark

import (
	"encoding/json"
	"fmt"

	starjson "go.starlark.net/lib/json"
	star "go.starlark.net/starlark"

	"github.com/GizClaw/flowcraft/core/agent-graph/script"
)

// decode is Starlark's own JSON reader.
//
// It is a hand-written scanner, not the reflective one in encoding/json, and it
// produces Starlark values directly - which is why a JSON state value now crosses
// the bridge as bytes.
var decode = starjson.Module.Members["decode"]

// The two keys every projected message carries. They are package values so
// building a transcript does not allocate the same two strings 200 times.
var (
	roleKey = star.String("role")
	textKey = star.String("text")
)

// toStarlark converts the node's projection into Starlark values.
//
// The domain is what encoding/json produces — nil, bool, float64, string, []any,
// map[string]any — plus the integer forms a host may build by hand. Anything else
// is refused rather than approximated: a rule that silently reads a zero for a
// value it cannot see is worse than one that fails.
func toStarlark(value any, thread *star.Thread) (star.Value, error) {
	switch typed := value.(type) {
	case nil:
		return star.None, nil
	case bool:
		return star.Bool(typed), nil
	case string:
		return star.String(typed), nil
	case float64:
		return star.Float(typed), nil
	case int:
		return star.MakeInt(typed), nil
	case int64:
		return star.MakeInt64(typed), nil
	case []any:
		list := star.NewList(make([]star.Value, 0, len(typed)))
		for i, item := range typed {
			converted, err := toStarlark(item, thread)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			if err := list.Append(converted); err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
		}
		return list, nil
	case script.Messages:
		return newMessages(typed), nil
	case json.RawMessage:
		return decodeJSON(string(typed), thread)
	case map[string]any:
		dict := star.NewDict(len(typed))
		for key, item := range typed {
			converted, err := toStarlark(item, thread)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			if err := dict.SetKey(star.String(key), converted); err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
		}
		return dict, nil
	default:
		return nil, fmt.Errorf("a %T cannot be projected into a script", value)
	}
}

// fromStarlarkDict converts the value a script returns: the map of state writes.
//
// A dict key must be a string, because the node's write keys are state key names;
// anything else would have to be stringified into a key nobody declared.
func fromStarlarkDict(dict *star.Dict) (map[string]any, error) {
	out := make(map[string]any, dict.Len())
	for _, key := range dict.Keys() {
		name, ok := star.AsString(key)
		if !ok {
			return nil, fmt.Errorf("a %s cannot be a state key", key.Type())
		}
		value, found, err := dict.Get(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !found {
			continue
		}
		item, err := fromStarlark(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = item
	}
	return out, nil
}

// fromStarlark converts one returned value back into the node's domain.
//
// Only the kinds a script can meaningfully write are accepted: a state write is
// data, and a function or a set is not.
func fromStarlark(value star.Value) (any, error) {
	switch typed := value.(type) {
	case star.NoneType:
		return nil, nil
	case star.Bool:
		return bool(typed), nil
	case star.String:
		return string(typed), nil
	case star.Float:
		return float64(typed), nil
	case star.Int:
		number, ok := typed.Int64()
		if !ok {
			return nil, fmt.Errorf("%s is outside the range a state write accepts", typed.String())
		}
		return number, nil
	case *star.List:
		out := make([]any, 0, typed.Len())
		for i := 0; i < typed.Len(); i++ {
			item, err := fromStarlark(typed.Index(i))
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			out = append(out, item)
		}
		return out, nil
	case star.Tuple:
		out := make([]any, 0, len(typed))
		for i, item := range typed {
			converted, err := fromStarlark(item)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			out = append(out, converted)
		}
		return out, nil
	case *star.Dict:
		return fromStarlarkDict(typed)
	default:
		return nil, fmt.Errorf("a %s cannot be written to state", value.Type())
	}
}

// decodeJSON parses one document into Starlark values.
//
// The module's decoder is a builtin, so the call needs a thread; passing the
// invocation's own thread keeps a rule's step budget and cancellation in force
// while a large document is parsed.
func decodeJSON(text string, thread *star.Thread) (star.Value, error) {
	if thread == nil {
		thread = &star.Thread{Name: "script:json"}
	}
	return star.Call(thread, decode, star.Tuple{star.String(text)}, nil)
}
