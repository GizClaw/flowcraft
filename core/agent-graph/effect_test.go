package graph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// effectRecordKind is the record kind the tool node writes.
const effectRecordKind = "graph.effect"

const effectDoc = `
version: 1
entry: a
state:
  flag: {type: bool}
nodes:
  - {id: a, kind: effect}
edges:
  - {from: a, to: end}
`

// effectKind records a side effect the way a tool node does: started before the
// work, the outcome with the step's save.
type effectKind struct{}

func (effectKind) Name() string { return "effect" }

func (effectKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	if in.Checkpoint == nil {
		return nil, errors.New("effect: no checkpoint")
	}
	started, err := json.Marshal(agent.EffectRecord{
		CallID: "call-1", ToolName: "read", Status: agent.EffectStarted,
	})
	if err != nil {
		return nil, err
	}
	if err := in.Checkpoint([]Record{{Kind: effectRecordKind, Payload: started}}); err != nil {
		return nil, err
	}
	done, err := json.Marshal(agent.EffectRecord{
		CallID: "call-1", ToolName: "read", Status: agent.EffectDone,
	})
	if err != nil {
		return nil, err
	}
	return Static(Result{Records: []Record{{Kind: effectRecordKind, Payload: done}}}), nil
}

// A before-effect is durable before the work runs, and its outcome rides the
// step's save: the pair is what makes a crash reconcilable.
func TestEffectsAreRecordedAroundTheCall(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, effectDoc, effectKind{})
	run, err := g.Start(context.Background(), nil,
		WithStore(store, "task-1"),
		WithFingerprint("sha256:x"),
		WithTrace(telemetry.TraceStep),
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	events, err := store.Events(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var effects []agent.EffectRecord
	for _, event := range events {
		if event.Kind != effectRecordKind {
			continue
		}
		var effect agent.EffectRecord
		if err := json.Unmarshal(event.Payload, &effect); err != nil {
			t.Fatalf("decode effect: %v", err)
		}
		effects = append(effects, effect)
	}
	if len(effects) != 2 {
		t.Fatalf("effects = %d, want started and done", len(effects))
	}
	if effects[0].Status != agent.EffectStarted || effects[1].Status != agent.EffectDone {
		t.Errorf("effects = %+v, want started before done", effects)
	}
	checkpoint, err := store.Load(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var projection struct {
		ContextHash string `json:"context_hash"`
	}
	if err := json.Unmarshal(checkpoint.Sections[sectionGraph], &projection); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	if projection.ContextHash == "" {
		t.Error("a traced run wrote no context hash")
	}
}

// A started effect with no outcome is not guessed at: the resume is refused
// until the host reconciles it.
func TestUncertainEffectRefusesResume(t *testing.T) {
	store := agenttest.NewMemStore()
	started, err := json.Marshal(agent.EffectRecord{
		CallID: "call-1", ToolName: "read", Status: agent.EffectStarted,
	})
	if err != nil {
		t.Fatalf("marshal effect: %v", err)
	}
	if _, _, err := store.Save(context.Background(), agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:x",
		Task:        agent.Task{ID: "task-1", SessionID: "c1", State: agent.StateWorking},
	}, []agent.EventRecord{{Kind: effectRecordKind, Payload: started}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	g := build(t, effectDoc, effectKind{})
	if _, err := g.Start(context.Background(), nil,
		WithStore(store, "task-1"),
		WithFingerprint("sha256:x"),
	); !errors.Is(err, agent.ErrEffectUncertain) {
		t.Fatalf("Start = %v, want ErrEffectUncertain", err)
	}
}
