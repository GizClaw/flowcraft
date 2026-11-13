package remote

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// Client defaults.
const (
	// defaultHold is how long a read or a wait asks the server to hold the
	// request open. It must stay under any proxy's idle timeout, which is why
	// it is smaller than the server's ceiling.
	defaultPollHold = 20 * time.Second
	// defaultRetries is how many times an operation that is safe to repeat is
	// retried after a transport failure. Writes are never retried here.
	defaultRetries = 3
	// drainTimeout bounds the best-effort session drain on Close.
	drainTimeout = 10 * time.Second
	// operationTimeout bounds an operation whose interface takes no context.
	operationTimeout = 10 * time.Second
)

// Runner is a sandbox.Runner that executes on a remote server.
//
// It is a proxy, not an implementation: every operation is one message, and
// every capability it reports is the server's own declaration. The one exception
// is documented rather than silent — [sandbox.SessionFeatures].Events is cleared
// because this client does not proxy the push stream yet, so a caller that reads
// Capabilities learns that Watch is unavailable before trying it.
//
// Policy stays on this side of the wire: engine decorators wrap this runner
// exactly like a local one, and the server re-validates what arrives.
type Runner struct {
	transport Transport
	caps      sandbox.Capabilities
	hold      time.Duration
	retries   int

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// Option configures a Runner.
type Option func(*clientOptions)

type clientOptions struct {
	token     string
	transport Transport
	http      []HTTPOption
	hold      time.Duration
	retries   int
}

// WithToken authenticates every request with a bearer token.
func WithToken(token string) Option {
	return func(o *clientOptions) { o.token = token }
}

// WithTransport replaces the transport entirely. It is what a test uses to drive
// the client without a listener, and what a future bidirectional transport plugs
// into.
func WithTransport(t Transport) Option {
	return func(o *clientOptions) { o.transport = t }
}

// WithHTTPOptions passes transport-level options (TLS, client, timeout) to the
// default HTTP transport. They are nested rather than flattened so the client and
// transport vocabularies cannot collide as each grows.
func WithHTTPOptions(opts ...HTTPOption) Option {
	return func(o *clientOptions) { o.http = append(o.http, opts...) }
}

// WithPollHold sets how long a read or a wait asks the server to hold. A short
// hold costs more requests; a long one costs latency on cancellation.
func WithPollHold(d time.Duration) Option {
	return func(o *clientOptions) {
		if d > 0 {
			o.hold = d
		}
	}
}

// New connects to a server and negotiates the version.
//
// The connection is not a resource the server owns: closing the returned Runner
// terminates the sessions this client started and releases its handles, and
// leaves the server's runner alone.
func New(ctx context.Context, url string, opts ...Option) (*Runner, error) {
	cfg := clientOptions{hold: defaultPollHold, retries: defaultRetries}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.transport == nil {
		httpOpts := append([]HTTPOption{WithHTTPToken(cfg.token)}, cfg.http...)
		cfg.transport = NewHTTPTransport(url, httpOpts...)
	}

	r := &Runner{
		transport: cfg.transport,
		hold:      cfg.hold,
		retries:   cfg.retries,
		sessions:  make(map[string]*session),
	}
	hello, err := r.hello(ctx)
	if err != nil {
		return nil, err
	}
	if hello.Version != wire.Version {
		return nil, errdefs.NotAvailablef(
			"sandbox-remote: server speaks wire version %d, this client speaks %d",
			hello.Version, wire.Version)
	}
	r.caps = hello.Capabilities
	// Watch is not proxied yet: say so instead of advertising a capability
	// that would fail on the first call.
	r.caps.Features.Events = false
	return r, nil
}

func (r *Runner) hello(ctx context.Context) (wire.Hello, error) {
	var out wire.Hello
	if err := r.transport.Do(ctx, OpHello, nil, &out); err != nil {
		return wire.Hello{}, err
	}
	return out, nil
}

// Capabilities implements sandbox.Runner: the server's declaration, with the
// push-event feature cleared because this client does not carry it.
func (r *Runner) Capabilities() sandbox.Capabilities { return r.caps }

// Start implements sandbox.Runner.
func (r *Runner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	var out wire.StartResponse
	if err := r.do(ctx, OpStart, wire.FromSpec(spec), &out); err != nil {
		return nil, err
	}
	return r.track(out), nil
}

// Attach implements sandbox.SessionAttacher: a handle to a session the server
// already manages, addressed by the id it minted.
func (r *Runner) Attach(ctx context.Context, id string) (sandbox.Session, error) {
	var out wire.StartResponse
	if err := r.do(ctx, OpAttach, wire.SessionRef{ID: id}, &out); err != nil {
		return nil, err
	}
	return r.track(out), nil
}

// List implements sandbox.Runner.
func (r *Runner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	var out []sandbox.SessionInfo
	err := r.retry(ctx, OpList, nil, &out)
	return out, err
}

// Terminate implements sandbox.Runner: the management path, which kills the
// process by id. A session handle in this client stays readable afterwards,
// exactly as the contract says of a terminated session.
func (r *Runner) Terminate(ctx context.Context, id string) error {
	return r.do(ctx, OpTerminate, wire.SessionRef{ID: id}, nil)
}

// OpenJournal implements sandbox.JournalProvider.
func (r *Runner) OpenJournal(ctx context.Context) (sandbox.FileJournal, error) {
	if !r.caps.Journal.Enabled {
		return nil, errdefs.NotAvailablef(
			"sandbox-remote: the server runner has no file journal attached")
	}
	if err := r.closedErr(); err != nil {
		return nil, err
	}
	return &journal{runner: r}, nil
}

// Close implements sandbox.Runner: it terminates the sessions this client still
// holds and releases them. Sessions started through other clients of the same
// server are untouched — they are not this client's to end.
func (r *Runner) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	held := make([]*session, 0, len(r.sessions))
	for id, s := range r.sessions {
		held = append(held, s)
		delete(r.sessions, id)
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	var err error
	for _, s := range held {
		if cerr := s.close(ctx); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// do sends one message and decodes the answer. A closed runner refuses
// everything except the drain Close already performs.
func (r *Runner) do(ctx context.Context, op Op, req any, res any) error {
	if err := r.closedErr(); err != nil {
		return err
	}
	body, err := encodeRequest(req)
	if err != nil {
		return err
	}
	return r.transport.Do(ctx, op, body, res)
}

// retry repeats an operation that is safe to repeat — a read is a cursor, a wait
// is a cache lookup, a list is a snapshot — after a transport failure. Anything
// the server answered is final and is not retried.
func (r *Runner) retry(ctx context.Context, op Op, req any, res any) error {
	body, err := encodeRequest(req)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt <= r.retries; attempt++ {
		last = r.transport.Do(ctx, op, body, res)
		if last == nil {
			return nil
		}
		if !isTransportError(last) || ctx.Err() != nil {
			return last
		}
	}
	return last
}

func encodeRequest(req any) ([]byte, error) {
	if req == nil {
		return nil, nil
	}
	body, err := wire.Encode(req)
	if err != nil {
		return nil, errdefs.Validationf("sandbox-remote: encode %T: %w", req, err)
	}
	return body, nil
}

// isTransportError reports whether the failure happened before the server
// answered, which is the only case worth repeating.
func isTransportError(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}

// holdMS is the hold the client asks for, in the unit the wire carries.
func (r *Runner) holdMS() int { return int(r.hold / time.Millisecond) }

func (r *Runner) closedErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errdefs.NotAvailablef("sandbox-remote: this runner is closed")
	}
	return nil
}

func (r *Runner) track(resp wire.StartResponse) sandbox.Session {
	s := &session{
		runner: r,
		id:     resp.ID,
		pid:    resp.PID,
		caps:   resp.Capabilities,
	}
	r.mu.Lock()
	r.sessions[s.id] = s
	r.mu.Unlock()
	return s
}

func (r *Runner) forget(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

var (
	_ sandbox.Runner          = (*Runner)(nil)
	_ sandbox.JournalProvider = (*Runner)(nil)
	_ sandbox.SessionAttacher = (*Runner)(nil)
)
