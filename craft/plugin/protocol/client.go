package protocol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The environment craft hands a plugin process. WireEnv names the socket
// the plugin dials; the plugin never listens on anything itself.
const (
	WireEnv    = "FLOWCRAFT_PLUGIN_WIRE"
	IDEnv      = "FLOWCRAFT_PLUGIN_ID"
	DataDirEnv = "FLOWCRAFT_PLUGIN_DATA_DIR"
)

// Target renders a unix socket path as a gRPC target. The endpoint form
// ("unix:" followed by the path) is the one the resolver accepts on every
// platform, unlike the "unix:///" form, which reads the leading slash as
// an authority on Windows, where the path starts with a drive letter.
func Target(path string) string {
	return "unix:" + filepath.ToSlash(path)
}

// Endpoint reads the socket path craft advertised to this process.
func Endpoint() (string, error) {
	value := os.Getenv(WireEnv)
	if value == "" {
		return "", errdefs.Validationf(
			"plugin: %s is not set; a plugin process is started by craft", WireEnv)
	}
	return value, nil
}

// Handler is the plugin half of the wire: what the plugin contributes,
// and how it answers one invocation.
type Handler interface {
	// Declare returns the plugin's whole contribution. It is called once
	// per connection, after Hello; craft replaces whatever it held with the
	// returned snapshot. host is this connection's primitive surface: pass
	// it to [Invoke] to run one of the primitives host.about lists.
	Declare(ctx context.Context, hello *HelloResponse, host HostClient) (*Declaration, error)
	// Call answers one invocation. The result is JSON, because a plugin's
	// tool schemas are only known at runtime; a returned error crosses the
	// socket with the class KindOf gives it.
	Call(ctx context.Context, request *CallRequest) ([]byte, error)
}

// ClientOptions configures the plugin half of one connection.
type ClientOptions struct {
	PluginID string
	Version  string
	// Concurrency is how many invocations this process will run at once,
	// and therefore how many Call streams it opens.
	Concurrency int
	// Capabilities are the optional features this plugin understands.
	Capabilities []string
}

// Client is one plugin's connection to craft.
type Client struct {
	conn   *grpc.ClientConn
	plugin PluginClient
	host   HostClient
	opts   ClientOptions

	mu       sync.RWMutex
	hello    *HelloResponse
	streams  []*callStream
	draining bool
	drained  chan struct{}
	drainOne sync.Once
}

// Dial connects to the socket craft advertised.
func Dial(ctx context.Context, endpoint string, opts ClientOptions) (*Client, error) {
	if endpoint == "" {
		return nil, errdefs.Validationf("plugin: endpoint is required")
	}
	conn, err := grpc.NewClient(
		Target(endpoint),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, errdefs.NotAvailablef("plugin: dial %s: %w", endpoint, err)
	}
	return &Client{
		conn:    conn,
		plugin:  NewPluginClient(conn),
		host:    NewHostClient(conn),
		opts:    opts,
		drained: make(chan struct{}),
	}, nil
}

// Host is the primitive surface this connection may use. The connection
// is the identity, so there is nothing to pass in.
func (c *Client) Host() HostClient { return c.host }

// Invoke runs one host primitive on this connection; see the package-level
// [Invoke].
func (c *Client) Invoke(
	ctx context.Context,
	resource, op string,
	params, out any,
) error {
	return Invoke(ctx, c.host, resource, op, params, out)
}

// Hello opens the connection: it negotiates the wire version and learns
// which grants craft decided on and which generation this process is.
func (c *Client) Hello(ctx context.Context) (*HelloResponse, error) {
	response, err := c.plugin.Hello(ctx, &HelloRequest{
		WireVersion:        Version,
		PluginId:           c.opts.PluginID,
		PluginVersion:      c.opts.Version,
		MaxConcurrentCalls: uint32(c.Concurrency()),
		Capabilities:       c.opts.Capabilities,
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.hello = response
	c.mu.Unlock()
	return response, nil
}

// Concurrency is how many Call streams this client opens.
func (c *Client) Concurrency() int {
	if c.opts.Concurrency < 1 {
		return 1
	}
	return c.opts.Concurrency
}

// HelloResponse returns what Hello answered, or nil before it ran.
func (c *Client) HelloResponse() *HelloResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hello
}

// Declare publishes one full snapshot of the plugin's contribution.
func (c *Client) Declare(
	ctx context.Context,
	revision uint64,
	declaration *Declaration,
) error {
	_, err := c.plugin.Declare(ctx, &DeclareRequest{
		Revision:    revision,
		Declaration: declaration,
	})
	return err
}

// Ready tells craft the declaration is in effect and calls may begin. It
// is sent after the Call streams are up: craft refuses a Ready that arrives
// with no way to dispatch work.
func (c *Client) Ready(ctx context.Context, revision uint64) error {
	_, err := c.plugin.Ready(ctx, &ReadyRequest{Revision: revision})
	return err
}

// Close releases the connection and every stream on it.
func (c *Client) Close() error {
	c.mu.Lock()
	streams := c.streams
	c.streams = nil
	c.mu.Unlock()
	for _, stream := range streams {
		stream.close()
	}
	return c.conn.Close()
}

// Serve runs the plugin half of one connection until craft closes it or ctx
// ends: Hello, the Call streams, Declare, Ready, then dispatch.
func (c *Client) Serve(ctx context.Context, handler Handler) error {
	if handler == nil {
		return errdefs.Validationf("plugin: handler is required")
	}
	hello, err := c.Hello(ctx)
	if err != nil {
		return err
	}
	for i := 0; i < c.Concurrency(); i++ {
		if err := c.openStream(ctx, handler); err != nil {
			return err
		}
	}
	declaration, err := handler.Declare(ctx, hello, c.host)
	if err != nil {
		return err
	}
	if declaration == nil {
		declaration = &Declaration{}
	}
	if err := c.Declare(ctx, 1, declaration); err != nil {
		return err
	}
	if err := c.Ready(ctx, 1); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case <-c.drained:
		return nil
	}
}

// callStream is one plugin-opened Call stream. Sends are serialized because
// gRPC allows one sender at a time, while the invocations on it overlap.
type callStream struct {
	stream grpc.BidiStreamingClient[CallEvent, CallEvent]
	sendMu sync.Mutex
	closed chan struct{}
	once   sync.Once
}

func (s *callStream) close() { s.once.Do(func() { close(s.closed) }) }

func (s *callStream) send(event *CallEvent) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(event)
}

func (c *Client) openStream(ctx context.Context, handler Handler) error {
	stream, err := c.plugin.Call(ctx)
	if err != nil {
		return errdefs.NotAvailablef("plugin: open call stream: %w", err)
	}
	opened := &callStream{stream: stream, closed: make(chan struct{})}
	c.mu.Lock()
	c.streams = append(c.streams, opened)
	c.mu.Unlock()
	go c.receive(opened, handler)
	return nil
}

// receive dispatches the requests craft writes into one stream. It returns
// when the stream ends — which for a healthy connection means craft closed
// it, and for a broken one means the host is gone.
func (c *Client) receive(stream *callStream, handler Handler) {
	defer stream.close()
	for {
		event, err := stream.stream.Recv()
		if err != nil {
			c.mu.Lock()
			for i, candidate := range c.streams {
				if candidate == stream {
					c.streams = append(c.streams[:i], c.streams[i+1:]...)
					break
				}
			}
			all := len(c.streams) == 0
			c.mu.Unlock()
			if all {
				c.finish()
			}
			return
		}
		switch {
		case event.GetDrain() != nil:
			c.beginDrain()
		case event.GetRequest() != nil:
			go c.dispatch(stream, handler, event.GetRequest())
		}
	}
}

// dispatch answers one invocation. A call that arrives once the drain began
// is refused rather than answered: the host is finishing what it already
// sent, and a new call would extend a wait that has already started.
func (c *Client) dispatch(
	stream *callStream,
	handler Handler,
	request *CallRequest,
) {
	callID := request.GetCallId()
	if c.isDraining() {
		_ = stream.send(&CallEvent{Event: &CallEvent_Error{Error: &CallError{
			Kind:    CallError_UNAVAILABLE,
			Message: "plugin is draining; retry its next generation",
			CallId:  callID,
		}}})
		return
	}
	ctx := context.Background()
	cancel := func() {}
	if millis := request.GetTimeoutMillis(); millis > 0 {
		ctx, cancel = context.WithTimeout(
			ctx, time.Duration(millis)*time.Millisecond)
	}
	defer cancel()
	payload, err := handler.Call(ctx, request)
	if err != nil {
		_ = stream.send(&CallEvent{Event: &CallEvent_Error{
			Error: NewCallError(err, callID),
		}})
		return
	}
	_ = stream.send(&CallEvent{Event: &CallEvent_Result{Result: &CallResult{
		CallId:     callID,
		ResultJson: payload,
	}}})
}

func (c *Client) isDraining() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.draining
}

// beginDrain stops the plugin accepting new calls and acknowledges once
// there is nothing left in flight.
func (c *Client) beginDrain() {
	c.mu.Lock()
	c.draining = true
	streams := append([]*callStream(nil), c.streams...)
	c.mu.Unlock()
	// The acknowledgement is best effort: the host tears the connection
	// down either way, and a send that fails because it already did is not
	// an error worth propagating to a process that is about to exit.
	for _, stream := range streams {
		_ = stream.send(&CallEvent{Event: &CallEvent_Drained{Drained: &Drained{}}})
	}
}

func (c *Client) finish() {
	c.drainOne.Do(func() { close(c.drained) })
}

// Main is the whole plugin entry point: it reads the endpoint craft
// advertised, dials it, and serves until the host is done. A plugin binary
// is a call to this and nothing else.
func Main(ctx context.Context, handler Handler, opts ClientOptions) error {
	endpoint, err := Endpoint()
	if err != nil {
		return err
	}
	client, err := Dial(ctx, endpoint, opts)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return client.Serve(ctx, handler)
}

// ErrClosed is what a call on a torn-down connection reports.
var ErrClosed = errors.New("plugin: the connection to the host is closed")
