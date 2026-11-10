package graph

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
	"github.com/GizClaw/flowcraft/core/message"
)

// benchKind writes one boolean, so a benchmark measures the engine rather than a
// node.
type benchKind struct{}

func (benchKind) Name() string { return "bench" }

func (benchKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	return Static(Result{State: []StateUpdate{{Key: "flag", Value: Bool(true)}}}), nil
}

const benchDoc = `
version: 1
entry: a
state:
  flag: {type: bool}
nodes:
  - {id: a, kind: bench}
edges:
  - {from: a, to: end}
`

// benchDocBase writes a base every ten steps, the shape a durable run uses.
const benchDocBase = `
version: 1
checkpoint: {every: 10}
entry: a
state:
  flag: {type: bool}
nodes:
  - {id: a, kind: bench}
edges:
  - {from: a, to: end}
`

// benchDocFanOut runs two nodes in one frontier, the concurrent path.
const benchDocFanOut = `
version: 1
entry: root
state:
  flag: {type: bool, merge: last}
nodes:
  - {id: root, kind: bench}
  - {id: a, kind: bench}
  - {id: b, kind: bench}
edges:
  - {from: root, to: a}
  - {from: root, to: b}
  - {from: a, to: end}
  - {from: b, to: end}
`

const benchSuspendDoc = `
version: 1
entry: ask
state:
  text: {type: text}
nodes:
  - {id: ask, kind: ask, config: {key: text}}
edges:
  - {from: ask, to: end}
output: {key: text}
`

func benchmarkGraph(b *testing.B, body string, extra ...NodeKind) *Graph {
	b.Helper()
	document, err := Parse([]byte(body))
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	registry, err := NewRegistry(extra...)
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	graph, err := Build(document, registry)
	if err != nil {
		b.Fatalf("Build: %v", err)
	}
	return graph
}

func benchmarkStore(b *testing.B) *checkpoint.SQLite {
	b.Helper()
	store, err := checkpoint.Open(filepath.Join(b.TempDir(), "checkpoints.db"))
	if err != nil {
		b.Fatalf("checkpoint.Open: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// BenchmarkRunOneStep is the whole loop for one frontier: start, invoke, merge,
// route and report the status.
func BenchmarkRunOneStep(b *testing.B) {
	g := benchmarkGraph(b, benchDoc, benchKind{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		run, err := g.Start(ctx, nil)
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		for _, err := range run.Events(ctx) {
			if err != nil {
				b.Fatalf("Events: %v", err)
			}
		}
	}
}

// BenchmarkRunFanOut is the same loop with two nodes in one frontier: the step
// takes the goroutine path instead of the inline one.
func BenchmarkRunFanOut(b *testing.B) {
	g := benchmarkGraph(b, benchDocFanOut, benchKind{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		run, err := g.Start(ctx, nil)
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		for _, err := range run.Events(ctx) {
			if err != nil {
				b.Fatalf("Events: %v", err)
			}
		}
	}
}

// BenchmarkPersistStep is the durable cost of one step: encode the write, bump
// the projection and append the event, in one transaction.
func BenchmarkPersistStep(b *testing.B) {
	store := benchmarkStore(b)
	g := benchmarkGraph(b, benchDoc, benchKind{})
	ctx := context.Background()
	run, err := g.Start(ctx, nil, WithStore(store, "task-1"), WithFingerprint("sha256:bench"))
	if err != nil {
		b.Fatalf("Start: %v", err)
	}
	updates := []StateUpdate{{Key: "flag", Value: Bool(true)}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		run.step++
		run.persist(ctx, updates)
	}
	b.StopTimer()
	if _, err := store.Load(ctx, "task-1"); err != nil {
		b.Fatalf("store never received the step: %v", err)
	}
}

// BenchmarkPersistBaseEvery10 adds the base that bounds replay: one snapshot and
// one more event, once per ten steps.
func BenchmarkPersistBaseEvery10(b *testing.B) {
	store := benchmarkStore(b)
	g := benchmarkGraph(b, benchDocBase, benchKind{})
	ctx := context.Background()
	run, err := g.Start(ctx, nil, WithStore(store, "task-1"), WithFingerprint("sha256:bench"))
	if err != nil {
		b.Fatalf("Start: %v", err)
	}
	updates := []StateUpdate{{Key: "flag", Value: Bool(true)}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		run.step++
		run.persist(ctx, updates)
	}
	b.StopTimer()
	if _, err := store.Load(ctx, "task-1"); err != nil {
		b.Fatalf("store never received the step: %v", err)
	}
}

// BenchmarkRestore is a restart: read the projection, the base blob it names and
// only the events after that base, then replay them.
func BenchmarkRestore(b *testing.B) {
	for _, steps := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("steps=%d", steps), func(b *testing.B) {
			store := benchmarkStore(b)
			g := benchmarkGraph(b, benchDocBase, benchKind{})
			ctx := context.Background()
			run, err := g.Start(ctx, nil, WithStore(store, "task-1"), WithFingerprint("sha256:bench"))
			if err != nil {
				b.Fatalf("Start: %v", err)
			}
			updates := []StateUpdate{{Key: "flag", Value: Bool(true)}}
			for i := 0; i < steps; i++ {
				run.step++
				run.persist(ctx, updates)
			}
			options := runOptions{
				store: store, taskID: "task-1",
				sessionID: "bench", fingerprint: "sha256:bench",
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, found, err := g.restore(ctx, options); err != nil || !found {
					b.Fatalf("restore = %v, found %v", err, found)
				}
			}
		})
	}
}

// BenchmarkSuspendResume is the round trip a paused task pays: run to the
// question, then answer it.
func BenchmarkSuspendResume(b *testing.B) {
	g := benchmarkGraph(b, benchSuspendDoc, askKind{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		run, err := g.Start(ctx, nil)
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		for _, err := range run.Events(ctx) {
			if err != nil {
				b.Fatalf("Events: %v", err)
			}
		}
		if run.State() != agent.StateInputRequired {
			b.Fatalf("state = %s, want input-required", run.State())
		}
		for _, err := range run.Answer(ctx, message.NewTextContent("answer")) {
			if err != nil {
				b.Fatalf("Answer: %v", err)
			}
		}
	}
}

// BenchmarkSuspendResumeDurable is the same round trip with a store: the pause
// is persisted, a fresh process restores it, and the answer persists the end.
func BenchmarkSuspendResumeDurable(b *testing.B) {
	store := benchmarkStore(b)
	g := benchmarkGraph(b, benchSuspendDoc, askKind{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		taskID := fmt.Sprintf("task-%d", i)
		first, err := g.Start(ctx, nil, WithStore(store, taskID), WithFingerprint("sha256:bench"))
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		for _, err := range first.Events(ctx) {
			if err != nil {
				b.Fatalf("Events: %v", err)
			}
		}
		// A restarted process knows only the checkpoint.
		resumed, found, err := g.restore(ctx, runOptions{
			store: store, taskID: taskID,
			sessionID: "bench", fingerprint: "sha256:bench",
		})
		if err != nil || !found {
			b.Fatalf("restore = %v, found %v", err, found)
		}
		for _, err := range resumed.Answer(ctx, message.NewTextContent("answer")) {
			if err != nil {
				b.Fatalf("Answer: %v", err)
			}
		}
	}
}

// BenchmarkCondition is one edge evaluation against a ten-key snapshot. The
// environment is rebuilt per step, so this is the routing cost the loop pays.
func BenchmarkCondition(b *testing.B) {
	condition, err := CompileCondition(`state.k0 == "v" and step < 100`)
	if err != nil {
		b.Fatalf("CompileCondition: %v", err)
	}
	values := make(map[string]Value, 10)
	for i := 0; i < 10; i++ {
		values["k"+string(rune('0'+i))] = Text("v")
	}
	snapshot := NewSnapshot(values)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := condition.Evaluate(1, snapshot); err != nil {
			b.Fatalf("Evaluate: %v", err)
		}
	}
}

// BenchmarkMessagesFromSlice rebuilds a transcript the way a restore does: one
// allocation for the whole chain, sharing the messages.
func BenchmarkMessagesFromSlice(b *testing.B) {
	for _, size := range []int{10, 1_000, 10_000} {
		b.Run(fmt.Sprintf("messages=%d", size), func(b *testing.B) {
			messages := make([]message.Message, size)
			for i := range messages {
				messages[i] = message.NewTextMessage(message.RoleUser, "turn")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if got := MessagesFromSlice(messages).Len(); got != len(messages) {
					b.Fatalf("Len = %d", got)
				}
			}
		})
	}
}
