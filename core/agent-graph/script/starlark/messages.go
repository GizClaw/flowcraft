package starlark

import (
	"fmt"
	"strings"

	star "go.starlark.net/starlark"

	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/message"
)

// messagesValue is a transcript seen as a sequence of {"role", "text"} dicts.
//
// It is a view, not a list. Length, the last line and one index are answered
// without touching the other messages, because a rule that asks "how many turns
// have we had" or "what did they just say" should not pay for the whole
// transcript. Only iterating over every message materializes every message, which
// is the case that genuinely needs them.
//
// The author-facing shape is unchanged: len(), t[i] (including negative indices),
// t[a:b] and `for message in t` all work, and each element is the same dict a list
// of dicts would have held. The three attributes are the narrow accessors that make
// the common questions O(1).
//
// It deliberately does not implement [star.Mapping]: x[y] checks Mapping before
// Indexable, so a keyed view would turn t[-1] into "key -1 not in messages".
type messagesValue struct {
	history script.Messages
	// items is the materialized transcript, filled on the first random access so
	// repeated indices do not copy the sequence again.
	items []message.Message
}

var (
	_ star.Value     = (*messagesValue)(nil)
	_ star.Indexable = (*messagesValue)(nil)
	_ star.Sliceable = (*messagesValue)(nil)
	_ star.Iterable  = (*messagesValue)(nil)
	_ star.HasAttrs  = (*messagesValue)(nil)
)

// newMessages wraps one projected transcript.
func newMessages(history script.Messages) star.Value {
	return &messagesValue{history: history}
}

// Type implements [star.Value].
func (*messagesValue) Type() string { return "messages" }

// String renders the sequence the way a list of dicts would. It is the one
// operation that has to materialize everything, and nothing calls it unless a rule
// asks for the text of the transcript.
func (m *messagesValue) String() string {
	if m.history.Len() == 0 {
		return "[]"
	}
	items := m.materialized()
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, messageValueOf(item).String())
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Freeze implements [star.Value]. The view is read-only, so freezing is a no-op.
func (*messagesValue) Freeze() {}

// Truth implements [star.Value].
func (m *messagesValue) Truth() star.Bool { return star.Bool(m.history.Len() > 0) }

// Hash implements [star.Value]: a transcript is not a key.
func (*messagesValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable: messages")
}

// Len implements [star.Indexable]. It is the length of the sequence, not of a
// materialized copy.
func (m *messagesValue) Len() int { return m.history.Len() }

// Index implements [star.Indexable]. The interpreter has already normalized a
// negative index into range.
func (m *messagesValue) Index(i int) star.Value { return messageValueOf(m.materialized()[i]) }

// Slice implements [star.Sliceable]. Only the requested range is built.
func (m *messagesValue) Slice(start, end, step int) star.Value {
	items := m.materialized()
	list := star.NewList(nil)
	for i := start; step > 0 && i < end; i += step {
		_ = list.Append(messageValueOf(items[i]))
	}
	for i := start; step < 0 && i > end; i += step {
		_ = list.Append(messageValueOf(items[i]))
	}
	return list
}

// Iterate implements [star.Iterable]: one dict at a time, built on demand.
func (m *messagesValue) Iterate() star.Iterator { return &messagesIterator{view: m} }

// Attr implements [star.HasAttrs]: the accessors that avoid materializing.
func (m *messagesValue) Attr(name string) (star.Value, error) {
	switch name {
	case "count":
		return star.MakeInt(m.Len()), nil
	case "first":
		if m.Len() == 0 {
			return star.None, nil
		}
		return messageValueOf(m.materialized()[0]), nil
	case "last":
		if m.Len() == 0 {
			return star.None, nil
		}
		return messageValueOf(m.materialized()[m.Len()-1]), nil
	}
	return nil, nil
}

// AttrNames implements [star.HasAttrs].
func (*messagesValue) AttrNames() []string { return []string{"count", "first", "last"} }

// materialized returns the transcript's messages, copying the sequence once.
func (m *messagesValue) materialized() []message.Message {
	if m.items == nil {
		m.items = m.history.Slice()
	}
	return m.items
}

// messagesIterator walks the view lazily.
type messagesIterator struct {
	view *messagesValue
	next int
}

func (it *messagesIterator) Next(p *star.Value) bool {
	if it.next >= it.view.Len() {
		return false
	}
	*p = messageValueOf(it.view.materialized()[it.next])
	it.next++
	return true
}

func (*messagesIterator) Done() {}
