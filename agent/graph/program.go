package graph

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// Program is a compiled graph: the document plus the decisions that belong to the deployment rather
// than to any one session.
//
// It is built once and shared by every session of that deployment — compiling, validating the edges
// and registering the node kinds all happen in [Compile] — while a session gets a [driver], which is
// cheap because the graph is already there.
type Program struct {
	g    *Graph
	opts ProgramOptions
}

// ProgramOptions are the deployment's decisions about one compiled graph.
type ProgramOptions struct {
	// Manifest is the version combination this program records with each run.
	Manifest agent.Manifest
	// Replay answers model calls from the task's recorded responses instead of calling out. A miss
	// is an error, which is what makes it useful for evidence rather than for a demo.
	Replay bool
}

// manifestOf is which manifest a run records: the host's, when the turn carried one.
//
// The two must agree, because they are the same claim read from two sides — the checkpoint
// section a resume reads and the run record a dataset stores. A host assembles the manifest
// once per deployment and hands it to every turn, so it wins; WithManifest is the fallback
// for a caller that has no host to ask.
func manifestOf(declared, fromTurn agent.Manifest) agent.Manifest {
	if fromTurn != (agent.Manifest{}) {
		return fromTurn
	}
	return declared
}

// ProgramOption configures a Program.
type ProgramOption func(*ProgramOptions)

// WithProgramManifest records the version combination a run executed under.
func WithProgramManifest(manifest agent.Manifest) ProgramOption {
	return func(o *ProgramOptions) { o.Manifest = manifest }
}

// ReplayProgram answers model calls from the task's records instead of calling out.
func ReplayProgram() ProgramOption { return func(o *ProgramOptions) { o.Replay = true } }

// NewProgram wraps a compiled graph.
func NewProgram(g *Graph, opts ...ProgramOption) *Program {
	var o ProgramOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &Program{g: g, opts: o}
}

// Open binds the program to one session.
//
// It cannot fail: nothing is compiled or dialled here, because the graph was compiled when the
// deployment was installed. The SessionInfo is kept for the run's identity, which the graph records
// with every step.
func (p *Program) Open(info agent.SessionInfo) (agent.Driver, error) {
	return &driver{program: p, session: info.SessionID}, nil
}

// Close releases the program. A graph holds the document and nothing else, so there is nothing to
// release; the method exists because the contract asks for it.
func (p *Program) Close() error { return nil }

// driver runs one session's turns over the shared program.
//
// It owns the one run that session may have in flight, which is what lets it address a queued steer
// without a key: the session table refuses a second turn, so there is never more than one.
type driver struct {
	program *Program
	session string

	mu  sync.Mutex
	run *Run
	// steer is the input waiting for the run's next step boundary, and its handle. The graph applies
	// one steer per boundary, so a second one queued before that boundary is refused rather than
	// silently replacing the first.
	steer *queuedSteer
	seq   int
}

type queuedSteer struct {
	id    agent.SteerID
	input agent.SteerInput
}

var (
	_ agent.Driver  = (*driver)(nil)
	_ agent.Steerer = (*driver)(nil)
	_ agent.Rounder = (*driver)(nil)
	_ agent.Program = (*Program)(nil)
)

// Round reports how many steps the current task has taken.
//
// The run writes the same number into its own projection on every step, so this exists for the
// terminal save the decorator makes: one answer for "how far did this task get", written by
// whoever writes the checkpoint last.
func (d *driver) Round() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.run == nil {
		return 0
	}
	return d.run.step
}

// Run handles one turn: it starts the session's task, or continues one the graph already holds.
//
// A run that stopped to ask is not continued here: its input would be taken as the answer to a
// question this caller never saw, which is what [agent.ErrAwaitingResume] reports. [driver.Resume]
// is the verb for that, and the host calls it only when a caller means to answer.
//
// Everything the turn needs arrives in the request — the store, the fingerprint, the trace level, the
// retention of response bodies — because the driver is bound to the session, not to the process.
func (d *driver) Run(ctx context.Context, req agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		run, err := d.runFor(ctx, req, true)
		if err != nil {
			yield(nil, err)
			return
		}
		switch run.State() {
		case agent.StateInputRequired:
			yield(nil, agent.ErrAwaitingResume)
		case agent.StateSubmitted:
			run.advance(ctx, yield)
		case agent.StateWorking:
			yield(nil, fmt.Errorf("graph: the task is already running"))
		default:
			yield(nil, fmt.Errorf("graph: the task is %s; start a new one", run.State()))
		}
		d.forgetSteerWhenDone(run)
	}
}

// Resume continues the run the graph stopped on, with the answer the caller supplies.
//
// It restores the run the same way Run does, so a conversation that was resumed from the store can
// be answered in a new process: the checkpoint carries the suspended step, and nothing the run had
// already executed is replayed.
func (d *driver) Resume(ctx context.Context, req agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		run, err := d.runFor(ctx, req, false)
		if err != nil {
			yield(nil, err)
			return
		}
		if run.State() != agent.StateInputRequired {
			// Nothing to continue: answering a run that never asked would be a second way to feed it
			// input, which is the pair of verbs this split exists to keep apart.
			yield(nil, errdefs.Conflictf(
				"graph: the run is %s, not waiting for an answer", run.State()))
			return
		}
		for event, resumeErr := range run.Resume(ctx, req.Input.Content) {
			if !yield(event, resumeErr) {
				return
			}
		}
		d.forgetSteerWhenDone(run)
	}
}

// Paused reports whether the run stopped to ask.
//
// A driver with no run loaded reports false: a conversation restored from the store has not started
// here yet, and the checkpoint's state is what says whether it is waiting.
func (d *driver) Paused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run != nil && d.run.State() == agent.StateInputRequired
}

// forgetSteerWhenDone drops the mirror of the input slot once the run can take no more input.
func (d *driver) forgetSteerWhenDone(run *Run) {
	if !run.State().Terminal() {
		return
	}
	d.mu.Lock()
	d.steer = nil
	d.mu.Unlock()
}

// runFor returns the run this turn belongs to, opening one when the turn starts a new task.
//
// seedInput decides whether the request's message becomes the run's first input: a run that is
// being started takes it, and a run that is being resumed must not, because the answer travels in
// the request the resume hands to [graph.Run.Resume].
func (d *driver) runFor(ctx context.Context, req agent.Request, seedInput bool) (*Run, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	run := d.run
	if run != nil && (run.State().Terminal() || run.taskID != req.Task.ID) {
		// A task is one turn. A finished run is history, and a run under another task id is a
		// different turn of the same conversation: either way this message opens a new run, and the
		// keys the document marks `carry` are what cross the boundary.
		run = nil
		d.run = nil
	}
	if run != nil {
		return run, nil
	}
	// A document that declares no input slot takes the message as the trigger only: there is
	// nowhere to seed it, and inventing a slot would change what the document said.
	var seed []message.Message
	if seedInput && d.program.g.input != "" {
		seed = []message.Message{req.Input}
	}
	started, err := d.program.g.Start(ctx, seed,
		WithBinding(req.Binding),
		WithTools(req.Tools),
		WithModels(req.Models),
		WithStore(req.Store, req.Task.ID),
		WithSession(d.session),
		WithFingerprint(req.Fingerprint),
		WithManifest(manifestOf(d.program.opts.Manifest, req.Manifest)),
		WithTrace(req.Trace),
		WithBodies(req.Bodies),
		WithReplay(d.program.opts.Replay),
	)
	if err != nil {
		return nil, err
	}
	d.run = started
	return started, nil
}

// Steer queues a message for the run's next step boundary and returns its handle.
//
// The current node is not interrupted: a long model call finishes first, and the message reaches the
// graph at the beginning of the next frontier. A session with no run in flight reports
// [agent.ErrSessionIdle] — the host checks the same thing, but a driver called directly has to say it
// too.
func (d *driver) Steer(in agent.SteerInput) (agent.SteerID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	run := d.run
	if run == nil {
		return "", agent.ErrSessionIdle
	}
	switch run.State() {
	case agent.StateInputRequired:
		return "", agent.ErrSessionIdle
	case agent.StateCompleted, agent.StateFailed, agent.StateCanceled, agent.StateRejected:
		return "", agent.ErrRunEnded
	}
	if d.steer != nil {
		return "", fmt.Errorf("graph: a steer is already queued for the next boundary")
	}
	// The handle is minted before the run sees the input, so the run can name the same handle in the
	// receipt it emits when the input never reaches the transcript.
	d.seq++
	id := agent.SteerID(fmt.Sprintf("graph-%d", d.seq))
	if _, err := run.queueSteer(id, in.Message, in.Intent); err != nil {
		return "", err
	}
	d.steer = &queuedSteer{id: id, input: in}
	return id, nil
}

// Unsteer withdraws a queued steer. Once the graph has applied it, the message is in the transcript
// and the recourse is a new steer rather than a rewrite.
func (d *driver) Unsteer(id agent.SteerID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.steer == nil || d.steer.id != id {
		return agent.ErrSteerUnknown
	}
	applied := !d.run.withdrawSteer()
	d.steer = nil
	if applied {
		return agent.ErrSteerApplied
	}
	return nil
}

// AcceptsInput reports whether a message offered between turns reaches the conversation.
//
// The document decides it: a graph with no input slot takes a turn's message as its trigger and
// nothing else, so a delivery that arrived while this session was idle has nowhere to land.
func (d *driver) AcceptsInput() bool { return d.program.g.input != "" }

// Close releases the driver. The task lives in the store, so a closed driver can be rebuilt and
// resumed from the checkpoint.
func (d *driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.run = nil
	d.steer = nil
	return nil
}
