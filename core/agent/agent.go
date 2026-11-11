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
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"

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
	// Store makes sessions resumable. Nil keeps them in memory only.
	Store Store
	// Identity is the fingerprint a checkpoint must carry to be resumed.
	// Empty disables the check.
	Identity Fingerprint
	// Trace controls the spans this agent emits. Zero emits none; a durable
	// host usually asks for TraceNode so a run, its frontiers and its nodes
	// are attributable. The cost is a span per unit, not per event.
	Trace telemetry.TraceLevel
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
}

// NewAgent builds the session table. It does not open anything: a runner is
// constructed on the first message of its session.
func NewAgent(opts AgentOptions) (*Agent, error) {
	if opts.NewRunner == nil {
		return nil, errors.New("agent: NewRunner is required")
	}
	return &Agent{
		opts:       opts,
		table:      newTable(tableShards),
		taskPrefix: newTaskPrefix(),
	}, nil
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
		entry, runCtx, cancel, err := a.acquire(ctx, sessionID, settings.taskID)
		if err != nil {
			yield(nil, err)
			return
		}
		defer a.release(entry)
		defer cancel()
		if a.opts.RunContext != nil {
			runCtx = a.opts.RunContext(runCtx, sessionID)
		}
		var span trace.Span
		if a.opts.Trace.Allows(telemetry.TraceRun) {
			runCtx, span = telemetry.TracerWithSuffix("agent").Start(runCtx, "agent.run",
				trace.WithAttributes(runctx.Attrs(runCtx)...))
			defer span.End()
		}

		for event, runErr := range entry.runner.Run(runCtx, msg) {
			if runErr != nil {
				if span != nil {
					span.RecordError(runErr)
					span.SetStatus(codes.Error, "")
				}
				yield(nil, runErr)
				return
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
	// keep every session's runner reachable.
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		shard.entries = make(map[string]*session)
		shard.mu.Unlock()
	}
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
			// Sweep after the slot is taken: the entry in hand is running, so no
			// pass can evict what this call is about to use.
			evicted = append(evicted, a.sweep(time.Now())...)
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
	if a.opts.Store != nil {
		if taskID != "" {
			checkpoint, err := a.opts.Store.Load(ctx, taskID)
			switch {
			case err == nil:
				if a.opts.Identity != "" && checkpoint.Fingerprint != a.opts.Identity {
					return nil, fmt.Errorf("%w: task %q", ErrDeploymentChanged, taskID)
				}
				state = checkpoint.Task.State
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

// sweep evicts idle sessions and trims the table to MaxSessions. It runs
// only when a limit is configured, so an unbounded table costs nothing per run.
func (a *Agent) sweep(now time.Time) []*session {
	if a.opts.IdleTimeout <= 0 && a.opts.MaxSessions <= 0 {
		return nil
	}
	var evicted []*session
	var evictedIDs []string
	total := 0
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		if a.opts.IdleTimeout > 0 {
			for id, entry := range shard.entries {
				if entry.running || entry.opening != nil || now.Sub(entry.lastUsed) < a.opts.IdleTimeout {
					continue
				}
				shard.remove(id)
				evicted = append(evicted, entry)
				evictedIDs = append(evictedIDs, id)
			}
		}
		total += len(shard.entries)
		shard.mu.Unlock()
	}
	for a.opts.MaxSessions > 0 && total > a.opts.MaxSessions {
		victim, where := a.oldestIdle()
		if where == nil {
			break
		}
		where.mu.Lock()
		if where.entries[victim.id] == victim {
			where.remove(victim.id)
			total--
			evicted = append(evicted, victim)
			evictedIDs = append(evictedIDs, victim.id)
		}
		where.mu.Unlock()
	}
	if a.opts.OnEvict != nil {
		for _, id := range evictedIDs {
			a.opts.OnEvict(id)
		}
	}
	return evicted
}

// oldestIdle finds the least recently used session that is not running.
func (a *Agent) oldestIdle() (*session, *shard) {
	var oldest *session
	var where *shard
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		for _, entry := range shard.entries {
			if entry.running || entry.opening != nil {
				continue
			}
			if oldest == nil || entry.lastUsed.Before(oldest.lastUsed) {
				oldest, where = entry, shard
			}
		}
		shard.mu.Unlock()
	}
	return oldest, where
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
