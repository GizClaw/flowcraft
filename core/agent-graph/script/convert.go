package script

import (
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// project reads the declared state keys into the backend's value domain.
//
// A declared key the graph does not declare is an error rather than a zero value:
// a rule that reads a typo'd name would compute on an empty world and return a
// plausible answer.
func project(in graph.Invoke, reads []string) (map[string]any, error) {
	state := make(map[string]any, len(reads))
	for _, key := range reads {
		value := in.State(key)
		if value.Kind() == "" {
			return nil, errdefs.Validationf(
				"script: node %q: state key %q is not declared by the graph", in.Node, key)
		}
		plain, err := plainValue(value)
		if err != nil {
			return nil, errdefs.Validationf("script: node %q: read %q: %w", in.Node, key, err)
		}
		state[key] = plain
	}
	return state, nil
}

// plainValue projects one state value.
//
// It reuses [graph.Plain], which is what a condition expression sees, so a script
// and a routing condition read the same state the same way. Media values are
// refused: a rule computes over facts, and a blob reference is not one.
func plainValue(value graph.Value) (any, error) {
	switch value.Kind() {
	case graph.KindText, graph.KindBool:
		return graph.Plain(value), nil
	case graph.KindJSON:
		// The bytes are handed over as they are: a backend that speaks JSON decodes
		// them with its own scanner, and one that speaks Starlark gets values built
		// from them. Decoding into an `any` here only to re-encode it a layer down
		// was the reflective half of a rule's cost.
		raw, err := value.JSON()
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case graph.KindMessages:
		history, err := value.Messages()
		if err != nil {
			return nil, err
		}
		// The sequence is handed over as itself: materializing one map per message
		// here and rebuilding it as a Starlark dict one layer down doubled the
		// cost of the projection that dominates a chat-shaped rule.
		return Messages{History: history}, nil
	default:
		return nil, fmt.Errorf("kind %q cannot be projected into a script", value.Kind())
	}
}

// writes validates one invocation's result and converts it into state updates.
//
// Three rules, each of which a deployment would otherwise discover the hard way:
// a key the config does not declare is refused, a value of the wrong kind is
// refused, and a total over the configured cap is refused rather than truncated.
func writes(in graph.Invoke, cfg Config, out map[string]any) ([]graph.StateUpdate, error) {
	allowed := make(map[string]struct{}, len(cfg.Writes))
	for _, key := range cfg.Writes {
		allowed[key] = struct{}{}
	}
	keys := make([]string, 0, len(out))
	for key := range out {
		keys = append(keys, key)
	}
	// A stable order makes the update list, the cap arithmetic and the reported
	// error independent of the backend's map iteration.
	sort.Strings(keys)

	updates := make([]graph.StateUpdate, 0, len(keys))
	total := 0
	for _, key := range keys {
		if _, ok := allowed[key]; !ok {
			return nil, errdefs.Validationf(
				"script: node %q wrote state key %q, which config.writes does not declare",
				in.Node, key)
		}
		declared := in.State(key)
		if declared.Kind() == "" {
			return nil, errdefs.Validationf(
				"script: node %q: state key %q is not declared by the graph", in.Node, key)
		}
		value, size, err := stateValue(declared.Kind(), out[key])
		if err != nil {
			return nil, errdefs.Validationf(
				"script: node %q: write %q: %w", in.Node, key, err)
		}
		total += size
		updates = append(updates, graph.StateUpdate{Key: key, Value: value})
	}
	if cfg.MaxOutputBytes > 0 && total > cfg.MaxOutputBytes {
		return nil, errdefs.Validationf(
			"script: node %q: writes are %d bytes, config.max_output_bytes is %d",
			in.Node, total, cfg.MaxOutputBytes)
	}
	return updates, nil
}

// stateValue converts one returned value into the declared state kind.
func stateValue(kind graph.Kind, value any) (graph.Value, int, error) {
	switch kind {
	case graph.KindText:
		text, ok := value.(string)
		if !ok {
			return graph.Value{}, 0, fmt.Errorf("want a string, got %T", value)
		}
		return graph.Text(text), len(text), nil
	case graph.KindBool:
		flag, ok := value.(bool)
		if !ok {
			return graph.Value{}, 0, fmt.Errorf("want a bool, got %T", value)
		}
		return graph.Bool(flag), 1, nil
	case graph.KindJSON:
		raw, err := json.Marshal(value)
		if err != nil {
			return graph.Value{}, 0, fmt.Errorf("encode as JSON: %w", err)
		}
		return graph.JSON(raw), len(raw), nil
	case graph.KindMessages:
		return messagesValue(value)
	default:
		return graph.Value{}, 0, fmt.Errorf("kind %q cannot be written by a script", kind)
	}
}

// messagesValue converts the returned transcript delta.
//
// The value is a delta, not the whole transcript: the state key merges with
// append, so a script that returned the history would duplicate everything it
// could already read.
func messagesValue(value any) (graph.Value, int, error) {
	items, ok := value.([]any)
	if !ok {
		return graph.Value{}, 0, fmt.Errorf(
			`want a list of {"role", "text"} objects, got %T`, value)
	}
	history := make([]message.Message, 0, len(items))
	total := 0
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return graph.Value{}, 0, fmt.Errorf("message %d: want an object, got %T", i, item)
		}
		role, _ := entry["role"].(string)
		text, _ := entry["text"].(string)
		if role == "" {
			return graph.Value{}, 0, fmt.Errorf("message %d: role is required", i)
		}
		msg := message.NewTextMessage(message.Role(role), text)
		if err := msg.Validate(); err != nil {
			return graph.Value{}, 0, fmt.Errorf("message %d: %w", i, err)
		}
		history = append(history, msg)
		total += len(role) + len(text)
	}
	return graph.MessagesValue(graph.MessagesFromSlice(history)), total, nil
}

// Messages is a projected transcript.
//
// It is the sequence, not a list of maps, because the two consumers want
// different shapes: a Starlark backend builds its dicts directly, and a backend
// that speaks JSON reads [Messages.Slice] and encodes what it needs. Both avoid
// an intermediate map per message.
type Messages struct {
	// History is the transcript: an immutable sequence a backend may render
	// however its language spells a list.
	History graph.Messages
}

// Slice returns the transcript's messages in order.
func (m Messages) Slice() []message.Message { return m.History.Slice() }

// Len reports how many messages the transcript holds.
func (m Messages) Len() int { return m.History.Len() }
