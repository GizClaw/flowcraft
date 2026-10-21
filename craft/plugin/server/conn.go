package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
)

// ConnOptions describes the process a connection is being opened for.
type ConnOptions struct {
	PluginID   string
	Version    string
	Generation uint64
	Grants     GrantSet
	// Validate checks a declaration before craft accepts it. A declaration
	// it refuses is a failed Declare, which is the one moment the plugin can
	// still be told why: the tools it named would otherwise be published
	// under names craft cannot use.
	Validate func(*protocol.Declaration) error
}

// Conn is one plugin process's connection: the socket craft listens on for
// it, the two services served over that socket, and the state the plugin
// declares through them.
//
// The connection is the identity. There is no token to mint, revoke, or
// leak: the grants in this struct are the ones Hello reports back, the Host
// service in front of them is bound to this Conn, and a process that dies
// takes both with its file descriptor.
type Conn struct {
	protocol.UnimplementedPluginServer

	srv  *Server
	opts ConnOptions

	path     string
	listener net.Listener
	server   *grpc.Server

	mu          sync.Mutex
	hello       *protocol.HelloRequest
	declaration *protocol.Declaration
	revision    uint64
	subscribers []declareHook
	nextHook    uint64
	streams     []*stream
	ready       bool
	closed      bool
	err         error
	nextStream  int

	readyCh   chan struct{}
	readyOnce sync.Once
	closedCh  chan struct{}
	closeOnce sync.Once
	drainedCh chan struct{}
	drainOnce sync.Once

	// capacity bounds the calls in flight; the plugin declared it in
	// Hello, so the host applies exactly the backpressure the plugin asked
	// for instead of queueing work the process cannot take.
	capacity chan struct{}
	semOnce  sync.Once

	// calls numbers the invocations this connection dispatches. It is what
	// correlates a CallResult with the CallRequest that asked for it.
	calls atomic.Uint64

	// rateStart and rateCount are the fixed one-second window of this
	// connection's primitive calls. It is per connection because the
	// connection is the identity: there is no token to key it by.
	rateStart time.Time
	rateCount int
}

// declareHook is one OnDeclare subscription. Subscriptions are identified
// by sequence number because func values cannot be compared.
type declareHook struct {
	id uint64
	fn func(*protocol.Declaration)
}

// stream is one plugin-opened Call stream. Sends are serialized because
// gRPC allows one sender at a time; the invocations carried on it overlap
// and are correlated by call id.
type stream struct {
	stream  grpc.BidiStreamingServer[protocol.CallEvent, protocol.CallEvent]
	sendMu  sync.Mutex
	pending sync.Map // call id -> chan *protocol.CallEvent
}

func (s *stream) send(event *protocol.CallEvent) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(event)
}

// deliver hands one terminal event to the call waiting for it.
func (s *stream) deliver(callID string, event *protocol.CallEvent) {
	value, ok := s.pending.LoadAndDelete(callID)
	if !ok {
		return
	}
	if ch, ok := value.(chan *protocol.CallEvent); ok {
		deliver(ch, event)
	}
}

// NewConn opens one plugin's socket. It does not start a process: the
// caller spawns the plugin with [protocol.WireEnv] pointing here and then
// waits with [Conn.WaitReady].
func (s *Server) NewConn(opts ConnOptions) (*Conn, error) {
	if opts.PluginID == "" {
		return nil, errdefs.Validationf("plugin host: plugin id is required")
	}
	path, err := socketPath(s.runtimeDir)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		if isUnsupportedSocketError(err) {
			return nil, UnsupportedError()
		}
		return nil, errdefs.NotAvailablef(
			"plugin %s: listen on %s: %w", opts.PluginID, path, err)
	}
	conn := &Conn{
		srv:       s,
		opts:      opts,
		path:      path,
		listener:  listener,
		readyCh:   make(chan struct{}),
		closedCh:  make(chan struct{}),
		drainedCh: make(chan struct{}),
	}
	// The size bound Hello advertises is the bound this server enforces: an
	// argument or result larger than MaxCallBytes is refused by the
	// transport instead of being advertised as refused.
	limit := int(s.maxCallBytes)
	conn.server = grpc.NewServer(
		grpc.MaxRecvMsgSize(limit),
		grpc.MaxSendMsgSize(limit),
	)
	protocol.RegisterPluginServer(conn.server, conn)
	protocol.RegisterHostServer(conn.server, &hostService{conn: conn})
	s.track(conn)
	go conn.serve()
	return conn, nil
}

// Path is the socket the plugin dials.
func (c *Conn) Path() string { return c.path }

// PluginID names the plugin this connection belongs to.
func (c *Conn) PluginID() string { return c.opts.PluginID }

// Generation is the process this connection was opened for.
func (c *Conn) Generation() uint64 { return c.opts.Generation }

func (c *Conn) serve() {
	// A listener craft closes stops this goroutine with ErrClosed, which is
	// the expected end and not worth a line. Anything else means the
	// endpoint stopped serving on its own, which the host has to see.
	if err := c.server.Serve(c.listener); err != nil &&
		!errors.Is(err, net.ErrClosed) &&
		!errors.Is(err, grpc.ErrServerStopped) {
		c.fail(errdefs.NotAvailablef(
			"plugin %s: the wire stopped serving: %w", c.opts.PluginID, err))
	}
}

// Hello is the first call on a connection. It is where the grants are
// stated (never in the manifest alone) and where a wire mismatch is
// refused.
func (c *Conn) Hello(
	_ context.Context,
	request *protocol.HelloRequest,
) (*protocol.HelloResponse, error) {
	if err := protocol.CheckCompatible(request.GetWireVersion()); err != nil {
		c.fail(err)
		return nil, err
	}
	if request.GetPluginId() != c.opts.PluginID {
		err := errdefs.Validationf(
			"plugin host: connection for %q announced itself as %q",
			c.opts.PluginID, request.GetPluginId())
		c.fail(err)
		return nil, err
	}
	negotiated, err := protocol.NegotiatedVersion(request.GetWireVersion())
	if err != nil {
		c.fail(err)
		return nil, err
	}
	c.mu.Lock()
	if c.hello != nil {
		c.mu.Unlock()
		return nil, errdefs.Conflictf(
			"plugin host: %s has already said Hello on this connection",
			c.opts.PluginID)
	}
	c.hello = request
	c.mu.Unlock()
	return &protocol.HelloResponse{
		WireVersion:        negotiated,
		SessionId:          c.opts.PluginID,
		Generation:         c.opts.Generation,
		GrantedPermissions: c.grants(),
		MaxCallBytes:       c.srv.maxCallBytes,
	}, nil
}

// Declare replaces the plugin's whole contribution with one snapshot. A
// revision that is not newer than the one craft holds is refused: the
// snapshot model makes a stale one a bug in the plugin, not a state to
// merge.
func (c *Conn) Declare(
	_ context.Context,
	request *protocol.DeclareRequest,
) (*protocol.DeclareResponse, error) {
	revision := request.GetRevision()
	if c.opts.Validate != nil {
		if err := c.opts.Validate(request.GetDeclaration()); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	if revision <= c.revision {
		current := c.revision
		c.mu.Unlock()
		return nil, errdefs.Validationf(
			"plugin %s: declaration revision %d is not newer than %d",
			c.opts.PluginID, revision, current)
	}
	c.declaration = request.GetDeclaration()
	c.revision = revision
	subscribers := append([]declareHook(nil), c.subscribers...)
	c.mu.Unlock()
	for _, hook := range subscribers {
		hook.fn(request.GetDeclaration())
	}
	return &protocol.DeclareResponse{Revision: revision}, nil
}

// Ready says the declaration is in effect. It waits briefly for the Call
// streams that make dispatch possible: the plugin opens them before Ready,
// but the two are different RPCs and gRPC orders nothing between them.
func (c *Conn) Ready(
	ctx context.Context,
	_ *protocol.ReadyRequest,
) (*protocol.ReadyResponse, error) {
	if err := c.awaitStream(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	c.readyOnce.Do(func() { close(c.readyCh) })
	return &protocol.ReadyResponse{}, nil
}

// awaitStream blocks until at least one Call stream is registered, or ctx
// ends. It is not a poll loop over the network: the two halves of the
// handshake are separate RPCs, and this is the one place they are joined.
func (c *Conn) awaitStream(ctx context.Context) error {
	deadline := time.Now().Add(readyStreamGrace)
	for {
		c.mu.Lock()
		open := len(c.streams)
		closed := c.closed
		c.mu.Unlock()
		if open > 0 {
			return nil
		}
		if closed {
			return errdefs.NotAvailablef(
				"plugin %s: the connection is closed", c.opts.PluginID)
		}
		if time.Now().After(deadline) {
			return errdefs.Validationf(
				"plugin %s: Ready arrived with no Call stream open",
				c.opts.PluginID)
		}
		select {
		case <-ctx.Done():
			return errdefs.FromContext(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// readyStreamGrace bounds the wait for a plugin's Call streams between its
// Hello and its Ready. It is short on purpose: the streams are opened by the
// same process, and a plugin that takes longer than this to open one is not
// one craft should be dispatching work to.
const readyStreamGrace = 5 * time.Second

// Call is the reverse direction: craft writes invocations into the stream
// the plugin opened, and reads results back from it.
func (c *Conn) Call(s grpc.BidiStreamingServer[protocol.CallEvent, protocol.CallEvent]) error {
	opened := &stream{stream: s}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errdefs.NotAvailablef(
			"plugin %s: the connection is closed", c.opts.PluginID)
	}
	c.streams = append(c.streams, opened)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		for i, candidate := range c.streams {
			if candidate == opened {
				c.streams = append(c.streams[:i], c.streams[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		opened.pending.Range(func(key, value any) bool {
			opened.pending.Delete(key)
			if ch, ok := value.(chan *protocol.CallEvent); ok {
				deliver(ch, nil)
			}
			return true
		})
	}()
	for {
		event, err := s.Recv()
		if err != nil {
			return nil
		}
		switch {
		case event.GetResult() != nil:
			opened.deliver(event.GetResult().GetCallId(), event)
		case event.GetError() != nil:
			opened.deliver(event.GetError().GetCallId(), event)
		case event.GetProgress() != nil:
			// Progress is advisory: the host has nowhere to put it yet,
			// and dropping it is better than queueing it behind a call
			// that has already answered.
		case event.GetDrained() != nil:
			c.drainOnce.Do(func() { close(c.drainedCh) })
		}
	}
}

// deliver is a non-blocking send: the receiver is either there or gone, and
// a call whose receiver gave up must not wedge the stream's reader.
func deliver(ch chan *protocol.CallEvent, event *protocol.CallEvent) {
	select {
	case ch <- event:
	default:
	}
}

// Declaration returns the snapshot craft currently holds and its revision.
// The declaration is nil before the plugin declares anything.
func (c *Conn) Declaration() (*protocol.Declaration, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.declaration, c.revision
}

// IsReady reports whether the plugin finished its handshake.
func (c *Conn) IsReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// OnDeclare subscribes to declaration changes. The returned function
// unsubscribes this one; calling it more than once is safe.
func (c *Conn) OnDeclare(fn func(*protocol.Declaration)) func() {
	if fn == nil {
		return func() {}
	}
	c.mu.Lock()
	c.nextHook++
	hook := declareHook{id: c.nextHook, fn: fn}
	c.subscribers = append(c.subscribers, hook)
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			for i, candidate := range c.subscribers {
				if candidate.id == hook.id {
					c.subscribers = append(c.subscribers[:i], c.subscribers[i+1:]...)
					return
				}
			}
		})
	}
}

// Grants returns the permissions craft decided on for this process.
func (c *Conn) Grants() GrantSet {
	grants := make(GrantSet, len(c.opts.Grants))
	for key, value := range c.opts.Grants {
		grants[key] = value
	}
	return grants
}

func (c *Conn) grants() []string {
	out := make([]string, 0, len(c.opts.Grants))
	for grant := range c.opts.Grants {
		out = append(out, grant)
	}
	return out
}

// WaitReady blocks until the plugin has said Ready, reporting why it never
// will. A Validation-class error is terminal — the connection can never
// become ready — which is what makes an enable roll back.
func (c *Conn) WaitReady(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-c.readyCh:
		return nil
	case <-c.closedCh:
		if err := c.Err(); err != nil {
			return err
		}
		return errdefs.NotAvailablef(
			"plugin %s: the connection closed before Ready", c.opts.PluginID)
	case <-ctx.Done():
		return c.notReady(ctx.Err())
	case <-time.After(timeout):
		return c.notReady(context.DeadlineExceeded)
	}
}

func (c *Conn) notReady(cause error) error {
	if err := c.Err(); err != nil {
		return err
	}
	return errdefs.Timeoutf(
		"plugin %s: not ready after the wire handshake: %v", c.opts.PluginID, cause)
}

// Err returns the terminal error of this connection, if the handshake
// failed or the endpoint stopped serving.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) fail(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closedCh) })
}

// Fail records a terminal error on this connection and releases everything
// waiting on it. It is how the host reports a process that exited on its
// own: the socket craft is listening on stays open — nothing told it to
// close — so the connection would otherwise wait out its timeout for a
// plugin that is already gone.
func (c *Conn) Fail(err error) { c.fail(err) }

// CallTool invokes one declared tool. It is the craft-side half of the
// reverse RPC: acquire a slot the plugin declared, pick a stream, write the
// request, wait for the correlated answer.
func (c *Conn) CallTool(
	ctx context.Context,
	toolName string,
	arguments []byte,
	timeout time.Duration,
) ([]byte, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	target, err := c.pickStream()
	if err != nil {
		return nil, err
	}
	callID := fmt.Sprintf("%s-%d", c.opts.PluginID, c.calls.Add(1))
	reply := make(chan *protocol.CallEvent, 1)
	target.pending.Store(callID, reply)
	defer target.pending.Delete(callID)
	request := &protocol.CallEvent{Event: &protocol.CallEvent_Request{
		Request: &protocol.CallRequest{
			CallId:        callID,
			Tool:          toolName,
			ArgumentsJson: arguments,
			TimeoutMillis: timeout.Milliseconds(),
		},
	}}
	if err := target.send(request); err != nil {
		return nil, errdefs.NotAvailablef(
			"plugin %s: send call to %q: %w", c.opts.PluginID, toolName, err)
	}
	select {
	case event := <-reply:
		if event == nil {
			return nil, errdefs.NotAvailablef(
				"plugin %s: the call stream closed during %q",
				c.opts.PluginID, toolName)
		}
		if failure := event.GetError(); failure != nil {
			return nil, protocol.ErrorOf(failure)
		}
		return event.GetResult().GetResultJson(), nil
	case <-c.closedCh:
		return nil, errdefs.NotAvailablef(
			"plugin %s: the connection closed during %q",
			c.opts.PluginID, toolName)
	case <-ctx.Done():
		return nil, errdefs.FromContext(ctx.Err())
	}
}

// acquire takes one of the slots the plugin declared in Hello.
func (c *Conn) acquire(ctx context.Context) error {
	c.semOnce.Do(func() {
		size := 1
		c.mu.Lock()
		if declared := int(c.hello.GetMaxConcurrentCalls()); declared > 0 {
			size = declared
		}
		c.mu.Unlock()
		if size > maxConcurrentCalls {
			size = maxConcurrentCalls
		}
		c.capacity = make(chan struct{}, size)
	})
	select {
	case c.capacity <- struct{}{}:
		return nil
	case <-c.closedCh:
		return errdefs.NotAvailablef(
			"plugin %s: the connection is closed", c.opts.PluginID)
	case <-ctx.Done():
		return errdefs.FromContext(ctx.Err())
	}
}

func (c *Conn) release() {
	select {
	case <-c.capacity:
	default:
	}
}

// maxConcurrentCalls bounds what a plugin may declare. A process that asks
// for a hundred thousand slots is asking the host to open that many calls
// against it.
const maxConcurrentCalls = 64

// pickStream chooses the next Call stream in rotation. Any number of
// streams is accepted; the declared capacity, not the stream count, is what
// bounds concurrency.
func (c *Conn) pickStream() (*stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errdefs.NotAvailablef(
			"plugin %s: the connection is closed", c.opts.PluginID)
	}
	if len(c.streams) == 0 {
		return nil, errdefs.NotAvailablef(
			"plugin %s: no Call stream is open", c.opts.PluginID)
	}
	c.nextStream = (c.nextStream + 1) % len(c.streams)
	return c.streams[c.nextStream], nil
}

// Drain asks the plugin to stop accepting new calls and waits, bounded, for
// it to acknowledge. A plugin that never does is reported; the caller
// closes the connection anyway.
func (c *Conn) Drain(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	c.mu.Lock()
	streams := append([]*stream(nil), c.streams...)
	c.mu.Unlock()
	if len(streams) == 0 {
		return nil
	}
	event := &protocol.CallEvent{Event: &protocol.CallEvent_Drain{
		Drain: &protocol.DrainRequest{TimeoutMillis: timeout.Milliseconds()},
	}}
	for _, target := range streams {
		// Best effort: a stream that already ended is not a drain
		// failure, and the other streams still carry the message.
		_ = target.send(event)
	}
	select {
	case <-c.drainedCh:
		return nil
	case <-c.closedCh:
		return nil
	case <-ctx.Done():
		return errdefs.Timeoutf(
			"plugin %s: drain did not complete: %w", c.opts.PluginID, ctx.Err())
	case <-time.After(timeout):
		return errdefs.Timeoutf(
			"plugin %s: drain did not complete in %s", c.opts.PluginID, timeout)
	}
}

// Close tears the connection down: the listener and socket go away, the
// server stops, and every wait on this connection is released.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closedCh) })
	c.srv.untrack(c)
	// Stop ends the Call streams, which is what releases every call still
	// waiting on one; the graceful path would wait for a plugin that has
	// already been told to drain.
	c.server.Stop()
	err := c.listener.Close()
	if removeErr := removeSocket(c.path); removeErr != nil {
		telemetry.WarnErr(context.Background(),
			"plugin host: cannot remove the plugin socket", removeErr,
			attribute.String("plugin", c.opts.PluginID))
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return errdefs.Internal(err)
	}
	return nil
}

// allow implements the fixed one-second window of this connection's
// primitive calls.
func (c *Conn) allow() bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rateStart.IsZero() || now.Sub(c.rateStart) >= time.Second {
		c.rateStart = now
		c.rateCount = 0
	}
	c.rateCount++
	return c.rateCount <= c.srv.limit
}
