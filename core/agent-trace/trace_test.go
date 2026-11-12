package agenttrace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-trace"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// recorder builds a task whose log holds one of everything a graph engine and a
// protocol adapter write.
type recorder struct {
	t     *testing.T
	store agent.Store
	seq   int
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	return &recorder{t: t, store: agenttest.NewMemStore()}
}

// save appends one batch of facts, the way a step's save does.
func (r *recorder) save(events ...agent.EventRecord) {
	r.t.Helper()
	r.seq++
	if _, _, err := r.store.Save(context.Background(), agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:deployment",
		Task:        agent.Task{ID: "task-1", SessionID: "conv-1", State: agent.StateCompleted, Round: r.seq},
		Sections: map[string]json.RawMessage{
			graph.SectionGraph: mustJSON(r.t, graph.Projection{
				Step: r.seq, ContextHash: "sha256:context",
			}),
			graph.SectionManifest: mustJSON(r.t, agent.Manifest{
				Deployment: "sha256:deployment", Engine: "graph",
			}),
		},
	}, events); err != nil {
		r.t.Fatalf("Save: %v", err)
	}
}

// modelRef is what the request asked for; resolvedMetadata is what answered.
func modelRef(provider, name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: provider, Name: name}}
}

func resolvedMetadata(name string) inference.Metadata {
	return inference.Metadata{
		Model:      model.ModelID{Provider: "stub", Name: name},
		RequestID:  "req-1",
		ResponseID: "resp-1",
	}
}

func usageOf(in, out int64) usage.Usage {
	return usage.Usage{InputTokens: in, OutputTokens: out}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// timeline is a two-step run with a seeded input, a model call, a tool execution
// and a message the protocol adapter appended.
func timeline(t *testing.T) *recorder {
	t.Helper()
	r := newRecorder(t)
	seed := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

	// Step 0: the input the run was seeded with, before any marker.
	r.save(agent.EventRecord{
		Kind: graph.RecordState,
		At:   seed,
		Payload: mustJSON(t, graph.StateWrite{Key: "messages", Value: graph.EncodedValue{
			Kind:     graph.KindMessages,
			Messages: []message.Message{message.NewTextMessage(message.RoleUser, "hi")},
		}}),
	})

	// Step 1: the model answered.
	r.save(
		agent.EventRecord{
			Kind:    "message",
			At:      seed.Add(time.Second),
			Payload: mustJSON(t, message.NewTextMessage(message.RoleUser, "hi")),
		},
		agent.EventRecord{
			Kind:    graph.RecordFrontier,
			At:      seed.Add(2 * time.Second),
			Payload: mustJSON(t, graph.FrontierRecord{Step: 1, Nodes: []string{"planner"}}),
		},
		agent.EventRecord{
			Kind: graph.RecordLLMCall,
			At:   seed.Add(2 * time.Second),
			Payload: mustJSON(t, agent.LLMCallRecord{
				RequestHash: "sha256:request",
				Model:       modelRef("stub", "alias"),
				Metadata:    resolvedMetadata("resolved"),
				Usage:       usageOf(10, 5),
				LatencyMs:   320,
				// The answer rides a blob, as the engine stores it.
				Response: mustJSON(t, message.NewTextMessage(message.RoleAssistant, "from record")),
			}),
		},
	)

	// Step 2: the tool ran, and an unknown record from a foreign engine came along.
	r.save(
		agent.EventRecord{
			Kind:    graph.RecordFrontier,
			At:      seed.Add(3 * time.Second),
			Payload: mustJSON(t, graph.FrontierRecord{Step: 2, Nodes: []string{"worker"}}),
		},
		agent.EventRecord{
			Kind:    graph.RecordEffect,
			At:      seed.Add(3 * time.Second),
			Payload: mustJSON(t, agent.EffectRecord{CallID: "c-1", ToolName: "read", Status: agent.EffectDone, DurationMs: 12}),
		},
		agent.EventRecord{
			Kind:    "other.engine.fact",
			At:      seed.Add(4 * time.Second),
			Payload: json.RawMessage(`{"note":"not mine"}`),
		},
	)
	return r
}

// Facts keep the log's order and dates, and are grouped by the marker that
// introduced their step.
func TestReadGroupsFactsByStep(t *testing.T) {
	r := timeline(t)
	trace, err := agenttrace.Read(context.Background(), r.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(trace.Steps) != 3 {
		t.Fatalf("steps = %d, want the seeded step and two frontiers", len(trace.Steps))
	}
	if trace.Steps[0].Ordinal != 0 || len(trace.Steps[0].Nodes) != 0 {
		t.Errorf("seed step = %+v, want ordinal 0 with no nodes", trace.Steps[0])
	}
	if got := trace.Steps[1].Ordinal; got != 1 {
		t.Errorf("second step ordinal = %d, want 1", got)
	}
	if got := trace.Steps[1].Nodes; len(got) != 1 || got[0] != "planner" {
		t.Errorf("first frontier nodes = %v, want planner", got)
	}
	// The adapter's message was written before the step-1 marker, so it belongs
	// to the seed: the log's order decides, not what the fact is about.
	if len(trace.Facts) != 7 {
		t.Fatalf("facts = %d, want 7", len(trace.Facts))
	}
	for i := 1; i < len(trace.Facts); i++ {
		if trace.Facts[i-1].Seq >= trace.Facts[i].Seq {
			t.Errorf("sequences not increasing at %d", i)
		}
		if trace.Facts[i].At.Before(trace.Facts[i-1].At) {
			t.Errorf("times go backwards at %d: %v then %v", i, trace.Facts[i-1].At, trace.Facts[i].At)
		}
	}
	if trace.Facts[6].Step != 2 {
		t.Errorf("the last fact belongs to step %d, want 2", trace.Facts[6].Step)
	}
}

// A foreign record is kept, not dropped: a reader that hides what it does not
// understand cannot be used on the incident that needs it.
func TestReadKeepsUnknownKinds(t *testing.T) {
	r := timeline(t)
	trace, err := agenttrace.Read(context.Background(), r.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var opaque *agenttrace.Fact
	for i := range trace.Facts {
		if trace.Facts[i].Kind == "other.engine.fact" {
			opaque = &trace.Facts[i]
		}
	}
	if opaque == nil {
		t.Fatal("the foreign record was dropped")
	}
	if opaque.Value != nil {
		t.Errorf("a foreign record decoded to %T", opaque.Value)
	}
	if !strings.Contains(string(opaque.Payload), "not mine") {
		t.Errorf("payload = %s, want the raw bytes", opaque.Payload)
	}
}

// The projection and the manifest ride the checkpoint's sections, not the log:
// without reading both, a trace cannot be attributed.
func TestReadReportsManifestAndProjection(t *testing.T) {
	r := timeline(t)
	trace, err := agenttrace.Read(context.Background(), r.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if trace.Manifest.Engine != "graph" {
		t.Errorf("manifest = %+v, want the recorded one", trace.Manifest)
	}
	if trace.Projection.ContextHash == "" {
		t.Error("the projection was not decoded: context_hash is unreadable again")
	}
	if trace.Fingerprint != "sha256:deployment" {
		t.Errorf("fingerprint = %q", trace.Fingerprint)
	}
}

// The typed views are what a dataset reads; they are derived from the same facts
// the operator's renderer sees.
func TestTraceViews(t *testing.T) {
	r := timeline(t)
	trace, err := agenttrace.Read(context.Background(), r.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	calls := trace.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Metadata.Model.Name != "resolved" {
		t.Errorf("call metadata = %+v, want the resolved model", calls[0].Metadata)
	}
	if got := trace.Usage(); got.InputTokens != 10 || got.OutputTokens != 5 {
		t.Errorf("usage = %+v, want the call's accounting", got)
	}
	if effects := trace.Effects(); len(effects) != 1 || effects[0].ToolName != "read" {
		t.Errorf("effects = %+v", effects)
	}
	if messages := trace.Messages(); len(messages) != 1 || messages[0].Role != message.RoleUser {
		t.Errorf("messages = %+v, want the adapter's message", messages)
	}
	state := trace.State()
	value, ok := state["messages"]
	if !ok {
		t.Fatalf("the folded state holds %d keys and not the seeded messages", len(state))
	}
	transcript, err := value.Messages()
	if err != nil {
		t.Fatalf("messages value: %v", err)
	}
	if got := transcript.Slice(); len(got) != 1 || got[0].Content.Text() != "hi" {
		t.Errorf("transcript = %+v", got)
	}
}

// The answer comes back from the record, byte for byte.
func TestTraceResponse(t *testing.T) {
	r := timeline(t)
	trace, err := agenttrace.Read(context.Background(), r.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	answer, err := trace.Response(context.Background(), 0)
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if answer.Content.Text() != "from record" {
		t.Errorf("answer = %q", answer.Content.Text())
	}
	if _, err := trace.Response(context.Background(), 7); err == nil {
		t.Error("a call that does not exist answered")
	}
}

// A read is scoped: WithAfter continues a timeline instead of repeating it.
func TestReadAfterASequence(t *testing.T) {
	r := timeline(t)
	all, err := agenttrace.Read(context.Background(), r.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tail, err := agenttrace.Read(context.Background(), r.store, "task-1",
		agenttrace.WithAfter(all.Facts[4].Seq))
	if err != nil {
		t.Fatalf("Read after: %v", err)
	}
	if len(tail.Facts) != len(all.Facts)-5 {
		t.Errorf("tail = %d facts, want %d", len(tail.Facts), len(all.Facts)-5)
	}
	limited, err := agenttrace.Read(context.Background(), r.store, "task-1", agenttrace.WithLimit(2))
	if err != nil {
		t.Fatalf("Read limited: %v", err)
	}
	if len(limited.Facts) != 2 {
		t.Errorf("limited = %d facts, want 2", len(limited.Facts))
	}
}

// A task the store does not hold is the store's own answer, not a new error.
func TestReadMissingTask(t *testing.T) {
	store := agenttest.NewMemStore()
	_, err := agenttrace.Read(context.Background(), store, "ghost")
	if !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("err = %v, want ErrNoCheckpoint", err)
	}
}

// An undated fact renders as unknown rather than as the epoch, and the renderer
// never invents an ordering it does not have.
func TestWriteTextRendersTheTimeline(t *testing.T) {
	r := timeline(t)
	trace, err := agenttrace.Read(context.Background(), r.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var out bytes.Buffer
	if err := agenttrace.WriteText(&out, trace); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"task task-1",
		"seed",
		"step 1  planner",
		"llm.call",
		"alias → resolved",
		"effect",
		"read",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered timeline does not mention %q:\n%s", want, rendered)
		}
	}
	// A row written before events carried a time renders as unknown, not as the
	// epoch: a wrong time is harder to notice than a missing one.
	undatedTrace, err := agenttrace.Read(context.Background(), undated{r.store}, "task-1")
	if err != nil {
		t.Fatalf("Read undated: %v", err)
	}
	out.Reset()
	if err := agenttrace.WriteText(&out, undatedTrace); err != nil {
		t.Fatalf("WriteText undated: %v", err)
	}
	if !strings.Contains(out.String(), "?  ") {
		t.Errorf("an undated row did not render as unknown:\n%s", out.String())
	}

	// A filter keeps the kind the operator asked for and drops the rest.
	out.Reset()
	if err := agenttrace.WriteText(&out, trace, agenttrace.WithKinds(graph.RecordLLMCall)); err != nil {
		t.Fatalf("WriteText filtered: %v", err)
	}
	if strings.Contains(out.String(), "effect") {
		t.Errorf("the kind filter kept an effect:\n%s", out.String())
	}
}

// undated hides the store's own stamps, standing in for the rows an older
// database holds: the store dates what it writes, but a row written before the
// field existed has no time at all.
type undated struct{ agent.Store }

func (u undated) EventsAfter(ctx context.Context, taskID string, after int64) ([]agent.EventRecord, error) {
	events, err := u.Store.EventsAfter(ctx, taskID, after)
	if err != nil {
		return nil, err
	}
	for i := range events {
		events[i].At = time.Time{}
	}
	return events, nil
}
