package session

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
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
	resolver            InstanceResolver
	hostFactory         HostFactory
	router              *event.Router
	idleTimeout         time.Duration
	sinkBuffer          int
	speculativeEvents   int
	speculativeBytes    int
	deliveryConcurrency int
	maxSessions         int
	checkpoints         agent.CheckpointStore
	resume              bool
	observer            SessionObserver
	catalogProvider     CatalogProvider

	mu        sync.Mutex
	entries   map[Key]*managerEntry
	removed   map[string]struct{}
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
	return &Manager{
		resolver:            resolver,
		hostFactory:         hostFactory,
		router:              router,
		idleTimeout:         opts.idleTimeout,
		sinkBuffer:          opts.sinkBuffer,
		speculativeEvents:   opts.speculativeEvents,
		speculativeBytes:    opts.speculativeBytes,
		deliveryConcurrency: opts.deliveryConcurrency,
		maxSessions:         opts.maxSessions,
		checkpoints:         opts.checkpoints,
		resume:              opts.resume,
		observer:            opts.observer,
		catalogProvider:     opts.catalogProvider,
		entries:             make(map[Key]*managerEntry),
		removed:             make(map[string]struct{}),
	}, nil
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
	if _, gone := m.removed[key.AgentID]; gone {
		return nil, errdefs.NotFoundf(
			"runtime session: agent %q is not deployed", key.AgentID)
	}
	if entry := m.entries[key]; entry != nil {
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

	instance, ok := m.resolver.Instance(key.AgentID)
	if !ok {
		return nil, errdefs.NotFoundf("runtime session: agent %q is not deployed", key.AgentID)
	}
	if instance == nil {
		return nil, errdefs.Internalf("runtime session: resolver returned a nil instance for agent %q", key.AgentID)
	}
	session := newSession(
		key, instance, m.hostFactory, m.router, m.sinkBuffer,
		m.speculativeEvents, m.speculativeBytes, m.deliveryConcurrency,
		m.checkpoints, m.resume,
		m.catalogProvider,
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
	_ = session.close()
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
