package server

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
)

// newTestServer builds the craft half of the wire for one test.
func newTestServer(t *testing.T, opts ServerOptions) *Server {
	t.Helper()
	if opts.Registry == nil {
		opts.Registry = NewRegistry()
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// newTestConn opens one connection for a plugin process that does not
// exist: these tests serve the plugin half in this process, so the
// handshake and the calls it enables are exercised without spawning
// anything.
func newTestConn(t *testing.T, srv *Server, opts ConnOptions) *Conn {
	t.Helper()
	conn, err := srv.NewConn(opts)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// servePlugin dials conn's socket and serves handler on it.
func servePlugin(
	t *testing.T,
	ctx context.Context,
	conn *Conn,
	handler protocol.Handler,
	opts protocol.ClientOptions,
) (*protocol.Client, chan error) {
	t.Helper()
	client, err := protocol.Dial(ctx, conn.Path(), opts)
	if err != nil {
		t.Fatalf("Dial(%s): %v", conn.Path(), err)
	}
	t.Cleanup(func() { _ = client.Close() })
	served := make(chan error, 1)
	go func() { served <- client.Serve(ctx, handler) }()
	return client, served
}

// echoPlugin declares two tools and answers them, so a test can drive a
// real connection without a real plugin.
type echoPlugin struct {
	hello     *protocol.HelloResponse
	tools     []string
	enterSlow chan struct{}
	release   chan struct{}
}

func (p *echoPlugin) Declare(
	ctx context.Context,
	hello *protocol.HelloResponse,
	host protocol.HostClient,
) (*protocol.Declaration, error) {
	p.hello = hello
	declaration := &protocol.Declaration{}
	for _, name := range p.tools {
		declaration.Tools = append(declaration.Tools, &protocol.ToolDecl{
			Name:        name,
			Description: "echo " + name,
			InputSchema: []byte(`{"type":"object"}`),
		})
	}
	return declaration, nil
}

func (p *echoPlugin) Call(
	ctx context.Context,
	request *protocol.CallRequest,
) ([]byte, error) {
	switch request.GetTool() {
	case "echo":
		return append([]byte("echo:"), request.GetArgumentsJson()...), nil
	case "slow":
		if p.enterSlow != nil {
			p.enterSlow <- struct{}{}
		}
		if p.release != nil {
			<-p.release
		}
		return []byte("done"), nil
	default:
		return nil, errdefs.Validationf("echo plugin refuses %q", request.GetTool())
	}
}

func TestConnServesThePluginHandshake(t *testing.T) {
	srv := newTestServer(t, ServerOptions{HostVersion: "test"})
	conn := newTestConn(t, srv, ConnOptions{
		PluginID:   "demo",
		Version:    "0.1.0",
		Generation: 7,
		Grants:     GrantSet{"events:emit": true},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plugin := &echoPlugin{tools: []string{"echo", "slow"}}
	_, served := servePlugin(t, ctx, conn, plugin, protocol.ClientOptions{
		PluginID:    "demo",
		Version:     "0.1.0",
		Concurrency: 2,
	})
	if err := conn.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if !conn.IsReady() {
		t.Fatal("the connection is not ready after WaitReady returned")
	}
	if got, want := plugin.hello.GetGeneration(), uint64(7); got != want {
		t.Fatalf("generation = %d, want %d", got, want)
	}
	if got, want := plugin.hello.GetWireVersion(), protocol.Version; got != want {
		t.Fatalf("negotiated version = %q, want %q", got, want)
	}
	if got, want := plugin.hello.GetMaxCallBytes(), uint32(DefaultMaxCallBytes); got != want {
		t.Fatalf("max call bytes = %d, want %d", got, want)
	}
	if !conn.Grants().Has("events:emit") {
		t.Fatalf("grants = %v, want events:emit", conn.Grants())
	}
	declaration, revision := conn.Declaration()
	if revision != 1 {
		t.Fatalf("revision = %d, want 1", revision)
	}
	if len(declaration.GetTools()) != 2 {
		t.Fatalf("declaration = %v, want two tools", declaration.GetTools())
	}

	out, err := conn.CallTool(ctx, "echo", []byte(`{"text":"hi"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got, want := string(out), `echo:{"text":"hi"}`; got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
	if _, err := conn.CallTool(ctx, "other", nil, 5*time.Second); !errdefs.IsValidation(err) {
		t.Fatalf("unknown tool = %v, want the plugin's validation class", err)
	}

	if err := conn.Drain(ctx, 5*time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("the plugin's Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin's Serve did not return after the host closed the wire")
	}
	if _, err := os.Stat(conn.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket %s outlived its connection", conn.Path())
	}
}

func TestConnRefusesAnotherPluginsIdentity(t *testing.T) {
	srv := newTestServer(t, ServerOptions{})
	conn := newTestConn(t, srv, ConnOptions{PluginID: "demo"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := protocol.Dial(ctx, conn.Path(), protocol.ClientOptions{
		PluginID: "intruder",
		Version:  "0.1.0",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Hello(ctx); err == nil {
		t.Fatal("the host accepted another plugin's identity")
	}
	// The refusal is terminal, which is what makes an enable roll back
	// instead of waiting out a process that can never become ready.
	if err := conn.WaitReady(ctx, 5*time.Second); !errdefs.IsValidation(err) {
		t.Fatalf("WaitReady = %v, want the terminal validation error", err)
	}
}

func TestConnRefusesForeignWireVersion(t *testing.T) {
	srv := newTestServer(t, ServerOptions{})
	conn := newTestConn(t, srv, ConnOptions{PluginID: "demo"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := grpc.NewClient(protocol.Target(conn.Path()),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = raw.Close() }()
	plugin := protocol.NewPluginClient(raw)
	if _, err := plugin.Hello(ctx, &protocol.HelloRequest{
		WireVersion: "2.0",
		PluginId:    "demo",
	}); err == nil {
		t.Fatal("the host accepted a wire version it cannot speak")
	}
	if err := conn.WaitReady(ctx, 5*time.Second); !errdefs.IsValidation(err) {
		t.Fatalf("WaitReady = %v, want the terminal validation error", err)
	}
}

func TestConnReadyRequiresAStream(t *testing.T) {
	srv := newTestServer(t, ServerOptions{})
	conn := newTestConn(t, srv, ConnOptions{PluginID: "demo"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := protocol.Dial(ctx, conn.Path(), protocol.ClientOptions{
		PluginID: "demo",
		Version:  "0.1.0",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Hello(ctx); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	// A Ready with no Call stream would make the plugin unreachable the
	// moment the host started dispatching, so it is refused.
	short, cancelShort := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancelShort()
	if _, err := conn.Ready(short, &protocol.ReadyRequest{}); !errdefs.IsTimeout(err) {
		t.Fatalf("Ready without a stream = %v, want a timeout", err)
	}
	if conn.IsReady() {
		t.Fatal("the connection is ready although no stream was open")
	}
}

func TestConnDeclareRevisionIsMonotonic(t *testing.T) {
	srv := newTestServer(t, ServerOptions{})
	var refused atomic.Int32
	conn := newTestConn(t, srv, ConnOptions{
		PluginID: "demo",
		Validate: func(declaration *protocol.Declaration) error {
			if len(declaration.GetTools()) > 1 {
				refused.Add(1)
				return errdefs.Validationf("one tool at a time")
			}
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plugin := &echoPlugin{tools: []string{"echo"}}
	client, _ := servePlugin(t, ctx, conn, plugin, protocol.ClientOptions{
		PluginID: "demo",
		Version:  "0.1.0",
	})
	if err := conn.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	seen := make(chan *protocol.Declaration, 4)
	conn.OnDeclare(func(declaration *protocol.Declaration) { seen <- declaration })
	next := &protocol.Declaration{Tools: []*protocol.ToolDecl{{Name: "echo"}}}
	if err := client.Declare(ctx, 2, next); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, revision := conn.Declaration(); revision != 2 {
		t.Fatalf("revision = %d, want 2", revision)
	}
	select {
	case declaration := <-seen:
		if len(declaration.GetTools()) != 1 {
			t.Fatalf("subscriber saw %v, want the new snapshot", declaration.GetTools())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the declaration subscriber was not called")
	}
	if err := client.Declare(ctx, 2, next); err == nil {
		t.Fatal("a stale revision was accepted")
	}
	if err := client.Declare(ctx, 3, &protocol.Declaration{
		Tools: []*protocol.ToolDecl{{Name: "echo"}, {Name: "other"}},
	}); err == nil {
		t.Fatal("a declaration the validator refused was accepted")
	}
	if got := refused.Load(); got != 1 {
		t.Fatalf("validator saw %d refusals, want 1", got)
	}
	if _, revision := conn.Declaration(); revision != 2 {
		t.Fatalf("revision = %d after a refused declaration, want 2", revision)
	}
}

func TestConnCloseReleasesPendingCalls(t *testing.T) {
	srv := newTestServer(t, ServerOptions{})
	conn := newTestConn(t, srv, ConnOptions{PluginID: "demo"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plugin := &echoPlugin{
		tools:     []string{"slow"},
		enterSlow: make(chan struct{}, 1),
		release:   make(chan struct{}),
	}
	servePlugin(t, ctx, conn, plugin, protocol.ClientOptions{
		PluginID: "demo",
		Version:  "0.1.0",
	})
	if err := conn.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := conn.CallTool(ctx, "slow", nil, time.Minute)
		result <- err
	}()
	select {
	case <-plugin.enterSlow:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached the plugin")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-result:
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("an in-flight call came back as %v, want unavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the call waiting for the plugin")
	}
	close(plugin.release)
}
