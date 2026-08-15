package sandbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/rs/xid"
)

// registryWaitTimeout bounds the synchronous state sync after
// Terminate. A process that survived SIGKILL (rare kernel-level
// stall) must not wedge the manager forever.
const registryWaitTimeout = 10 * time.Second

// registryCloseTimeout bounds the whole Close drain. Terminate's own
// state sync is capped by registryWaitTimeout, but the per-session
// Terminate call has no deadline of its own; sharing one deadline
// across Close guarantees a Session whose Terminate blocks cannot
// wedge shutdown indefinitely.
const registryCloseTimeout = 30 * time.Second

// SessionStarter implements one backend's spawn: it turns a SessionSpec
// into a launched Session. It is the injection seam shared by every
// backend's Runner (local, remote, and future seatbelt/bwrap) so
// session bookkeeping stays in one place.
//
// Policy is applied once, at Start: Read/Write/Resize/Terminate do not
// re-negotiate Env/Net/Resources. Unsupported requests (e.g. TTY on a
// backend without a pty) fail at Start with errdefs.NotAvailable.
type SessionStarter func(ctx context.Context, spec SessionSpec) (Session, error)

// NewSessionRegistry returns a session registry whose sessions are
// tracked in-process and started by starter. It implements the ID
// uniqueness / generation, List, Terminate-by-ID, and Close removal
// contract so every backend gets identical session semantics.
func NewSessionRegistry(starter SessionStarter) *SessionRegistry {
	return &SessionRegistry{
		starter:  starter,
		sessions: make(map[string]*sessionRecord),
	}
}

// SessionRegistry tracks in-process sessions for one backend. It
// implements Start/List/Terminate with ID uniqueness, session generation,
// and Close removal semantics shared by every sandbox backend.
type SessionRegistry struct {
	starter  SessionStarter
	mu       sync.Mutex
	sessions map[string]*sessionRecord
}

type sessionRecord struct {
	id      string
	spec    SessionSpec
	session Session
	pid     int
	started time.Time
	exited  bool
	exit    SessionExit
	err     error
	// ready is closed once the spawn settles: the session is assigned
	// (spawn succeeded) or the record was removed (spawn failed). It
	// lets Close wait out a start race instead of failing on a session
	// that is still starting.
	ready chan struct{}
}

func (r *SessionRegistry) Start(ctx context.Context, spec SessionSpec) (Session, error) {
	if r.starter == nil {
		return nil, errdefs.NotAvailablef("sandbox: session starter not configured")
	}
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("sandbox: SessionSpec.Argv must name a command")
	}
	id := spec.ID
	if id == "" {
		id = xid.New().String()
	}

	rec := &sessionRecord{
		id:      id,
		spec:    spec,
		started: time.Now(),
		ready:   make(chan struct{}),
	}
	r.mu.Lock()
	if _, exists := r.sessions[id]; exists {
		r.mu.Unlock()
		return nil, errdefs.Conflictf("sandbox: session id %q already exists", id)
	}
	r.sessions[id] = rec
	r.mu.Unlock()

	// Resolve the ID before spawning so the backend's Session handle
	// and the registry record share one identifier.
	spec.ID = id
	sess, err := r.starter(ctx, spec)
	if err != nil {
		r.remove(id)
		close(rec.ready)
		return nil, err
	}
	if sess == nil {
		r.remove(id)
		close(rec.ready)
		return nil, errdefs.Internalf("sandbox: session starter returned a nil session")
	}

	r.mu.Lock()
	rec.session = sess
	rec.pid = sess.PID()
	r.mu.Unlock()
	close(rec.ready)

	go r.track(id, sess)
	return &registrySession{inner: sess, reg: r, id: id}, nil
}

func (r *SessionRegistry) track(id string, sess Session) {
	exit, err := sess.Wait(context.Background())
	r.mu.Lock()
	if rec := r.sessions[id]; rec != nil {
		rec.exited = true
		rec.exit = exit
		rec.err = err
	}
	r.mu.Unlock()
}

func (r *SessionRegistry) List(context.Context) ([]SessionInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionInfo, 0, len(r.sessions))
	for _, rec := range r.sessions {
		info := SessionInfo{
			ID:        rec.id,
			Argv:      append([]string(nil), rec.spec.Argv...),
			TTY:       rec.spec.TTY,
			PID:       rec.pid,
			StartedAt: rec.started,
			Running:   !rec.exited,
		}
		if rec.exited {
			info.Exit = &rec.exit
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

func (r *SessionRegistry) Terminate(ctx context.Context, id string) error {
	r.mu.Lock()
	rec := r.sessions[id]
	if rec == nil {
		r.mu.Unlock()
		return errdefs.NotFoundf("sandbox: unknown session id %q", id)
	}
	exited := rec.exited
	session := rec.session
	r.mu.Unlock()
	if exited {
		return nil
	}
	if session == nil {
		return errdefs.NotAvailablef("sandbox: session %q is still starting", id)
	}
	if err := session.Terminate(ctx); err != nil {
		return err
	}
	// Synchronise the record: List must reflect the termination as soon
	// as Terminate returns, not whenever the background tracker next
	// gets scheduled. Wait is already satisfied for a terminated
	// session, so this is a quick state sync, not a second reaping.
	waitCtx, cancel := context.WithTimeout(context.Background(), registryWaitTimeout)
	defer cancel()
	exit, err := rec.session.Wait(waitCtx)
	r.mu.Lock()
	if cur := r.sessions[id]; cur == rec && !rec.exited {
		rec.exited = true
		rec.exit = exit
		rec.err = err
	}
	r.mu.Unlock()
	return nil
}

// Close terminates every session still tracked by the registry. It is
// the lifecycle drain backends delegate to from [Runner.Close]: after
// Close returns, no session started through this registry remains
// running. Records are kept so List still reports what ran; repeated
// calls are safe because Terminate skips already-exited sessions.
//
// Close is bounded: the whole drain shares one deadline
// (registryCloseTimeout) so a Session whose Terminate blocks cannot
// wedge shutdown. Sessions still starting are waited on rather than
// reported as NotAvailable, so a start race does not fail Close.
func (r *SessionRegistry) Close() error {
	return r.closeWithTimeout(registryCloseTimeout)
}

// closeWithTimeout is Close with an explicit overall budget. It is
// split out so tests can exercise the deadline without waiting the
// full registryCloseTimeout.
func (r *SessionRegistry) closeWithTimeout(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	r.mu.Lock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	r.mu.Unlock()

	var errs []error
	for _, id := range ids {
		if err := r.closeOne(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// closeOne terminates one session for Close. When the session is still
// starting (its record is published before the spawn returns), it waits
// for the spawn to settle — bounded by ctx — so a start race does not
// surface as a spurious NotAvailable error. A record that vanished in
// the meantime (spawn failed, or the session was already closed) has
// nothing to drain and is not an error.
func (r *SessionRegistry) closeOne(ctx context.Context, id string) error {
	r.mu.Lock()
	rec := r.sessions[id]
	starting := rec != nil && rec.session == nil && !rec.exited
	r.mu.Unlock()

	if rec == nil {
		return nil
	}
	if starting {
		select {
		case <-rec.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		r.mu.Lock()
		rec = r.sessions[id]
		r.mu.Unlock()
		if rec == nil {
			return nil
		}
	}
	if err := r.Terminate(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *SessionRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

// registrySession forwards to the underlying session and removes the
// registry record on Close. ID is the registry-resolved ID so callers
// always see the stable identifier, including manager-generated ones.
type registrySession struct {
	inner Session
	reg   *SessionRegistry
	id    string
	once  sync.Once
}

func (s *registrySession) ID() string { return s.id }
func (s *registrySession) PID() int   { return s.inner.PID() }

func (s *registrySession) Read(ctx context.Context, afterSeq int64, maxBytes int) (SessionOutput, error) {
	return s.inner.Read(ctx, afterSeq, maxBytes)
}

func (s *registrySession) Write(ctx context.Context, data []byte) error {
	return s.inner.Write(ctx, data)
}

func (s *registrySession) CloseInput() error {
	return s.inner.CloseInput()
}

func (s *registrySession) Resize(ctx context.Context, rows, cols int) error {
	return s.inner.Resize(ctx, rows, cols)
}

func (s *registrySession) Signal(ctx context.Context, sig SessionSignal) error {
	return s.inner.Signal(ctx, sig)
}

func (s *registrySession) Terminate(ctx context.Context) error {
	return s.inner.Terminate(ctx)
}

func (s *registrySession) Wait(ctx context.Context) (SessionExit, error) {
	return s.inner.Wait(ctx)
}

func (s *registrySession) Watch(ctx context.Context) (SessionWatcher, error) {
	return s.inner.Watch(ctx)
}

func (s *registrySession) Close() error {
	err := s.inner.Close()
	s.once.Do(func() { s.reg.remove(s.id) })
	return err
}

// Capabilities forwards the underlying session's declaration. The
// wrapper adds nothing and narrows nothing.
func (s *registrySession) Capabilities() SessionCapabilities {
	return s.inner.Capabilities()
}
