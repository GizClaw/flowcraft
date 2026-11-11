package graph

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// RunnerConfig is one session's graph engine: where it persists, under
// which deployment, and which task it is.
type RunnerConfig struct {
	Store     agent.Store
	Identity  agent.Fingerprint
	SessionID string
	TaskID    string
	// Manifest is the version combination the run records.
	Manifest agent.Manifest
	// Trace controls the spans the graph emits.
	Trace telemetry.TraceLevel
	// Replay answers model calls from this task's records instead of calling
	// out. A miss is an error.
	Replay bool
}

// Runner drives one session's graph.
//
// It is the engine contract a deployment sees: one message in, events out. The
// graph itself is a compiled document shared by every session; the runner
// owns the single run and its checkpoint identity.
type Runner struct {
	g   *Graph
	cfg RunnerConfig

	mu  sync.Mutex
	run *Run
}

var (
	_ agent.Runner       = (*Runner)(nil)
	_ agent.Steerer      = (*Runner)(nil)
	_ agent.ResultReader = (*Runner)(nil)
)

// NewRunner binds a compiled graph to one session.
func NewRunner(g *Graph, cfg RunnerConfig) *Runner {
	return &Runner{g: g, cfg: cfg}
}

// State reports the run's lifecycle, submitted before the first message.
func (r *Runner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil {
		return agent.StateSubmitted
	}
	return r.run.State()
}

// Run handles one message: it starts the task, or answers the question it paused
// on. The graph decides which from its own state, so the caller never has to.
//
// A document that declares no input slot receives the message as a trigger: it
// has nowhere to put the text, and a caller that needs it answered should give
// the graph an input slot.
func (r *Runner) Run(ctx context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.mu.Lock()
		run := r.run
		if run == nil {
			// A document that declares no input slot takes the message as the
			// trigger only: there is nowhere to seed it, and inventing a slot
			// would change what the document said.
			var seed []message.Message
			if r.g.input != "" {
				seed = []message.Message{msg}
			}
			started, err := r.g.Start(
				ctx,
				seed,
				WithStore(r.cfg.Store, r.cfg.TaskID),
				WithSession(r.cfg.SessionID),
				WithFingerprint(r.cfg.Identity),
				WithManifest(r.cfg.Manifest),
				WithTrace(r.cfg.Trace),
				WithReplay(r.cfg.Replay),
			)
			if err != nil {
				r.mu.Unlock()
				yield(nil, err)
				return
			}
			r.run = started
			run = started
		}
		r.mu.Unlock()

		switch run.State() {
		case agent.StateInputRequired:
			for event, answerErr := range run.Answer(ctx, msg.Content) {
				if !yield(event, answerErr) {
					return
				}
			}
		case agent.StateSubmitted:
			run.advance(ctx, yield)
		case agent.StateWorking:
			yield(nil, fmt.Errorf("graph: the task is already running"))
		default:
			yield(nil, fmt.Errorf("graph: the task is %s; start a new one", run.State()))
		}
	}
}

// Steer queues a message for the run's next step boundary. The current node is
// not interrupted: a long model call finishes first, and the message reaches
// the graph at the beginning of the next frontier.
func (r *Runner) Steer(ctx context.Context, msg message.Message, kind agent.InterruptKind) (agent.Interruption, error) {
	r.mu.Lock()
	run := r.run
	r.mu.Unlock()
	if run == nil {
		return agent.Interruption{}, agent.ErrSessionIdle
	}
	return run.queueSteer(msg, kind)
}

// Cancel stops the session's task. A run in flight is stopped through its
// context by the caller; a paused one is closed here, because nothing else will.
//
// A fresh process has no run in memory: the checkpoint is what says the task is
// paused. Cancelling must therefore restore it and record the state where a
// restart will find it, instead of silently doing nothing and letting the task
// resume later.
func (r *Runner) Cancel(ctx context.Context, reason string) error {
	r.mu.Lock()
	run := r.run
	r.mu.Unlock()
	if run == nil {
		restored, found, err := r.g.restore(ctx, runOptions{
			store:       r.cfg.Store,
			taskID:      r.cfg.TaskID,
			sessionID:   r.cfg.SessionID,
			fingerprint: r.cfg.Identity,
		})
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		r.mu.Lock()
		if r.run != nil {
			// A Run started while the checkpoint was being read; the task it
			// opened is the one the caller wants stopped.
			run = r.run
		} else {
			r.run = restored
			run = restored
		}
		r.mu.Unlock()
	}
	return run.cancel(ctx, reason)
}

// Result returns the run's final assistant message and state. A process that
// did not run the task restores it from the checkpoint first.
func (r *Runner) Result(ctx context.Context) (agent.Result, error) {
	r.mu.Lock()
	run := r.run
	r.mu.Unlock()
	if run == nil {
		restored, found, err := r.g.restore(ctx, runOptions{
			store:       r.cfg.Store,
			taskID:      r.cfg.TaskID,
			sessionID:   r.cfg.SessionID,
			fingerprint: r.cfg.Identity,
		})
		if err != nil {
			return agent.Result{}, err
		}
		if !found {
			return agent.Result{}, agent.ErrNoCheckpoint
		}
		r.mu.Lock()
		if r.run == nil {
			r.run = restored
		}
		run = r.run
		r.mu.Unlock()
	}
	return run.Result(ctx)
}

// Close releases the runner; the graph holds nothing but the document.
func (r *Runner) Close() error { return nil }

// cancel closes a task that is not running: the state is recorded where a
// restart will find it.
func (r *Run) cancel(ctx context.Context, reason string) error {
	if r.state.Terminal() {
		return nil
	}
	r.state = agent.StateCanceled
	r.pending = nil
	_ = r.persist(ctx, nil, nil)
	return nil
}
