package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// A run picks up where its checkpoint left it.
func TestCheckpointRoundTrip(t *testing.T) {
	store := agenttest.NewMemStore()
	counter := &countKind{}
	g := build(t, `
version: 1
entry: a
state:
  seed: {type: text}
nodes:
  - {id: a, kind: const, config: {key: seed, value: x}}
  - {id: b, kind: count}
edges:
  - {from: a, to: b}
  - {from: b, to: end}
output: {key: seed}
`, counter)
	run, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.State() != agent.StateCompleted {
		t.Fatalf("state = %s", run.State())
	}
	calls := counter.count("a") + counter.count("b")

	restored, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.State() != agent.StateCompleted {
		t.Fatalf("restored state = %s, want completed", restored.State())
	}
	if got, _ := restored.Value("seed").Text(); got != "x" {
		t.Errorf("restored seed = %q, want x", got)
	}
	// A terminal run is read-only: draining it must not re-execute anything.
	if _, err := drainRun(t, restored); err != nil {
		t.Fatalf("drain restored: %v", err)
	}
	if counter.count("a")+counter.count("b") != calls {
		t.Errorf("nodes re-ran on a restored terminal run")
	}
}

// A suspended run survives a restart: the frontier and the lifecycle state are
// both restored, so Answer continues the same task.
func TestSuspendSurvivesARestart(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, `
version: 1
entry: ask
state:
  text: {type: text}
nodes:
  - {id: ask, kind: ask, config: {key: text}}
edges:
  - {from: ask, to: end}
output: {key: text}
`, askKind{})
	run, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.State() != agent.StateInputRequired {
		t.Fatalf("state = %s", run.State())
	}

	restored, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.State() != agent.StateInputRequired {
		t.Fatalf("restored state = %s, want input-required", restored.State())
	}
	var text string
	for event, err := range answer(context.Background(), restored, "answer") {
		if err != nil {
			t.Fatalf("answer: %v", err)
		}
		if typed, ok := event.(agent.Text); ok {
			text = typed.Text
		}
	}
	if text != "answer" {
		t.Errorf("output = %q, want the answer", text)
	}
}

// A checkpoint taken under another deployment is refused.
func TestFingerprintMismatchIsRefused(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`, &countKind{})
	first, err := g.Start(context.Background(), nil, WithStore(store, "run-1"), WithFingerprint("sha256:one"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, first); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := g.Start(context.Background(), nil, WithStore(store, "run-1"), WithFingerprint("sha256:two")); err == nil {
		t.Fatal("a checkpoint from another deployment was accepted")
	}
}

// The per-step record stays small: an appended message is one delta, not the whole
// transcript.
func TestCheckpointStoresDeltasNotTheTranscript(t *testing.T) {
	store := agenttest.NewMemStore()
	var doc strings.Builder
	doc.WriteString("version: 1\nentry: n0\ninput: messages\nstate:\n  messages: {type: messages, merge: append}\n")
	doc.WriteString("nodes:\n")
	const steps = 10
	for i := 0; i < steps; i++ {
		fmt.Fprintf(&doc, "  - {id: n%d, kind: append, config: {key: messages, text: t%d, delta: true}}\n", i, i)
	}
	doc.WriteString("edges:\n")
	for i := 0; i < steps-1; i++ {
		fmt.Fprintf(&doc, "  - {from: n%d, to: n%d}\n", i, i+1)
	}
	fmt.Fprintf(&doc, "  - {from: n%d, to: end}\n", steps-1)
	doc.WriteString("output: {key: messages}\n")

	g := build(t, doc.String(), appendKind{})
	run, err := g.Start(context.Background(), []message.Message{
		message.NewTextMessage(message.RoleUser, "start"),
	}, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	events, err := store.Events(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	stateEvents := 0
	for _, event := range events {
		if event.Kind != eventState {
			continue
		}
		stateEvents++
		if len(event.Payload) > 1024 {
			t.Fatalf("state event is %d bytes; the transcript leaked into the delta", len(event.Payload))
		}
	}
	if stateEvents < steps {
		t.Fatalf("state events = %d, want at least one per step", stateEvents)
	}
	restored, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	messages, err := restored.Value("messages").Messages()
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if messages.Len() != steps+1 {
		t.Fatalf("restored transcript = %d messages, want the seed plus %d deltas",
			messages.Len(), steps)
	}
}

// failingStore fails a configured number of saves.
type failingStore struct {
	agent.Store
	failures int
}

func (s *failingStore) Save(ctx context.Context, checkpoint agent.Checkpoint, events []agent.EventRecord, _ ...agent.SaveOption) (int64, int64, error) {
	if s.failures > 0 {
		s.failures--
		return 0, 0, errors.New("disk full")
	}
	return s.Store.Save(ctx, checkpoint, events)
}

// A failed save does not fail the run; the next save writes a base so the log
// never has a hole.
func TestFailedSaveFallsBackToABase(t *testing.T) {
	store := &failingStore{Store: agenttest.NewMemStore(), failures: 1}
	counter := &countKind{}
	g := build(t, `
version: 1
entry: a
state:
  seed: {type: text}
nodes:
  - {id: a, kind: const, config: {key: seed, value: x}}
  - {id: b, kind: count}
  - {id: c, kind: count}
edges:
  - {from: a, to: b}
  - {from: b, to: c}
  - {from: c, to: end}
output: {key: seed}
`, counter)
	run, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	events, err := store.Events(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	base := false
	for _, event := range events {
		if event.Kind == eventBase {
			base = true
		}
	}
	if !base {
		t.Fatalf("no base was written after a failed save; events = %d", len(events))
	}
	restored, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, _ := restored.Value("seed").Text(); got != "x" {
		t.Errorf("restored seed = %q, want x", got)
	}
}

// checkpoint.every bounds replay: one base per interval, and a restore still
// lands on the same state.
func TestCheckpointWritesABaseEveryNSteps(t *testing.T) {
	store := agenttest.NewMemStore()
	var doc strings.Builder
	doc.WriteString("version: 1\nentry: n0\ncheckpoint: {every: 3}\nstate:\n  count: {type: json}\n")
	doc.WriteString("nodes:\n")
	const steps = 7
	for i := 0; i < steps; i++ {
		fmt.Fprintf(&doc, "  - {id: n%d, kind: bump, config: {key: count}}\n", i)
	}
	doc.WriteString("edges:\n")
	for i := 0; i < steps-1; i++ {
		fmt.Fprintf(&doc, "  - {from: n%d, to: n%d}\n", i, i+1)
	}
	fmt.Fprintf(&doc, "  - {from: n%d, to: end}\n", steps-1)

	g := build(t, doc.String(), bumpKind{})
	run, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	events, err := store.Events(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	bases := 0
	for _, event := range events {
		if event.Kind == eventBase {
			bases++
		}
	}
	if bases != 1 {
		t.Fatalf("bases = %d, want only the newest: the ones it superseded are truncated", bases)
	}
	// Compaction is the point of the interval: the log holds the newest base and
	// the deltas after it, not every step since the beginning.
	if len(events) >= steps {
		t.Fatalf("events = %d for %d steps, want the log compacted", len(events), steps)
	}
	restored, err := g.Start(context.Background(), nil, WithStore(store, "run-1"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, want := restored.Value("count").text, run.Value("count").text; got != want {
		t.Errorf("restored count = %s, want %s", got, want)
	}
}

// bumpKind increments a JSON counter, one step per node.
type bumpKind struct{}

func (bumpKind) Name() string { return "bump" }

func (bumpKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(in.Config, &config); err != nil {
		return nil, err
	}
	current := 0
	if raw, err := in.Snapshot().Get(config.Key).JSON(); err == nil {
		_ = json.Unmarshal(raw, &current)
	}
	encoded, err := json.Marshal(current + 1)
	if err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{{Key: config.Key, Value: JSON(encoded)}}}), nil
}

// The first save records the version combination: a badcase report can then say
// which deployment, engine and tool schema produced the trace.
func TestManifestIsWrittenToTheFirstSave(t *testing.T) {
	store := agenttest.NewMemStore()
	g := build(t, `
version: 1
entry: a
state:
  flag: {type: text}
nodes:
  - {id: a, kind: const, config: {key: flag, value: "true"}}
edges:
  - {from: a, to: end}
`)
	manifest := agent.Manifest{
		Deployment: "sha256:deployment",
		Engine:     "graph",
		Tools:      "sha256:tools",
		Harness:    "test",
	}
	run, err := g.Start(context.Background(), nil,
		WithStore(store, "run-1"),
		WithFingerprint("sha256:deployment"),
		WithManifest(manifest),
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	checkpoint, err := store.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, ok := checkpoint.Sections["manifest"]
	if !ok {
		t.Fatal("the manifest was not written")
	}
	var stored agent.Manifest
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if stored.Engine != "graph" || stored.Tools != "sha256:tools" || stored.Harness != "test" {
		t.Errorf("manifest = %+v", stored)
	}
	if stored.ID() != manifest.ID() || stored.ID() == "" {
		t.Errorf("manifest id = %q, want %q", stored.ID(), manifest.ID())
	}
}
