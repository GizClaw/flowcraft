package agent

import (
	"context"
	"iter"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Program is what a deployment selects: the already-built thing that knows how to hold a
// conversation.
//
// It is the two-level half that is easy to miss. A program is compiled or connected once
// — a graph is parsed and validated, a peer is dialled, a loop's prompt is assembled — and
// every session of that deployment then runs through it. What a session gets is a
// [Driver]: a cheap object bound to one conversation.
//
// The split is what makes both sides simple. The program owns the expensive, shared,
// immutable part; the driver owns the cheap, per-conversation part (its in-flight queue,
// its cancellation). Neither holds the other's state, and "who closes what" has one
// answer each.
type Program interface {
	// Open binds this program to one session and returns its driver.
	//
	// It is called on the session's goroutine and must be cheap: compiling, dialling and
	// validating belong to the program's construction. A program is opened concurrently
	// by the sessions it serves, so Open must be safe for concurrent use.
	Open(info SessionInfo) (Driver, error)
	// Close releases what the program holds for every session — a connection, a compiled
	// artefact, a background goroutine. It runs when the deployment is removed, after its
	// last session has closed.
	Close() error
}

// Driver runs one session's turns.
//
// It is the per-session half of a [Program]: one instance per conversation, created by
// [Program.Open] and closed when the conversation ends. Because there is one instance per
// session, and a session has at most one turn in flight, the run-scoped verbs below need
// no key — the instance is the key.
//
// It owns no authority and no task state: what the session may do is in the [tool.Binding]
// the host attaches to every call, and where the task stands is in the checkpoint. A driver
// that held either would be a second source of truth for it.
type Driver interface {
	// Run handles req.Input, continuing from req.Task when it carries one and starting a
	// task when it is zero, and yields every event as it happens.
	//
	// Events must be yielded as they are produced: a caller may be streaming text to a
	// user, and the latency of a turn is the latency of the model.
	Run(ctx context.Context, req Request) iter.Seq2[Event, error]
	// Close releases what this conversation's driver holds. The task lives in the store,
	// so a closed driver can be rebuilt and resumed.
	Close() error
}

// Steerer is the optional capability of a driver that takes operator input mid-turn.
//
// It is an interface on the driver rather than a method on [Driver] because a driver with
// no step boundary to apply input at must say so, instead of accepting input it will never
// use. The host asks for it once, at the call site.
type Steerer interface {
	// Steer queues input for the next step boundary and returns its handle. A nil error
	// means queued, never applied: application is reported by an [Interrupted] event,
	// which is the only place it can be observed in order with the rest of the stream.
	Steer(input SteerInput) (SteerID, error)
	// Unsteer withdraws a queued input. Once the driver has applied it, it reports
	// [ErrSteerApplied]: the message is in the transcript, and the recourse is a new steer
	// rather than a rewrite.
	Unsteer(id SteerID) error
}

// Rounder is the optional capability of a driver that can say how far a task has got.
//
// The turn decorator writes the final checkpoint, and the projection's Round is the one
// column a reader uses to compare two deployments. A driver that steps (the graph) already
// writes its own projection and reports the same number; a driver that only observes a peer
// (a proxy) has nothing else to report, and leaving the field at zero made "how far did this
// task get" unanswerable for it.
type Rounder interface {
	// Round returns how far the task has got: the driver's own step count, or the number
	// of progress reports it has folded. A driver with nothing to say returns zero, which
	// leaves the projection's value alone.
	Round() int
}

// Canceller is the optional capability of a driver that must tell someone else about a
// cancellation.
//
// A local driver does not need it: cancelling the turn's context stops its steps. A driver
// that talks to a peer does, because dropping the context leaves the peer running.
type Canceller interface {
	// Cancel asks the peer to stop. A run that already finished reports nil.
	Cancel(ctx context.Context, reason string) error
}

// Request is everything one turn needs that the driver cannot build itself.
//
// Every field is either session-scoped (what this session may do) or process-scoped (how
// this process executes). Nothing is read from the context: the previous design let a host
// stuff the tool projection and the trace association into ctx, which made the turn depend
// on ambient state that no signature mentioned.
type Request struct {
	// Agent is the deployment name: the seat a host policy is bound to.
	Agent string
	// Task is where to continue; the zero value starts a new task.
	Task Task
	// Input is the message that triggered this turn.
	Input message.Message
	// Binding is the session's authority: identity, grant, tool projection and the
	// capability pocket, already narrowed for this session.
	Binding tool.Binding
	// Tools executes tool calls through the process chain, under Binding.
	Tools tool.Dispatcher
	// Models is the process's routed model access. It is not a permission: the grant's
	// model list is what decides which models this session may call, and the route
	// enforces it.
	Models *inference.Provider
	// Ordinal is this turn's 1-based position among the session's runs. The host counts
	// it: a driver sees one run and cannot know how many its session has had, and a
	// resumed process restarts the count.
	Ordinal int
	// Fingerprint identifies the deployment writing this checkpoint, so a resume can
	// refuse to continue under a different one instead of silently mixing two.
	Fingerprint Fingerprint
	// Store persists the task: the narrow half of the host's store, which is what a driver may
	// do with a task it does not own. Nil keeps the turn in memory only, which is what a test or
	// a stateless deployment wants.
	//
	// It is [DriverStore] and not [Store] on purpose: a driver writes the projection and the
	// facts beside it, and reading another task or deleting one is not part of running a turn.
	Store DriverStore
	// Hooks are the host's policy at the turn boundaries.
	Hooks Hooks
	// Trace is how much of this turn to record.
	Trace telemetry.TraceLevel
	// Bodies keeps the bytes of each model response, which is what a replay answers from. It is a
	// retention decision rather than a level: the answer is the user's content.
	Bodies bool
	// Manifest is the version combination this deployment runs, assembled by the host at
	// install time.
	//
	// It is a value rather than a string so the two consumers agree: the turn writes its id
	// into the run record, and a driver writes the same manifest into its checkpoint. A host
	// that computed an id for the record and let the driver name the manifest itself would be
	// recording two answers to one question.
	Manifest Manifest
	// OnRun receives one finished run, when the host installed a sink.
	//
	// It is called once per turn, after the terminal checkpoint is durable and before the
	// after-turn hooks, so a sink and a hook see the same answer. Nil keeps the run
	// in-process only, which is what an embedded caller that reads its own metrics wants.
	OnRun func(RunRecord)
}
