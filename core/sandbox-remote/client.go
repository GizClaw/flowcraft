package remote

import (
	"context"
	"errors"
	"math/rand/v2"
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
	// retryBudget is what the backoff schedule below can add to one operation:
	// 20ms + 100ms + 500ms. A caller that needs a tighter bound sets its own
	// context.
	retryBudget = 620 * time.Millisecond
	// drainTimeout bounds the best-effort session drain on Close.
	drainTimeout = 10 * time.Second
	// operationTimeout bounds an operation whose interface takes no context.
	operationTimeout = 10 * time.Second
)

// Runner is a sandbox.Runner that executes on a remote server.
//
// It is a proxy, not an implementation: every operation is one message, and
// every capability it reports is the server's own declaration, verbatim. A
// capability the far end does not claim is a capability this client does not
// claim either — which is what makes reading Capabilities enough to decide.
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
	cfg := newClientOptions(opts)
	if cfg.transport == nil {
		cfg.transport = NewHTTPTransport(url, cfg.httpOptions()...)
	}
	return newRunner(ctx, cfg)
}

// newClientOptions applies the caller's options over the defaults.
func newClientOptions(opts []Option) clientOptions {
	cfg := clientOptions{hold: defaultPollHold, retries: defaultRetries}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// httpOptions is what the HTTP transport needs: the token, plus whatever the
// caller passed through WithHTTPOptions.
func (o clientOptions) httpOptions() []HTTPOption {
	return append([]HTTPOption{WithHTTPToken(o.token)}, o.http...)
}

// newRunner finishes construction: build the proxy, then negotiate the version.
func newRunner(ctx context.Context, cfg clientOptions) (*Runner, error) {
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

// doEncoded sends a body the caller already framed and decodes the answer. It is
// for the one op whose payload is not a JSON message.
func (r *Runner) doEncoded(ctx context.Context, op Op, body []byte, res any) error {
	if err := r.closedErr(); err != nil {
		return err
	}
	return r.transport.Do(ctx, op, body, res)
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
	return r.retryTransport(ctx, func() error {
		return r.transport.Do(ctx, op, body, res)
	})
}

// retryBytes is retry for an operation whose answer is a raw body.
func (r *Runner) retryBytes(ctx context.Context, op Op, req any) ([]byte, error) {
	body, err := encodeRequest(req)
	if err != nil {
		return nil, err
	}
	var out []byte
	err = r.retryTransport(ctx, func() error {
		got, callErr := r.transport.DoBytes(ctx, op, body)
		if callErr == nil {
			out = got
		}
		return callErr
	})
	return out, err
}

// retryTransport runs attempt until it succeeds, fails with something the server
// decided, or exhausts the budget.
//
// The wait between attempts is the point: retrying a connection failure
// immediately turns one troubled server into a stream of requests, and the second
// attempt would arrive before the first failure had a chance to clear.
func (r *Runner) retryTransport(ctx context.Context, attempt func() error) error {
	var last error
	for i := 0; i <= r.retries; i++ {
		if i > 0 {
			if err := retrySleep(ctx, retryBackoff(i-1)); err != nil {
				// The caller gave up while we were waiting: that is the more
				// truthful reason to report.
				return errdefs.FromContext(err)
			}
		}
		last = attempt()
		if last == nil {
			return nil
		}
		// A caller that has given up gets its own cancellation reported, not the
		// transport failure that happened to race it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errdefs.FromContext(ctxErr)
		}
		if !isTransportError(last) {
			return last
		}
	}
	return last
}

// backoffFor is the wait before retry attempt n (zero-based). It grows so a
// server that is restarting has time to come back, and stops growing at half a
// second so a doomed operation fails inside about one.
func backoffFor(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 20 * time.Millisecond
	case 1:
		return 100 * time.Millisecond
	default:
		return 500 * time.Millisecond
	}
}

// retryBackoff adds jitter to the schedule. A fleet that restarted together would
// otherwise retry in lockstep, which is the one case where these delays make a
// herd worse instead of better. Both it and retrySleep are variables so a test can
// assert the schedule without waiting for it.
var retryBackoff = func(attempt int) time.Duration {
	base := backoffFor(attempt)
	if base <= 0 {
		return 0
	}
	// Up to +20%: never shorter than the schedule, so the "do not hammer" property
	// holds for every draw.
	return base + time.Duration(rand.Int64N(int64(base)/5+1))
}

// retrySleep waits for d or until the context ends.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
