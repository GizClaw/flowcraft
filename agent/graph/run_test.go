// Execution: the frontier loop, suspension, limits and streaming.
package graph

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestRunLinear(t *testing.T) {
	g := build(t, `
version: 1
entry: answer
state:
  text: {type: text}
nodes:
  - {id: answer, kind: const, config: {key: text, value: hello}}
edges:
  - {from: answer, to: end}
output: {key: text}
`)
	events, err := drain(t, g)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := textOf(events); got != "hello" {
		t.Errorf("output = %q, want hello", got)
	}
}

// Two unconditional edges from one node run both targets in the same step: fan-out
// needs no fork node.
func TestUnconditionalEdgesFanOut(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: start
limits: {concurrency: 2}
state:
  a: {type: text}
  b: {type: text}
nodes:
  - {id: start, kind: const, config: {key: a, value: x}}
  - {id: left, kind: count}
  - {id: right, kind: count}
edges:
  - {from: start, to: left}
  - {from: start, to: right}
  - {from: left, to: end}
  - {from: right, to: end}
output: {key: a}
`, counter)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
	if counter.count("left") != 1 || counter.count("right") != 1 {
		t.Fatalf("calls = %d/%d, want one each", counter.count("left"), counter.count("right"))
	}
}

// A node reached by two branches in one step runs once: join needs no join node.
func TestJoinedNodeRunsOnce(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: start
state:
  a: {type: text}
nodes:
  - {id: start, kind: const, config: {key: a, value: x}}
  - {id: left, kind: count}
  - {id: right, kind: count}
  - {id: join, kind: count}
edges:
  - {from: start, to: left}
  - {from: start, to: right}
  - {from: left, to: join}
  - {from: right, to: join}
  - {from: join, to: end}
output: {key: a}
`, counter)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := counter.count("join"); got != 1 {
		t.Fatalf("join ran %d times, want 1", got)
	}
}

func TestConditionalEdgesRoute(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: start
state:
  route: {type: text}
nodes:
  - {id: start, kind: const, config: {key: route, value: left}}
  - {id: left, kind: count}
  - {id: right, kind: count}
edges:
  - {from: start, to: left, when: 'state.route == "left"'}
  - {from: start, to: right, when: 'state.route == "right"'}
  - {from: left, to: end}
  - {from: right, to: end}
output: {key: route}
`, counter)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
	if counter.count("left") != 1 || counter.count("right") != 0 {
		t.Fatalf("calls = %d/%d, want left only", counter.count("left"), counter.count("right"))
	}
}

// A cycle is allowed; max_steps is what stops it.
func TestCycleBoundedByMaxSteps(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: loop
limits: {max_steps: 3}
nodes:
  - {id: loop, kind: count}
edges:
  - {from: loop, to: loop}
`, counter)
	_, err := drain(t, g)
	if err == nil || !strings.Contains(err.Error(), "max_steps") {
		t.Fatalf("run = %v, want a max_steps error", err)
	}
	if got := counter.count("loop"); got != 3 {
		t.Errorf("loop ran %d times, want 3", got)
	}
}

// A skipped node does not run, but its outgoing edge still fires.
func TestSkipRoutesWithoutRunning(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: gate
state:
  done: {type: bool, merge: last}
nodes:
  - {id: gate, kind: count, skip: 'true'}
  - {id: next, kind: count}
edges:
  - {from: gate, to: next}
  - {from: next, to: end}
`, counter)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := counter.count("gate"); got != 0 {
		t.Errorf("skipped node ran %d times", got)
	}
	if got := counter.count("next"); got != 1 {
		t.Errorf("next ran %d times, want 1", got)
	}
}

func TestStateMergeSingleRejectsASecondWriter(t *testing.T) {
	g := build(t, `
version: 1
entry: a
state:
  flag: {type: text}
nodes:
  - {id: a, kind: const, config: {key: flag, value: first}}
  - {id: b, kind: const, config: {key: flag, value: second}}
edges:
  - {from: a, to: b}
  - {from: b, to: end}
`)
	// Sequential writes to a single key are fine; two writers in one step are not.
	if _, err := drain(t, g); err != nil {
		t.Fatalf("sequential writes: %v", err)
	}

	g = build(t, `
version: 1
entry: start
state:
  flag: {type: text}
  seed: {type: text}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: a, kind: const, config: {key: flag, value: first}}
  - {id: b, kind: const, config: {key: flag, value: second}}
edges:
  - {from: start, to: a}
  - {from: start, to: b}
  - {from: a, to: end}
  - {from: b, to: end}
output: {key: seed}
`)
	if _, err := drain(t, g); err == nil {
		t.Fatal("two writers in one step = nil, want an error")
	}
}

func TestStateMergeAppendConcatenates(t *testing.T) {
	decl := map[string]StateKey{"trace": {Kind: KindMessages, Merge: MergeAppend}}
	store := newStateStore(decl)
	updates := []StateUpdate{
		{Key: "trace", Value: MessagesValue(MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "a"),
		}))},
		{Key: "trace", Value: MessagesValue(MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "b"),
		}))},
	}
	if err := store.apply(updates, decl); err != nil {
		t.Fatalf("apply: %v", err)
	}
	merged, err := store.values["trace"].Messages()
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if merged.Len() != 2 {
		t.Fatalf("len = %d, want 2", merged.Len())
	}
	if got := merged.Slice()[1].Content.Text(); got != "b" {
		t.Errorf("second = %q, want b", got)
	}
}

// A media answer becomes an artifact whose bytes live in the blob store.
func TestMediaOutputBecomesAnArtifact(t *testing.T) {
	blobs := &memBlobs{}
	doc, err := Parse([]byte(`
version: 1
entry: img
state:
  image: {type: image}
nodes:
  - {id: img, kind: emit, config: {key: image, media_type: image/png, value: png-bytes}}
edges:
  - {from: img, to: end}
output: {key: image}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	g, err := Build(doc, registry(t), WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var artifact agent.Artifact
	var sawText bool
	run, startErr := g.Start(context.Background(), nil)
	if startErr != nil {
		t.Fatalf("start: %v", startErr)
	}
	for event, err := range run.Events(context.Background()) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		switch typed := event.(type) {
		case agent.Text:
			sawText = true
		case agent.ArtifactWritten:
			artifact = typed.Artifact
		}
	}
	if sawText {
		t.Error("a media output emitted Text")
	}
	if artifact.MediaType != "image/png" {
		t.Fatalf("artifact = %+v, want image/png", artifact)
	}
	if got := string(blobs.data[artifact.Blob]); got != "png-bytes" {
		t.Errorf("stored blob = %q, want png-bytes", got)
	}
}

// The store requirement is a run-time fact: the build cannot know what a node
// will produce.
func TestMediaOutputWithoutAStoreFailsAtRun(t *testing.T) {
	g := build(t, `
version: 1
entry: img
state:
  image: {type: image}
nodes:
  - {id: img, kind: emit, config: {key: image, media_type: image/png, value: x}}
edges:
  - {from: img, to: end}
output: {key: image}
`)
	if _, err := drain(t, g); err == nil || !strings.Contains(err.Error(), "blob store") {
		t.Fatalf("run = %v, want a blob store error", err)
	}
}

// max_steps: 0 is no cap: the loop ends because its own condition says so.
func TestMaxStepsZeroIsUnlimited(t *testing.T) {
	g := build(t, `
version: 1
entry: tick
limits: {max_steps: 0}
state:
  done: {type: bool, merge: last}
nodes:
  - {id: tick, kind: tick, config: {stop_after: 4}}
edges:
  - {from: tick, to: tick, when: '!state.done'}
  - {from: tick, to: end, when: 'state.done'}
`, tickKind{})
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// max_nodes bounds routed nodes, not steps: a loop with no step cap still stops.
func TestMaxNodesBoundsRouting(t *testing.T) {
	g := build(t, `
version: 1
entry: tick
limits: {max_steps: 0, max_nodes: 3}
state:
  done: {type: bool, merge: last}
nodes:
  - {id: tick, kind: tick, config: {stop_after: 100}}
edges:
  - {from: tick, to: tick, when: '!state.done'}
`, tickKind{})
	_, err := drain(t, g)
	if err == nil || !strings.Contains(err.Error(), "max_nodes") {
		t.Fatalf("run = %v, want a max_nodes error", err)
	}
}

func TestRunTimeout(t *testing.T) {
	g := build(t, `
version: 1
entry: slow
limits: {timeout: 30ms}
nodes:
  - {id: slow, kind: sleep, config: {d: 2s}}
`, sleepKind{})
	start := time.Now()
	_, err := drain(t, g)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run = %v, want a run deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("run took %s, want it bounded by limits.timeout", elapsed)
	}
}

func TestNodeTimeout(t *testing.T) {
	g := build(t, `
version: 1
entry: slow
nodes:
  - {id: slow, kind: sleep, config: {d: 2s}, timeout: 30ms}
`, sleepKind{})
	start := time.Now()
	_, err := drain(t, g)
	if err == nil || !strings.Contains(err.Error(), "slow") {
		t.Fatalf("run = %v, want a node error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("run took %s, want it bounded by the node timeout", elapsed)
	}
}

func TestNodeRetries(t *testing.T) {
	flaky := &flakyKind{failures: 2}
	g := build(t, `
version: 1
entry: work
nodes:
  - {id: work, kind: flaky, retries: 2}
`, flaky)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run with retries: %v", err)
	}
	if flaky.calls != 3 {
		t.Errorf("calls = %d, want three attempts", flaky.calls)
	}

	flaky = &flakyKind{failures: 2}
	g = build(t, `
version: 1
entry: work
nodes:
  - {id: work, kind: flaky}
`, flaky)
	if _, err := drain(t, g); err == nil {
		t.Fatal("run without retries = nil, want a failure")
	}
	if flaky.calls != 1 {
		t.Errorf("calls = %d, want one attempt", flaky.calls)
	}
}

func TestConcurrencyBound(t *testing.T) {
	parallel := &parallelKind{}
	g := build(t, `
version: 1
entry: start
limits: {concurrency: 2}
state:
  seed: {type: text}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: a, kind: parallel}
  - {id: b, kind: parallel}
  - {id: c, kind: parallel}
  - {id: d, kind: parallel}
edges:
  - {from: start, to: a}
  - {from: start, to: b}
  - {from: start, to: c}
  - {from: start, to: d}
  - {from: a, to: end}
  - {from: b, to: end}
  - {from: c, to: end}
  - {from: d, to: end}
output: {key: seed}
`, parallel)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
	parallel.mu.Lock()
	defer parallel.mu.Unlock()
	if parallel.max > 2 {
		t.Errorf("max concurrent = %d, want at most 2", parallel.max)
	}
}

// A node may ask a question; the run stops at the step boundary and Answer
// continues the same run with the node re-entered.
func TestSuspendAndResume(t *testing.T) {
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

	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.State() != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", run.State())
	}
	// The suspending node's own writes are discarded: it will produce them again.
	if got, _ := run.Value("text").Text(); got != "" {
		t.Errorf("state text = %q after suspension, want the draft discarded", got)
	}

	var text string
	for event, err := range answer(context.Background(), run, "forty-two") {
		if err != nil {
			t.Fatalf("answer: %v", err)
		}
		if typed, ok := event.(agent.Text); ok {
			text = typed.Text
		}
	}
	if text != "forty-two" {
		t.Errorf("output = %q, want forty-two", text)
	}
	if run.State() != agent.StateCompleted {
		t.Errorf("state = %s, want completed", run.State())
	}
}

// Two questions in one step have no single answer, so the run fails instead of
// asking the caller to guess.
func TestTwoSuspendsInOneStepFail(t *testing.T) {
	g := build(t, `
version: 1
entry: start
limits: {concurrency: 2}
state:
  a: {type: text}
  b: {type: text}
  seed: {type: text}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: left, kind: ask, config: {key: a}}
  - {id: right, kind: ask, config: {key: b}}
edges:
  - {from: start, to: left}
  - {from: start, to: right}
output: {key: seed}
`, askKind{})
	_, err := drain(t, g)
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("run = %v, want a single-suspend error", err)
	}
}

func TestAnswerWithoutSuspensionFails(t *testing.T) {
	g := build(t, `
version: 1
entry: a
state:
  text: {type: text}
nodes:
  - {id: a, kind: const, config: {key: text, value: x}}
edges:
  - {from: a, to: end}
output: {key: text}
`)
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, err := range answer(context.Background(), run, "x") {
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "not waiting for input") {
			t.Fatalf("answer error = %v", err)
		}
		return
	}
	t.Fatal("Answer on a completed run = nil, want an error")
}

func (*textStream) Result() (Result, error) { return Result{}, nil }

func (*textStream) Close() error { return nil }

// A sibling of a suspended node keeps its routing: the edges of the frontier
// that finished before the suspension still fire after the resume.
func TestSiblingRoutingSurvivesSuspend(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: start
limits: {concurrency: 2}
state:
  seed: {type: text}
  answer: {type: text}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: side, kind: count}
  - {id: ask, kind: ask, config: {key: answer}}
  - {id: after, kind: count}
edges:
  - {from: start, to: side}
  - {from: start, to: ask}
  - {from: side, to: after}
  - {from: ask, to: after}
  - {from: after, to: end}
output: {key: seed}
`, counter, askKind{})

	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range run.Events(context.Background()) {
	}
	if run.State() != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", run.State())
	}
	if counter.count("side") != 1 || counter.count("after") != 0 {
		t.Fatalf("before resume: side=%d after=%d", counter.count("side"), counter.count("after"))
	}

	for event, err := range answer(context.Background(), run, "done") {
		if err != nil {
			t.Fatalf("answer: %v", err)
		}
		_ = event
	}
	if counter.count("side") != 1 {
		t.Errorf("side re-ran: %d calls", counter.count("side"))
	}
	if counter.count("after") != 1 {
		t.Errorf("after ran %d times, want the sibling's routing to fire", counter.count("after"))
	}
}

// A terminal run is read-only: a second Events call must not re-run the frontier.
func TestEventsAfterFailureIsRefused(t *testing.T) {
	flaky := &flakyKind{failures: 1}
	g := build(t, `
version: 1
entry: work
nodes:
  - {id: work, kind: flaky}
`, flaky)
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err == nil {
		t.Fatal("run = nil, want a failure")
	}
	if run.State() != agent.StateFailed {
		t.Fatalf("state = %s, want failed", run.State())
	}
	calls := flaky.calls
	events, err := drainRun(t, run)
	if err != nil {
		t.Fatalf("second Events: %v", err)
	}
	if flaky.calls != calls {
		t.Errorf("node re-ran on a terminal run: %d -> %d", calls, flaky.calls)
	}
	last, ok := events[len(events)-1].(agent.Status)
	if !ok || last.State != agent.StateFailed {
		t.Fatalf("second Events ended with %+v, want the failed status", events[len(events)-1])
	}
}

// A caller that stops reading ends the run; the step is not replayed later.
func TestConsumerStopCancelsTheRun(t *testing.T) {
	stream := &streamKind{texts: []string{"one", "two", "three"}}
	g := build(t, `
version: 1
entry: talk
nodes:
  - {id: talk, kind: stream}
edges:
  - {from: talk, to: end}
`, stream)
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for event := range run.Events(context.Background()) {
		// Stop on the first answer delta, so the node has really started.
		if _, ok := event.(agent.Text); ok {
			break
		}
	}
	if run.State() != agent.StateCanceled {
		t.Fatalf("state = %s after the consumer stopped, want canceled", run.State())
	}
	// A second read must not re-run the node.
	for range run.Events(context.Background()) {
	}
	if stream.count() != 1 {
		t.Errorf("node ran %d times after a consumer stop, want 1", stream.count())
	}
}

// The document names the state key the run input is seeded into.
func TestInputSlotSeedsMessages(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: a
input: messages
state:
  messages: {type: messages}
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`, counter)
	run, err := g.Start(context.Background(), []message.Message{
		message.NewTextMessage(message.RoleSystem, "be brief"),
		message.NewTextMessage(message.RoleUser, "hello"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	seeded, err := run.Value("messages").Messages()
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if seeded.Len() != 2 {
		t.Fatalf("seeded = %d messages, want the system turn plus the user turn", seeded.Len())
	}
	if role := seeded.Slice()[0].Role; role != message.RoleSystem {
		t.Errorf("first seeded role = %q, want system", role)
	}
}

// A graph without an input slot refuses a caller that supplies messages.
func TestStartRejectsInputWithoutASlot(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`, counter)
	if _, err := g.Start(context.Background(), []message.Message{
		message.NewTextMessage(message.RoleUser, "hello"),
	}); err == nil {
		t.Fatal("Start accepted messages for a graph with no input slot")
	}
}

// The snapshot is a read-only view: keys can be read, not written.
func TestSnapshotExposesTheState(t *testing.T) {
	snapshot := NewSnapshot(map[string]Value{
		"route": Text("left"),
		"count": JSON([]byte("2")),
	})
	if got, _ := snapshot.Get("route").Text(); got != "left" {
		t.Errorf("route = %q", got)
	}
	if snapshot.Len() != 2 {
		t.Errorf("len = %d, want 2", snapshot.Len())
	}
	seen := 0
	snapshot.Range(func(string, Value) bool { seen++; return true })
	if seen != 2 {
		t.Errorf("Range visited %d entries, want 2", seen)
	}
}

func (*statusStream) Result() (Result, error) { return Result{}, nil }

func (*statusStream) Close() error { return nil }

// A node may not report a lifecycle event: the run owns that vocabulary.
func TestNodeCannotEmitEngineEvents(t *testing.T) {
	g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: status}
edges:
  - {from: a, to: end}
`, emitStatusKind{})
	_, err := drain(t, g)
	if err == nil || !strings.Contains(err.Error(), "which the engine owns") {
		t.Fatalf("run = %v, want an engine-owns error", err)
	}
}

// ---------- batch 4: contract coverage ----------

func (*recordingStream) Result() (Result, error) { return Result{}, nil }

// A node's deltas reach the caller in order, and its stream is closed.
func TestNodeStreamForwardsEventsInOrder(t *testing.T) {
	var closed atomic.Bool
	g := build(t, `
version: 1
entry: talk
nodes:
  - {id: talk, kind: recording}
edges:
  - {from: talk, to: end}
`, recordingKind{texts: []string{"one", "two", "three"}, closed: &closed})
	var texts []string
	run, startErr := g.Start(context.Background(), nil)
	if startErr != nil {
		t.Fatalf("start: %v", startErr)
	}
	for event, err := range run.Events(context.Background()) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if typed, ok := event.(agent.Text); ok {
			texts = append(texts, typed.Text)
		}
	}
	if strings.Join(texts, ",") != "one,two,three" {
		t.Errorf("texts = %v", texts)
	}
	if !closed.Load() {
		t.Error("the node's stream was not closed")
	}
}

// merge: last applies writes in declaration order, not completion order.
func TestMergeLastUsesDeclarationOrder(t *testing.T) {
	release := make(chan struct{})
	first := kindFunc{name: "first", run: func(context.Context, Invoke) (Result, error) {
		<-release // the second-declared writer finishes first
		return Result{State: []StateUpdate{{Key: "flag", Value: Text("first")}}}, nil
	}}
	second := kindFunc{name: "second", run: func(context.Context, Invoke) (Result, error) {
		close(release)
		return Result{State: []StateUpdate{{Key: "flag", Value: Text("second")}}}, nil
	}}
	g := build(t, `
version: 1
entry: start
limits: {concurrency: 2}
state:
  seed: {type: text}
  flag: {type: text, merge: last}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: a, kind: first}
  - {id: b, kind: second}
edges:
  - {from: start, to: a}
  - {from: start, to: b}
  - {from: a, to: end}
  - {from: b, to: end}
output: {key: flag}
`, first, second)
	events, err := drain(t, g)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := textOf(events); got != "second" {
		t.Errorf("flag = %q, want the later declaration", got)
	}
}

// A skipped node consumes routing budget, so an all-skipped cycle terminates.
func TestSkipCycleIsBoundedByMaxNodes(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: gate
limits: {max_steps: 0, max_nodes: 3}
state:
  done: {type: bool, merge: last}
nodes:
  - {id: gate, kind: count, skip: '!state.done'}
edges:
  - {from: gate, to: gate, when: '!state.done'}
`, counter)
	_, err := drain(t, g)
	if err == nil || !strings.Contains(err.Error(), "max_nodes") {
		t.Fatalf("run = %v, want a max_nodes error", err)
	}
	if counter.count("gate") != 0 {
		t.Errorf("the skipped node ran %d times", counter.count("gate"))
	}
}

// A false skip condition runs the node normally.
func TestSkipFalseRunsTheNode(t *testing.T) {
	counter := &countKind{}
	g := build(t, `
version: 1
entry: gate
state:
  done: {type: bool, merge: last}
nodes:
  - {id: gate, kind: count, skip: 'state.done'}
edges:
  - {from: gate, to: end}
`, counter)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
	if counter.count("gate") != 1 {
		t.Errorf("gate ran %d times, want 1", counter.count("gate"))
	}
}

func (*failAfterTextStream) Result() (Result, error) { return Result{}, nil }

func (*failAfterTextStream) Close() error { return nil }

// A failure after the first delta is final: retrying would repeat delivered text.
func TestRetryIsSkippedAfterEvents(t *testing.T) {
	flaky := &failAfterTextKind{}
	g := build(t, `
version: 1
entry: work
nodes:
  - {id: work, kind: fail-after-text, retries: 2}
edges:
  - {from: work, to: end}
`, flaky)
	events, err := drain(t, g)
	if err == nil {
		t.Fatal("run = nil, want a failure")
	}
	if flaky.count() != 1 {
		t.Errorf("attempts = %d, want 1 (a retry would duplicate text)", flaky.count())
	}
	if got := textOf(events); got != "partial" {
		t.Errorf("text = %q, want the delivered delta once", got)
	}
}

func TestNonRetryableFailureIsNotRetried(t *testing.T) {
	calls := 0
	bad := kindFunc{name: "bad", run: func(context.Context, Invoke) (Result, error) {
		calls++
		return Result{}, errdefs.Validationf("bad config")
	}}
	g := build(t, `
version: 1
entry: work
nodes:
  - {id: work, kind: bad, retries: 2}
edges:
  - {from: work, to: end}
`, bad)
	if _, err := drain(t, g); err == nil {
		t.Fatal("run = nil, want a failure")
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want 1 for a validation failure", calls)
	}
}

func TestSuspendTwiceInSequence(t *testing.T) {
	g := build(t, `
version: 1
entry: first
state:
  a: {type: text}
  b: {type: text}
nodes:
  - {id: first, kind: ask, config: {key: a}}
  - {id: second, kind: ask, config: {key: b}}
edges:
  - {from: first, to: second}
  - {from: second, to: end}
output: {key: b}
`, askKind{})
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.State() != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", run.State())
	}
	for range answer(context.Background(), run, "one") {
	}
	if run.State() != agent.StateInputRequired {
		t.Fatalf("state = %s after the first answer, want the second question", run.State())
	}
	var text string
	for event, err := range answer(context.Background(), run, "two") {
		if err != nil {
			t.Fatalf("second answer: %v", err)
		}
		if typed, ok := event.(agent.Text); ok {
			text = typed.Text
		}
	}
	if text != "two" {
		t.Errorf("output = %q, want two", text)
	}
	if got, _ := run.Value("a").Text(); got != "one" {
		t.Errorf("a = %q, want the first answer", got)
	}
}

// A sibling's writes commit even when another node suspends.
func TestSiblingWritesCommitWhenOneNodeSuspends(t *testing.T) {
	g := build(t, `
version: 1
entry: start
limits: {concurrency: 2}
state:
  seed: {type: text}
  answer: {type: text}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: side, kind: const, config: {key: seed, value: committed}}
  - {id: ask, kind: ask, config: {key: answer}}
edges:
  - {from: start, to: side}
  - {from: start, to: ask}
  - {from: side, to: end}
  - {from: ask, to: end}
`, askKind{})
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.State() != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", run.State())
	}
	// The sibling's write landed; the suspended node's draft did not.
	if got, _ := run.Value("seed").Text(); got != "committed" {
		t.Errorf("seed = %q, want the sibling's write", got)
	}
	if got, _ := run.Value("answer").Text(); got != "" {
		t.Errorf("answer = %q, want the draft discarded", got)
	}
}

// An answer that is already a blob reference needs no store at publish time.
func TestImageRefOutputSkipsTheStore(t *testing.T) {
	g := build(t, `
version: 1
entry: img
state:
  image: {type: image}
nodes:
  - {id: img, kind: ref, config: {key: image}}
edges:
  - {from: img, to: end}
output: {key: image}
`, refKind{})
	var artifact agent.Artifact
	run, startErr := g.Start(context.Background(), nil)
	if startErr != nil {
		t.Fatalf("start: %v", startErr)
	}
	for event, err := range run.Events(context.Background()) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if typed, ok := event.(agent.ArtifactWritten); ok {
			artifact = typed.Artifact
		}
	}
	if artifact.Blob != "blob-7" || artifact.MediaType != "image/png" {
		t.Fatalf("artifact = %+v, want the reference", artifact)
	}
}

// A store that cannot accept the answer fails the run instead of dropping it.
func TestBlobStoreFailureFailsTheRun(t *testing.T) {
	doc, err := Parse([]byte(`
version: 1
entry: img
state:
  image: {type: image}
nodes:
  - {id: img, kind: emit, config: {key: image, media_type: image/png, value: x}}
edges:
  - {from: img, to: end}
output: {key: image}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	g, err := Build(doc, registry(t), WithBlobStore(failingBlobs{}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := drain(t, g); err == nil || !strings.Contains(err.Error(), "store output") {
		t.Fatalf("run = %v, want a store error", err)
	}
}

// A session output reports the newest message's text.
func TestMessagesOutputReportsTheNewestText(t *testing.T) {
	g := build(t, `
version: 1
entry: talk
input: messages
state:
  messages: {type: messages}
nodes:
  - {id: talk, kind: append, config: {key: messages, text: latest}}
edges:
  - {from: talk, to: end}
output: {key: messages}
`, appendKind{})
	events, err := drain(t, g, message.NewTextMessage(message.RoleUser, "old"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := textOf(events); got != "latest" {
		t.Errorf("output = %q, want the newest message", got)
	}
}

// A structured state value cannot be a run answer.
func TestJSONOutputCannotBeReported(t *testing.T) {
	g := build(t, `
version: 1
entry: meta
state:
  meta: {type: json}
nodes:
  - {id: meta, kind: meta}
edges:
  - {from: meta, to: end}
output: {key: meta}
`, kindFunc{name: "meta", run: func(context.Context, Invoke) (Result, error) {
		return Result{State: []StateUpdate{{Key: "meta", Value: JSON([]byte(`{"a":1}`))}}}, nil
	}})
	if _, err := drain(t, g); err == nil || !strings.Contains(err.Error(), "cannot be reported") {
		t.Fatalf("run = %v, want a cannot-be-reported error", err)
	}
}

// The concurrency limit is exact: the third node does not start while two hold
// their slots.
func TestConcurrencyBoundIsExact(t *testing.T) {
	barrier := &barrierKind{started: make(chan struct{}, 8), release: make(chan struct{})}
	g := build(t, `
version: 1
entry: start
limits: {concurrency: 2}
state:
  seed: {type: text}
nodes:
  - {id: start, kind: const, config: {key: seed, value: x}}
  - {id: a, kind: barrier}
  - {id: b, kind: barrier}
  - {id: c, kind: barrier}
edges:
  - {from: start, to: a}
  - {from: start, to: b}
  - {from: start, to: c}
  - {from: a, to: end}
  - {from: b, to: end}
  - {from: c, to: end}
output: {key: seed}
`, barrier)

	done := make(chan error, 1)
	go func() {
		run, err := g.Start(context.Background(), nil)
		if err != nil {
			done <- err
			return
		}
		for _, err := range run.Events(context.Background()) {
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-barrier.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d nodes started", i)
		}
	}
	select {
	case <-barrier.started:
		t.Fatal("a third node started while the limit is 2")
	case <-time.After(100 * time.Millisecond):
	}
	close(barrier.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not finish after the release")
	}
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if barrier.max > 2 {
		t.Errorf("max concurrency = %d, want at most 2", barrier.max)
	}
}

// An undeclared state key fails the step, naming the key.
func TestUndeclaredStateKeyFailsTheStep(t *testing.T) {
	g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: writer}
edges:
  - {from: a, to: end}
`, kindFunc{name: "writer", run: func(context.Context, Invoke) (Result, error) {
		return Result{State: []StateUpdate{{Key: "absent", Value: Text("x")}}}, nil
	}})
	if _, err := drain(t, g); err == nil || !strings.Contains(err.Error(), "is not declared") {
		t.Fatalf("run = %v, want an undeclared-key error", err)
	}
}

// A value whose kind does not match the declaration fails the step.
func TestWrongKindWriteFailsTheStep(t *testing.T) {
	g := build(t, `
version: 1
entry: a
state:
  meta: {type: json}
nodes:
  - {id: a, kind: writer}
edges:
  - {from: a, to: end}
`, kindFunc{name: "writer", run: func(context.Context, Invoke) (Result, error) {
		return Result{State: []StateUpdate{{Key: "meta", Value: Text("x")}}}, nil
	}})
	if _, err := drain(t, g); err == nil || !strings.Contains(err.Error(), "want json") {
		t.Fatalf("run = %v, want a kind-mismatch error", err)
	}
}

// merge: append concatenates in declaration order across steps.
func TestAppendMergesAcrossSteps(t *testing.T) {
	g := build(t, `
version: 1
entry: a
state:
  trace: {type: messages, merge: append}
nodes:
  - {id: a, kind: append, config: {key: trace, text: a, delta: true}}
  - {id: b, kind: append, config: {key: trace, text: b, delta: true}}
edges:
  - {from: a, to: b}
  - {from: b, to: end}
`, appendKind{})
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	trace, err := run.Value("trace").Messages()
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if trace.Len() != 2 {
		t.Fatalf("trace = %d messages, want 2", trace.Len())
	}
	turns := trace.Slice()
	if turns[0].Content.Text() != "a" || turns[1].Content.Text() != "b" {
		t.Errorf("trace order = %q, %q", turns[0].Content.Text(), turns[1].Content.Text())
	}
}

// A panicking node fails the run instead of the process.
func TestNodePanicFailsTheRun(t *testing.T) {
	g := build(t, `
version: 1
entry: boom
nodes:
  - {id: boom, kind: boom}
edges:
  - {from: boom, to: end}
`, kindFunc{name: "boom", run: func(context.Context, Invoke) (Result, error) {
		panic("boom")
	}})
	if _, err := drain(t, g); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("run = %v, want a panic report", err)
	}
}
