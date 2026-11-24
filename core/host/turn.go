package host

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/identity/inject"
	"github.com/GizClaw/flowcraft/core/message"
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
		if !session.running.CompareAndSwap(false, true) {
			yield(nil, errdefs.Conflictf("host: session %q already has a turn in flight", sessionID))
			return
		}
		defer session.running.Store(false)

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
		Bind:      s.deploy.grant.Digest(),
	})
	if err != nil {
		// A host that already attached an identity owns the trace; the turn runs under it.
		return ctx
	}
	return attached
}

// request assembles one turn's request.
//
// The task is the session's: a conversation continues the task it started, so its checkpoint key is
// the session id and its round count is what the turn ordinal is derived from.
func (a *Agents) request(ctx context.Context, session *Session, msg message.Message) (agent.Request, error) {
	session.ordinal++
	task := agent.Task{ID: session.id, SessionID: session.id, State: agent.StateWorking}
	if a.store != nil {
		loaded, err := a.store.LatestForSession(ctx, session.id)
		switch {
		case err == nil:
			task = loaded.Task
			task.State = agent.StateWorking
		case errors.Is(err, agent.ErrNoCheckpoint):
			// A new conversation: the task starts here.
		default:
			return agent.Request{}, fmt.Errorf("host: loading session %q: %w", session.id, err)
		}
	}
	return agent.Request{
		Agent:       session.agent,
		Task:        task,
		Input:       msg,
		Binding:     session.binding,
		Tools:       a.res.dispatch,
		Models:      a.res.models,
		Ordinal:     session.ordinal,
		Fingerprint: a.fingerprint,
		Store:       a.store,
		Hooks:       session.deploy.hooks,
		Trace:       session.deploy.trace,
	}, nil
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
	if !session.running.Load() {
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
func (a *Agents) Cancel(ctx context.Context, sessionID, reason string) error {
	session, ok := a.Session(sessionID)
	if !ok {
		return errdefs.NotFoundf("host: session %q is not open", sessionID)
	}
	cancel := session.takeCancel()
	if cancel == nil {
		return agent.ErrSessionIdle
	}
	cancel()
	if canceller, ok := session.driver.(agent.Canceller); ok {
		return canceller.Cancel(ctx, reason)
	}
	return nil
}
