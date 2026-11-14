package graph

import (
	"context"
	"strconv"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/goccy/go-json"
)

// factKind records one durable fact and writes nothing.
type factKind struct{}

func (factKind) Name() string { return "fact" }

func (factKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	return Static(Result{Records: []Record{{
		Kind:    "test.fact",
		Payload: json.RawMessage(`{"n":1}`),
	}}}), nil
}

// inspectKind writes how many facts of one kind the task recorded.
type inspectKind struct{}

type inspectConfig struct {
	Key string `json:"key"`
}

func (inspectKind) Name() string { return "inspect" }

func (inspectKind) DecodeConfig(raw json.RawMessage) (any, error) {
	return ConfigOf(Invoke{Config: raw}, decodeInspect)
}

func decodeInspect(raw json.RawMessage) (inspectConfig, error) {
	var config inspectConfig
	if err := DecodeConfig(raw, &config); err != nil {
		return inspectConfig{}, err
	}
	return config, nil
}

func (inspectKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	config, err := ConfigOf(in, decodeInspect)
	if err != nil {
		return nil, err
	}
	facts := in.Records("test.fact")
	return Static(Result{State: []StateUpdate{{
		Key:   config.Key,
		Value: Text(strconv.Itoa(len(facts))),
	}}}), nil
}

// TestRecordsSurviveARestore pins the contract a replaying node depends on: a
// durable fact another step recorded is readable after the task was restored in a
// fresh process, not only while the run that wrote it is still in memory.
func TestRecordsSurviveARestore(t *testing.T) {
	store := agenttest.NewMemStore()
	document := `
version: 1
entry: write
state:
  seen: {type: text}
  answer: {type: text}
nodes:
  - {id: write, kind: fact}
  - {id: ask, kind: ask, config: {key: answer}}
  - {id: check, kind: inspect, config: {key: seen}}
edges:
  - {from: write, to: ask}
  - {from: ask, to: check}
  - {from: check, to: end}
output: {key: seen}
`
	g := build(t, document, factKind{}, inspectKind{}, askKind{})

	// First process: record the fact, then stop on the question.
	first, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, first); err != nil {
		t.Fatalf("run to suspension: %v", err)
	}
	if first.State() != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", first.State())
	}

	// Second process: a new Run restores the task, and the resumed step reads the
	// fact the first process recorded.
	restored, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	var events []agent.Event
	for event, err := range answer(context.Background(), restored, "continue") {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		events = append(events, event)
	}
	if got := textOf(events); got != "1" {
		t.Fatalf("inspect saw %q facts, want the one recorded before the restore", got)
	}
}
