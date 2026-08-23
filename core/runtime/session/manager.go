package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

var ErrManagerClosed = errdefs.NotAvailablef("runtime session: manager is closed")

// InstanceResolver is the minimum borrowed deployment view needed by Manager.
type InstanceResolver interface {
	Instance(id string) (*agent.Agent, bool)
}

type managerEntry struct {
	session        *Session
	leases         int
	idleGeneration uint64
	timer          *time.Timer
}

// Manager shares Sessions by Key and reclaims unleased idle Sessions.
// It borrows its resolver, HostFactory, and event router and never
// closes them.
type Manager struct {
	router              *event.Router
	idleTimeout         time.Duration
	sinkBuffer          int
	speculativeEvents   int
	speculativeBytes    int
	deliveryConcurrency int
	maxSessions         int
	observer            SessionObserver

	// deps is the current epoch's execution snapshot; epochs tracks
	// every live (current or retired-but-referenced) epoch.
	deps     Deps
	epochSeq uint64
	epochs   map[uint64]*epochState

	mu        sync.Mutex
	entries   map[Key]*managerEntry
	removed   map[string]struct{}
	deleting  map[Key]struct{}
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// agentDrainPollInterval is how often RemoveAgent re-checks whether
// the target agent's sessions have become idle while draining.
const agentDrainPollInterval = 10 * time.Millisecond

// NewManager constructs a Session manager over borrowed runtime dependencies.
func NewManager(
	resolver InstanceResolver,
	hostFactory HostFactory,
	router *event.Router,
	options ...ManagerOption,
) (*Manager, error) {
	if isNil(resolver) {
		return nil, errdefs.Validationf("runtime session: instance resolver is required")
	}
	if isNil(hostFactory) {
		return nil, errdefs.Validationf("runtime session: HostFactory is required")
	}
	if router == nil {
		return nil, errdefs.Validationf("runtime session: event router is required")
	}

	opts := managerOptions{
		idleTimeout: defaultIdleTimeout, sinkBuffer: defaultSinkBuffer,
		speculativeEvents:   defaultSpeculativeBufferEvents,
		speculativeBytes:    defaultSpeculativeBufferBytes,
		deliveryConcurrency: defaultDeliveryConcurrency,
		maxSessions:         defaultMaxSessions,
	}
	for _, option := range options {
		if isNil(option) {
			return nil, errdefs.Validationf("runtime session: ManagerOption must not be nil")
		}
		if err := option(&opts); err != nil {
			return nil, err
		}
	}
	if opts.resume {
		if isNil(opts.checkpoints) {
			return nil, errdefs.Validationf(
				"runtime session: resume requires a checkpoint store")
		}
		if _, ok := opts.checkpoints.(agent.CheckpointDeleter); !ok {
			return nil, errdefs.Validationf(
				"runtime session: resume requires a checkpoint store that implements CheckpointDeleter")
		}
	}
	m := &Manager{
		router:              router,
		idleTimeout:         opts.idleTimeout,
		sinkBuffer:          opts.sinkBuffer,
		speculativeEvents:   opts.speculativeEvents,
		speculativeBytes:    opts.speculativeBytes,
		deliveryConcurrency: opts.deliveryConcurrency,
		maxSessions:         opts.maxSessions,
		observer:            opts.observer,
		entries:             make(map[Key]*managerEntry),
		removed:             make(map[string]struct{}),
		deleting:            make(map[Key]struct{}),
		epochSeq:            1,
		epochs:              make(map[uint64]*epochState),
	}
	m.initEpoch(resolver, hostFactory, opts.catalogProvider,
		opts.checkpoints, opts.resume)
	return m, nil
}

func (m *Manager) initEpoch(
	resolver InstanceResolver,
	hostFactory HostFactory,
	catalogProvider CatalogProvider,
	checkpoints agent.CheckpointStore,
	resume bool,
) {
	m.epochs[1] = &epochState{
		deps: Deps{
			Resolver:        resolver,
			HostFactory:     hostFactory,
			CatalogProvider: catalogProvider,
			Checkpoints:     checkpoints,
			Resume:          resume,
			Epoch:           1,
		},
	}
	m.deps = m.epochs[1].deps
}

// currentDeps returns the current epoch's dependency snapshot. It is
// used by session-level operations that run outside a turn (e.g.
// Resumable); turn-scoped operations must use the epoch acquired at
// Start so a concurrent swap cannot tear them.
func (m *Manager) currentDeps() Deps {
	if m == nil {
		return Deps{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deps
}

// Open returns an independent Lease over the Session identified by key.
func (m *Manager) Open(ctx context.Context, key Key) (*Lease, error) {
	return m.open(ctx, key)
}

// GetOrCreate lazily creates a Session and returns an independent Lease.
func (m *Manager) GetOrCreate(ctx context.Context, key Key) (*Lease, error) {
	return m.open(ctx, key)
}

func (m *Manager) open(ctx context.Context, key Key) (*Lease, error) {
	if m == nil {
		return nil, ErrManagerClosed
	}
	if isNil(ctx) {
		return nil, errdefs.Validationf("runtime session: context is required")
	}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errdefs.FromContext(err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrManagerClosed
	}
	if _, deleting := m.deleting[key]; deleting {
		return nil, errdefs.NotAvailablef(
			"runtime session: session %q/%q is being deleted",
			key.AgentID, key.ContextID)
	}
	if _, gone := m.removed[key.AgentID]; gone {
		return nil, errdefs.NotFoundf(
			"runtime session: agent %q is not deployed", key.AgentID)
	}
	if entry := m.entries[key]; entry != nil {
		if entry.session.isClosing() {
			return nil, errdefs.NotAvailablef(
				"runtime session: session %q/%q is closing",
				key.AgentID, key.ContextID)
		}
		entry.leases++
		entry.idleGeneration++
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
		return newLease(m, key, entry.session), nil
	}
	if m.maxSessions > 0 && len(m.entries) >= m.maxSessions {
		return nil, errdefs.RateLimitf(
			"runtime session: max sessions reached (%d)", m.maxSessions)
	}

	instance, ok := m.deps.Resolver.Instance(key.AgentID)
	if !ok {
		return nil, errdefs.NotFoundf("runtime session: agent %q is not deployed", key.AgentID)
	}
	if instance == nil {
		return nil, errdefs.Internalf(
			"runtime session: resolver returned a nil instance for agent %q",
			key.AgentID)
	}
	session := newSession(
		key, m, m.router, m.sinkBuffer,
		m.speculativeEvents, m.speculativeBytes, m.deliveryConcurrency,
		func(changed *Session) {
			m.activityChanged(key, changed)
		},
		m.observer)
	m.entries[key] = &managerEntry{session: session, leases: 1}
	return newLease(m, key, session), nil
}

func newLease(manager *Manager, key Key, session *Session) *Lease {
	return &Lease{manager: manager, key: key, session: session}
}

func (m *Manager) release(key Key, session *Session) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[key]
	if entry == nil || entry.session != session || entry.leases == 0 {
		return nil
	}
	entry.leases--
	if entry.leases == 0 && session.isIdle() {
		if _, gone := m.removed[key.AgentID]; !gone {
			m.scheduleIdleTimerLocked(key, entry)
		}
	}
	return nil
}

func (m *Manager) activityChanged(key Key, session *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	entry := m.entries[key]
	if entry == nil || entry.session != session {
		return
	}

	entry.idleGeneration++
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	if _, gone := m.removed[key.AgentID]; gone {
		return
	}
	if entry.leases == 0 && session.isIdle() {
		m.scheduleIdleTimerLocked(key, entry)
	}
}

func (m *Manager) scheduleIdleTimerLocked(key Key, entry *managerEntry) {
	if _, gone := m.removed[key.AgentID]; gone {
		return
	}
	entry.idleGeneration++
	generation := entry.idleGeneration
	session := entry.session
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.timer = time.AfterFunc(m.idleTimeout, func() {
		m.reclaim(key, session, generation)
	})
}

func (m *Manager) reclaim(key Key, session *Session, generation uint64) {
	m.mu.Lock()
	entry := m.entries[key]
	if m.closed || entry == nil || entry.session != session ||
		entry.idleGeneration != generation || entry.leases != 0 || !session.isIdle() {
		m.mu.Unlock()
		return
	}
	if _, gone := m.removed[key.AgentID]; gone {
		m.mu.Unlock()
		return
	}
	delete(m.entries, key)
	entry.timer = nil
	m.mu.Unlock()
	if err := session.close(); err != nil {
		telemetry.WarnErr(context.Background(), "runtime session: close idle-reclaimed session failed", err,
			otellog.String(telemetry.AttrAgentID, key.AgentID),
			otellog.String(telemetry.AttrConversationID, key.ContextID))
		return
	}
	telemetry.Debug(context.Background(), "runtime session: idle session reclaimed",
		otellog.String(telemetry.AttrAgentID, key.AgentID),
		otellog.String(telemetry.AttrConversationID, key.ContextID))
}

// RemoveAgent blocks new session activity for the named agent and
// drains its live sessions: it waits (bounded by ctx) for every active
// turn to finish naturally, then closes the now-idle sessions. On ctx
// expiry the tombstone stays in place (new opens keep failing), the
// sessions are left intact, and no partial removal state is produced —
// callers may retry. Repeated calls are idempotent.
func (m *Manager) RemoveAgent(ctx context.Context, name string) error {
	if m == nil {
		return nil
	}
	if isNil(ctx) {
		return errdefs.Validationf("runtime session: context is required")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	m.removed[name] = struct{}{}
	// Stop idle-reclamation timers and invalidate in-flight callbacks so
	// sessions are not reclaimed while we drain them.
	for key, entry := range m.entries {
		if key.AgentID != name {
			continue
		}
		entry.idleGeneration++
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
	}
	m.mu.Unlock()

	if err := m.awaitAgentIdle(ctx, name); err != nil {
		telemetry.Error(ctx, "runtime session: agent drain timed out",
			otellog.String(telemetry.AttrAgentID, name),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return err
	}

	var closing []*Session
	m.mu.Lock()
	for key, entry := range m.entries {
		if key.AgentID != name {
			continue
		}
		if entry.session.markClosing() {
			closing = append(closing, entry.session)
		}
		delete(m.entries, key)
	}
	m.mu.Unlock()

	for _, s := range closing {
		s.notifySessionClosing(true)
	}
	var errs []error
	for _, s := range closing {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ReopenAgent clears the tombstone left by RemoveAgent, re-admitting
// session activity for the name. It is used when re-registering an
// agent under the same ID.
func (m *Manager) ReopenAgent(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.removed, name)
	m.mu.Unlock()
}

// DeleteSession removes one session's durable state and closes its live
// Session, identified by key (agent id + context id). It is the
// by-key counterpart of RemoveAgent for the desktop lifecycle
// (delete/archive a conversation): after it returns, the checkpoint
// store no longer carries the session's committed board (history),
// parked run checkpoint, or resumable request for that key, and a
// later Open starts a fresh session with empty history.
//
// Semantics mirror RemoveAgent:
//
//   - New opens for the key are refused until deletion finishes.
//   - Any live Session is drained (bounded by ctx), then closed. On ctx
//     expiry the delete marker is rolled back, the session is left
//     intact, and no partial removal state is produced — callers may
//     retry.
//   - Repeated calls are idempotent: a key with no live Session and no
//     persisted state deletes successfully.
//
// Deletion of the checkpoint store entries runs only after the live
// Session confirms its active turn has stopped (afterTurn has run), so a
// turn that started in the drain window cannot resurrect state underneath
// the delete. Session.close's shutdown wait is budgeted: if it times out —
// or the session is already closing with an active turn — DeleteSession
// returns a retryable error with the durable state left in place, and the
// caller retries once the turn has stopped. Store failures are returned
// (joined with close errors) so callers can retry.
func (m *Manager) DeleteSession(ctx context.Context, key Key) error {
	if m == nil {
		return nil
	}
	if isNil(ctx) {
		return errdefs.Validationf("runtime session: context is required")
	}
	if err := key.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errdefs.FromContext(err)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if _, deleting := m.deleting[key]; deleting {
		m.mu.Unlock()
		return errdefs.NotAvailablef(
			"runtime session: deletion already in progress for key %q/%q",
			key.AgentID, key.ContextID)
	}
	m.deleting[key] = struct{}{}
	// Stop idle-reclamation timers and invalidate in-flight callbacks so
	// the session is not reclaimed while we drain it.
	entry := m.entries[key]
	if entry != nil {
		entry.idleGeneration++
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
	}
	closingWithTurn := entry != nil &&
		entry.session.isClosing() && entry.session.hasActiveTurn()
	m.mu.Unlock()

	if closingWithTurn {
		m.mu.Lock()
		delete(m.deleting, key)
		m.mu.Unlock()
		return errdefs.NotAvailablef(
			"runtime session: session %q/%q is already closing with an active turn; deletion incomplete, retry once the turn stops",
			key.AgentID, key.ContextID)
	}

	if err := m.awaitKeyIdle(ctx, key); err != nil {
		telemetry.Error(ctx, "runtime session: session drain timed out during delete",
			otellog.String(telemetry.AttrAgentID, key.AgentID),
			otellog.String(telemetry.AttrConversationID, key.ContextID),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		m.mu.Lock()
		delete(m.deleting, key)
		if entry := m.entries[key]; entry != nil {
			m.scheduleIdleTimerIfIdleLocked(key, entry)
		}
		m.mu.Unlock()
		return err
	}

	var closing *Session
	m.mu.Lock()
	if entry := m.entries[key]; entry != nil {
		if entry.session.markClosing() {
			closing = entry.session
		}
		// Keep the entry in place until the turn has fully stopped:
		// close()'s shutdown wait is budgeted and may time out, and a
		// retry (or Open) must be able to see a still-running turn. The
		// entry is removed only after close confirms the session stopped.
	}
	m.mu.Unlock()

	var errs []error
	if closing != nil {
		closing.notifySessionClosing(true)
		if err := closing.close(); err != nil {
			errs = append(errs, err)
		}
		if closing.hasActiveTurn() {
			// close()'s shutdown wait is budgeted and may have timed out:
			// the turn can still be running and its afterTurn would write
			// session state back after we delete it. Leave the durable
			// state and the entry in place, roll back the delete marker,
			// and ask the caller to retry once the turn has stopped.
			errs = append(errs, errdefs.NotAvailablef(
				"runtime session: session %q/%q still has an active turn after close; deletion incomplete, retry once the turn stops",
				key.AgentID, key.ContextID))
			m.mu.Lock()
			delete(m.deleting, key)
			m.mu.Unlock()
			return errors.Join(errs...)
		}
	}
	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()
	if err := m.deletePersistedState(key); err != nil {
		errs = append(errs, err)
	}

	// Re-admit opens only after both the live Session and the durable
	// state are gone, so a concurrent Open cannot load state that the
	// delete is about to remove.
	m.mu.Lock()
	delete(m.deleting, key)
	m.mu.Unlock()

	return errors.Join(errs...)
}

// awaitKeyIdle waits (bounded by ctx) until the key's Session has no
// active turns, prompts, or sinks. A key with no live Session is
// immediately idle.
func (m *Manager) awaitKeyIdle(ctx context.Context, key Key) error {
	ticker := time.NewTicker(agentDrainPollInterval)
	defer ticker.Stop()
	for {
		if m.keyIdle(key) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errdefs.FromContext(ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) keyIdle(key Key) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[key]
	if entry == nil {
		return true
	}
	if entry.session.isIdle() {
		return true
	}
	// A closing session whose turn already stopped never reports idle
	// (isIdle requires !closing), but there is nothing left to drain:
	// close() is a no-op and the durable state can be removed safely.
	return entry.session.isClosing() && !entry.session.hasActiveTurn()
}

// scheduleIdleTimerIfIdleLocked re-arms the idle-reclamation timer for a
// session that still has no leases and is idle. Callers must hold mu.
func (m *Manager) scheduleIdleTimerIfIdleLocked(key Key, entry *managerEntry) {
	if m.closed || entry == nil {
		return
	}
	if _, gone := m.removed[key.AgentID]; gone {
		return
	}
	if entry.leases == 0 && entry.session.isIdle() {
		m.scheduleIdleTimerLocked(key, entry)
	}
}

// deletePersistedState removes the checkpoint-store entries for one
// session: the durable session-state record (committed board, parked
// run marker, resumable request) and the parked run's checkpoint. Store
// access is bounded with a background timeout so a slow store cannot
// hold the caller's context hostage after the drain phase completed. The
// store is resolved from the manager's current epoch: if a deployment
// swaps checkpoint store instances between a session's writes and the
// delete, cleanup lands on the current store (deployments sharing one
// store singleton — the normal layout — are unaffected).
func (m *Manager) deletePersistedState(key Key) error {
	deps := m.currentDeps()
	if !deps.Resume || deps.Checkpoints == nil {
		return nil
	}
	deleter, ok := deps.Checkpoints.(agent.CheckpointDeleter)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	stateID := sessionStateID(key)
	cp, err := deps.Checkpoints.Load(ctx, stateID)
	if err != nil {
		return fmt.Errorf("runtime session: load session state for delete: %w", err)
	}
	if cp == nil {
		return nil
	}
	var state sessionState
	if len(cp.Payload) > 0 {
		if err := json.Unmarshal(cp.Payload, &state); err != nil {
			// A corrupt record hides the parked run id; remove the
			// session-state key anyway and report the decode failure so
			// the caller knows cleanup may be incomplete.
			return errors.Join(
				fmt.Errorf("runtime session: decode session state for delete: %w", err),
				deleter.Delete(ctx, stateID))
		}
	}
	if state.ResumableRunID != "" {
		if err := deleter.Delete(ctx, state.ResumableRunID); err != nil {
			return fmt.Errorf(
				"runtime session: delete parked run checkpoint %q: %w",
				state.ResumableRunID, err)
		}
	}
	if err := deleter.Delete(ctx, stateID); err != nil {
		return fmt.Errorf("runtime session: delete session state: %w", err)
	}
	return nil
}

func (m *Manager) awaitAgentIdle(ctx context.Context, name string) error {
	ticker := time.NewTicker(agentDrainPollInterval)
	defer ticker.Stop()
	for {
		if m.agentIdle(name) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errdefs.FromContext(ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) agentIdle(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, entry := range m.entries {
		if key.AgentID == name && !entry.session.isIdle() {
			return false
		}
	}
	return true
}

// Close stops reclamation, refuses new leases, and closes every Session.
// Borrowed deployment, router, and host dependencies remain owned by Runtime.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		sessions := make([]*Session, 0, len(m.entries))
		closing := make([]*Session, 0, len(m.entries))
		for key, entry := range m.entries {
			entry.idleGeneration++
			if entry.timer != nil {
				entry.timer.Stop()
				entry.timer = nil
			}
			sessions = append(sessions, entry.session)
			if entry.session.markClosing() {
				closing = append(closing, entry.session)
			}
			delete(m.entries, key)
		}
		m.mu.Unlock()

		for _, session := range closing {
			session.notifySessionClosing(true)
		}

		var closeErrors []error
		for _, session := range sessions {
			if err := session.close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
		m.closeErr = errors.Join(closeErrors...)
	})
	if m.closeErr != nil {
		telemetry.Error(context.Background(), "runtime session: manager close failed",
			otellog.String(telemetry.AttrErrorMessage, m.closeErr.Error()))
	}
	return m.closeErr
}

func (m *Manager) sessionCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
