package realm

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/actor"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// ErrActorStopped is returned by Send when the actor has been stopped.
var ErrActorStopped = actor.ErrStopped

// actorContext is a request delivered to an AgentActor's mailbox.
type actorContext struct {
	agent *model.Agent
	req   *workflow.Request
}

// RunResult is the outcome of a single execution.
type RunResult = actor.Result[*workflow.Result]

// ActorOption configures an AgentActor.
type ActorOption func(*actorOpts)

type actorOpts struct {
	ctx        context.Context
	persistent bool
	inboxSize  int
	source     string
}

// WithPersistent marks the actor as long-lived (never reaped by idle timeout).
func WithPersistent() ActorOption {
	return func(o *actorOpts) { o.persistent = true }
}

// WithInboxSize overrides the default mailbox buffer size.
func WithInboxSize(n int) ActorOption {
	return func(o *actorOpts) { o.inboxSize = n }
}

// WithSource tags the actor with a creation source (e.g. "chat", "gateway", "kanban").
func WithSource(source string) ActorOption {
	return func(o *actorOpts) { o.source = source }
}

// WithActorContext sets a parent context for the inner actor.
func WithActorContext(ctx context.Context) ActorOption {
	return func(o *actorOpts) { o.ctx = ctx }
}

// AgentActor is an agent-scoped execution unit that delegates to an inner
// actor.Actor for serial mailbox processing.
type AgentActor struct {
	realmID string
	agentID string
	bus     event.EventBus
	inner   *actor.Actor[actorContext, *workflow.Result]
}

// ActorKey returns the stable actor identifier.
func (a *AgentActor) ActorKey() string { return a.agentID }

// RealmID returns the owning realm ID.
func (a *AgentActor) RealmID() string { return a.realmID }

// AgentID returns the agent ID this actor is bound to.
func (a *AgentActor) AgentID() string { return a.agentID }

// Source returns the creation source tag.
func (a *AgentActor) Source() string { return a.inner.Source() }

// RunFunc is the execution function signature used by AgentActor.
type RunFunc func(ctx context.Context, agent *model.Agent, req *workflow.Request) (*workflow.Result, error)

// NewAgentActor creates and starts a new AgentActor.
func NewAgentActor(realmID, agentID string, bus event.EventBus, runFn RunFunc, opts ...ActorOption) *AgentActor {
	o := &actorOpts{}
	for _, opt := range opts {
		opt(o)
	}

	var innerOpts []actor.Option
	if o.persistent {
		innerOpts = append(innerOpts, actor.WithPersistent())
	}
	if o.inboxSize > 0 {
		innerOpts = append(innerOpts, actor.WithInboxSize(o.inboxSize))
	}
	if o.source != "" {
		innerOpts = append(innerOpts, actor.WithSource(o.source))
	}
	if o.ctx != nil {
		innerOpts = append(innerOpts, actor.WithContext(o.ctx))
	}

	a := &AgentActor{
		realmID: realmID,
		agentID: agentID,
		bus:     bus,
	}

	handler := func(ctx context.Context, msg actorContext) (*workflow.Result, error) {
		return runFn(ctx, msg.agent, msg.req)
	}

	a.inner = actor.New(handler, innerOpts...)
	return a
}

// Send delivers a run request to the actor's mailbox.
func (a *AgentActor) Send(agent *model.Agent, req *workflow.Request) <-chan RunResult {
	return a.inner.Send(actorContext{agent: agent, req: req})
}

// Bus returns the realm-scoped persistent EventBus.
func (a *AgentActor) Bus() event.EventBus { return a.bus }

// IsRunning reports whether the actor is currently executing a request.
func (a *AgentActor) IsRunning() bool { return a.inner.IsRunning() }

// IsPersistent reports whether the actor is exempt from idle reaping.
func (a *AgentActor) IsPersistent() bool { return a.inner.IsPersistent() }

// LastActive returns the time of the last activity.
func (a *AgentActor) LastActive() time.Time { return a.inner.LastActive() }

// Abort cancels the currently running request without stopping the actor.
func (a *AgentActor) Abort() bool { return a.inner.Abort() }

// Stop cancels the actor's context and signals the mailbox to drain.
func (a *AgentActor) Stop() { a.inner.Stop() }
