package graph

import (
	"context"
	"strconv"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// lengthKind writes how many messages one state key holds, which is the cheapest
// way to observe what a turn inherited.
type lengthKind struct{}

type lengthConfig struct {
	Key  string `json:"key"`
	From string `json:"from"`
}

func (lengthKind) Name() string { return "length" }

func (lengthKind) DecodeConfig(raw json.RawMessage) (any, error) {
	return ConfigOf(Invoke{Config: raw}, decodeLength)
}

func decodeLength(raw json.RawMessage) (lengthConfig, error) {
	var config lengthConfig
	if err := DecodeConfig(raw, &config); err != nil {
		return lengthConfig{}, err
	}
	return config, nil
}

func (lengthKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	config, err := ConfigOf(in, decodeLength)
	if err != nil {
		return nil, err
	}
	history, err := in.State(config.From).Messages()
	if err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{{
		Key:   config.Key,
		Value: Text(strconv.Itoa(history.Len())),
	}}}), nil
}

const carryDocument = `
version: 1
entry: measure
input: log
state:
  log: {type: messages, merge: append, carry: true}
  scratch: {type: text, carry: true}
  seen: {type: text}
  fresh: {type: text}
nodes:
  - {id: measure, kind: length, config: {key: seen, from: log}}
edges:
  - {from: measure, to: end}
output: {key: seen}
`

// TestCarryContinuesTheSession pins the point of `carry`: a turn is a task, a
// session is the conversation, and the keys the document marks cross the boundary.
func TestCarryContinuesTheSession(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, carryDocument, lengthKind{})

	first, err := g.Start(context.Background(),
		[]message.Message{message.NewTextMessage(message.RoleUser, "第一句")},
		WithStore(store, "task-1"), WithSession("conv-1"))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	firstEvents, err := drainRun(t, first)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := textOf(firstEvents); got != "1" {
		t.Fatalf("first turn saw %q messages", got)
	}

	second, err := g.Start(context.Background(),
		[]message.Message{message.NewTextMessage(message.RoleUser, "第二句")},
		WithStore(store, "task-2"), WithSession("conv-1"))
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	secondEvents, err := drainRun(t, second)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := textOf(secondEvents); got != "2" {
		t.Fatalf("second turn saw %q messages, want the carried transcript plus this one", got)
	}

	// A different session starts from nothing: the conversation is the session.
	other, err := g.Start(context.Background(),
		[]message.Message{message.NewTextMessage(message.RoleUser, "另起一段")},
		WithStore(store, "task-3"), WithSession("conv-2"))
	if err != nil {
		t.Fatalf("other start: %v", err)
	}
	otherEvents, err := drainRun(t, other)
	if err != nil {
		t.Fatalf("other run: %v", err)
	}
	if got := textOf(otherEvents); got != "1" {
		t.Fatalf("a new session saw %q messages, want only its own", got)
	}
}

// copyKind echoes one text key into another, so a run can report what it inherited.
type copyKind struct{}

type copyConfig struct {
	Key  string `json:"key"`
	From string `json:"from"`
}

func (copyKind) Name() string { return "copy" }

func (copyKind) DecodeConfig(raw json.RawMessage) (any, error) {
	return ConfigOf(Invoke{Config: raw}, decodeCopy)
}

func decodeCopy(raw json.RawMessage) (copyConfig, error) {
	var config copyConfig
	if err := DecodeConfig(raw, &config); err != nil {
		return copyConfig{}, err
	}
	return config, nil
}

func (copyKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	config, err := ConfigOf(in, decodeCopy)
	if err != nil {
		return nil, err
	}
	text, err := in.State(config.From).Text()
	if err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{{Key: config.Key, Value: Text(text)}}}), nil
}

// TestCarryIsPerKey is the other half of the contract: only the keys the document
// marks cross the task boundary, and a new session carries nothing.
func TestCarryIsPerKey(t *testing.T) {
	store := agenttest.NewMemStore()
	writer := build(t, `
version: 1
entry: kept
state:
  kept: {type: text, carry: true}
  dropped: {type: text}
nodes:
  - {id: kept, kind: const, config: {key: kept, value: remembered}}
  - {id: dropped, kind: const, config: {key: dropped, value: transient}}
edges:
  - {from: kept, to: dropped}
  - {from: dropped, to: end}
output: {key: kept}
`)
	first, err := writer.Start(context.Background(), nil, WithStore(store, "task-1"), WithSession("conv-1"))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := drainRun(t, first); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// A reader that reports what it inherited, without writing either key.
	reader := build(t, `
version: 1
entry: probe
state:
  kept: {type: text, carry: true}
  dropped: {type: text}
  seen_kept: {type: text}
  seen_dropped: {type: text}
nodes:
  - {id: probe, kind: copy, config: {key: seen_kept, from: kept}}
  - {id: probe2, kind: copy, config: {key: seen_dropped, from: dropped}}
edges:
  - {from: probe, to: probe2}
  - {from: probe2, to: end}
output: {key: seen_kept}
`, copyKind{})
	same, err := reader.Start(context.Background(), nil, WithStore(store, "task-2"), WithSession("conv-1"))
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	events, err := drainRun(t, same)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := textOf(events); got != "remembered" {
		t.Fatalf("carried key = %q, want the previous task's value", got)
	}

	fresh, err := reader.Start(context.Background(), nil, WithStore(store, "task-3"), WithSession("conv-2"))
	if err != nil {
		t.Fatalf("third start: %v", err)
	}
	events, err = drainRun(t, fresh)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := textOf(events); got != "" {
		t.Fatalf("a new session carried %q, want nothing", got)
	}
}
