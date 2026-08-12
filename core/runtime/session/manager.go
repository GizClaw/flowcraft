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
	resolver          InstanceResolver
	hostFactory       HostFactory
	router            *event.Router
	idleTimeout       time.Duration
	sinkBuffer        int
	speculativeEvents int
	speculativeBytes  int
	checkpoints       agent.CheckpointStore
	resume            bool
	observer          SessionObserver
	catalogProvider   CatalogProvider

	mu        sync.Mutex
	entries   map[Key]*managerEntry
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

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
		speculativeEvents: defaultSpeculativeBufferEvents,
		speculativeBytes:  defaultSpeculativeBufferBytes,
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
		resolver:          resolver,
		hostFactory:       hostFactory,
		router:            router,
		idleTimeout:       opts.idleTimeout,
		sinkBuffer:        opts.sinkBuffer,
		speculativeEvents: opts.speculativeEvents,
		speculativeBytes:  opts.speculativeBytes,
		checkpoints:       opts.checkpoints,
		resume:            opts.resume,
		observer:          opts.observer,
		catalogProvider:   opts.catalogProvider,
		entries:           make(map[Key]*managerEntry),
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
	if entry := m.entries[key]; entry != nil {
		entry.leases++
		entry.idleGeneration++
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
		return newLease(m, key, entry.session), nil
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
		m.speculativeEvents, m.speculativeBytes,
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
		m.scheduleIdleTimerLocked(key, entry)
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
	if entry.leases == 0 && session.isIdle() {
		m.scheduleIdleTimerLocked(key, entry)
	}
}

func (m *Manager) scheduleIdleTimerLocked(key Key, entry *managerEntry) {
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
	delete(m.entries, key)
	entry.timer = nil
	m.mu.Unlock()
	_ = session.close()
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
