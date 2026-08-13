package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	sdktool "github.com/GizClaw/flowcraft/core/tool"
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
	key                 Key
	instance            *agent.Agent
	hostFactory         HostFactory
	router              *event.Router
	sinkBuffer          int
	speculativeEvents   int
	speculativeBytes    int
	deliveryConcurrency int
	checkpoints         agent.CheckpointStore
	resume              bool
	catalogProvider     CatalogProvider

	startMu        sync.Mutex
	mu             sync.Mutex
	activeTurns    int
	activePrompts  int
	attachedSinks  int
	active         *Turn
	closing        bool
	catalog        sdktool.Session
	activityNotify func(*Session)
	observer       SessionObserver
	closeOnce      sync.Once
	closeErr       error
}

func newSession(
	key Key,
	instance *agent.Agent,
	hostFactory HostFactory,
	router *event.Router,
	sinkBuffer int,
	speculativeEvents int,
	speculativeBytes int,
	deliveryConcurrency int,
	checkpoints agent.CheckpointStore,
	resume bool,
	catalogProvider CatalogProvider,
	activityNotify func(*Session),
	observer SessionObserver,
) *Session {
	return &Session{
		key:                 key,
		instance:            instance,
		hostFactory:         hostFactory,
		router:              router,
		sinkBuffer:          sinkBuffer,
		speculativeEvents:   speculativeEvents,
		speculativeBytes:    speculativeBytes,
		deliveryConcurrency: deliveryConcurrency,
		checkpoints:         checkpoints,
		resume:              resume,
		catalogProvider:     catalogProvider,
		activityNotify:      activityNotify,
		observer:            observer,
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
	startLocked := true
	defer func() {
		if startLocked {
			s.startMu.Unlock()
		}
	}()

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
		_, _ = old.Wait(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, errdefs.FromContext(err)
	}

	runID, err := s.nextRunID()
	if err != nil {
		return nil, err
	}
	resumeFrom, resumeCtx, err := s.loadResume(ctx, runID)
	if err != nil {
		return nil, err
	}
	request.ContextID = s.key.ContextID
	request.RunID = runID
	turn := newTurn(s, runID, ctx)
	turn.resumeFrom = resumeFrom
	turn.resumeCtx = resumeCtx
	catalog, err := s.catalogFor(ctx)
	if err != nil {
		turn.cancel()
		return nil, err
	}
	if catalog != nil {
		turn.runCtx = sdktool.WithSession(turn.runCtx, catalog)
	}
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
		attachment.setDeliveryConcurrency(s.deliveryConcurrency)
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
	// Single subscription to the whole run namespace: `>` matches both
	// the run lifecycle events (run-end delimits attempts) and the
	// stream deltas. Two subscriptions would deliver every stream delta
	// twice.
	detach, attachErr := s.router.Attach(
		context.Background(),
		agent.PatternRun(runID),
		coordinator,
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

	startLocked = false
	s.startMu.Unlock()
	s.notifySessionStarted(turn)
	go turn.execute(s.instance, request)
	return turn, nil
}

// nextRunID returns the stable run id for the session key when resume
// is enabled, otherwise a fresh random id.
func (s *Session) nextRunID() (string, error) {
	if s.resume {
		return stableRunID(s.key), nil
	}
	return randomID()
}

// loadResume loads the checkpoint for runID when resume is enabled.
// It returns nil, nil for a fresh start. A checkpoint whose engine
// cannot resume is a configuration error, not a silent fresh run.
func (s *Session) loadResume(
	ctx context.Context,
	runID string,
) (*agent.Checkpoint, *agent.ResumeContext, error) {
	if !s.resume || s.checkpoints == nil {
		return nil, nil, nil
	}
	cp, err := s.checkpoints.Load(ctx, runID)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"runtime session: load checkpoint %s: %w", runID, err)
	}
	if cp == nil {
		return nil, nil, nil
	}
	if !agent.IsResumable(s.instance.Engine) {
		return nil, nil, errdefs.NotAvailablef(
			"runtime session: engine %T does not support resume but checkpoint %s exists",
			s.instance.Engine, runID)
	}
	if resumer, ok := agent.AsResumer(s.instance.Engine); ok {
		if err := resumer.CanResume(*cp); err != nil {
			return nil, nil, fmt.Errorf(
				"runtime session: checkpoint %s is not resumable: %w", runID, err)
		}
	}
	startedAt := cp.OriginalStartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	resumeCtx := agent.ResumeContext{
		Attempt:      2,
		StartedAt:    startedAt,
		Signal:       "session",
		CheckpointAt: cp.Timestamp,
	}
	return cp, &resumeCtx, nil
}

// cleanupCheckpoint removes a committed run's checkpoint so the next
// turn starts fresh. Interrupted, failed, and canceled runs keep
// theirs. Deletion is best-effort.
func (s *Session) cleanupCheckpoint(runID string, result *agent.Result) {
	if s == nil || !s.resume || s.checkpoints == nil ||
		result == nil || !result.Committed {
		return
	}
	deleter, ok := s.checkpoints.(agent.CheckpointDeleter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	_ = deleter.Delete(ctx, runID)
}

// stableRunID derives the logical run id for one session key. It is
// stable across process restarts so resume can find the checkpoint.
func stableRunID(key Key) string {
	sum := sha256.Sum256([]byte(key.AgentID + "\x00" + key.ContextID))
	return "run-" + hex.EncodeToString(sum[:16])
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
		s.notifySessionClosed(s.closeErr)
	})
	return s.closeErr
}

// catalogFor returns the session catalog, creating it once via the
// provider on the first Start. Start is serialized by startMu, so the
// creation path is race-free; the defensive branch handles a provider
// shared with code paths outside this Session.
func (s *Session) catalogFor(ctx context.Context) (sdktool.Session, error) {
	s.mu.Lock()
	catalog := s.catalog
	s.mu.Unlock()
	if catalog != nil {
		return catalog, nil
	}
	if s.catalogProvider == nil {
		return nil, nil
	}
	catalog, err := s.catalogProvider.NewCatalog(ctx, s.instance)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, errdefs.Internalf(
			"runtime session: CatalogProvider returned a nil catalog")
	}
	s.mu.Lock()
	if s.catalog == nil {
		s.catalog = catalog
	}
	s.mu.Unlock()
	return catalog, nil
}

func (s *Session) beginClose() {
	s.notifySessionClosing(s.markClosing())
}

// markClosing transitions the Session to the closing state exactly once.
func (s *Session) markClosing() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.closing = true
	return true
}

func (s *Session) notifySessionClosing(first bool) {
	if !first || s == nil || s.observer == nil {
		return
	}
	s.observer.OnSessionClosing(s)
}

func (s *Session) notifySessionClosed(err error) {
	if s == nil || s.observer == nil {
		return
	}
	s.observer.OnSessionClosed(s, err)
}

func (s *Session) notifySessionStarted(turn *Turn) {
	if s == nil || s.observer == nil {
		return
	}
	s.observer.OnSessionStarted(s, turn)
}

func (s *Session) turnFinished(turn *Turn, result *agent.Result, err error) {
	s.mu.Lock()
	if s.active == turn {
		s.active = nil
	}
	s.mu.Unlock()
	s.changeActivity(activityTurn, -1)
	if s.observer != nil {
		s.observer.OnTurnFinished(s, turn, result, err)
	}
}
