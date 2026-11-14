package starlark

import (
	"fmt"

	star "go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/GizClaw/flowcraft/core/message"
)

// messageValue is one message, read-only.
//
// It answers the two questions a rule asks - `message["role"]` and
// `message["text"]` - and it prints like the dict it replaces, but it is a single
// value instead of a dict plus a hashtable, which is what makes walking a
// transcript cheap.
//
// It is a mapping rather than an attribute bag because `["role"]` is the spelling
// scripts already use; `message.role` and `message.get("role")` are answered too,
// so both habits work. Nothing writes: a rule computes new state and returns it,
// and the transcript it read is not its to edit.
type messageValue struct {
	role string
	text string
}

var (
	_ star.Value      = messageValue{}
	_ star.Mapping    = messageValue{}
	_ star.HasAttrs   = messageValue{}
	_ star.Comparable = messageValue{}
)

func messageValueOf(item message.Message) messageValue {
	return messageValue{role: string(item.Role), text: contentText(item.Content)}
}

// contentText reads a message's text.
//
// A message carries one text part in every transcript this node sees, and
// Content.Text asks the content to concatenate its parts into a new string; taking
// the part directly keeps a projection from allocating strings it already has.
func contentText(content message.Content) string {
	if len(content.Parts) == 1 {
		if part, ok := content.Parts[0].(message.TextPart); ok {
			return part.Text
		}
	}
	return content.Text()
}

// Type implements [star.Value].
func (messageValue) Type() string { return "message" }

// String renders the dict shape, because a message in a log line should read the
// way it always did. The rendering is the slow path; reading a field is not.
func (m messageValue) String() string {
	return `{"role": ` + star.String(m.role).String() + `, "text": ` + star.String(m.text).String() + `}`
}

// Freeze implements [star.Value]. The value is already read-only.
func (messageValue) Freeze() {}

// Truth implements [star.Value].
func (m messageValue) Truth() star.Bool { return star.Bool(m.role != "" || m.text != "") }

// Hash implements [star.Value]: a message is not a key.
func (messageValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable: message")
}

// Get implements [star.Mapping]: message["role"], message["text"], and
// `"role" in message`.
func (m messageValue) Get(key star.Value) (star.Value, bool, error) {
	name, ok := star.AsString(key)
	if !ok {
		return nil, false, nil
	}
	switch name {
	case "role":
		return star.String(m.role), true, nil
	case "text":
		return star.String(m.text), true, nil
	}
	return nil, false, nil
}

// Attr implements [star.HasAttrs]: message.role, message.text, and a dict-shaped
// message.get(key, default).
func (m messageValue) Attr(name string) (star.Value, error) {
	switch name {
	case "role":
		return star.String(m.role), nil
	case "text":
		return star.String(m.text), nil
	case "get":
		return star.NewBuiltin("get", m.get), nil
	}
	return nil, nil
}

// AttrNames implements [star.HasAttrs].
func (messageValue) AttrNames() []string { return []string{"get", "role", "text"} }

// get is message.get(key, default = None).
func (m messageValue) get(_ *star.Thread, _ *star.Builtin, args star.Tuple, kwargs []star.Tuple) (star.Value, error) {
	var key star.Value
	var fallback star.Value = star.None
	if err := star.UnpackArgs("get", args, kwargs, "key", &key, "default?", &fallback); err != nil {
		return nil, err
	}
	if value, found, err := m.Get(key); err != nil {
		return nil, err
	} else if found {
		return value, nil
	}
	return fallback, nil
}

// CompareSameType implements [star.Comparable]: two messages are equal when they
// say the same thing, which is what a rule asserting on a cue would compare.
func (m messageValue) CompareSameType(op syntax.Token, y star.Value, _ int) (bool, error) {
	other, ok := y.(messageValue)
	if !ok {
		return false, fmt.Errorf("message %s %s", op, y.Type())
	}
	switch op {
	case syntax.EQL:
		return m == other, nil
	case syntax.NEQ:
		return m != other, nil
	}
	return false, fmt.Errorf("message is not ordered: %s", op)
}
