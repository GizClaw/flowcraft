package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Sentinels a caller can test for.
var (
	// ErrSessionRequired reports a call without a session id.
	ErrSessionRequired = errors.New("agent: session id is required")
	// ErrSessionBusy reports a second run on a session that has one
	// in flight. The protocol's spelling is "task execution is already in
	// progress"; a caller that wants to wait can retry or watch the store.
	ErrSessionBusy = errors.New("agent: the session already has a run in flight")
	// ErrClosed reports work offered to a draining or closed agent.
	ErrClosed = errors.New("agent: not accepting work")
	// ErrEffectUncertain reports a resume whose checkpoint holds a side effect
	// with no recorded outcome. The host reconciles it instead of re-running.
	ErrEffectUncertain = errors.New("agent: an effect has no recorded outcome")
	// ErrTaskMismatch reports a protocol retry addressing another task than the
	// one the session is on.
	ErrTaskMismatch = errors.New("agent: the session is on another task")
	// ErrTaskInProgress reports a stored task that stopped mid-step (a process
	// died while it ran). It takes no message: cancel it, then send again.
	ErrTaskInProgress = errors.New("agent: the task stopped mid-run; cancel it before sending")
	// ErrTaskFinished reports a message addressed to a task that already
	// finished. A host message without an id starts a new task instead.
	ErrTaskFinished = errors.New("agent: the task already finished")
	// ErrDeploymentChanged reports a checkpoint produced under another
	// deployment. Continuing it would run a transcript under different
	// permissions.
	ErrDeploymentChanged = errors.New("agent: the checkpoint belongs to another deployment")
	// ErrSessionIdle reports a steer on a session that has no run in
	// flight. The message belongs in Run, not in Steer.
	ErrSessionIdle = errors.New("agent: the session has no run in flight")
	// ErrSteerUnsupported reports an engine that does not implement Steerer.
	ErrSteerUnsupported = errors.New("agent: this engine does not support steering")
	// ErrRunEnded reports a steer that arrived after the run it targeted ended.
	// The message was not applied; sending it again starts the work it names.
	ErrRunEnded = errors.New("agent: the run ended before the steer was applied")
)

// AgentOptions are the dependencies an Agent runs on.
//
// Nothing here is a loop: the engine is the Runner, and the Agent is the table
// of sessions plus the one-run-per-session rule.
type AgentOptions struct {
	// Name is the deployment name. Hooks and records report it, so a host policy
	// can be bound to one agent (a seat) without inspecting the context.
	Name string
	// Hooks are the host's policy at the boundaries of a turn.
	Hooks Hooks
	// Store makes sessions resumable. Nil keeps them in memory only.
	Store Store
	// Identity is the fingerprint a checkpoint must carry to be resumed.
	// Empty disables the check.
	Identity Fingerprint
	// Trace controls the spans this agent emits. Zero emits none; a durable
	// host usually asks for TraceNode so a run, its frontiers and its nodes
	// are attributable. The cost is a span per unit, not per event.
	Trace telemetry.TraceLevel
	// OnRun observes every finished run. It runs synchronously on the run's
	// goroutine, after the last event, so it must be fast and non-blocking.
	// Runs that never acquired a session (busy, closed) report nothing.
	OnRun func(RunRecord)
	// NewRunner builds the engine for one session. The task id is settled
	// before this is called, so an engine may key its checkpoint by it.
	//
	// It is called without any table lock held, once per session (or once
	// per task, when a finished task rolls over), so a slow store or a dial to a
	// peer blocks nobody else.
	NewRunner RunnerFactory
	// RunContext gives the context one run executes under: the tool projection,
	// run attributes, trace association. The agent package does not own the
	// runtime, so the host decides what goes in.
	RunContext func(ctx context.Context, sessionID string) context.Context
	// IdleTimeout drops an idle session from memory. Zero keeps it.
	IdleTimeout time.Duration
	// MaxSessions bounds the table; the oldest idle session is evicted
	// first. Zero is unbounded.
	MaxSessions int
	// OnEvent observes every event of every run. Optional.
	//
	// It runs synchronously on the run's own goroutine, before the event is
	// yielded, so a slow observer slows the run it observes. Keep it short and
	// non-blocking; hand slow work to another goroutine.
	OnEvent func(sessionID string, event Event)
	// OnEvict is called when a session leaves the table: idle timeout or
	// MaxSessions. A finished task rolling over to the next one is the same
	// session and does not fire it. Optional.
	//
	// It may be called on any goroutine — the background sweep is where most
	// evictions happen — never while holding a table lock, and it must be short
	// and safe for concurrent use. A hook that blocks slows the sweep, not a run.
	OnEvict func(sessionID string)
}

// Agent owns deployed sessions and nothing else.
//
// It is not an interface: engines implement Runner, and this is the one
// implementation of the table, the slot and the lifecycle around them. Its
// surface mirrors the protocol's: Run is a request, State is a lookup, Cancel
// is a cancel. There is no submit, no queue and no pending count, because a
// message is handled by the call that carries it.
//
// One session has one driver. A host that mints its own task ids and a
// protocol that issues its own cannot share a session: when the two
// disagree the agent reports ErrTaskMismatch instead of guessing which task the
// message belongs to.
type Agent struct {
	opts AgentOptions
	// table is the sharded session index: runs on distinct sessions
	// never share a lock.
	table    *table
	closed   atomic.Bool
	draining atomic.Bool
	// taskPrefix is drawn once per agent; each shard appends its own counter, so
	// minting an id never touches a shared cache line.
	taskPrefix string
	// sweeper drops idle sessions in the background. The table is a cache, and
	// looking at it on the message path made every run pay for the whole table.
	sweeper sweeper
}

// sweeper is the background idle eviction: its goroutine, how often it looks, and
// how Close waits for it.
//
// It exists because the scan is O(sessions) and the message path is O(1): a table
// with ten thousand sessions spent a hundred microseconds per message looking for
// sessions nothing had asked about.
type sweeper struct {
	// stop is closed by Close; nil when the table has no limit to enforce.
	stop chan struct{}
	// done is waited on before Close touches the table, so a sweep cannot close a
	// runner the shutdown is closing.
	done sync.WaitGroup
	// interval is how often the loop runs; zero means no loop.
	interval time.Duration
}

// session is one session's runner and whether a run owns it.
type session struct {
	id     string
	taskID string
	runner Runner
	// state is read by State and written by every Status event; an atomic keeps
	// observers off the table lock. It carries the state's code, not the string:
	// storing the string would box it on every event.
	state atomic.Uint32
	// lastUsed drives idle eviction.
	lastUsed time.Time
	running  bool
	// cancelRun stops the run in flight. Cancel uses it instead of calling the
	// runner: an engine drives one goroutine, so Runner.Cancel must not run
	// while Runner.Run does.
	cancelRun context.CancelFunc
	// done is closed when the current run ends, so a caller that arrived while
	// the session was busy can wait for the bookkeeping instead of racing
	// it.
	done chan struct{}
	// opening is non-nil while the runner is being built. Waiters block on it
	// instead of building a second runner, and no table lock is held while the
	// store or the peer is being reached.
	opening chan struct{}
	// openErr is set when the build failed; the waiter that observes it drops
	// the entry.
	openErr error
	// turn counts accepted Run calls for this session in this process. It is
	// carried across a terminal task replacement and is not persisted.
	turn atomic.Uint32
}

// NewAgent builds the session table. It does not open anything: a runner is
// constructed on the first message of its session.
func NewAgent(opts AgentOptions) (*Agent, error) {
	if opts.NewRunner == nil {
		return nil, errors.New("agent: NewRunner is required")
	}
	built := &Agent{
		opts:       opts,
		table:      newTable(tableShards),
		taskPrefix: newTaskPrefix(),
	}
	built.sweeper.interval = sweepInterval(opts)
	built.startSweeper()
	return built, nil
}

// sweepInterval is how often idle sessions are looked at, or zero when there is
// no limit to enforce.
//
// The cadence follows the timeout it enforces: evicting a session four times more
// often than its timeout is pointless, and a minute of slack on a cache costs
// nothing. The floor keeps a pathologically short timeout from spinning, and the
// ceiling keeps a long one from letting the table grow for hours.
func sweepInterval(opts AgentOptions) time.Duration {
	if opts.IdleTimeout <= 0 && opts.MaxSessions <= 0 {
		return 0
	}
	if opts.IdleTimeout <= 0 {
		// The cap is enforced on the message path; this pass only trims drift.
		return time.Minute
	}
	interval := opts.IdleTimeout / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	return interval
}

// startSweeper runs the idle eviction loop when the table has a limit.
func (a *Agent) startSweeper() {
	if a.sweeper.interval <= 0 {
		return
	}
	stop := make(chan struct{})
	a.sweeper.stop = stop
	a.sweeper.done.Add(1)
	go a.runSweeper(stop, a.sweeper.interval)
}

func (a *Agent) runSweeper(stop <-chan struct{}, interval time.Duration) {
	defer a.sweeper.done.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			// A host hook that panics must not take the eviction with it: the
			// table would then grow for the life of the process.
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						telemetry.Warn(context.Background(),
							"agent: a sweep panicked; the table will be swept again",
							attribute.String("panic", fmt.Sprint(recovered)))
					}
				}()
				a.evict(a.sweep(now))
			}()
		}
	}
}

// stopSweeper stops the loop and waits for it, so nothing sweeps while Close is
// closing what the sweep would have closed.
func (a *Agent) stopSweeper() {
	if a.sweeper.stop == nil {
		return
	}
	close(a.sweeper.stop)
	a.sweeper.done.Wait()
	a.sweeper.stop = nil
}

// evict closes what a sweep dropped, outside every lock. The run path does this
// for its own evictions; the background loop has no caller to defer it to.
func (a *Agent) evict(evicted []*session) {
	for _, gone := range evicted {
		if gone.runner != nil {
			_ = gone.runner.Close()
		}
	}
}

// RunnerFactory builds one session's engine. It is what an engine kind
// returns and what AgentOptions takes, so the two cannot drift.
type RunnerFactory func(ctx context.Context, sessionID, taskID string) (Runner, error)

// TurnOption configures one turn.
type TurnOption func(*turnOptions)

type turnOptions struct {
	// taskID is the identity the caller knows the task by. A protocol passes
	// the id it issued; a host leaves it empty and the agent adopts the stored
	// task or mints one.
	taskID string
}

// WithTaskID addresses one task explicitly. A protocol adapter uses it to keep
// the id its client knows; a stored task under that id is resumed.
func WithTaskID(id string) TurnOption {
	return func(o *turnOptions) { o.taskID = id }
}

// Run handles one message: it starts a Task, or answers the question a paused
// Task asked. Both cases are the same request on the wire, so they are one call
// here.
//
// The returned sequence carries the run: nothing happens until it is consumed,
// a busy session reports ErrSessionBusy as its first item, and a
// consumer that stops early cancels the run. One consumer per turn — the model
// is a request with a response, not a subscription.
func (a *Agent) Run(ctx context.Context, sessionID string, msg message.Message, opts ...TurnOption) iter.Seq2[Event, error] {
	var settings turnOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	return func(yield func(Event, error) bool) {
		turn := Turn{Agent: a.opts.Name, SessionID: sessionID, Message: msg}
		if a.opts.Hooks.BeforeTurn != nil {
			if err := a.opts.Hooks.BeforeTurn(ctx, &turn); err != nil {
				yield(nil, err)
				return
			}
			msg = turn.Message
		}
		entry, runCtx, cancel, err := a.acquire(ctx, sessionID, settings.taskID)
		if err != nil {
			yield(nil, err)
			return
		}
		defer a.release(entry)
		defer cancel()
		var failure error
		var startedAt time.Time
		var runUsage usage.Usage
		if a.opts.RunContext != nil {
			runCtx = a.opts.RunContext(runCtx, sessionID)
		}
		turn.TaskID = entry.taskID
		runCtx = runctx.WithRun(runCtx, runctx.Run{
			TaskID: entry.taskID,
			Turn:   int(entry.turn.Add(1)),
		})
		var span trace.Span
		if a.opts.Trace.Allows(telemetry.TraceRun) {
			runCtx, span = telemetry.TracerWithSuffix("agent").Start(runCtx, "agent.run",
				trace.WithAttributes(runctx.Attrs(runCtx)...))
			defer span.End()
		}
		// The run record is only built for a host that asked for one: the
		// default path pays neither the clock nor the deferred closure.
		if a.opts.OnRun != nil {
			startedAt = time.Now()
			defer func() {
				record := RunRecord{
					SessionID: sessionID,
					TaskID:    entry.taskID,
					State:     entry.loadState(),
					ErrorKind: errorKind(failure),
					StartedAt: startedAt,
					LatencyMs: time.Since(startedAt).Milliseconds(),
					Usage:     runUsage,
				}
				if run, ok := runctx.From(runCtx); ok {
					record.TenantID = run.TenantID
					record.Manifest = run.Manifest
				}
				if span != nil {
					if sc := span.SpanContext(); sc.IsValid() {
						record.TraceID = sc.TraceID().String()
						record.SpanID = sc.SpanID().String()
					}
				}
				a.opts.OnRun(record)
			}()
		}

		for event, runErr := range entry.runner.Run(runCtx, msg) {
			if runErr != nil {
				failure = runErr
				if span != nil {
					span.RecordError(runErr)
					span.SetStatus(codes.Error, "")
				}
				yield(nil, runErr)
				return
			}
			if accounting, ok := event.(UsageEvent); ok && a.opts.OnRun != nil {
				addUsage(&runUsage, accounting.Usage)
			}
			a.record(entry, event)
			if a.opts.OnEvent != nil {
				a.opts.OnEvent(sessionID, event)
			}
			if !yield(event, nil) {
				// The consumer walked away: the step already ran, so leaving it
				// to continue would keep spending on a task nobody reads.
				_ = entry.runner.Cancel(context.WithoutCancel(ctx), "consumer stopped reading")
				return
			}
		}
		a.afterTurn(ctx, entry, turn)
	}
}

// afterTurn reports the finished turn to the host.
//
// It runs after the last event, so the turn's work is durable and a failing hook
// must not turn a completed run into a failed one: the error is logged and the
// stream stays clean.
func (a *Agent) afterTurn(ctx context.Context, entry *session, turn Turn) {
	if a.opts.Hooks.AfterTurn == nil {
		return
	}
	result := Result{TaskID: entry.taskID, State: entry.loadState()}
	if reader, ok := entry.runner.(ResultReader); ok {
		// The run is over, so the caller's cancellation no longer applies to a
		// read that describes work already done.
		if snapshot, err := reader.Result(context.WithoutCancel(ctx)); err == nil {
			snapshot.TaskID = entry.taskID
			result = snapshot
		}
	}
	if err := a.opts.Hooks.AfterTurn(context.WithoutCancel(ctx), turn, result); err != nil {
		telemetry.WarnErr(ctx, "agent: an after-turn hook failed", err)
	}
}

// State reports where the session's Task stands. A session that was
// never opened is looked up in the store, so this is cheap enough to ask from a
// protocol adapter on every request.
//
// A session whose runner is still being built reports submitted: the Task
// has not started, and blocking a lookup on a dial or a store read would make an
// operator's query slower than the work it observes.
func (a *Agent) State(ctx context.Context, sessionID string) (TaskState, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", ErrSessionRequired
	}
	if a.closed.Load() {
		return "", ErrClosed
	}
	shard := a.table.shard(sessionID)
	shard.mu.Lock()
	entry, found := shard.lookup(sessionID)
	shard.mu.Unlock()
	if found {
		return entry.loadState(), nil
	}
	if a.opts.Store == nil {
		return StateSubmitted, nil
	}
	checkpoint, err := a.opts.Store.LatestForSession(ctx, sessionID)
	if errors.Is(err, ErrNoCheckpoint) {
		return StateSubmitted, nil
	}
	if err != nil {
		return "", err
	}
	if a.opts.Identity != "" && checkpoint.Fingerprint != a.opts.Identity {
		return StateSubmitted, nil
	}
	return checkpoint.Task.State, nil
}

// Steer hands a message to the session's run in flight, to be applied at its
// next step boundary.
//
// The engine decides what a boundary is; this layer only checks that a run exists
// and that the engine can take one. A session that is idle — including one
// waiting for input, whose message belongs in Run — reports ErrSessionIdle,
// and an engine without the optional Steerer reports ErrSteerUnsupported.
func (a *Agent) Steer(ctx context.Context, sessionID string, msg message.Message, kind InterruptKind) (Interruption, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Interruption{}, ErrSessionRequired
	}
	if kind == "" {
		kind = InterruptSteer
	}
	shard := a.table.shard(sessionID)
	shard.mu.Lock()
	entry, found := shard.lookup(sessionID)
	if !found || !entry.running {
		shard.mu.Unlock()
		return Interruption{}, ErrSessionIdle
	}
	steerer, ok := entry.runner.(Steerer)
	if !ok {
		shard.mu.Unlock()
		return Interruption{}, ErrSteerUnsupported
	}
	shard.mu.Unlock()
	return steerer.Steer(ctx, msg, kind)
}

// Cancel stops the session's Task. A session that is not open is
// looked up in the store, so a task paused by a process that has since died is
// still cancellable. Nothing to cancel is not an error.
func (a *Agent) Cancel(ctx context.Context, sessionID, reason string) error {
	if strings.TrimSpace(sessionID) == "" {
		return ErrSessionRequired
	}
	// Cancel reaches the engine and may open a stored session, so a closed
	// agent must refuse it. Draining is allowed: stopping runs is what a
	// drain waits for.
	if a.closed.Load() {
		return ErrClosed
	}
	shard := a.table.shard(sessionID)
	shard.mu.Lock()
	entry, found := shard.lookup(sessionID)
	shard.mu.Unlock()
	if found {
		if err := waitOpening(ctx, shard, entry); err != nil {
			return err
		}
		if entry.runner == nil {
			return nil
		}
		return a.stopSession(ctx, entry, reason)
	}
	if a.opts.Store == nil {
		return nil
	}
	checkpoint, err := a.opts.Store.LatestForSession(ctx, sessionID)
	switch {
	case errors.Is(err, ErrNoCheckpoint):
		return nil
	case err != nil:
		return err
	}
	if checkpoint.Task.State.Terminal() {
		return nil
	}
	if a.opts.Identity != "" && checkpoint.Fingerprint != a.opts.Identity {
		a.abandon(ctx, checkpoint)
		return nil
	}
	opened, err := a.ensure(ctx, sessionID, checkpoint.Task.ID)
	if err != nil {
		return err
	}
	if opened.runner == nil {
		return nil
	}
	return a.stopSession(ctx, opened, reason)
}

// Drain stops accepting work and waits for the runs in flight.
//
// A timeout does not cancel them — that is Close's job — so the error reports
// what is still running and lets the caller decide.
func (a *Agent) Drain(ctx context.Context) error {
	if a.closed.Load() || a.draining.Load() {
		return nil
	}
	a.draining.Store(true)
	for _, shard := range a.table.shards {
		if err := shard.wait(ctx); err != nil {
			busy := a.runningIDs()
			return fmt.Errorf("agent: %d sessions still running (%s): %w",
				len(busy), strings.Join(busy, ", "), err)
		}
	}
	return nil
}

// Close stops the agent: runs are cancelled, and every runner is released.
func (a *Agent) Close() error {
	if a.closed.Swap(true) {
		return nil
	}
	// Before anything else: a sweep that runs during the shutdown would close a
	// runner this is about to close, and the second close is the host's problem.
	a.stopSweeper()
	var entries []*session
	var stops []context.CancelFunc
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		for _, entry := range shard.entries {
			entries = append(entries, entry)
			if entry.cancelRun != nil {
				stops = append(stops, entry.cancelRun)
			}
		}
		shard.mu.Unlock()
	}
	for _, stop := range stops {
		stop()
	}
	for _, shard := range a.table.shards {
		_ = shard.wait(context.Background())
	}
	var first error
	for _, entry := range entries {
		if entry.runner == nil {
			continue
		}
		if err := entry.runner.Close(); err != nil && first == nil {
			first = err
		}
	}
	// Drop the table: a closed agent answers no reads, so the entries would only
	// keep every session's runner reachable. The counter goes with it, so a late
	// cap check sees an empty table rather than the one that just went away.
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		shard.entries = make(map[string]*session)
		shard.mu.Unlock()
	}
	a.table.sessions.Store(0)
	return first
}

// acquire takes the session's run slot, rolling a finished task over to the
// next one.
func (a *Agent) acquire(ctx context.Context, sessionID, taskID string) (*session, context.Context, context.CancelFunc, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil, nil, ErrSessionRequired
	}
	shard := a.table.shard(sessionID)
	var evicted []*session
	defer func() {
		// Closing a runner may block, so it happens after the shard is unlocked.
		for _, gone := range evicted {
			if gone.runner != nil {
				_ = gone.runner.Close()
			}
		}
	}()
	for {
		if a.closed.Load() || a.draining.Load() {
			return nil, nil, nil, ErrClosed
		}
		shard.mu.Lock()
		entry, found := shard.lookup(sessionID)
		if !found {
			placeholder := newSession(sessionID)
			placeholder.opening = make(chan struct{})
			shard.insert(placeholder)
			shard.mu.Unlock()
			a.build(shard, placeholder, ctx, sessionID, taskID)
			continue
		}
		if entry.opening != nil {
			wait := entry.opening
			shard.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, nil, nil, ctx.Err()
			}
			continue
		}
		if entry.openErr != nil {
			err := entry.openErr
			shard.remove(sessionID)
			shard.mu.Unlock()
			return nil, nil, nil, err
		}
		if a.closed.Load() || a.draining.Load() {
			shard.mu.Unlock()
			return nil, nil, nil, ErrClosed
		}
		if entry.running {
			shard.mu.Unlock()
			return nil, nil, nil, ErrSessionBusy
		}
		switch {
		case taskID != "" && taskID != entry.taskID:
			if !entry.loadState().Terminal() {
				shard.mu.Unlock()
				return nil, nil, nil, fmt.Errorf("%w: session %q is on %q, not %q",
					ErrTaskMismatch, sessionID, entry.taskID, taskID)
			}
			next := newSession(sessionID)
			next.turn.Store(entry.turn.Load())
			next.opening = make(chan struct{})
			shard.insert(next)
			evicted = append(evicted, entry)
			shard.mu.Unlock()
			a.build(shard, next, ctx, sessionID, taskID)
		case entry.loadState().Terminal():
			if taskID != "" {
				shard.mu.Unlock()
				return nil, nil, nil, fmt.Errorf("%w: task %q", ErrTaskFinished, taskID)
			}
			next := newSession(sessionID)
			next.turn.Store(entry.turn.Load())
			next.opening = make(chan struct{})
			shard.insert(next)
			evicted = append(evicted, entry)
			shard.mu.Unlock()
			a.build(shard, next, ctx, sessionID, "")
		case entry.loadState() == StateWorking:
			// A stored task that stopped mid-step may have had side effects; it
			// has to be cancelled before it takes another message.
			shard.mu.Unlock()
			return nil, nil, nil, fmt.Errorf("%w: session %q", ErrTaskInProgress, sessionID)
		default:
			runCtx, cancel := context.WithCancel(ctx)
			entry.running = true
			entry.done = make(chan struct{})
			entry.cancelRun = cancel
			entry.lastUsed = time.Now()
			shard.begin()
			shard.mu.Unlock()
			// Only the cap is enforced here: it is a memory bound, and a bound
			// checked once a minute is not a bound. Scanning for idle sessions is
			// the background sweeper's work, and this call no longer pays for it.
			// The slot is taken first, so no trim can evict the entry in hand.
			evicted = append(evicted, a.trim()...)
			return entry, runCtx, cancel, nil
		}
	}
}

// ensure returns the built session, opening it if needed, without taking
// the run slot. Cancel uses it to reach a task paused by an earlier process.
func (a *Agent) ensure(ctx context.Context, sessionID, taskID string) (*session, error) {
	shard := a.table.shard(sessionID)
	for {
		if a.closed.Load() {
			return nil, ErrClosed
		}
		shard.mu.Lock()
		entry, found := shard.lookup(sessionID)
		switch {
		case !found:
			placeholder := newSession(sessionID)
			placeholder.opening = make(chan struct{})
			shard.insert(placeholder)
			shard.mu.Unlock()
			a.build(shard, placeholder, ctx, sessionID, taskID)
		case entry.opening != nil:
			wait := entry.opening
			shard.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case entry.openErr != nil:
			err := entry.openErr
			shard.remove(sessionID)
			shard.mu.Unlock()
			return nil, err
		default:
			shard.mu.Unlock()
			return entry, nil
		}
	}
}

// build fills a placeholder with the opened session and wakes the waiters.
// The store read and the runner factory run without any lock held.
func (a *Agent) build(shard *shard, placeholder *session, ctx context.Context, sessionID, taskID string) {
	opened, err := a.openSession(ctx, shard, sessionID, taskID)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if a.closed.Load() {
		if err == nil {
			_ = opened.runner.Close()
		}
		err = ErrClosed
	}
	if placeholder.opening != nil {
		close(placeholder.opening)
		placeholder.opening = nil
	}
	if err != nil {
		placeholder.openErr = err
		return
	}
	placeholder.taskID = opened.taskID
	placeholder.runner = opened.runner
	placeholder.storeState(opened.loadState())
	placeholder.lastUsed = time.Now()
}

// openSession reads the store and builds the runner. It runs without any
// table lock: it is the slow half of opening a session.
func (a *Agent) openSession(ctx context.Context, shard *shard, sessionID, taskID string) (*session, error) {
	state := StateSubmitted
	resumed := false
	if a.opts.Store != nil {
		if taskID != "" {
			checkpoint, err := a.opts.Store.Load(ctx, taskID)
			switch {
			case err == nil:
				if a.opts.Identity != "" && checkpoint.Fingerprint != a.opts.Identity {
					return nil, fmt.Errorf("%w: task %q", ErrDeploymentChanged, taskID)
				}
				state = checkpoint.Task.State
				resumed = true
			case errors.Is(err, ErrNoCheckpoint):
				// A new task under the id the protocol issued.
			default:
				return nil, err
			}
		} else {
			checkpoint, err := a.opts.Store.LatestForSession(ctx, sessionID)
			switch {
			case err == nil && !checkpoint.Task.State.Terminal():
				if a.opts.Identity == "" || checkpoint.Fingerprint == a.opts.Identity {
					taskID = checkpoint.Task.ID
					state = checkpoint.Task.State
					resumed = true
				} else {
					// The session belongs to another deployment: it cannot
					// be continued here, so it is closed where it stands instead
					// of being ignored again on every open.
					a.abandon(ctx, checkpoint)
				}
			case err != nil && !errors.Is(err, ErrNoCheckpoint):
				return nil, err
			}
		}
	}
	if taskID == "" {
		taskID = shard.nextID(a.taskPrefix)
	}
	runner, err := a.opts.NewRunner(ctx, sessionID, taskID)
	if err != nil {
		return nil, err
	}
	if a.opts.Hooks.OnSession != nil {
		if err := a.opts.Hooks.OnSession(ctx, SessionInfo{
			Agent:     a.opts.Name,
			SessionID: sessionID,
			TaskID:    taskID,
			State:     state,
			Resumed:   resumed,
		}); err != nil {
			// The session is not in the table yet, so nothing else owns the runner.
			_ = runner.Close()
			return nil, err
		}
	}
	session := newSession(sessionID)
	session.taskID = taskID
	session.runner = runner
	session.storeState(state)
	return session, nil
}

// abandon closes a stored task this deployment cannot continue: it was produced
// under another deployment, so it is marked canceled rather than adopted on
// every open. The write is best effort — the task is already unusable here.
func (a *Agent) abandon(ctx context.Context, checkpoint Checkpoint) {
	if checkpoint.Task.State.Terminal() || a.opts.Store == nil {
		return
	}
	checkpoint.Task.State = StateCanceled
	if _, _, err := a.opts.Store.Save(ctx, checkpoint, nil); err != nil {
		telemetry.WarnErr(ctx, "agent: could not close a task from another deployment", err)
	}
}

// stopSession cancels a session: through its run context when a run is
// in flight, through the runner when it is idle.
func (a *Agent) stopSession(ctx context.Context, entry *session, reason string) error {
	shard := a.table.shard(entry.id)
	shard.mu.Lock()
	if entry.running {
		stop := entry.cancelRun
		shard.mu.Unlock()
		if stop != nil {
			stop()
		}
		return nil
	}
	shard.mu.Unlock()
	return entry.runner.Cancel(ctx, reason)
}

// waitOpening blocks until the session's runner is built.
func waitOpening(ctx context.Context, shard *shard, entry *session) error {
	shard.mu.Lock()
	wait := entry.opening
	shard.mu.Unlock()
	if wait == nil {
		return nil
	}
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release gives the slot back and records where the run left the Task.
func (a *Agent) release(entry *session) {
	state := entry.runner.State()
	shard := a.table.shard(entry.id)
	shard.mu.Lock()
	entry.storeState(state)
	entry.running = false
	entry.cancelRun = nil
	entry.lastUsed = time.Now()
	if entry.done != nil {
		close(entry.done)
		entry.done = nil
	}
	shard.end()
	shard.mu.Unlock()
}

// record keeps the session's state in step with the events it reports. It
// runs on the run's goroutine and takes no lock: State reads the same atomic.
func (a *Agent) record(entry *session, event Event) {
	if status, ok := event.(Status); ok {
		entry.storeState(status.State)
	}
}

// runningIDs lists the sessions with a run in flight, for a Drain that ran
// out of patience.
func (a *Agent) runningIDs() []string {
	var ids []string
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		for id, entry := range shard.entries {
			if entry.running {
				ids = append(ids, id)
			}
		}
		shard.mu.Unlock()
	}
	return ids
}

// sweep evicts idle sessions and then enforces the cap. It is the background
// loop's whole pass, and it costs nothing when no limit is configured.
func (a *Agent) sweep(now time.Time) []*session {
	if a.opts.IdleTimeout <= 0 && a.opts.MaxSessions <= 0 {
		return nil
	}
	evicted := a.evictIdle(now)
	return append(evicted, a.trim()...)
}

// evictIdle drops the sessions whose idle timeout has passed.
func (a *Agent) evictIdle(now time.Time) []*session {
	if a.opts.IdleTimeout <= 0 {
		return nil
	}
	var evicted []*session
	var evictedIDs []string
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		for id, entry := range shard.entries {
			if entry.running || entry.opening != nil || now.Sub(entry.lastUsed) < a.opts.IdleTimeout {
				continue
			}
			if shard.remove(id) != nil {
				evicted = append(evicted, entry)
				evictedIDs = append(evictedIDs, id)
			}
		}
		shard.mu.Unlock()
	}
	a.fireEvict(evictedIDs)
	return evicted
}

// trim evicts least-recently-used idle sessions until the table is within
// MaxSessions.
//
// It is the one limit the message path enforces, because a cap is a memory
// bound and a bound checked once a minute is not a bound. Within the cap it
// costs one atomic load.
func (a *Agent) trim() []*session {
	max := int64(a.opts.MaxSessions)
	if max <= 0 || a.table.count() <= max {
		return nil
	}
	var evicted []*session
	var evictedIDs []string
	for a.table.count() > max {
		victim := a.oldestIdle()
		if victim == nil {
			break
		}
		where := a.table.shard(victim.id)
		where.mu.Lock()
		if where.entries[victim.id] == victim {
			where.remove(victim.id)
			evicted = append(evicted, victim)
			evictedIDs = append(evictedIDs, victim.id)
		}
		where.mu.Unlock()
	}
	a.fireEvict(evictedIDs)
	return evicted
}

// fireEvict reports every session a pass dropped. It runs outside the table
// locks: a hook is host code and may call back into the agent.
func (a *Agent) fireEvict(ids []string) {
	if a.opts.OnEvict == nil {
		return
	}
	for _, id := range ids {
		a.opts.OnEvict(id)
	}
}

// oldestIdle finds the least recently used session that is not running.
// oldestIdle returns the least recently used idle session, or nil when every
// session is running or opening. The shard is derived from the session's id at
// the call site, so a caller cannot hold a session without its shard.
func (a *Agent) oldestIdle() *session {
	var oldest *session
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		for _, entry := range shard.entries {
			if entry.running || entry.opening != nil {
				continue
			}
			if oldest == nil || entry.lastUsed.Before(oldest.lastUsed) {
				oldest = entry
			}
		}
		shard.mu.Unlock()
	}
	return oldest
}

// newSession starts an entry in the submitted state.
func newSession(sessionID string) *session {
	entry := &session{id: sessionID, lastUsed: time.Now()}
	entry.storeState(StateSubmitted)
	return entry
}

// loadState and storeState are the atomic view of a session's lifecycle.
func (c *session) loadState() TaskState { return stateFromCode(c.state.Load()) }

func (c *session) storeState(state TaskState) { c.state.Store(stateCode(state)) }

// stateCode and stateFromCode pack the lifecycle into the atomic. A switch is
// cheaper than an interface box per Status event.
func stateCode(state TaskState) uint32 {
	switch state {
	case StateWorking:
		return 2
	case StateInputRequired:
		return 3
	case StateCompleted:
		return 4
	case StateFailed:
		return 5
	case StateCanceled:
		return 6
	case StateRejected:
		return 7
	case StateAuthRequired:
		return 8
	default:
		return 1 // submitted
	}
}

func stateFromCode(code uint32) TaskState {
	switch code {
	case 2:
		return StateWorking
	case 3:
		return StateInputRequired
	case 4:
		return StateCompleted
	case 5:
		return StateFailed
	case 6:
		return StateCanceled
	case 7:
		return StateRejected
	case 8:
		return StateAuthRequired
	default:
		return StateSubmitted
	}
}

// newTaskPrefix draws the random half of every task id this agent mints. It is
// drawn once, so the cost of an id is a counter increment plus a short
// conversion, not a syscall per task.
func newTaskPrefix() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "task-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "task-" + hex.EncodeToString(raw[:])
}
