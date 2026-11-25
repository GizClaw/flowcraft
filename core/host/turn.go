package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/identity/inject"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// AgentsOption configures [NewAgents].
type AgentsOption func(*Agents)

// WithStore gives the table somewhere to keep sessions' tasks.
//
// Without one, a turn still runs and still streams, and nothing survives the process: that is a
// legitimate deployment for a demo and a broken one for anything else, so the choice is the host's
// and it is explicit.
func WithStore(store agent.Store) AgentsOption {
	return func(a *Agents) { a.store = store }
}

// WithIdentity declares the process facts every session's identity is made of.
//
// The tenant and the authority are the host's: no document states them, because they describe who
// owns this process rather than what a deployment asks for. Empty values leave the corresponding
// attribute off the span, which is honest — a process that does not know its tenant should not
// pretend otherwise.
func WithIdentity(tenant, authority string) AgentsOption {
	return func(a *Agents) {
		a.tenant = tenant
		a.authority = authority
	}
}

// WithApproval installs the host's rule for starting a command.
//
// Without one, a session's process capability is the backend itself: approval is opt-in, and a
// process that has not configured it has not said "yes" to anything it should not have.
func WithApproval(approval Approval) AgentsOption {
	return func(a *Agents) { a.approval = &approval }
}

// WithHarness names the build this process is.
//
// It is what makes a run record say *which binary* produced it — the one fact a deployment
// document cannot know, and the one a regression is most often attributable to. Empty leaves
// the manifest field out rather than inventing "unknown", so a reader can tell a build that
// did not say from a build that said nothing.
func WithHarness(harness string) AgentsOption {
	return func(a *Agents) { a.harness = harness }
}

// WithRunSink hands every finished turn to a sink.
//
// The record is the join key between a trace and whatever a host keeps beside it — a dataset
// row, a billing line, an evaluation harness — and it is built by the turn decorator, which is
// the only layer that sees the whole stream: the state, the classification and the usage totals
// are read from one fold, so a row cannot disagree with the span it names.
//
// The sink is called on the turn's goroutine, after the terminal checkpoint is durable and
// before the after-turn hooks. It must be cheap and must not fail the turn: the work it
// describes is already done, and a sink that blocked would delay the caller's last event.
func WithRunSink(sink func(agent.RunRecord)) AgentsOption {
	return func(a *Agents) { a.runSink = sink }
}

// WithFingerprint names the deployment set this process was built from.
//
// It travels into every checkpoint, so a resume can refuse a deployment that is not the one that
// wrote it. Empty disables the comparison, which is what a process without a digest should say
// rather than compare against nothing.
func WithFingerprint(fingerprint agent.Fingerprint) AgentsOption {
	return func(a *Agents) { a.fingerprint = fingerprint }
}

// Run runs one turn for a session and streams its events.
//
// The turn is assembled here and executed by [agent.RunTurn]: the binding, the tool table, the model
// access and the checkpoint store are all things this layer owns, and the driver is the only part
// that varies. That split is why a driver never sees a Resource, a table or a session id.
//
// One session has at most one turn in flight — the rule the session table states and [Agents.Steer]
// depends on — so a second concurrent Run is refused instead of queued.
func (a *Agents) Run(ctx context.Context, sessionID string, msg message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		session, ok := a.Session(sessionID)
		if !ok {
			yield(nil, errdefs.NotFoundf("host: session %q is not open", sessionID))
			return
		}
		if err := session.beginTurn(); err != nil {
			yield(nil, err)
			return
		}
		defer session.endTurn()

		runCtx, cancel := context.WithCancel(session.identity(ctx))
		session.setCancel(cancel)
		defer func() {
			session.setCancel(nil)
			cancel()
			session.touchNow(time.Now())
		}()

		req, err := a.request(runCtx, session, msg)
		if err != nil {
			yield(nil, err)
			return
		}

		// The failure box is installed before the run span opens, not inside RunTurn, so both
		// spans of this turn read the same classification: the decorator reuses a box it finds
		// rather than making a second one.
		runCtx = agent.WithFailureBox(runCtx)
		spanCtx, span := startRunSpan(runCtx, session)
		defer span.End()
		state, failures := agent.TaskState(""), 0
		for event, err := range agent.RunTurn(spanCtx, req, session.driver) {
			if status, ok := event.(agent.Status); ok && status.State.Terminal() {
				state = status.State
			}
			if err != nil {
				failures++
			}
			if !yield(event, err) {
				finishRunSpan(spanCtx, span, state, failures)
				return
			}
			if err != nil {
				finishRunSpan(spanCtx, span, state, failures)
				return
			}
		}
		finishRunSpan(spanCtx, span, state, failures)
	}
}

// identity returns ctx carrying this session's run identity.
//
// It is attached here, per turn, because that is where the agent finds it: [agent.RunTurn] adds the
// turn facts on top of an attached session, and every span below — the turn, the tool calls, the
// sandbox — renders the same identity. Attaching at open instead would make a session's second turn
// fail, since one trace carries one session and the injection refuses a second attach.
func (s *Session) identity(ctx context.Context) context.Context {
	attached, err := inject.Session(ctx, identity.Session{
		Tenant:    s.tenant,
		Agent:     s.agent,
		Authority: s.authority,
		SessionID: s.id,
		Bind:      s.bindID,
	})
	if err != nil {
		// A host that already attached an identity owns the trace; the turn runs under it.
		return ctx
	}
	if s.chain.ID == "" {
		return attached
	}
	// The chain goes on after the session, because that is the order the injection points
	// allow: a lineage is a fact about a run that already has a conversation. Failing here is a
	// programming error rather than a deployment one — the session either has a chain at open or
	// it does not — so the turn keeps the session identity and the loss is logged.
	withChain, chainErr := inject.Chain(attached, s.chain)
	if chainErr != nil {
		telemetry.WarnErr(ctx, "host: attaching the delegation lineage failed", chainErr)
		return attached
	}
	return withChain
}

// request assembles one turn's request.
//
// A task is one turn and a session is the conversation, so the task id is minted per turn — but only
// while the previous one is still open: a suspended task is continued under its own id, and a finished
// one is history the engine carries forward through the keys its document marks `carry`.
func (a *Agents) request(ctx context.Context, session *Session, msg message.Message) (agent.Request, error) {
	session.ordinal++
	task := agent.Task{ID: session.id, SessionID: session.id, State: agent.StateWorking}
	if a.store != nil {
		loaded, err := a.store.LatestForSession(ctx, session.id)
		switch {
		case err == nil && !loaded.Task.State.Terminal():
			task = loaded.Task
			task.State = agent.StateWorking
		case err == nil:
			// The previous turn ended: this one is a new task, and the conversation crosses the boundary
			// through the document's carried keys rather than through one endless run.
			id, err := newTaskID(session.id)
			if err != nil {
				return agent.Request{}, err
			}
			task.ID = id
		case errors.Is(err, agent.ErrNoCheckpoint):
			// A new conversation: the task starts here.
		default:
			return agent.Request{}, fmt.Errorf("host: loading session %q: %w", session.id, err)
		}
	}
	return agent.Request{
		Agent:   session.agent,
		Task:    task,
		Input:   msg,
		Binding: session.binding,
		Tools:   a.res.dispatch,
		// The model access is filtered by the session's own grant: a model the
		// session may not call is refused at the seam rather than by every driver
		// remembering to ask. See modelgate.go.
		Models:      a.modelAccess(session.binding.Grant),
		Ordinal:     session.ordinal,
		Fingerprint: a.fingerprint,
		// The host keeps the full store; the turn gets the half a driver may use.
		Store:  agent.DriverStoreOf(a.store),
		Hooks:  session.deploy.hooks,
		Trace:  session.deploy.observability.Trace,
		Bodies: session.deploy.observability.Bodies,
		// The full manifest stays on the request because a driver writes it into its
		// checkpoint; the id travels beside it so the turn does not hash it again.
		Manifest:   session.manifest,
		ManifestID: session.manifestID,
		OnRun:      a.runSink,
	}, nil
}

// newTaskID mints the id of a new task: the session's id plus a random suffix.
//
// "Fresh" is a durability requirement, not a readability one. The store keys tasks by id, so a
// repeated id merges two tasks' projections and interleaves their event logs — and every counter
// this could use restarts: the session's ordinal starts again when a conversation is opened (or
// the process is), which made the second turn after a reopen collide with a task the store still
// held. A random suffix has no such history, and nothing parses a task id.
func newTaskID(sessionID string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("host: mint a task id: %w", err)
	}
	return sessionID + "/" + hex.EncodeToString(suffix[:]), nil
}

// Steer hands operator input to the session's running turn.
//
// The driver decides whether it can take input at all — a driver with no step boundary says so by
// not implementing [agent.Steerer] — and the session table is what makes the input routable without
// a key: a session has at most one turn in flight.
func (a *Agents) Steer(ctx context.Context, sessionID string, in agent.SteerInput) (agent.SteerID, error) {
	session, ok := a.Session(sessionID)
	if !ok {
		recordSteer(ctx, in, steerIdle)
		return "", errdefs.NotFoundf("host: session %q is not open", sessionID)
	}
	if !session.isRunning() {
		recordSteer(ctx, in, steerIdle)
		return "", agent.ErrSessionIdle
	}
	steerer, ok := session.driver.(agent.Steerer)
	if !ok {
		recordSteer(ctx, in, steerUnsupported)
		return "", agent.ErrNotSteerable
	}
	id, err := steerer.Steer(in)
	if err != nil {
		recordSteer(ctx, in, steerFailed)
		return id, err
	}
	recordSteer(ctx, in, steerAccepted)
	return id, nil
}

// Cancel stops the session's running turn.
//
// The context is cancelled here, which is what stops a local driver's steps; a driver that talks to a
// peer also gets told, because dropping the context leaves the peer running.
//
// It opens a span of its own: the cancel happens on the caller's goroutine, in the trace of
// whoever asked, while the turn span it stops belongs to another goroutine. One trace then says
// who stopped the run and the other says what the run did with that — the turn adds an
// "agent.canceled" event beside its terminal status, and neither has to see the other's context.
func (a *Agents) Cancel(ctx context.Context, sessionID, reason string) error {
	session, ok := a.Session(sessionID)
	if !ok {
		recordCancel(ctx, "", cancelNotFound)
		return errdefs.NotFoundf("host: session %q is not open", sessionID)
	}
	ctx, span := telemetry.Start(ctx, telemetry.TracerWithSuffix("host"), spanCancel, telemetry.TraceNone,
		trace.WithAttributes(attribute.String(identity.AttrAgentName, session.agent)))
	span.SetAttributes(telemetry.AttrsFor(span, ctx)...)
	defer span.End()

	cancel := session.takeCancel()
	if cancel == nil {
		// Idle is not a failure of the caller's intent: the run it wanted to stop had
		// already finished. It is still worth counting, because a deployment whose operators
		// keep cancelling finished runs is a deployment racing its own turn loop.
		span.SetStatus(codes.Ok, "idle")
		recordCancel(ctx, session.agent, cancelIdle)
		return agent.ErrSessionIdle
	}
	cancel()
	outcome := cancelContext
	if canceller, ok := session.driver.(agent.Canceller); ok {
		if err := canceller.Cancel(ctx, reason); err != nil {
			span.SetAttributes(attribute.String(telemetry.AttrErrorKind, string(agent.FailureUnclassified)))
			span.SetStatus(codes.Error, "peer_refused")
			recordCancel(ctx, session.agent, cancelRefused)
			return err
		}
		outcome = cancelForwarded
	}
	span.SetStatus(codes.Ok, "")
	recordCancel(ctx, session.agent, outcome)
	return nil
}
