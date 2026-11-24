package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Run is one execution of a graph: its state, its frontier and its step counter.
//
// A graph is built once and serves every session, so everything that differs
// between executions lives here. A run may suspend — a node asks the caller a
// question — and Answer continues the same Run, which is what a protocol adapter
// needs to expose: Run starts or continues, Resume answers.
//
// A Run is driven by one goroutine. Events, Answer, State and Value are not safe
// for concurrent use; two calls that both advance the run are a race, and Value
// reads the state map without a lock.
type Run struct {
	g *Graph
	// values is the state; store is where it is checkpointed, if anywhere.
	values      *stateStore
	store       agent.Store
	taskID      string
	sessionID   string
	fingerprint agent.Fingerprint
	needBase    bool
	// namedBase is the base the projection names; pendingBase is one this run
	// wrote and has not confirmed in a projection yet.
	namedBase        *baseRef
	pendingBase      *baseRef
	compactedThrough int64
	active           []string
	// pending keeps the frontier of the step that suspended. When the suspended
	// node resumes, its siblings have already run: their outgoing edges must
	// still fire, so the frontier is the union of both.
	pending []string
	// startedAt is when the run began, so a restored run keeps the budget it
	// already spent instead of receiving a fresh one.
	startedAt time.Time
	deadline  time.Time
	step      int
	// stepMarker is the frontier record the next save writes first: it names the
	// step every fact after it belongs to.
	stepMarker *agent.Record
	routed     int
	state      agent.TaskState
	// answer is the reply to a suspended question; it is handed to the nodes of
	// the resumed step only.
	answer message.Content
	// question is what the suspended node asked. It is reported by Result while
	// the run waits for input.
	question message.Content
	// streamed is the text the last model round reported as deltas. The output
	// publish uses it to avoid reporting the same answer a second time: a
	// caller that folds Text events would otherwise see it twice.
	streamed strings.Builder
	// manifest is the version combination the run executed under; the first
	// save writes it into the checkpoint.
	manifest        agent.Manifest
	manifestWritten bool
	// trace is how much of the run is traced: spans, and the per-call evidence
	// the tier carries.
	trace agent.TraceLevel
	// bodies keeps each recorded model response's bytes, which is what a replay
	// answers from.
	bodies bool
	// calls indexes the model calls this run recorded, by request hash. Replay
	// answers from it; a live run only grows it.
	calls map[string]agent.LLMCallRecord
	// records indexes every other durable fact by kind. A node that replays reads
	// its own kind; the model-call index above stays separate because replay of a
	// model call also has to reload a response blob.
	records map[string][]agent.Record
	// replay answers from calls instead of calling out.
	replay bool
	// The session's tool surface, carried by the turn: nothing here is read from ambient
	// state, so two runs cannot see each other's authority.
	binding      tool.Binding
	tools        tool.Dispatcher
	models       *inference.Provider
	defaultModel model.ModelRef
	// stateMu guards the lifecycle: it is read by a caller that steers and written by the run.
	stateMu sync.RWMutex
	// steerMu guards the steer a caller queued for the next boundary.
	steerMu sync.Mutex
	steer   *steerRequest
	// persistMu serializes saves: a node may checkpoint a side effect while
	// the loop is between steps.
	persistMu sync.Mutex
}

// steerRequest is one message waiting for the next frontier. It carries the handle the caller holds,
// so a drop at the end of the run can name the input it discarded.
type steerRequest struct {
	id      agent.SteerID
	message message.Message
	kind    agent.SteerIntent
}

// Start begins a run. The caller then drains Events, or calls Answer if it
// suspends.
//
// The context bounds the store work a durable start does — restoring the
// previous run and writing the seeded input — not the run itself, which Events
// drives.
//
// The run's input goes into the state key the document named, and nowhere else:
// a graph without an input slot refuses a caller that supplies messages, instead
// of dropping them.
func (g *Graph) Start(ctx context.Context, messages []message.Message, opts ...RunOption) (*Run, error) {
	var settings runOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	if run, found, err := g.restore(ctx, settings); err != nil {
		return nil, err
	} else if found {
		return run, nil
	}
	store := newStateStore(g.state)
	// A session continues itself: the keys the document marks `carry` start from the
	// previous task's values instead of the zero value.
	if err := g.carryInto(ctx, settings, store); err != nil {
		return nil, err
	}
	seeded := false
	switch {
	case g.input != "" && len(messages) > 0:
		if existing, carried := store.values[g.input]; carried && g.state[g.input].Merge == MergeAppend {
			// A carried transcript continues: the turn is appended, not written over.
			history, err := existing.Messages()
			if err != nil {
				return nil, err
			}
			store.values[g.input] = MessagesValue(history.Concat(MessagesFromSlice(messages)))
		} else {
			store.values[g.input] = MessagesValue(MessagesFromSlice(messages))
		}
		seeded = true
	case g.input == "" && len(messages) > 0:
		return nil, fmt.Errorf(
			"graph: the document declares no input slot; add `input: <state key>` or send no messages")
	}
	run := &Run{
		g:            g,
		values:       store,
		store:        settings.store,
		taskID:       settings.taskID,
		sessionID:    settings.sessionID,
		fingerprint:  settings.fingerprint,
		manifest:     settings.manifest,
		trace:        settings.trace,
		bodies:       settings.bodies,
		replay:       settings.replay,
		binding:      settings.binding,
		tools:        settings.tools,
		models:       settings.models,
		defaultModel: settings.defaultModel,
		active:       []string{g.entry},
		state:        agent.StateSubmitted,
	}
	run.startedAt = time.Now()
	if g.timeout > 0 {
		run.deadline = run.startedAt.Add(g.timeout)
	}
	// A durable run exists as soon as it starts: the seeded input is a step-zero
	// write, so a restart finds it in the log instead of losing it.
	if seeded && run.store != nil {
		_ = run.persist(ctx, []StateUpdate{{
			Key:   g.input,
			Value: store.values[g.input],
		}}, nil)
	}
	return run, nil
}

// Run starts a graph and streams it to completion. A run that suspends cannot
// be answered from here; use Start with Answer, or NewRunner, which drives both.
func (g *Graph) Run(ctx context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	run, err := g.Start(ctx, []message.Message{msg})
	if err != nil {
		return func(yield func(agent.Event, error) bool) { yield(nil, err) }
	}
	return run.Events(ctx)
}

// State is the run's lifecycle: submitted, working, input-required, completed,
// failed or canceled.
func (r *Run) State() agent.TaskState {
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	return r.state
}

// setState records the lifecycle. It takes the write lock because a steer arrives on the caller's
// goroutine and questions the state while the run's own goroutine advances it.
func (r *Run) setState(state agent.TaskState) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.state = state
}

// Value reads one state key. It exists for a host that wants the result without
// declaring an output event, and for tests.
func (r *Run) Value(key string) Value { return r.values.values[key] }

// Events streams the run until it finishes or suspends.
func (r *Run) Events(ctx context.Context) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) { r.advance(ctx, yield) }
}

// Answer continues a suspended run with the caller's reply. It is the second
// half of the single entry a protocol adapter needs: the message that arrives
// while the task waits for input is the answer, not a new request.
func (r *Run) Answer(ctx context.Context, content message.Content) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if r.state != agent.StateInputRequired {
			yield(nil, fmt.Errorf("graph: run is %s, not waiting for input", r.state))
			return
		}
		r.answer = content
		r.setState(agent.StateWorking)
		r.advance(ctx, yield)
	}
}

// advance drives the frontier loop.
func (r *Run) advance(ctx context.Context, yield func(agent.Event, error) bool) {
	defer r.reportDropped(yield)
	switch r.state {
	case agent.StateInputRequired:
		yield(nil, fmt.Errorf("graph: run is waiting for input; call Answer"))
		return
	}
	// A terminal run is read-only: re-entering it must not re-run the frontier
	// that failed or was canceled.
	if r.state.Terminal() {
		yield(agent.Status{State: r.state}, nil)
		return
	}
	if !r.deadline.IsZero() {
		if time.Now().After(r.deadline) {
			r.setState(agent.StateCanceled)
			yield(agent.Status{State: agent.StateCanceled, Reason: "run deadline exceeded"}, nil)
			yield(nil, context.DeadlineExceeded)
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, r.deadline)
		defer cancel()
	}
	r.setState(agent.StateWorking)
	if !r.emit(yield, agent.Status{State: agent.StateWorking}) {
		return
	}

	for len(r.active) > 0 {
		if err := ctx.Err(); err != nil {
			r.setState(agent.StateCanceled)
			_ = r.persist(ctx, nil, nil)
			yield(agent.Status{State: agent.StateCanceled, Reason: err.Error()}, nil)
			yield(nil, err)
			return
		}
		if max := r.g.maxSteps; max > 0 && r.step >= max {
			r.fail(ctx, yield, errdefs.BudgetExceededf("graph: exceeded limits.max_steps %d", max))
			return
		}
		outcome, err := r.frontier(ctx, yield)
		switch outcome {
		case frontierStopped:
			return
		case frontierFailed:
			r.fail(ctx, yield, err)
			return
		case frontierSuspended:
			return
		}
	}

	if r.g.output != "" {
		if !r.publish(ctx, r.values.values[r.g.output], yield) {
			// The caller stopped while the answer was being delivered. The work
			// is done, so the run is complete; only the delivery was cut short.
			r.setState(agent.StateCompleted)
			return
		}
	}
	r.setState(agent.StateCompleted)
	_ = r.persist(ctx, nil, nil)
	yield(agent.Status{State: agent.StateCompleted}, nil)
}

// frontierOutcome says why one iteration of the loop ended.
type frontierOutcome int

const (
	frontierContinue frontierOutcome = iota
	frontierSuspended
	frontierStopped
	frontierFailed
)

// frontier runs one iteration: route accounting, the step, suspension and the
// next frontier. It owns the step span, whose lifetime is exactly one
// iteration, and the per-step turn identity the child spans inherit.
func (r *Run) frontier(ctx context.Context, yield func(agent.Event, error) bool) (outcome frontierOutcome, err error) {
	ctx, end := r.startStepSpan(ctx)
	defer func() { end(err) }()

	if ok, err := r.applySteer(yield); err != nil {
		return frontierFailed, err
	} else if !ok {
		return frontierStopped, nil
	}

	// A skipped node still routes, so it consumes budget: a cycle of skipped
	// nodes terminates instead of spinning forever.
	r.routed += len(r.active)
	if max := r.g.maxNodes; max > 0 && r.routed > max {
		return frontierFailed, errdefs.BudgetExceededf("graph: exceeded limits.max_nodes %d", max)
	}

	// A step marker outlives nothing here: it is the first record of the step's
	// save, so a run without a store has no reader for it.
	if r.store != nil {
		r.markFrontier()
	}
	suspended, question, err := r.runStep(ctx, yield)
	if errors.Is(err, errConsumerStopped) {
		// The caller stopped reading. Side effects cannot be undone, so the
		// run ends here rather than replaying the step later.
		r.setState(agent.StateCanceled)
		return frontierStopped, nil
	}
	if err != nil {
		return frontierFailed, err
	}
	if len(suspended) > 0 {
		if len(suspended) > 1 {
			return frontierFailed, fmt.Errorf(
				"graph: nodes %v asked for input in one step; only one may", suspended)
		}
		r.pending = r.active
		r.active = suspended
		r.answer = message.Content{}
		r.question = question
		r.setState(agent.StateInputRequired)
		reason := ""
		if text := question.Text(); text != "" {
			reason = text
		}
		_ = r.persist(ctx, nil, nil)
		yield(agent.Status{State: agent.StateInputRequired, Reason: reason}, nil)
		return frontierSuspended, nil
	}
	r.answer = message.Content{}

	next, err := r.nextFrontier()
	if err != nil {
		return frontierFailed, err
	}
	r.active = next
	r.step++
	if !r.emit(yield, agent.Status{
		State:  agent.StateWorking,
		Reason: fmt.Sprintf("step %d", r.step),
	}) {
		return frontierStopped, nil
	}
	return frontierContinue, nil
}

// startStepSpan opens the step span. With tracing off it returns the context
// unchanged and a no-op end, so the loop pays one branch.
func (r *Run) startStepSpan(ctx context.Context) (context.Context, func(error)) {
	if r.trace == agent.TraceNone {
		return ctx, func(error) {}
	}
	if !r.trace.AtLeast(agent.TraceRun) {
		return ctx, func(error) {}
	}
	ctx, span := telemetry.TracerWithSuffix("graph").Start(ctx, "graph.step",
		trace.WithAttributes(telemetry.Attrs(ctx)...),
		trace.WithAttributes(
			attribute.Int("graph.step", r.step+1),
			attribute.Int("graph.frontier", len(r.active)),
			attribute.Int("graph.routed", r.routed),
		))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
		span.End()
	}
}

// startNodeSpan opens one node's span, a child of the step span.
func (r *Run) startNodeSpan(ctx context.Context, id string, kind NodeKind) (context.Context, func(error)) {
	if !r.trace.AtLeast(agent.TraceNode) {
		return ctx, func(error) {}
	}
	ctx, span := telemetry.TracerWithSuffix("graph").Start(ctx, "graph.node",
		trace.WithAttributes(telemetry.Attrs(ctx)...),
		trace.WithAttributes(
			attribute.Int("graph.step", r.step+1),
			attribute.String("graph.node.id", id),
			attribute.String("graph.node.kind", kind.Name()),
		))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
		span.End()
	}
}

// classify records why a node failed, in the turn's closed vocabulary.
//
// It is the driver's half of the contract: the decorator owns timeout, cancellation and panic,
// and everything else is a diagnosis only the layer that made the call can name. A refusal wins
// over the kind's own answer — a policy denial is the cause, and "the tool failed" would send a
// reader looking for a bug in the tool.
func (r *Run) classify(ctx context.Context, kind NodeKind, err error) {
	if err == nil || errors.Is(err, errConsumerStopped) {
		return
	}
	if errdefs.IsPolicyDenied(err) {
		agent.RecordFailure(ctx, agent.FailurePolicyDenied)
		return
	}
	if classifier, ok := kind.(FailureClassifier); ok {
		agent.RecordFailure(ctx, classifier.FailureKind())
	}
}

// emit yields one progress event. A consumer that stopped leaves the run
// canceled: the step already ran, so replaying it later would repeat its side
// effects. Suspension and completion are not progress events and are yielded
// directly by the caller.
func (r *Run) emit(yield func(agent.Event, error) bool, event agent.Event) bool {
	if yield(event, nil) {
		return true
	}
	r.setState(agent.StateCanceled)
	return false
}

// fail reports a terminal failure.
func (r *Run) fail(ctx context.Context, yield func(agent.Event, error) bool, err error) {
	r.setState(agent.StateFailed)
	_ = r.persist(ctx, nil, nil)
	yield(agent.Status{State: agent.StateFailed, Reason: err.Error()}, nil)
	yield(nil, err)
}

// reportDropped tells a steered caller that its input never reached the transcript.
//
// A run can end with an input still queued — the last step was the last frontier — and the return value
// of Steer only ever promised "queued". Without this receipt the caller waits for the Interrupted event
// that says it was applied, which will never come, while the host's own counter has already recorded
// the steer as accepted. It is an event rather than an error because the turn did not fail, and a
// failure is what a host retries.
//
// It runs once, at the end of the turn — after the terminal Status, since that is when the run knows it
// has stopped — and only for a terminal run: a suspended run keeps its queued input, because the next
// turn resumes into the boundary that would have consumed it.
func (r *Run) reportDropped(yield func(agent.Event, error) bool) {
	if !r.State().Terminal() {
		return
	}
	r.steerMu.Lock()
	pending := r.steer
	r.steer = nil
	r.steerMu.Unlock()
	if pending == nil {
		return
	}
	yield(steerDroppedEvent(SteerDropped{
		ID:     pending.id,
		Intent: pending.kind,
		Step:   r.step,
	}), nil)
}

// stepResult is one node's outcome, tagged with its position so writes apply in
// declaration order regardless of completion order.
type stepResult struct {
	index  int
	result Result
	err    error
}

// runStepInline runs a one-node frontier on the caller's goroutine.
//
// It matches the concurrent path event for event: the same stream pull, the same
// consumer-stop rule, the same panic barrier, and writes applied before the
// checkpoint. Only the scheduling overhead is gone.
func (r *Run) runStepInline(
	ctx context.Context,
	yield func(agent.Event, error) bool,
	id string,
	inv Invoke,
) ([]string, message.Content, error) {
	stopped := false
	emit := func(event agent.Event) bool {
		switch typed := event.(type) {
		case agent.StreamStart:
			r.streamed.Reset()
		case agent.Text:
			r.streamed.WriteString(typed.Text)
		}
		if !yield(event, nil) {
			stopped = true
			return false
		}
		return true
	}
	kind := r.g.nodes[id].kind
	if !yield(nodeInvokeEvent(r.step+1, id, kind.Name(), NodeStarted), nil) {
		stopped = true
		return nil, message.Content{}, errConsumerStopped
	}
	nodeCtx, end := r.startNodeSpan(ctx, id, kind)
	result, err := r.invokeInline(nodeCtx, id, inv, emit)
	r.classify(nodeCtx, kind, err)
	end(err)
	// The consumer may have stopped while the node was streaming; a stopped
	// consumer is not offered another event.
	if !stopped {
		if !yield(nodeInvokeEvent(r.step+1, id, kind.Name(), NodeDone), nil) {
			stopped = true
		}
	}
	if stopped {
		return nil, message.Content{}, errConsumerStopped
	}
	if err != nil {
		return nil, message.Content{}, fmt.Errorf("graph: node %q: %w", id, err)
	}
	if result.Suspend != nil {
		r.question = result.Suspend.Question
		_ = r.persist(ctx, nil, result.Records)
		return []string{id}, result.Suspend.Question, nil
	}
	if err := r.values.apply(result.State, r.g.state); err != nil {
		return nil, message.Content{}, err
	}
	_ = r.persist(ctx, result.State, result.Records)
	if event, ok := r.stateEvent(result.State); ok {
		if !yield(event, nil) {
			return nil, message.Content{}, errConsumerStopped
		}
	}
	return nil, message.Content{}, nil
}

// invokeInline is invokeOnce with the panic barrier the goroutine used to give.
func (r *Run) invokeInline(
	ctx context.Context,
	id string,
	inv Invoke,
	emit func(agent.Event) bool,
) (result Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			agent.RecordFailure(ctx, agent.FailurePanicked)
			err = fmt.Errorf("node %q panicked: %v", id, recovered)
		}
	}()
	return r.invokeOnce(ctx, id, inv, emit)
}

// runStep invokes one frontier, forwards the events its streams produce, and
// applies its writes.
//
// Each node gets a goroutine that pulls its stream; the main goroutine reads
// completed nodes and produced events at the same time, so text reaches the
// caller while the step is still running. State is still applied at the end, in
// declaration order; only the interleaving of events from different nodes is a
// function of scheduling, which is inherent to streaming a parallel step.
func (r *Run) runStep(
	ctx context.Context,
	yield func(agent.Event, error) bool,
) ([]string, message.Content, error) {
	active := r.active
	snapshot := r.values.snapshot()
	run := make([]bool, len(active))
	invocations := make([]Invoke, len(active))
	var skipEnv map[string]any
	running, single := 0, -1
	for i, id := range active {
		spec := r.g.nodes[id]
		if spec.skip != nil {
			if skipEnv == nil {
				skipEnv = Env(r.step, Snapshot{values: snapshot})
			}
			skip, err := spec.skip.EvaluateEnv(skipEnv)
			if err != nil {
				return nil, message.Content{}, fmt.Errorf("graph: node %q skip: %w", id, err)
			}
			if skip {
				continue
			}
		}
		run[i] = true
		running++
		single = i
		invocations[i] = Invoke{
			Node:     id,
			Config:   spec.config,
			Decoded:  spec.decoded,
			State:    func(key string) Value { return snapshot[key] },
			Snapshot: func() Snapshot { return Snapshot{values: snapshot} },
			Step:     r.step,
			Answer:   r.answer,
			Lookup: func(hash string) (agent.LLMCallRecord, bool) {
				return r.lookup(ctx, hash)
			},
			Records: func(kind string) []agent.Record {
				return r.records[kind]
			},
			Replay:       r.replay,
			Trace:        r.trace,
			Bodies:       r.bodies,
			Binding:      r.binding,
			Tools:        r.tools,
			Models:       r.models,
			DefaultModel: r.defaultModel,
			Checkpoint: func(records []agent.Record) error {
				return r.persist(ctx, nil, records)
			},
		}
	}
	// A frontier with one node has nothing to run in parallel: it pays no
	// goroutine, channel or semaphore, which for a linear graph is every step.
	if running == 1 {
		return r.runStepInline(ctx, yield, active[single], invocations[single])
	}

	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	emitted := make(chan agent.Event, 16)
	done := make(chan stepResult, len(active))
	limit := r.g.concurrency
	if limit <= 0 || limit > len(active) {
		limit = len(active)
	}
	semaphore := make(chan struct{}, limit)
	var pumps sync.WaitGroup
	for i, id := range active {
		if !run[i] {
			continue
		}
		pumps.Add(1)
		go func(i int, id string) {
			defer pumps.Done()
			// A streaming node holds its slot for as long as its stream runs:
			// the concurrency limit bounds live calls, not merely starts.
			select {
			case semaphore <- struct{}{}:
			case <-stepCtx.Done():
				// Waiting for a slot is cancellable: a sibling failure or a
				// consumer stop must not let a queued node start work that the
				// step will throw away.
				done <- stepResult{index: i, err: stepCtx.Err()}
				return
			}
			defer func() { <-semaphore }()
			outcome := stepResult{index: i}
			defer func() {
				if rv := recover(); rv != nil {
					agent.RecordFailure(stepCtx, agent.FailurePanicked)
					outcome = stepResult{index: i, err: fmt.Errorf("node %q panicked: %v", id, rv)}
				}
				done <- outcome
			}()
			emit := func(event agent.Event) bool {
				select {
				case emitted <- event:
					return true
				case <-stepCtx.Done():
					return false
				}
			}
			kind := r.g.nodes[id].kind
			// A consumer that stopped must not be offered another event: the end is
			// reported only when the start was taken.
			announced := emit(nodeInvokeEvent(r.step+1, id, kind.Name(), NodeStarted))
			nodeCtx, end := r.startNodeSpan(stepCtx, id, kind)
			outcome.result, outcome.err = r.invokeOnce(nodeCtx, id, invocations[i], emit)
			r.classify(nodeCtx, kind, outcome.err)
			end(outcome.err)
			if announced {
				emit(nodeInvokeEvent(r.step+1, id, kind.Name(), NodeDone))
			}
		}(i, id)
	}
	go func() {
		pumps.Wait()
		close(emitted)
	}()

	results := make([]Result, len(active))
	errs := make([]error, len(active))
	remaining := running
	events := emitted
	stopped := false
	failed := false
	for remaining > 0 || events != nil {
		select {
		case outcome := <-done:
			remaining--
			if outcome.err != nil {
				errs[outcome.index] = outcome.err
				if !failed {
					// One failing node fails the step; stopping its siblings
					// saves work they would throw away, and the drain below
					// still lets them return.
					failed = true
					cancel()
				}
			}
			results[outcome.index] = outcome.result
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if stopped || failed {
				continue
			}
			switch typed := event.(type) {
			case agent.StreamStart:
				r.streamed.Reset()
			case agent.Text:
				r.streamed.WriteString(typed.Text)
			}
			if !yield(event, nil) {
				// The caller stopped consuming: cancel the step and drain what
				// the nodes already produced so no goroutine is left blocked.
				stopped = true
				cancel()
			}
		}
	}
	if stopped {
		return nil, message.Content{}, errConsumerStopped
	}
	// Declaration order decides which failure is reported, not arrival order.
	if err := firstNodeError(active, errs); err != nil {
		return nil, message.Content{}, err
	}

	// Declaration order decides how writes merge; completion order never does.
	var (
		updates   []StateUpdate
		records   []agent.Record
		suspended []string
		question  message.Content
	)
	for i, result := range results {
		records = append(records, result.Records...)
		if result.Suspend != nil {
			suspended = append(suspended, active[i])
			if question.Text() == "" {
				question = result.Suspend.Question
			}
			continue
		}
		updates = append(updates, result.State...)
	}
	if err := r.values.apply(updates, r.g.state); err != nil {
		return nil, message.Content{}, err
	}
	_ = r.persist(ctx, updates, records)
	if event, ok := r.stateEvent(updates); ok {
		if !yield(event, nil) {
			return nil, message.Content{}, errConsumerStopped
		}
	}
	return suspended, question, nil
}

// queueSteer records a message for the next boundary. The run's own goroutine
// applies it, so a caller never touches state it does not own.
//
// The three refusals here are the whole contract of the input slot, checked where the slot is known
// rather than where the message is inserted: a refusal at the boundary would arrive after the caller
// was told the steer was queued, which is the one thing the return value promises.
func (r *Run) queueSteer(id agent.SteerID, msg message.Message, kind agent.SteerIntent) (agent.Interruption, error) {
	if r.g.input == "" {
		return agent.Interruption{}, errdefs.NotAvailablef(
			"graph: steer: the document declares no input slot")
	}
	// A steer is operator input, so it is a user turn. Accepting another role would let the engine
	// fabricate a claim nobody made — an assistant turn the model never wrote, a tool result no tool
	// produced — and the model node would refuse it one step later with an error that says nothing
	// about steering.
	if msg.Role != message.RoleUser {
		return agent.Interruption{}, errdefs.Validationf(
			"graph: steer: role %q is not a user turn", msg.Role)
	}
	// The slot must say how it merges, because a steer is a writer the document may not have accounted
	// for. "single" (the default) admits one writer per step, so a document that never considered
	// steering would die at an arbitrary later step with a merge error the caller cannot attribute to
	// its input. "append" adds the message to a transcript, and "last" replaces a mailbox — both are
	// defined outcomes, and which one is meant is the document's decision.
	if spec, ok := r.g.state[r.g.input]; !ok || spec.Merge == MergeSingle {
		return agent.Interruption{}, errdefs.NotAvailablef(
			"graph: steer: state %q merges by %s; a steered message needs merge: last or merge: append",
			r.g.input, mergeLabel(spec.Merge))
	}
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	r.steer = &steerRequest{id: id, message: msg, kind: kind}
	return agent.Interruption{Intent: kind}, nil
}

// mergeLabel names a merge rule for a message. The zero value is the strict default, so it has to say
// what it is rather than print nothing.
func mergeLabel(merge Merge) string {
	if merge == MergeSingle {
		return "single (the default)"
	}
	return string(merge)
}

// withdrawSteer clears the queued steer and reports whether it was still waiting. A false means the
// run already applied it, so the message is in the transcript and cannot be taken back.
func (r *Run) withdrawSteer() bool {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	had := r.steer != nil
	r.steer = nil
	return had
}

// applySteer appends the queued message to the input slot and reports the
// interruption. It runs on the run's goroutine, just before the step.
func (r *Run) applySteer(yield func(agent.Event, error) bool) (bool, error) {
	r.steerMu.Lock()
	request := r.steer
	r.steer = nil
	r.steerMu.Unlock()
	if request == nil {
		return true, nil
	}
	delta := MessagesValue(MessagesFromSlice([]message.Message{request.message}))
	if err := r.values.apply([]StateUpdate{{
		Key:   r.g.input,
		Value: delta,
	}}, r.g.state); err != nil {
		return false, fmt.Errorf("graph: apply steer: %w", err)
	}
	interruption := agent.Interruption{
		Intent:   request.kind,
		Boundary: r.step + 1,
	}
	if !yield(agent.Interrupted{Interruption: interruption}, nil) {
		return false, nil
	}
	return true, nil
}

// lookup returns a recorded call by request hash, loading the response blob a
// previous process stored.
func (r *Run) lookup(ctx context.Context, requestHash string) (agent.LLMCallRecord, bool) {
	if requestHash == "" || r.calls == nil {
		return agent.LLMCallRecord{}, false
	}
	record, ok := r.calls[requestHash]
	if !ok {
		return agent.LLMCallRecord{}, false
	}
	if len(record.Response) == 0 && record.ResponseRef != "" && r.store != nil {
		body, err := r.store.GetBlob(ctx, record.ResponseRef)
		if err != nil {
			return agent.LLMCallRecord{}, false
		}
		record.Response = body
	}
	return record, true
}

// invokeOnce runs one node, retrying a retryable failure that produced no events.
//
// A retry after events would duplicate text the caller already saw, so a failure
// that arrived mid-stream is final.
func (r *Run) invokeOnce(
	ctx context.Context,
	id string,
	inv Invoke,
	emit func(agent.Event) bool,
) (Result, error) {
	spec := r.g.nodes[id]
	attempts := spec.retries + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		result, produced, err := r.invokeStream(ctx, id, inv, emit)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if produced || attempt == attempts || !retryable(err) || ctx.Err() != nil {
			return Result{}, err
		}
		// A timer rather than time.After: the cancel path stops it, so no timer
		// outlives the iteration that armed it.
		backoff := time.NewTimer(time.Duration(attempt) * time.Second / 10)
		select {
		case <-ctx.Done():
			backoff.Stop()
			return Result{}, err
		case <-backoff.C:
		}
	}
	return Result{}, lastErr
}

// invokeStream opens one stream and pulls it to the end.
func (r *Run) invokeStream(
	ctx context.Context,
	id string,
	inv Invoke,
	emit func(agent.Event) bool,
) (Result, bool, error) {
	spec := r.g.nodes[id]
	invokeCtx := ctx
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		invokeCtx, cancel = context.WithTimeout(ctx, spec.timeout)
		defer cancel()
	}
	stream, err := spec.kind.Invoke(invokeCtx, inv)
	if err != nil {
		return Result{}, false, err
	}
	defer func() { _ = stream.Close() }()

	produced := false
	for {
		event, err := stream.Next(invokeCtx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, produced, err
		}
		if event == nil {
			continue
		}
		if !nodeMayEmit(event) {
			return Result{}, produced, fmt.Errorf(
				"node %q emitted a %s event, which the engine owns", id, event.Kind())
		}
		produced = true
		if !emit(event) {
			if ctx.Err() != nil {
				return Result{}, true, ctx.Err()
			}
			return Result{}, true, errConsumerStopped
		}
	}
	result, err := stream.Result()
	if err != nil {
		return Result{}, produced, err
	}
	return result, produced, nil
}

// nodeMayEmit reports whether a node may publish one event. Lifecycle events
// belong to the engine: a node that could report a completed status would be
// able to end the run behind the engine's back.
func nodeMayEmit(event agent.Event) bool {
	// Concrete types, not kinds: the engine forwards only the events a node has
	// any business producing, and lifecycle events belong to the run.
	//
	// StreamStart is a node's to report: it says a model stream opened, which is
	// what the loop resets its streamed text on, and a node that could not say so
	// would leave a live consumer unable to name the round's model.
	switch event.(type) {
	case agent.StreamStart, agent.Text, agent.Reasoning, agent.ToolCallStart,
		agent.ToolResult, agent.UsageEvent, agent.ArtifactWritten:
		return true
	}
	return false
}

// errConsumerStopped reports that the caller stopped consuming the run.
var errConsumerStopped = errors.New("graph: the caller stopped consuming events")

// firstNodeError returns the failure of the lowest-declared node.
func firstNodeError(active []string, errs []error) error {
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("graph: node %q: %w", active[i], err)
		}
	}
	return nil
}

// nextFrontier evaluates the outgoing edges of a finished step against the merged
// state.
func (r *Run) nextFrontier() ([]string, error) {
	snapshot := r.values.snapshot()
	// The environment is only needed when an edge carries a condition; building
	// it eagerly would project the whole state on every step, transcripts
	// included.
	var env map[string]any
	sources := r.active
	if len(r.pending) > 0 {
		// The siblings of a suspended node already ran; their edges fire now,
		// together with the resumed node's.
		sources = append(append([]string{}, r.active...), r.pending...)
	}
	r.pending = nil
	seen := make(map[string]bool, len(sources))
	visited := make(map[string]bool, len(sources))
	next := make([]string, 0, len(sources))
	for _, id := range sources {
		if visited[id] {
			continue
		}
		visited[id] = true
		for _, e := range r.g.edges[id] {
			if e.to == End {
				continue
			}
			fire := true
			if e.when != nil {
				if env == nil {
					env = Env(r.step, Snapshot{values: snapshot})
				}
				ok, err := e.when.EvaluateEnv(env)
				if err != nil {
					return nil, fmt.Errorf("graph: edge %q -> %q: %w", id, e.to, err)
				}
				fire = ok
			}
			if fire && !seen[e.to] {
				seen[e.to] = true
				next = append(next, e.to)
			}
		}
	}
	return next, nil
}

// publish turns the declared output into the event the protocol layer reads.
func (r *Run) publish(ctx context.Context, value Value, yield func(agent.Event, error) bool) bool {
	switch value.Kind() {
	case KindText:
		text, err := value.Text()
		if err != nil {
			yield(nil, err)
			return false
		}
		if text == r.streamed.String() {
			// The answer was already reported as deltas; publishing it again
			// would make a folding caller read it twice.
			return true
		}
		return yield(agent.Text{Text: text}, nil)
	case KindMessages:
		messages, err := value.Messages()
		if err != nil {
			yield(nil, err)
			return false
		}
		// The answer is the newest turn: its media if it has any, otherwise its
		// text. A generated image or video is published as an artifact instead
		// of failing for having no text.
		if media, ok := lastMedia(messages); ok {
			return r.publish(ctx, media, yield)
		}
		text, ok := lastText(messages)
		if !ok {
			yield(nil, fmt.Errorf("graph: output %q has no text to report", r.g.output))
			return false
		}
		return yield(agent.Text{Text: text}, nil)
	case KindImage, KindAudio, KindFile, KindVideo:
		media, err := value.Media()
		if err != nil {
			yield(nil, err)
			return false
		}
		blob := media.Blob
		if blob == "" {
			if r.g.blobs == nil {
				yield(nil, fmt.Errorf(
					"graph: output %q is %s, which needs a blob store", r.g.output, value.Kind()))
				return false
			}
			blob, err = r.g.blobs.PutBlob(ctx, media.Data)
			if err != nil {
				yield(nil, fmt.Errorf("graph: store output: %w", err))
				return false
			}
		}
		return yield(agent.ArtifactWritten{Artifact: agent.Artifact{
			ID:        blob,
			MediaType: media.MediaType,
			Blob:      blob,
		}}, nil)
	}
	yield(nil, fmt.Errorf("graph: output %q is %q, which cannot be reported", r.g.output, value.Kind()))
	return false
}

// lastMedia returns the first media part of the newest message that has one.
func lastMedia(messages Messages) (Value, bool) {
	var found Value
	ok := false
	messages.Each(func(msg message.Message) bool {
		for _, part := range msg.Content.Parts {
			if media, isMedia := mediaValue(part); isMedia {
				found, ok = media, true
				return false
			}
		}
		return true
	})
	return found, ok
}

// mediaValue turns one media part into a state value.
func mediaValue(part message.Part) (Value, bool) {
	switch typed := part.(type) {
	case message.ImagePart:
		return Image(typed.Source.MediaType(), typed.Source.Bytes()), true
	case message.AudioPart:
		return Audio(typed.Source.MediaType(), typed.Source.Bytes()), true
	case message.VideoPart:
		return Video(typed.Source.MediaType(), typed.Source.Bytes()), true
	case message.FilePart:
		return FileRef(typed.Source.MediaType(), typed.Source.URL()), true
	}
	return Value{}, false
}

// lastText returns the text of the newest message that has any.
func lastText(messages Messages) (string, bool) {
	text := ""
	found := false
	messages.Each(func(msg message.Message) bool {
		if value := msg.Content.Text(); value != "" {
			text, found = value, true
			return false
		}
		return true
	})
	return text, found
}

// retryable reports whether a node failure is worth another attempt. A validation,
// interruption, budget or policy failure fails the same way twice; so does a
// deadline, which the caller's context already owns.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch {
	case errdefs.IsValidation(err), errdefs.IsInterrupted(err), errdefs.IsAborted(err),
		errdefs.IsBudgetExceeded(err), errdefs.IsPolicyDenied(err):
		return false
	}
	return true
}
