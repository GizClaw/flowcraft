package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/telemetry"
	sdktool "github.com/GizClaw/flowcraft/core/tool"

	otellog "go.opentelemetry.io/otel/log"
)

type activityKind uint8

const (
	activityTurn activityKind = iota
	activityPrompt
	activitySink
)

// sessionCloseTurnTimeout bounds how long Session.close waits for
// the active turn to finish after shutdown (interrupt) is
// signalled. A non-cooperative engine must not hang Close (and
// with it Manager.Close / Runtime.Close) forever. It is a variable
// (not a const) so tests can shrink the budget.
var sessionCloseTurnTimeout = 30 * time.Second

// Session owns conversational activity for one Key while borrowing all
// deployment and event-routing dependencies from the runtime.
type Session struct {
	key                 Key
	manager             *Manager
	router              *event.Router
	sinkBuffer          int
	speculativeEvents   int
	speculativeBytes    int
	deliveryConcurrency int

	startMu       sync.Mutex
	mu            sync.Mutex
	activeTurns   int
	activePrompts int
	attachedSinks int
	active        *Turn
	closing       bool
	catalog       sdktool.Session
	catalogEpoch  uint64
	// ephemeral is a session-level property fixed by the first Start:
	// ephemeral sessions never persist session state or run checkpoints.
	// Writes happen under startMu; reads use mu (see isEphemeral).
	ephemeral      bool
	ephemeralSet   bool
	activityNotify func(*Session)
	observer       SessionObserver
	closeOnce      sync.Once
	closeErr       error
}

func newSession(
	key Key,
	manager *Manager,
	router *event.Router,
	sinkBuffer int,
	speculativeEvents int,
	speculativeBytes int,
	deliveryConcurrency int,
	activityNotify func(*Session),
	observer SessionObserver,
) *Session {
	return &Session{
		key:                 key,
		manager:             manager,
		router:              router,
		sinkBuffer:          sinkBuffer,
		speculativeEvents:   speculativeEvents,
		speculativeBytes:    speculativeBytes,
		deliveryConcurrency: deliveryConcurrency,
		activityNotify:      activityNotify,
		observer:            observer,
	}
}

// Start installs and asynchronously executes a new turn. A previous turn is
// interrupted and fully finalized before the replacement is constructed.
func (s *Session) Start(ctx context.Context, request agent.Request, sinks ...SinkSpec) (*Turn, error) {
	return s.start(ctx, request, startConfig{sinks: sinks})
}

// StartWithOptions is Start with option-based configuration: stream sinks,
// an AskUser override, and the ephemeral session property. The plain Start
// signature is unchanged for existing callers.
func (s *Session) StartWithOptions(ctx context.Context, request agent.Request, opts ...StartOption) (*Turn, error) {
	config, err := applyStartOptions(opts)
	if err != nil {
		return nil, err
	}
	return s.start(ctx, request, config)
}

func (s *Session) start(ctx context.Context, request agent.Request, config startConfig) (*Turn, error) {
	if s == nil {
		return nil, ErrSessionClosed
	}
	if ctx == nil {
		return nil, errdefs.Validationf("runtime session: Start context is required")
	}
	if err := validateSinks(config.sinks); err != nil {
		return nil, err
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	// The ephemeral property is fixed by the first Start and must be set
	// before loadSessionState so seeding and host wrapping can rely on it.
	if err := s.setEphemeralProperty(config.ephemeral); err != nil {
		return nil, err
	}

	// Pin this turn to one epoch: every dependency the turn uses comes
	// from this snapshot, so a concurrent generation swap cannot tear it.
	deps, epochRelease, err := s.manager.beginEpoch()
	if err != nil {
		return nil, err
	}
	epochHeld := true
	defer func() {
		if epochHeld {
			epochRelease()
		}
	}()

	release, err := s.interruptActive(ctx)
	if err != nil {
		return nil, err
	}
	activityHeld := true
	defer func() {
		if activityHeld {
			release()
		}
	}()

	runID, err := freshRunID()
	if err != nil {
		return nil, err
	}
	var snapshot *agent.BoardSnapshot
	if !s.isEphemeral() {
		state, err := s.loadSessionState(ctx, deps.Checkpoints, deps.Resume)
		if err != nil {
			return nil, err
		}
		if state != nil {
			snapshot = state.Board
		}
	}
	request.ContextID = s.key.ContextID
	request.RunID = runID
	turn, err := s.startTurnLocked(
		ctx, request, runID, deps, epochRelease, nil, nil, snapshot, &config)
	if err != nil {
		return nil, err
	}
	epochHeld = false
	activityHeld = false
	return turn, nil
}

// Resume re-executes the session's parked interrupted run. The parked
// run is the most recent turn that ended without committing; its
// checkpoint is loaded and replayed under the original run id and
// request, so the engine picks up where it stopped instead of starting
// fresh. A new user message MUST go through Start — Resume does not
// carry a request.
func (s *Session) Resume(ctx context.Context, sinks ...SinkSpec) (*Turn, error) {
	return s.resume(ctx, startConfig{sinks: sinks})
}

// ResumeWithOptions is Resume with option-based configuration. WithEphemeral
// is rejected: the session ephemeral property is fixed by the first Start.
func (s *Session) ResumeWithOptions(ctx context.Context, opts ...StartOption) (*Turn, error) {
	config, err := applyStartOptions(opts)
	if err != nil {
		return nil, err
	}
	if config.ephemeralSet {
		return nil, errdefs.Validationf(
			"runtime session: ResumeWithOptions cannot change the session ephemeral property")
	}
	return s.resume(ctx, config)
}

func (s *Session) resume(ctx context.Context, config startConfig) (*Turn, error) {
	if s == nil {
		return nil, ErrSessionClosed
	}
	if ctx == nil {
		return nil, errdefs.Validationf("runtime session: Resume context is required")
	}
	if err := validateSinks(config.sinks); err != nil {
		return nil, err
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	if s.isEphemeral() {
		return nil, errdefs.NotFoundf(
			"runtime session: ephemeral session has no resumable state")
	}

	deps, epochRelease, err := s.manager.beginEpoch()
	if err != nil {
		return nil, err
	}
	epochHeld := true
	defer func() {
		if epochHeld {
			epochRelease()
		}
	}()

	release, err := s.interruptActive(ctx)
	if err != nil {
		return nil, err
	}
	activityHeld := true
	defer func() {
		if activityHeld {
			release()
		}
	}()

	state, err := s.loadSessionState(ctx, deps.Checkpoints, deps.Resume)
	if err != nil {
		return nil, err
	}
	if state == nil || state.ResumableRunID == "" {
		return nil, errdefs.NotFoundf(
			"runtime session: no interrupted run to resume")
	}
	runID := state.ResumableRunID
	instance, ok := deps.Resolver.Instance(s.key.AgentID)
	if !ok {
		return nil, errdefs.NotFoundf(
			"runtime session: agent %q is not deployed", s.key.AgentID)
	}
	if instance == nil {
		return nil, errdefs.Internalf(
			"runtime session: resolver returned a nil instance for agent %q",
			s.key.AgentID)
	}
	resumeFrom, resumeCtx, err := s.loadResumableCheckpoint(
		ctx, runID, instance, deps.Checkpoints, deps.Resume)
	if err != nil {
		return nil, err
	}
	if resumeFrom == nil {
		// The parked marker points at a run whose checkpoint is gone
		// (store eviction, manual cleanup); drop the marker so the
		// session does not stay stuck in a resumable state.
		s.clearParkedRun(state, deps.Checkpoints, deps.Resume)
		return nil, errdefs.NotFoundf(
			"runtime session: checkpoint for parked run %s is gone", runID)
	}
	if state.Request == nil {
		return nil, errdefs.Validationf(
			"runtime session: parked run %s has no stored request", runID)
	}
	request := *state.Request
	request.ContextID = s.key.ContextID
	request.RunID = runID
	turn, err := s.startTurnLocked(
		ctx, request, runID, deps, epochRelease, resumeFrom, resumeCtx, nil, &config)
	if err != nil {
		return nil, err
	}
	epochHeld = false
	activityHeld = false
	return turn, nil
}

// Resumable reports whether a previous turn ended without committing
// and can be replayed via Resume. It returns the parked run id.
func (s *Session) Resumable(ctx context.Context) (string, bool, error) {
	state, err := s.parkedRunState(ctx)
	if err != nil {
		return "", false, err
	}
	if state == nil {
		return "", false, nil
	}
	return state.ResumableRunID, true, nil
}

// ParkedRequest returns the original request of the session's parked
// interrupted run, when one exists and its request was stored. ok is
// false whenever there is nothing to resume: a fresh or committed
// session, an ephemeral session, resume not configured, or a parked
// marker without a stored request. The returned request is a copy.
func (s *Session) ParkedRequest(ctx context.Context) (agent.Request, bool, error) {
	state, err := s.parkedRunState(ctx)
	if err != nil {
		return agent.Request{}, false, err
	}
	if state == nil || state.Request == nil {
		return agent.Request{}, false, nil
	}
	return *state.Request, true, nil
}

// parkedRunState loads the parked-run session state, or returns nil
// when there is nothing to resume: a nil session, an ephemeral
// session, resume not configured, or no persisted state.
func (s *Session) parkedRunState(ctx context.Context) (*sessionState, error) {
	if s == nil {
		return nil, nil
	}
	if s.isEphemeral() {
		return nil, nil
	}
	deps := s.manager.currentDeps()
	if !deps.Resume || deps.Checkpoints == nil {
		return nil, nil
	}
	if ctx == nil {
		return nil, errdefs.Validationf(
			"runtime session: parked run context is required")
	}
	state, err := s.loadSessionState(ctx, deps.Checkpoints, deps.Resume)
	if err != nil {
		return nil, err
	}
	if state == nil || state.ResumableRunID == "" {
		return nil, nil
	}
	return state, nil
}

// interruptActive stops any running turn and reserves one activity slot
// for the incoming turn. The returned release MUST be called on every
// path that does not install a turn; once a turn is installed the slot
// belongs to the running turn and is released by turnFinished.
func (s *Session) interruptActive(ctx context.Context) (func(), error) {
	s.changeActivity(activityTurn, 1)
	release := func() { s.changeActivity(activityTurn, -1) }

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		release()
		return nil, ErrSessionClosed
	}
	old := s.active
	s.mu.Unlock()
	if old != nil {
		if err := old.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
			telemetry.WarnErr(ctx, "runtime session: interrupt active turn failed", err,
				otellog.String(telemetry.AttrRunID, old.runID))
		}
		if _, err := old.Wait(ctx); err != nil {
			telemetry.Debug(ctx, "runtime session: active turn wait after interrupt",
				otellog.String(telemetry.AttrRunID, old.runID),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
		}
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, errdefs.FromContext(err)
	}
	return release, nil
}

// validateSinks rejects structurally invalid sink specifications.
func validateSinks(sinks []SinkSpec) error {
	for _, spec := range sinks {
		if err := spec.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(sinks))
	authorities := 0
	for _, spec := range sinks {
		if _, exists := seen[spec.ID]; exists {
			return errdefs.Validationf("runtime session: duplicate SinkSpec.ID %q", spec.ID)
		}
		seen[spec.ID] = struct{}{}
		if spec.Authority == AuthorityAuthoritative {
			authorities++
		}
	}
	if authorities > 1 {
		return errdefs.Validationf("runtime session: at most one authoritative sink is allowed per turn")
	}
	return nil
}

// startTurnLocked installs and asynchronously runs one turn. It must be
// called with startMu held and with the caller's activity slot already
// reserved. request.ContextID and request.RunID must already be set.
func (s *Session) startTurnLocked(
	ctx context.Context,
	request agent.Request,
	runID string,
	deps Deps,
	epochRelease func(),
	resumeFrom *agent.Checkpoint,
	resumeCtx *agent.ResumeContext,
	snapshot *agent.BoardSnapshot,
	config *startConfig,
) (*Turn, error) {
	turn := newTurn(s, runID, ctx)
	turn.release = epochRelease
	turn.checkpoints = deps.Checkpoints
	turn.resume = deps.Resume
	turn.resumeFrom = resumeFrom
	turn.resumeCtx = resumeCtx
	turn.snapshot = snapshot
	turn.askUserOverride = config.askUser
	instance, ok := deps.Resolver.Instance(s.key.AgentID)
	if !ok {
		turn.cancel()
		epochRelease()
		return nil, errdefs.NotFoundf(
			"runtime session: agent %q is not deployed", s.key.AgentID)
	}
	if instance == nil {
		turn.cancel()
		epochRelease()
		return nil, errdefs.Internalf(
			"runtime session: resolver returned a nil instance for agent %q",
			s.key.AgentID)
	}
	catalog, err := s.catalogFor(ctx, deps, instance)
	if err != nil {
		turn.cancel()
		epochRelease()
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
		epochRelease()
		return nil, err
	}
	host, err := deps.HostFactory.NewHost(turn.runCtx, hostRequest)
	if err != nil {
		turn.cancel()
		epochRelease()
		return nil, err
	}
	if isNil(host) {
		turn.cancel()
		epochRelease()
		return nil, errdefs.Internalf("runtime session: HostFactory returned nil Host")
	}
	if s.isEphemeral() {
		host = ephemeralHost{Host: host}
	}
	turn.host = host

	attachments := make([]*queuedSink, 0, len(config.sinks))
	raw := make([]*queuedSink, 0, len(config.sinks))
	confirmed := make([]*queuedSink, 0, len(config.sinks))
	for _, spec := range config.sinks {
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
	// twice. The coordinator must observe every event and delta in
	// order; a dropping subscription could lose run-end delimiters or
	// confirmed deltas, so it explicitly blocks instead of using the
	// bus default (external Runtime.Attach consumers keep the
	// non-blocking default).
	detach, attachErr := s.router.Attach(
		context.Background(),
		agent.PatternRun(runID),
		coordinator,
		event.WithAttachBackpressure(event.Block),
	)
	if attachErr != nil {
		for _, attachment := range attachments {
			attachment.detach(attachErr)
		}
		turn.cancel()
		epochRelease()
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
		epochRelease()
		return nil, ErrSessionClosed
	}
	s.active = turn
	turn.mu.Lock()
	turn.state = TurnRunning
	turn.mu.Unlock()
	s.mu.Unlock()

	s.notifySessionStarted(turn)
	go turn.execute(instance, request)
	return turn, nil
}

// ---------- Session state and resume ----------

// sessionState is the durable per-session record kept in the checkpoint
// store under a reserved "session-" key. It is NOT a run checkpoint:
//
//   - Board holds the session's last committed board (conversation
//     history). Every fresh Start seeds its run from this board so
//     history carries across turns without re-identifying a run.
//   - ResumableRunID names the most recent turn that ended without
//     committing. Resume replays that specific execution from its
//     checkpoint. Empty when no interrupted execution is parked.
//   - Request is the original request of the parked run; Resume must
//     re-execute under the same request (same task, same inputs).
type sessionState struct {
	ResumableRunID string               `json:"resumable_run_id,omitempty"`
	Request        *agent.Request       `json:"request,omitempty"`
	Board          *agent.BoardSnapshot `json:"-"`
}

// sessionStateID derives the durable key for one session's state. It is
// stable across process restarts and never collides with run ids
// ("run-" prefix) or prompt ids (bare hex).
func sessionStateID(key Key) string {
	sum := sha256.Sum256([]byte(key.AgentID + "\x00" + key.ContextID))
	return "session-" + hex.EncodeToString(sum[:16])
}

// freshRunID returns a new run id for a fresh turn.
func freshRunID() (string, error) {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errdefs.Internal(fmt.Errorf("runtime session: allocate run id: %w", err))
	}
	return "run-" + hex.EncodeToString(bytes[:]), nil
}

// loadSessionState reads the session's durable state, or (nil, nil)
// when resume is disabled, no store is wired, or no record exists yet.
func (s *Session) loadSessionState(
	ctx context.Context,
	checkpoints agent.CheckpointStore,
	resume bool,
) (*sessionState, error) {
	if s == nil || !resume || checkpoints == nil {
		return nil, nil
	}
	cp, err := checkpoints.Load(ctx, sessionStateID(s.key))
	if err != nil {
		return nil, fmt.Errorf("runtime session: load session state: %w", err)
	}
	if cp == nil {
		return nil, nil
	}
	state := &sessionState{Board: cp.Board}
	if len(cp.Payload) > 0 {
		if err := json.Unmarshal(cp.Payload, state); err != nil {
			return nil, fmt.Errorf("runtime session: decode session state: %w", err)
		}
	}
	return state, nil
}

// saveSessionState persists the session state record. Best-effort: a
// store failure leaves the previous record in place.
func (s *Session) saveSessionState(
	state *sessionState,
	checkpoints agent.CheckpointStore,
	resume bool,
) {
	if s == nil || !resume || checkpoints == nil || state == nil {
		return
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return
	}
	board := state.Board
	if board == nil {
		board = agent.NewBoard().Snapshot()
	}
	cp := agent.Checkpoint{
		ExecID:     sessionStateID(s.key),
		Board:      board,
		Payload:    payload,
		Attributes: map[string]string{"runtime.session.kind": "state"},
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := checkpoints.Save(ctx, cp); err != nil {
		telemetry.WarnErr(ctx, "runtime session: save session state checkpoint failed", err,
			otellog.String("runtime.session.id", sessionStateID(s.key)))
	}
}

// clearParkedRun drops the resumable marker (e.g. the parked run's
// checkpoint no longer exists) while preserving the committed board.
func (s *Session) clearParkedRun(
	state *sessionState,
	checkpoints agent.CheckpointStore,
	resume bool,
) {
	if state == nil {
		return
	}
	state.ResumableRunID = ""
	state.Request = nil
	s.saveSessionState(state, checkpoints, resume)
}

// afterTurn updates session state after a turn reaches its terminal
// state:
//
//   - Committed runs advance the session board and delete their run
//     checkpoint; nothing stays parked.
//   - Interrupted, canceled, failed, and aborted runs keep their
//     checkpoint and park their run id + original request so Resume
//     can replay them. The committed board is left untouched.
func (s *Session) afterTurn(turn *Turn, result *agent.Result) {
	if s == nil || turn == nil ||
		s.isEphemeral() || !turn.resume || turn.checkpoints == nil {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	prev, err := s.loadSessionState(ctx, turn.checkpoints, turn.resume)
	if err != nil {
		prev = nil
	}
	state := &sessionState{}
	if prev != nil {
		state.Board = prev.Board
	}
	if result != nil && result.Committed {
		s.deleteCheckpoint(turn.runID, turn.checkpoints)
		if result.LastBoard != nil {
			state.Board = result.LastBoard.Snapshot()
		}
		state.ResumableRunID = ""
		state.Request = nil
	} else {
		state.ResumableRunID = turn.runID
		request := turn.request
		state.Request = &request
	}
	s.saveSessionState(state, turn.checkpoints, turn.resume)
}

// deleteCheckpoint removes a committed run's checkpoint. Best-effort:
// a failed delete leaves an orphaned checkpoint under a run id that no
// session state references, which is harmless.
func (s *Session) deleteCheckpoint(
	runID string,
	checkpoints agent.CheckpointStore,
) {
	if checkpoints == nil {
		return
	}
	deleter, ok := checkpoints.(agent.CheckpointDeleter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := deleter.Delete(ctx, runID); err != nil {
		telemetry.WarnErr(ctx, "runtime session: delete parked-run checkpoint failed", err,
			otellog.String(telemetry.AttrRunID, runID))
	}
}

// loadResumableCheckpoint loads and validates the parked run's
// checkpoint. A missing checkpoint returns (nil, nil, nil) so the
// caller can clear the stale marker.
func (s *Session) loadResumableCheckpoint(
	ctx context.Context,
	runID string,
	instance *agent.Agent,
	checkpoints agent.CheckpointStore,
	resume bool,
) (*agent.Checkpoint, *agent.ResumeContext, error) {
	if !resume || checkpoints == nil {
		return nil, nil, nil
	}
	cp, err := checkpoints.Load(ctx, runID)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"runtime session: load checkpoint %s: %w", runID, err)
	}
	if cp == nil {
		return nil, nil, nil
	}
	if cp.ExecID != runID {
		return nil, nil, errdefs.Validationf(
			"runtime session: checkpoint exec id %q does not match parked run %q",
			cp.ExecID, runID)
	}
	if !agent.IsResumable(instance.Engine) {
		return nil, nil, errdefs.NotAvailablef(
			"runtime session: engine %T does not support resume but checkpoint %s exists",
			instance.Engine, runID)
	}
	if resumer, ok := agent.AsResumer(instance.Engine); ok {
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
		Signal:       "resume",
		CheckpointAt: cp.Timestamp,
	}
	return cp, &resumeCtx, nil
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

// hasActiveTurn reports whether a turn is still executing on the
// session. Session.close bounds its shutdown wait and treats a timeout as
// non-fatal, so a closed session can still have an active turn whose
// afterTurn would write durable state; callers that delete that state
// must confirm this returns false first.
func (s *Session) hasActiveTurn() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil
}

// isEphemeral reports whether the session was created as an ephemeral
// session: no session-state or run-checkpoint persistence, no history
// seeding, and no resumable state.
func (s *Session) isEphemeral() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ephemeral
}

// setEphemeralProperty fixes the session ephemeral property. It must be
// called with startMu held, before loadSessionState. Mixing ephemeral and
// persistent starts on the same session is rejected so a later turn can
// never resurrect state a session chose to discard.
func (s *Session) setEphemeralProperty(ephemeral bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ephemeralSet && s.ephemeral != ephemeral {
		return errdefs.Validationf(
			"runtime session: mixing ephemeral and persistent starts on the same session is not allowed")
	}
	s.ephemeral = ephemeral
	s.ephemeralSet = true
	return nil
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
			waitCtx, cancel := context.WithTimeout(
				context.Background(), sessionCloseTurnTimeout)
			_, s.closeErr = active.Wait(waitCtx)
			cancel()
			if s.closeErr != nil && errors.Is(s.closeErr, context.Canceled) {
				s.closeErr = nil
			}
			if s.closeErr != nil && errors.Is(s.closeErr, context.DeadlineExceeded) {
				telemetry.Warn(context.Background(),
					"runtime session: active turn did not stop within close budget",
					otellog.String(telemetry.AttrRunID, active.runID))
				s.closeErr = nil
			}
		}
		s.startMu.Unlock()
		s.notifySessionClosed(s.closeErr)
	})
	return s.closeErr
}

// catalogFor returns the session catalog for one epoch, creating it via
// the epoch's provider on first use. A catalog cached for a previous
// epoch is not reused: after a generation swap the session must build
// its injection view against the new epoch's assembly. Start is
// serialized by startMu, so the creation path is race-free; the
// defensive branch handles a provider shared with code paths outside
// this Session.
func (s *Session) catalogFor(
	ctx context.Context,
	deps Deps,
	instance *agent.Agent,
) (sdktool.Session, error) {
	s.mu.Lock()
	catalog := s.catalog
	epoch := s.catalogEpoch
	s.mu.Unlock()
	if catalog != nil && epoch == deps.Epoch {
		return catalog, nil
	}
	if deps.CatalogProvider == nil {
		return nil, nil
	}
	catalog, err := deps.CatalogProvider.NewCatalog(ctx, instance)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, errdefs.Internalf(
			"runtime session: CatalogProvider returned a nil catalog")
	}
	s.mu.Lock()
	if s.catalog == nil || s.catalogEpoch != deps.Epoch {
		s.catalog = catalog
		s.catalogEpoch = deps.Epoch
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
