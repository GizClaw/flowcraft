package graph

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/goccy/go-json"
)

// fixture writes a task the way a durable run does, and returns the store.
type fixture struct {
	t     *testing.T
	store *agenttest.MemStore
	round int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, store: agenttest.NewMemStore()}
}

// save appends one step's records with the projection it left behind.
func (f *fixture) save(events ...agent.EventRecord) {
	f.t.Helper()
	f.round++
	if _, _, err := f.store.Save(context.Background(), agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:deployment",
		Task:        agent.Task{ID: "task-1", SessionID: "conv-1", State: agent.StateCompleted, Round: f.round},
		Sections: map[string]json.RawMessage{
			sectionGraph: mustMarshal(f.t, projection{
				Step: f.round, Frontier: []string{"planner"}, ContextHash: "sha256:context",
			}),
			sectionManifest: mustMarshal(f.t, agent.Manifest{Engine: "graph"}),
		},
	}, events); err != nil {
		f.t.Fatalf("Save: %v", err)
	}
}

func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// timeline writes: a seeded input, a model call under step 1, and a step 2 with
// an effect plus a record from an engine this build does not know.
func timeline(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	seed := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

	f.save(agent.EventRecord{
		Kind: RecordState,
		At:   seed,
		Payload: mustMarshal(t, stateEvent{Key: "messages", Value: encodedValue{
			Kind:     KindMessages,
			Messages: []message.Message{message.NewTextMessage(message.RoleUser, "hi")},
		}}),
	})
	f.save(
		agent.EventRecord{
			Kind:    RecordFrontier,
			At:      seed.Add(time.Second),
			Payload: mustMarshal(t, frontierEvent{Step: 1, Nodes: []string{"planner"}}),
		},
		agent.EventRecord{
			Kind: RecordLLMCall,
			At:   seed.Add(time.Second),
			Payload: mustMarshal(t, agent.LLMCallRecord{
				RequestHash: "sha256:request",
				Model:       model.ModelRef{ID: model.ModelID{Provider: "stub", Name: "alias"}},
				Metadata: inference.Metadata{
					Model:      model.ModelID{Provider: "stub", Name: "resolved"},
					RequestID:  "req-1",
					ResponseID: "resp-1",
				},
				Usage:     usage.Usage{InputTokens: 10, OutputTokens: 5},
				LatencyMs: 320,
			}),
		},
	)
	f.save(
		agent.EventRecord{
			Kind:    RecordFrontier,
			At:      seed.Add(2 * time.Second),
			Payload: mustMarshal(t, frontierEvent{Step: 2, Nodes: []string{"worker"}, Replay: true}),
		},
		agent.EventRecord{
			Kind:    RecordEffect,
			At:      seed.Add(2 * time.Second),
			Payload: mustMarshal(t, agent.EffectRecord{CallID: "c-1", ToolName: "read", Status: agent.EffectDone, DurationMs: 12}),
		},
		agent.EventRecord{
			Kind: "host.fact", At: seed.Add(3 * time.Second), Payload: json.RawMessage(`{"note":"not mine"}`),
		},
	)
	return f
}

// The log is grouped at its markers: each frontier owns the records written after
// it, and what came before the first marker is the seed.
func TestReadGroupsRecordsByFrontier(t *testing.T) {
	f := timeline(t)
	read, err := Read(context.Background(), f.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(read.Steps) != 3 {
		t.Fatalf("steps = %d, want the seed and two frontiers", len(read.Steps))
	}
	if read.Steps[0].Ordinal != 0 || len(read.Steps[0].Nodes) != 0 {
		t.Errorf("seed step = %+v, want ordinal 0 with no nodes", read.Steps[0])
	}
	if got := read.Steps[1]; got.Ordinal != 1 || len(got.Nodes) != 1 || got.Nodes[0] != "planner" {
		t.Errorf("first frontier = %+v", got)
	}
	if !read.Steps[2].Replay {
		t.Error("the second frontier was not marked as a replay")
	}
	// The marker leads its step, so it is part of that step's records.
	if read.Steps[0].Records[0].Kind != RecordState {
		t.Errorf("the seed step holds %q", read.Steps[0].Records[0].Kind)
	}
	if read.Steps[1].Records[0].Kind != RecordFrontier {
		t.Errorf("the first frontier does not lead its step: %+v", read.Steps[1].Records)
	}
	if got := read.Steps[2].Records; len(got) != 3 {
		t.Fatalf("step 2 holds %d records, want the marker, the effect and the foreign fact", len(got))
	}
	for i := 1; i < len(read.Steps[2].Records); i++ {
		before, now := read.Steps[2].Records[i-1], read.Steps[2].Records[i]
		if before.Seq >= now.Seq {
			t.Errorf("sequences not increasing at step 2: %d then %d", before.Seq, now.Seq)
		}
	}
}

// A record from an engine this build does not know stays a record of the run.
func TestReadKeepsUnknownKinds(t *testing.T) {
	f := timeline(t)
	read, err := Read(context.Background(), f.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var found *agent.EventRecord
	for _, step := range read.Steps {
		for i := range step.Records {
			if step.Records[i].Kind == "host.fact" {
				found = &step.Records[i]
			}
		}
	}
	if found == nil {
		t.Fatal("a foreign record was dropped from the timeline")
	}
	if !strings.Contains(string(found.Payload), "not mine") {
		t.Errorf("payload = %s, want the raw bytes", found.Payload)
	}
}

// The projection and the manifest ride the checkpoint, not the log: without both
// a timeline cannot say where the run stood or under what it ran.
func TestReadReportsProjectionAndManifest(t *testing.T) {
	f := timeline(t)
	read, err := Read(context.Background(), f.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.Manifest.Engine != "graph" {
		t.Errorf("manifest = %+v", read.Manifest)
	}
	// The projection is the one the last save wrote: the fixture saved the seed
	// and two frontiers, so the run stopped on step 3.
	if read.Round != 3 || read.ContextHash != "sha256:context" {
		t.Errorf("projection = round %d context %q", read.Round, read.ContextHash)
	}
	if len(read.Frontier) != 1 || read.Frontier[0] != "planner" {
		t.Errorf("frontier = %v", read.Frontier)
	}
	if read.Fingerprint != "sha256:deployment" {
		t.Errorf("fingerprint = %q", read.Fingerprint)
	}
}

// A read is scoped: a caller following a run continues from a sequence, and a cap
// bounds how much it pulls.
func TestReadAfterAndLimit(t *testing.T) {
	f := timeline(t)
	all, err := Read(context.Background(), f.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	total := 0
	for _, step := range all.Steps {
		total += len(step.Records)
	}
	if total != 6 {
		t.Fatalf("records = %d, want 6", total)
	}
	last := all.Steps[1].Records[len(all.Steps[1].Records)-1]
	tail, err := Read(context.Background(), f.store, "task-1", WithAfter(last.Seq))
	if err != nil {
		t.Fatalf("Read after: %v", err)
	}
	count := 0
	for _, step := range tail.Steps {
		count += len(step.Records)
	}
	if count != total-int(last.Seq) {
		t.Errorf("tail = %d records, want %d", count, total-int(last.Seq))
	}
	limited, err := Read(context.Background(), f.store, "task-1", WithLimit(2))
	if err != nil {
		t.Fatalf("Read limited: %v", err)
	}
	narrowed := 0
	for _, step := range limited.Steps {
		narrowed += len(step.Records)
	}
	if narrowed != 2 {
		t.Errorf("limited read = %d records, want 2", narrowed)
	}
}

// ReadSession is "the last run of this conversation", which is what an operator
// asks for when they do not know the task id.
func TestReadSession(t *testing.T) {
	f := timeline(t)
	read, err := ReadSession(context.Background(), f.store, "conv-1")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if read.Task.ID != "task-1" {
		t.Errorf("task = %q", read.Task.ID)
	}
}

// A task the store does not hold is the store's own answer, not a new error.
func TestReadMissingTask(t *testing.T) {
	f := newFixture(t)
	if _, err := Read(context.Background(), f.store, "ghost"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Errorf("err = %v, want ErrNoCheckpoint", err)
	}
	if _, err := Read(context.Background(), nil, "ghost"); err == nil {
		t.Error("a read without a store was accepted")
	}
}

// undated hides the store's stamps, standing in for the rows an older database
// holds: the store dates what it writes, but a row written before the field
// existed has no time at all.
type undated struct{ agent.Store }

func (u undated) EventsAfter(ctx context.Context, taskID string, after int64) ([]agent.EventRecord, error) {
	records, err := u.Store.EventsAfter(ctx, taskID, after)
	if err != nil {
		return nil, err
	}
	for i := range records {
		records[i].At = time.Time{}
	}
	return records, nil
}

// The renderer is the operator's half: one line per record under its step, the
// time it happened, and a marker for a row nobody could date.
func TestWriteTextRendersTheTimeline(t *testing.T) {
	f := timeline(t)
	read, err := Read(context.Background(), f.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var out bytes.Buffer
	if err := WriteText(&out, read); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"task task-1", "seed", "step 1  planner", "step 2  worker", "(replayed)",
		"llm.call", "alias → resolved", "effect", "read", "host.fact",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the timeline does not mention %q:\n%s", want, rendered)
		}
	}
	// The kind filter keeps what the operator asked for and drops the rest.
	out.Reset()
	if err := WriteText(&out, read, WithKinds(RecordLLMCall)); err != nil {
		t.Fatalf("WriteText filtered: %v", err)
	}
	if strings.Contains(out.String(), "effect") {
		t.Errorf("the kind filter kept an effect:\n%s", out.String())
	}
	// A row with no time is unknown, not the epoch.
	undatedRead, err := Read(context.Background(), undated{f.store}, "task-1")
	if err != nil {
		t.Fatalf("Read undated: %v", err)
	}
	out.Reset()
	if err := WriteText(&out, undatedRead); err != nil {
		t.Fatalf("WriteText undated: %v", err)
	}
	if !strings.Contains(out.String(), "  ?  ") {
		t.Errorf("an undated row did not render as unknown:\n%s", out.String())
	}
	// A narrow width clips rather than wrapping.
	out.Reset()
	if err := WriteText(&out, read, WithWidth(40)); err != nil {
		t.Fatalf("WriteText narrow: %v", err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if len([]rune(line)) > 40 {
			t.Errorf("a %d-character line survived a 40-wide render: %q", len([]rune(line)), line)
			break
		}
	}
}

// A timeline read after the fact describes the same steps the live run took: the
// reader is only useful if it agrees with the writer.
func TestReadAgreesWithALiveRun(t *testing.T) {
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
	read, err := Read(context.Background(), store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(read.Steps) == 0 {
		t.Fatal("the run wrote no steps")
	}
	var effects int
	for _, step := range read.Steps {
		for _, record := range step.Records {
			if record.Kind == RecordEffect {
				effects++
			}
		}
	}
	if effects != 2 {
		t.Errorf("effects = %d, want the started and done pair", effects)
	}
	// The projection names where the run stopped, and the steps reach that far.
	if read.Round == 0 {
		t.Error("the projection recorded no step")
	}
	if read.ContextHash == "" {
		t.Error("a traced run left no context hash")
	}
}

// A kind this engine does not own is rendered by whoever does, through the hook:
// the vocabulary is open by design, and a renderer that could only print its own
// records would hide a node kind it was handed.
func TestWriteTextUsesTheCallersFormat(t *testing.T) {
	f := timeline(t)
	read, err := Read(context.Background(), f.store, "task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var asked []string
	var out bytes.Buffer
	format := func(record agent.EventRecord) (string, bool) {
		asked = append(asked, record.Kind)
		if record.Kind != "host.fact" {
			return "", false
		}
		return "foreign fact, rendered by its owner", true
	}
	if err := WriteText(&out, read, WithFormat(format)); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(out.String(), "foreign fact, rendered by its owner") {
		t.Errorf("the hook's line is missing:\n%s", out.String())
	}
	if len(asked) == 0 {
		t.Error("the hook was never asked")
	}
	// A hook that declines leaves the raw payload: the record is never dropped.
	out.Reset()
	declines := func(agent.EventRecord) (string, bool) { return "", false }
	if err := WriteText(&out, read, WithFormat(declines)); err != nil {
		t.Fatalf("WriteText declining: %v", err)
	}
	if !strings.Contains(out.String(), "host.fact") {
		t.Errorf("a declined record disappeared:\n%s", out.String())
	}
}
