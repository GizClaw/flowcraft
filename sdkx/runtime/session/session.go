package session

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

type activityKind uint8

const (
	activityTurn activityKind = iota
	activityPrompt
	activitySink
)

// Session owns conversational activity for one Key while borrowing all
// deployment and event-routing dependencies from the runtime.
type Session struct {
	key               Key
	instance          *deploy.Instance
	hostFactory       HostFactory
	router            *agent.StreamRouter
	sinkBuffer        int
	speculativeEvents int
	speculativeBytes  int

	startMu        sync.Mutex
	mu             sync.Mutex
	activeTurns    int
	activePrompts  int
	attachedSinks  int
	active         *Turn
	closing        bool
	activityNotify func(*Session)
	closeOnce      sync.Once
	closeErr       error
}

func newSession(
	key Key,
	instance *deploy.Instance,
	hostFactory HostFactory,
	router *agent.StreamRouter,
	sinkBuffer int,
	speculativeEvents int,
	speculativeBytes int,
	activityNotify func(*Session),
) *Session {
	return &Session{
		key:               key,
		instance:          instance,
		hostFactory:       hostFactory,
		router:            router,
		sinkBuffer:        sinkBuffer,
		speculativeEvents: speculativeEvents,
		speculativeBytes:  speculativeBytes,
		activityNotify:    activityNotify,
	}
}

// Start installs and asynchronously executes a new turn. A previous turn is
// interrupted and fully finalized before the replacement is constructed.
func (s *Session) Start(ctx context.Context, request agent.Request, sinks ...SinkSpec) (*Turn, error) {
	if s == nil {
		return nil, ErrSessionClosed
	}
	if ctx == nil {
		return nil, errdefs.Validationf("runtime session: Start context is required")
	}
	for _, spec := range sinks {
		if err := spec.Validate(); err != nil {
			return nil, err
		}
	}
	seen := make(map[string]struct{}, len(sinks))
	authorities := 0
	for _, spec := range sinks {
		if _, exists := seen[spec.ID]; exists {
			return nil, errdefs.Validationf("runtime session: duplicate SinkSpec.ID %q", spec.ID)
		}
		seen[spec.ID] = struct{}{}
		if spec.Authority == AuthorityAuthoritative {
			authorities++
		}
	}
	if authorities > 1 {
		return nil, errdefs.Validationf("runtime session: at most one authoritative sink is allowed per turn")
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}
	old := s.active
	s.mu.Unlock()
	s.changeActivity(activityTurn, 1)
	activityHeld := true
	defer func() {
		if activityHeld {
			s.changeActivity(activityTurn, -1)
		}
	}()
	if old != nil {
		_ = old.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput})
		_, _ = old.Wait(context.Background())
	}
	if err := ctx.Err(); err != nil {
		return nil, errdefs.FromContext(err)
	}

	runID, err := randomID()
	if err != nil {
		return nil, err
	}
	request.ContextID = s.key.ContextID
	request.RunID = runID
	turn := newTurn(s, runID, ctx)
	hostRequest := HostRequest{
		Key:        s.key,
		RunID:      runID,
		Interrupts: turn.interrupts,
		AskUser:    turn.askUser,
	}
	if err := hostRequest.Validate(); err != nil {
		turn.cancel()
		return nil, err
	}
	host, err := s.hostFactory.NewHost(turn.runCtx, hostRequest)
	if err != nil {
		turn.cancel()
		return nil, err
	}
	if isNil(host) {
		turn.cancel()
		return nil, errdefs.Internalf("runtime session: HostFactory returned nil Host")
	}
	turn.host = host

	attachments := make([]*queuedSink, 0, len(sinks))
	raw := make([]*queuedSink, 0, len(sinks))
	confirmed := make([]*queuedSink, 0, len(sinks))
	for _, spec := range sinks {
		size := spec.QueueSize
		if size == 0 {
			size = s.sinkBuffer
		}
		attachment := newQueuedSink(s, runID, spec, size)
		attachment.delivered = turn.sinkDelivered
		attachment.onDetach = func(err error) {
			turn.sinkDetached(spec.ID, err)
		}
		s.changeActivity(activitySink, 1)
		if spec.Visibility == VisibilityConfirmed {
			confirmed = append(confirmed, attachment)
			if spec.Authority == AuthorityAuthoritative {
				attachment.offered = turn.sinkOffered
				turn.configureAuthority(spec, size, attachment)
			}
		} else {
			raw = append(raw, attachment)
		}
		attachments = append(attachments, attachment)
		attachment.start()
	}
	coordinator := newStreamCoordinator(
		turn, raw, confirmed, s.speculativeEvents, s.speculativeBytes)
	detach, attachErr := s.router.Attach(
		runID,
		"runtime-session-"+runID,
		coordinator,
		agent.WithStreamRetainAfterRunEnd(),
	)
	if attachErr != nil {
		for _, attachment := range attachments {
			attachment.detach(attachErr)
		}
		turn.cancel()
		return nil, attachErr
	}
	turn.coordinator = coordinator
	turn.coordinatorDetach = detach
	turn.attachments = attachments

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		detach()
		for _, attachment := range attachments {
			attachment.detach(ErrSessionClosed)
		}
		turn.cancel()
		return nil, ErrSessionClosed
	}
	s.active = turn
	turn.mu.Lock()
	turn.state = TurnRunning
	turn.mu.Unlock()
	s.mu.Unlock()
	activityHeld = false

	go turn.execute(s.instance, request)
	return turn, nil
}

// Key returns this Session's immutable identity.
func (s *Session) Key() Key {
	if s == nil {
		return Key{}
	}
	return s.key
}

// changeActivity collects Turn, prompt, and sink transitions so Manager can
// re-evaluate idle reclamation.
func (s *Session) changeActivity(kind activityKind, delta int) {
	if s == nil || delta == 0 {
		return
	}
	s.mu.Lock()
	wasIdle := s.idleLocked()
	switch kind {
	case activityTurn:
		s.activeTurns += delta
		if s.activeTurns < 0 {
			s.activeTurns = 0
		}
	case activityPrompt:
		s.activePrompts += delta
		if s.activePrompts < 0 {
			s.activePrompts = 0
		}
	case activitySink:
		s.attachedSinks += delta
		if s.attachedSinks < 0 {
			s.attachedSinks = 0
		}
	}
	isIdle := s.idleLocked()
	notify := s.activityNotify
	s.mu.Unlock()

	if notify != nil && wasIdle != isIdle {
		notify(s)
	}
}

func (s *Session) isIdle() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idleLocked()
}

func (s *Session) idleLocked() bool {
	return !s.closing && s.activeTurns == 0 && s.activePrompts == 0 && s.attachedSinks == 0
}

func (s *Session) isClosing() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *Session) close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.beginClose()

		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active != nil {
			active.shutdown()
		}

		s.startMu.Lock()
		s.mu.Lock()
		active = s.active
		s.mu.Unlock()
		if active != nil {
			active.shutdown()
			_, s.closeErr = active.Wait(context.Background())
			if s.closeErr != nil && errors.Is(s.closeErr, context.Canceled) {
				s.closeErr = nil
			}
		}
		s.startMu.Unlock()
	})
	return s.closeErr
}

func (s *Session) beginClose() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
}

func (s *Session) turnFinished(turn *Turn) {
	s.mu.Lock()
	if s.active == turn {
		s.active = nil
	}
	s.mu.Unlock()
	s.changeActivity(activityTurn, -1)
}
