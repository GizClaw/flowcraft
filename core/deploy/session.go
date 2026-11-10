package deploy

import (
	"context"
	"iter"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Session is one session of a deployment: the handle a host drives.
//
// A deployed agent is a thing; a session is what carries a message, so the
// verbs live here. The id is bound once instead of repeating in every call.
type Session struct {
	agent     *agent.Agent
	sessionID string
}

// ID is the session's address.
func (c *Session) ID() string { return c.sessionID }

// Run handles one message: it starts the session's Task, or answers the
// question it paused on. One consumer per turn.
func (c *Session) Run(ctx context.Context, msg message.Message, opts ...agent.TurnOption) iter.Seq2[agent.Event, error] {
	return c.agent.Run(ctx, c.sessionID, msg, opts...)
}

// State reports where the session's Task stands.
func (c *Session) State(ctx context.Context) (agent.TaskState, error) {
	return c.agent.State(ctx, c.sessionID)
}

// Cancel stops the session's Task.
func (c *Session) Cancel(ctx context.Context, reason string) error {
	return c.agent.Cancel(ctx, c.sessionID, reason)
}

// protocolSession is one session as the protocol sees it: Run, Cancel
// and the state it reports. Close is the host's, not the protocol's.
type protocolSession struct {
	*Session
	taskID string
	// ctx is the request's; State may reach the store, and an unbounded lookup
	// would outlive the request that needed it.
	ctx context.Context
}

func (p *protocolSession) State() agent.TaskState {
	ctx := p.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	state, _ := p.Session.State(ctx)
	return state
}

func (p *protocolSession) Run(ctx context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	return p.Session.Run(ctx, msg, agent.WithTaskID(p.taskID))
}

func (p *protocolSession) Close() error { return nil }
