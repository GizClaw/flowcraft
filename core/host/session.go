package host

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"go.opentelemetry.io/otel/trace"
)

// The capability slots a session fills.
//
// OpenOption configures one session's open.
type OpenOption func(*openOptions)

type openOptions struct {
	chain identity.Chain
}

// WithChain records the delegation lineage the session runs under.
//
// It is the only way a session learns it is somebody's child: a caller-opened session has no
// chain, and a delegated edge is the only thing that knows the lineage. Without it the depth a
// grant's max_depth counts was always zero, so the limit never fired.
func WithChain(chain identity.Chain) OpenOption {
	return func(o *openOptions) { o.chain = chain }
}

// Session is one open conversation under one deployment.
//
// It is where the four nouns meet: the agent it belongs to, the cell that isolates it, the binding
// every tool call runs under, and the driver that runs its turns.
type Session struct {
	id        string
	agent     string
	tenant    string
	authority string
	deploy    *deployment
	cell      *cell
	binding   tool.Binding
	driver    agent.Driver
	// touch is when this session last did anything, in Unix nanos: idle eviction reads it from the
	// table's goroutine while a turn writes it from its own, so it is atomic rather than guarded.
	touch atomic.Int64
	// ordinal counts turns, which is what a turn's identity is derived from.
	ordinal int
	// chain is the delegation lineage this session runs under: empty for a session a caller opened,
	// one deeper for every edge below a delegate call.
	chain identity.Chain
	// turnMu guards the turn slot: whether one is in flight, how to stop it, and whether the
	// session is closing. One session has at most one turn, which is the rule that lets Steer and
	// Cancel address a turn without an id — and the reason a close has to coordinate with it
	// instead of just closing the driver underneath.
	turnMu sync.Mutex
	// running is true while a turn is in flight.
	running bool
	// cancel stops the running turn; nil when none is.
	cancel context.CancelFunc
	// closing is set by close before it stops the turn, so a turn that has not registered its
	// cancellation yet cannot start after the close decision.
	closing bool
	// turnDone is closed when the turn in flight ends. A close waits on it before releasing the
	// driver and the boundary the turn is using.
	turnDone chan struct{}
}

// beginTurn claims the session's turn slot, or reports why it cannot.
func (s *Session) beginTurn() error {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	switch {
	case s.closing:
		return errdefs.NotAvailablef("host: session %q is closing", s.id)
	case s.running:
		return errdefs.Conflictf("host: session %q already has a turn in flight", s.id)
	}
	s.running = true
	s.turnDone = make(chan struct{})
	return nil
}

// endTurn releases the slot and wakes a close that is waiting for the turn.
func (s *Session) endTurn() {
	s.turnMu.Lock()
	s.running = false
	done := s.turnDone
	s.turnDone = nil
	s.turnMu.Unlock()
	if done != nil {
		close(done)
	}
}

// isRunning reports whether a turn is in flight.
func (s *Session) isRunning() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return s.running
}

// setCancel records the running turn's cancellation, or clears it when the turn ends.
//
// A cancellation installed after a close has already decided to stop the session is applied
// immediately: otherwise a close that arrived between beginTurn and this call would wait for a
// turn nobody had told to stop.
func (s *Session) setCancel(cancel context.CancelFunc) {
	s.turnMu.Lock()
	s.cancel = cancel
	closing := s.closing
	s.turnMu.Unlock()
	if closing && cancel != nil {
		cancel()
	}
}

// takeCancel hands back the running turn's cancellation and forgets it, so a second Cancel is a
// no-op rather than a nil dereference.
func (s *Session) takeCancel() context.CancelFunc {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	cancel := s.cancel
	s.cancel = nil
	return cancel
}

// beginClose marks the session closing, stops the turn in flight and returns the channel that
// reports it ended (nil when nothing is running). The caller waits on it before releasing the
// driver and the boundary: a turn that is still streaming tools must not have them closed under it.
func (s *Session) beginClose() <-chan struct{} {
	s.turnMu.Lock()
	s.closing = true
	cancel := s.cancel
	done := s.turnDone
	s.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return done
}

// ID is the session's address.
func (s *Session) ID() string { return s.id }

// Agent is the deployment the session was opened under.
func (s *Session) Agent() string { return s.agent }

// Binding is the authority every call in this session runs under: what a host hands to
// [tool.Dispatcher.Execute] and what the turn assembler copies into the request.
func (s *Session) Binding() tool.Binding { return s.binding }

// Open opens a conversation under a deployment.
//
// The deployment is named explicitly rather than inferred: a session belongs to exactly one agent,
// and a host that guessed would be one that let a session drift between deployments.
//
// Everything expensive — the backend instance, the file handles, the driver — happens outside the
// table's lock, so opening one session never delays a turn in another.
func (a *Agents) Open(ctx context.Context, sessionID, agentName string, opts ...OpenOption) (*Session, error) {
	if sessionID == "" {
		return nil, errdefs.Validationf("host: a session needs an id")
	}
	var opening openOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&opening)
		}
	}
	deploy, err := a.admit(agentName, sessionID)
	if err != nil {
		return nil, err
	}

	session, _, err := a.buildSession(ctx, sessionID, agentName, deploy, opening)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	// The deployment may have been removed or reloaded while the boundary was being built: a
	// session opened under a program that is already gone would run against nothing.
	current, ok := a.entries[agentName]
	if a.closed || !ok || current != deploy {
		a.mu.Unlock()
		return nil, errors.Join(
			errdefs.NotAvailablef("host: agent %q changed while the session was opening", agentName),
			session.close())
	}
	// The limit is checked again here, not only in admit: the build happens outside the lock, so
	// two opens can both pass the first check and only this one is under the same lock as the
	// insert.
	if limit := current.grant.Limits.MaxSessions; limit > 0 && current.live >= limit {
		a.mu.Unlock()
		return nil, errors.Join(
			errdefs.NotAvailablef("host: agent %q holds %d sessions, which is its limit", agentName, limit),
			session.close())
	}
	if _, exists := a.sessions[sessionID]; exists {
		a.mu.Unlock()
		return nil, errors.Join(
			errdefs.Conflictf("host: session %q is already open", sessionID), session.close())
	}
	a.sessions[sessionID] = session
	a.byAge = append(a.byAge, session)
	deploy.live++
	a.mu.Unlock()
	recordSessionOpened(ctx, agentName)
	return session, nil
}

// admit answers the questions that need the table, and nothing else.
func (a *Agents) admit(agentName, sessionID string) (*deployment, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errdefs.NotAvailablef("host: the agent table is closed")
	}
	deploy, ok := a.entries[agentName]
	if !ok {
		return nil, errdefs.NotFoundf("host: agent %q is not deployed", agentName)
	}
	if limit := deploy.grant.Limits.MaxSessions; limit > 0 && deploy.live >= limit {
		return nil, errdefs.NotAvailablef(
			"host: agent %q holds %d sessions, which is its limit", agentName, limit)
	}
	if _, exists := a.sessions[sessionID]; exists {
		return nil, errdefs.Conflictf("host: session %q is already open", sessionID)
	}
	return deploy, nil
}

// buildSession builds one session's cell, binding and driver. It holds no lock.
func (a *Agents) buildSession(ctx context.Context, sessionID, agentName string, deploy *deployment, opening openOptions) (*Session, trace.Span, error) {
	// The span starts before the boundary is built, so a failure to build one is inside the session's
	// own trace rather than a span of its own.
	probe := &Session{id: sessionID, agent: agentName, tenant: a.tenant, authority: a.authority, deploy: deploy}
	ctx, span := startSessionSpan(ctx, probe)

	factory, ok := a.res.backendFactory(deploy.grant.Sandbox)
	if !ok {
		err := errdefs.NotAvailablef("host: sandbox %q is not configured", deploy.grant.Sandbox)
		failSessionSpan(ctx, span, err)
		return nil, span, err
	}
	built, err := openCell(ctx, factory, deploy.grant)
	if err != nil {
		failSessionSpan(ctx, span, err)
		return nil, span, err
	}

	capabilities := tool.NewCapSet()
	platform.TreeSlot.Provide(capabilities, built.tree)
	platform.ProcessSlot.Provide(capabilities,
		starterFor(built.runner, a.approval, agentName, sessionID))
	// The catalog is the process's: every session of this deployment searches the same discovery.
	if a.res.skills != nil {
		platform.SkillsSlot.Provide(capabilities, a.res.skills)
	}

	// The view is this conversation's own: what may be shown is the deployment's, but what this
	// session has discovered, how recently it used it and how much of its per-round budget it has
	// spent are facts of this conversation. One view per session is cheap because the index behind
	// it belongs to the deployment's factory and is shared.
	view := deploy.newView()
	if view == nil {
		openErr := errors.Join(
			errdefs.NotAvailablef("host: agent %q: the tool projection built nothing", agentName),
			built.Close())
		failSessionSpan(ctx, span, openErr)
		return nil, span, openErr
	}
	binding := tool.Bind(&sessionScratch{id: sessionID, values: map[string]any{}},
		deploy.grant).WithView(view).WithCaps(capabilities)

	driver, err := deploy.program.Open(agent.SessionInfo{
		Agent:     agentName,
		SessionID: sessionID,
	})
	if err != nil {
		openErr := errors.Join(errdefs.NotAvailablef("host: agent %q: %w", agentName, err), built.Close())
		failSessionSpan(ctx, span, openErr)
		return nil, span, openErr
	}
	session := &Session{
		id:        sessionID,
		agent:     agentName,
		tenant:    a.tenant,
		authority: a.authority,
		deploy:    deploy,
		cell:      built,
		binding:   binding,
		driver:    driver,
		chain:     opening.chain,
	}
	if err := a.announceSession(ctx, session); err != nil {
		openErr := errors.Join(err, session.close())
		failSessionSpan(ctx, span, openErr)
		return nil, span, openErr
	}
	session.touchNow(time.Now())
	// Delegation is filled only where the grant permits it: a session that may not delegate gets no
	// capability, and a delegate tool registered in the process is then refused at registration
	// instead of failing at the first call.
	if deploy.grant.Delegation != nil {
		head := &delegator{agents: a, parent: session}
		if limit := deploy.grant.Delegation.MaxConcurrent; limit > 0 {
			head.gate = make(chan struct{}, limit)
		}
		platform.DelegationSlot.Provide(capabilities, head)
	}
	span.End()
	return session, span, nil
}

// announceSession runs the deployment's OnSession hook for the conversation that is about to
// exist.
//
// It is the boundary the hook is documented on — "after a session is opened or restored, before
// its turn" — and the only place that knows both: the store read that says whether this is a
// resume happens here, once, rather than on every turn. An error refuses the open: a host that
// cannot prepare its memory should not run a turn blind.
func (a *Agents) announceSession(ctx context.Context, session *Session) error {
	hooks := session.deploy.hooks
	if hooks.OnSession == nil {
		return nil
	}
	info := agent.SessionInfo{Agent: session.agent, SessionID: session.id, State: agent.StateSubmitted}
	if a.store != nil {
		loaded, err := a.store.LatestForSession(ctx, session.id)
		switch {
		case err == nil && !loaded.Task.State.Terminal():
			info.State = loaded.Task.State
			info.TaskID = loaded.Task.ID
			info.Resumed = true
		case err == nil, errors.Is(err, agent.ErrNoCheckpoint):
			// A finished task is history, not a resume point; a missing one is a new conversation.
		default:
			return fmt.Errorf("host: session %q: reading its checkpoint: %w", session.id, err)
		}
	}
	if err := hooks.OnSession(ctx, info); err != nil {
		return fmt.Errorf("host: agent %q: on-session hook: %w", session.agent, err)
	}
	return nil
}

// Sessions lists the open session ids under one agent, oldest first.
func (a *Agents) Sessions(agentName string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.byAge))
	for _, session := range a.byAge {
		if session.agent == agentName {
			ids = append(ids, session.id)
		}
	}
	return ids
}

// Session returns an open session by id.
func (a *Agents) Session(sessionID string) (*Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[sessionID]
	return session, ok
}

// CloseSession closes one conversation: the driver first, because it may still hold a turn, then the
// cell that turn was running in.
func (a *Agents) CloseSession(ctx context.Context, sessionID string) error {
	a.mu.Lock()
	session, ok := a.sessions[sessionID]
	if !ok {
		a.mu.Unlock()
		return errdefs.NotFoundf("host: session %q is not open", sessionID)
	}
	drained := a.forgetLocked(session)
	a.mu.Unlock()
	recordSessionsClosed(ctx, session.agent, 1)
	err := session.close()
	if drained != nil {
		err = errors.Join(err, drained.close())
	}
	return err
}

// EvictIdle closes the sessions of one agent that have been idle past its limit, and returns the ids
// it closed.
//
// It is explicit rather than a background goroutine: eviction is a decision with an observable
// effect, so a caller that wants it on a timer puts this on one. A limit of zero keeps every session
// for the deployment's lifetime.
func (a *Agents) EvictIdle(ctx context.Context, agentName string, now time.Time) ([]string, error) {
	a.mu.Lock()
	deploy, ok := a.entries[agentName]
	if !ok {
		a.mu.Unlock()
		return nil, errdefs.NotFoundf("host: agent %q is not deployed", agentName)
	}
	idle := deploy.grant.Limits.Idle.Std()
	if idle <= 0 {
		a.mu.Unlock()
		return nil, nil
	}
	var expired []*Session
	var drained []*deployment
	for _, session := range a.byAge {
		if session.agent != agentName || now.Sub(session.lastTouch()) < idle {
			continue
		}
		// A turn in flight is not idle, however long it has been running: its session's last
		// touch is the previous turn's, so a slow model call would otherwise look like an
		// abandoned conversation and have its boundary torn out mid-command.
		if session.isRunning() {
			continue
		}
		expired = append(expired, session)
		if retired := a.forgetLocked(session); retired != nil {
			drained = append(drained, retired)
		}
	}
	a.mu.Unlock()
	recordSessionsClosed(ctx, agentName, len(expired))
	recordEvicted(ctx, agentName, len(expired))

	var errs []error
	ids := make([]string, 0, len(expired))
	for _, session := range expired {
		ids = append(ids, session.id)
		if err := session.close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, entry := range drained {
		if err := entry.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return ids, errors.Join(errs...)
}

// forgetLocked removes a session from the indexes and gives its slot back.
//
// It returns the deployment whose program can now be released, which happens when a reload retired
// it and this was the last session opened under it. The caller closes it outside the lock. The
// caller holds the lock.
func (a *Agents) forgetLocked(session *Session) *deployment {
	delete(a.sessions, session.id)
	for i, existing := range a.byAge {
		if existing == session {
			a.byAge = append(a.byAge[:i], a.byAge[i+1:]...)
			break
		}
	}
	if session.deploy.live > 0 {
		session.deploy.live--
	}
	if session.deploy.retired && session.deploy.live == 0 && session.deploy.program != nil {
		for i, entry := range a.retiring {
			if entry == session.deploy {
				a.retiring = append(a.retiring[:i], a.retiring[i+1:]...)
				break
			}
		}
		return session.deploy
	}
	return nil
}

// touchNow records activity, which is what idle eviction reads.
func (s *Session) touchNow(now time.Time) { s.touch.Store(now.UnixNano()) }

// lastTouch is the last recorded activity.
func (s *Session) lastTouch() time.Time { return time.Unix(0, s.touch.Load()) }

// close releases the session's driver and its boundary, after stopping and waiting out the turn
// that may be using them.
func (s *Session) close() error {
	if done := s.beginClose(); done != nil {
		<-done
	}
	var errs []error
	if s.driver != nil {
		if err := s.driver.Close(); err != nil {
			errs = append(errs, err)
		}
		s.driver = nil
	}
	if s.cell != nil {
		if err := s.cell.Close(); err != nil {
			errs = append(errs, err)
		}
		s.cell = nil
	}
	return errors.Join(errs...)
}

// sessionScratch is the per-session store a tool sees. dispatch partitions it by tool name, so one
// map per session is all this has to be.
type sessionScratch struct {
	id     string
	mu     sync.Mutex
	values map[string]any
}

func (s *sessionScratch) ID() string { return s.id }

func (s *sessionScratch) Scratch() tool.Scratch { return scratchView{s} }

type scratchView struct{ s *sessionScratch }

func (v scratchView) Load(key string) (any, bool) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	value, ok := v.s.values[key]
	return value, ok
}

func (v scratchView) Store(key string, value any) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	v.s.values[key] = value
}

func (v scratchView) Delete(key string) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	delete(v.s.values, key)
}

var _ tool.Session = (*sessionScratch)(nil)
