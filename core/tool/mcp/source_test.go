package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	sdktool "github.com/GizClaw/flowcraft/core/tool"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// recordingRegistrar records runtime tool publications.
type recordingRegistrar struct {
	mu    sync.Mutex
	tools map[string]sdktool.Tool
	added []string
}

func newRecordingRegistrar() *recordingRegistrar {
	return &recordingRegistrar{tools: make(map[string]sdktool.Tool)}
}

func (r *recordingRegistrar) Add(t sdktool.Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Definition().Name
	if _, exists := r.tools[name]; exists {
		return errdefs.Conflictf("tool: duplicate tool %q", name)
	}
	r.tools[name] = t
	r.added = append(r.added, name)
	return nil
}

func (r *recordingRegistrar) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

func (r *recordingRegistrar) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tools[name]
	return ok
}

// failNTimesTransport fails the first n Connect calls, then delegates
// to a freshly built transport. It simulates a server that is down at
// startup and comes up later.
type failNTimesTransport struct {
	remaining int32
	calls     atomic.Int32
	inner     func() (mcpsdk.Transport, error)
}

func (f *failNTimesTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	f.calls.Add(1)
	if atomic.AddInt32(&f.remaining, -1) >= 0 {
		return nil, errors.New("server is down")
	}
	t, err := f.inner()
	if err != nil {
		return nil, err
	}
	return t.Connect(ctx)
}

// blockingTransport fails every Connect by blocking until the attempt
// context expires, like a server that accepts the dial but never
// answers the handshake.
type blockingTransport struct {
	connects atomic.Int32
}

func (b *blockingTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	b.connects.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// rejectingTransport fails every Connect with a peer rejection, which
// connectError classifies as Validation: the server is there but will
// not accept us, so retrying cannot fix it.
type rejectingTransport struct{}

func (rejectingTransport) Connect(context.Context) (mcpsdk.Connection, error) {
	return nil, errors.New("unauthorized")
}

// TestMCPHelperProcess is a stdio MCP server executed as a child of
// the test binary (the standard go test re-exec pattern). It is a
// no-op unless FC_MCP_HELPER=1.
//
// Environment:
//   - FC_MCP_HELPER_DELAY_MS: sleep before serving (slow startup)
//   - FC_MCP_HELPER_EXIT_MS: exit after serving for this long
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("FC_MCP_HELPER") != "1" {
		return
	}
	if delay := envMillis(t, "FC_MCP_HELPER_DELAY_MS"); delay > 0 {
		time.Sleep(delay)
	}

	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "helper", Version: "test"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "helper_tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}},
		}, nil
	})

	ctx := context.Background()
	if exit := envMillis(t, "FC_MCP_HELPER_EXIT_MS"); exit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, exit)
		defer cancel()
	}
	_ = server.Run(ctx, &mcpsdk.StdioTransport{})
}

func envMillis(t *testing.T, key string) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return 0
	}
	ms, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("invalid %s: %v", key, err)
	}
	return time.Duration(ms) * time.Millisecond
}

// helperStdio builds a stdio transport that runs the helper process.
func helperStdio(delayMS, exitMS int) (mcpsdk.Transport, error) {
	env := map[string]string{"FC_MCP_HELPER": "1"}
	if delayMS > 0 {
		env["FC_MCP_HELPER_DELAY_MS"] = strconv.Itoa(delayMS)
	}
	if exitMS > 0 {
		env["FC_MCP_HELPER_EXIT_MS"] = strconv.Itoa(exitMS)
	}
	return Stdio(os.Args[0], []string{"-test.run=TestMCPHelperProcess"}, env)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSource_AddServerValidationErrors(t *testing.T) {
	src := NewSource(WithConnectTimeout(50 * time.Millisecond))
	t.Cleanup(func() { _ = src.Close() })

	ctx := context.Background()
	if err := src.AddServer(ctx, "  ", &blockingTransport{}); !errdefs.IsValidation(err) {
		t.Fatalf("empty name error = %v, want Validation", err)
	}
	if err := src.AddServer(ctx, "nil", nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil transport error = %v, want Validation", err)
	}

	if err := src.AddServer(ctx, "dup", &blockingTransport{}); err != nil {
		t.Fatalf("first AddServer: %v", err)
	}
	if err := src.AddServer(ctx, "dup", &blockingTransport{}); !errdefs.IsValidation(err) {
		t.Fatalf("duplicate name error = %v, want Validation", err)
	}
}

func TestSource_CanceledContextIsFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := NewSource(WithConnectTimeout(time.Second))
	t.Cleanup(func() { _ = src.Close() })

	err := src.AddServer(ctx, "gone", &blockingTransport{})
	if err == nil {
		t.Fatal("AddServer with canceled context returned nil")
	}
	src.mu.Lock()
	pending := len(src.retrying)
	src.mu.Unlock()
	if pending != 0 {
		t.Fatalf("canceled context scheduled %d retries, want 0", pending)
	}
}

func TestSource_InitialConnectPublishesTools(t *testing.T) {
	ctx := context.Background()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "mem", Version: "test"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "mem_tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
	})
	go func() { _, _ = server.Connect(ctx, serverT, nil) }()

	src := NewSource(WithConnectTimeout(2 * time.Second))
	t.Cleanup(func() { _ = src.Close() })
	reg := newRecordingRegistrar()
	src.Attach(reg)

	if err := src.AddServer(ctx, "mem", clientT); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "first connect to publish tools", func() bool {
		return reg.has("mem__mem_tool")
	})
	tools := src.Tools()
	if len(tools) != 1 || tools[0].Definition().Name != "mem__mem_tool" {
		t.Fatalf("Tools() = %v, want [mem__mem_tool]", tools)
	}
}

// TestSource_AddServerDoesNotBlockOnHungServer is the regression test
// for the startup stall: a server that accepts the transport but never
// completes the handshake must not hold AddServer past a small bound,
// even with a long per-attempt connect timeout.
func TestSource_AddServerDoesNotBlockOnHungServer(t *testing.T) {
	src := NewSource(WithConnectTimeout(5 * time.Second))
	t.Cleanup(func() { _ = src.Close() })

	start := time.Now()
	if err := src.AddServer(context.Background(), "hung", &blockingTransport{}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("AddServer blocked for %v with a hung server, want immediate return", elapsed)
	}
}

func TestSource_WaitReadyWaitsForFirstConnect(t *testing.T) {
	src := NewSource(WithConnectTimeout(2 * time.Second))
	t.Cleanup(func() { _ = src.Close() })
	reg := newRecordingRegistrar()
	src.Attach(reg)

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "mem", Version: "test"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "mem_tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
	})
	go func() { _, _ = server.Connect(context.Background(), serverT, nil) }()

	if err := src.AddServer(context.Background(), "mem", clientT); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := src.WaitReady(context.Background(), "mem", 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if !reg.has("mem__mem_tool") {
		t.Fatalf("tools not published after WaitReady; registrar has %v", reg.added)
	}

	// Once connected, WaitReady returns immediately.
	if err := src.WaitReady(context.Background(), "mem", 0); err != nil {
		t.Fatalf("WaitReady after connect: %v", err)
	}
}

// TestSource_WaitReadyReturnsGiveUpError covers the new semantic
// surface: a validation/rejection failure that used to return from
// AddServer is now a background give-up, surfaced through WaitReady.
func TestSource_WaitReadyReturnsGiveUpError(t *testing.T) {
	src := NewSource(WithConnectTimeout(time.Second))
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(context.Background(), "bad", rejectingTransport{}, WithRequired()); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	err := src.WaitReady(context.Background(), "bad", 5*time.Second)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("WaitReady = %v, want Validation", err)
	}
}

func TestSource_WaitReadyTimesOut(t *testing.T) {
	src := NewSource(WithConnectTimeout(5 * time.Second))
	t.Cleanup(func() { _ = src.Close() })

	if err := src.AddServer(context.Background(), "hung", &blockingTransport{}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	err := src.WaitReady(context.Background(), "hung", 50*time.Millisecond)
	if err == nil || !errdefs.IsTimeout(err) {
		t.Fatalf("WaitReady = %v, want Timeout", err)
	}
}

func TestSource_WaitReadyFailsWhenSourceCloses(t *testing.T) {
	src := NewSource(WithConnectTimeout(5 * time.Second))

	if err := src.AddServer(context.Background(), "hung", &blockingTransport{}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := src.WaitReady(context.Background(), "hung", time.Second)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("WaitReady after Close = %v, want NotAvailable", err)
	}
}

func TestSource_WaitReadyUnknownServer(t *testing.T) {
	src := NewSource()
	t.Cleanup(func() { _ = src.Close() })
	if err := src.WaitReady(context.Background(), "nope", 0); !errdefs.IsValidation(err) {
		t.Fatalf("WaitReady for unknown server = %v, want Validation", err)
	}
}

func TestSource_RetriesUntilServerComesUp(t *testing.T) {
	src := NewSource(
		WithConnectTimeout(2*time.Second),
		WithRetryBackoff(20*time.Millisecond, 50*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })
	reg := newRecordingRegistrar()
	src.Attach(reg)

	ft := &failNTimesTransport{
		remaining: 2,
		inner: func() (mcpsdk.Transport, error) {
			return helperStdio(0, 0)
		},
	}
	if err := src.AddServer(context.Background(), "late", ft); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "background retry to publish tools", func() bool {
		return reg.has("late__helper_tool")
	})
	if got := ft.calls.Load(); got != 3 {
		t.Fatalf("connect attempts = %d, want 3", got)
	}
}

func TestSource_TimeoutFailureRetriesAndCloseStops(t *testing.T) {
	src := NewSource(
		WithConnectTimeout(40*time.Millisecond),
		WithRetryBackoff(10*time.Millisecond, 20*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })
	bt := &blockingTransport{}

	if err := src.AddServer(context.Background(), "slow", bt); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if bt.connects.Load() < 2 {
		t.Fatalf("expected background retries, got %d connects", bt.connects.Load())
	}

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := bt.connects.Load()
	time.Sleep(100 * time.Millisecond)
	if got := bt.connects.Load(); got != after {
		t.Fatalf("connects grew after Close: %d -> %d", after, got)
	}
}

func TestSource_ReconnectsAfterServerExit(t *testing.T) {
	src := NewSource(
		WithConnectTimeout(2*time.Second),
		WithRetryBackoff(30*time.Millisecond, 100*time.Millisecond),
		WithLivenessInterval(50*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })

	tport, err := helperStdio(0, 400)
	if err != nil {
		t.Fatalf("helperStdio: %v", err)
	}
	counting := &countingTransport{inner: tport}
	if err := src.AddServer(context.Background(), "dying", counting); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "first connect", func() bool {
		return len(src.Tools()) == 1
	})
	tool := src.Tools()[0]

	execute := func() (message.Content, error) {
		return tool.Execute(context.Background(), "{}")
	}
	if _, err := execute(); err != nil {
		t.Fatalf("execute before server exit: %v", err)
	}

	waitFor(t, 5*time.Second, "server death to surface as NotAvailable", func() bool {
		_, err := execute()
		return err != nil && errdefs.IsNotAvailable(err)
	})
	waitFor(t, 10*time.Second, "reconnect to restore execution", func() bool {
		out, err := execute()
		return err == nil && out.Text() == "ok"
	})
	if got := counting.closeCount(); got < 1 {
		t.Fatal("dead session was never closed; its child was leaked")
	}
	// The go-sdk helper negotiates 2026-07-28, so this test exercises
	// the modern watch path: the exit is noticed through the connection
	// instead of a ping.
	session, err := sourceServer(t, src, "dying").currentSession()
	if err != nil {
		t.Fatalf("currentSession after reconnect: %v", err)
	}
	if !usesModernProtocol(session) {
		t.Fatal("helper session is not modern; the test premise is broken")
	}
}

// TestSource_WatchDoesNotProbeModernSessions locks in SEP-2575: ping
// was removed in 2026-07-28, so a conformant modern server answers it
// with MethodNotFound. The watcher must leave such a session alone;
// before the fix every interval tore the connection down and leaked
// its stdio child.
func TestSource_WatchDoesNotProbeModernSessions(t *testing.T) {
	src := NewSource(
		WithConnectTimeout(2*time.Second),
		WithRetryBackoff(20*time.Millisecond, 50*time.Millisecond),
		WithLivenessInterval(30*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })

	counting := &countingTransport{inner: newInMemoryServer(t, "modern",
		func(tport mcpsdk.Transport) mcpsdk.Transport {
			return &pingRejectTransport{inner: tport}
		})}
	if err := src.AddServer(context.Background(), "modern", counting); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "first connect", func() bool { return len(src.Tools()) == 1 })

	session, err := sourceServer(t, src, "modern").currentSession()
	if err != nil {
		t.Fatalf("currentSession: %v", err)
	}
	if !usesModernProtocol(session) {
		t.Fatal("in-memory session is not modern; the test premise is broken")
	}

	// Several liveness intervals with a healthy peer: no probe-driven
	// teardown and no reconnect attempts.
	time.Sleep(5 * 30 * time.Millisecond)
	if got := counting.closeCount(); got != 0 {
		t.Fatalf("healthy modern session Close calls = %d, want 0", got)
	}
	if got := counting.connectCount(); got != 1 {
		t.Fatalf("connect attempts = %d, want 1 (probe-driven reconnect loop)", got)
	}
}

// TestSource_WatchClosesDeadSessionBeforeRetry is the regression test
// for the leak: a session whose liveness probe fails must be closed
// exactly once — releasing its transport child — before the reconnect
// is scheduled. The peer stays alive and answers with a JSON-RPC
// error, which is what the field report observed and what kept the
// session (and its child) alive after being dropped from the slot.
func TestSource_WatchClosesDeadSessionBeforeRetry(t *testing.T) {
	src := NewSource(
		WithConnectTimeout(2*time.Second),
		WithRetryBackoff(20*time.Millisecond, 50*time.Millisecond),
		WithLivenessInterval(30*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() {
		if conn, err := serverT.Connect(context.Background()); err == nil {
			fakeLegacyServer(conn)
		}
	}()
	counting := &countingTransport{inner: &singleShotTransport{inner: clientT}}
	if err := src.AddServer(context.Background(), "legacy", counting); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "attach to a legacy session", func() bool { return len(src.Tools()) == 1 })

	srv := sourceServer(t, src, "legacy")
	session, err := srv.currentSession()
	if err != nil {
		t.Fatalf("currentSession: %v", err)
	}
	if usesModernProtocol(session) {
		t.Fatal("test session negotiated a modern protocol; the ping path is not exercised")
	}

	waitFor(t, 5*time.Second, "failed probe to close the dead session", func() bool {
		return counting.closeCount() == 1
	})
	// The peer stays healthy, so nothing else may close it.
	time.Sleep(150 * time.Millisecond)
	if got := counting.closeCount(); got != 1 {
		t.Fatalf("dead session Close calls = %d, want exactly 1", got)
	}
	if _, err := srv.currentSession(); err == nil {
		t.Fatal("srv.session still points at the dead session")
	}
	waitFor(t, 5*time.Second, "reconnect to be scheduled", func() bool {
		return counting.connectCount() >= 2
	})
}

// TestSource_ServerLivenessOffDisablesProbing covers WithServerLiveness:
// a legacy session whose peer rejects pings stays attached and is never
// redialed, because this server opted out of probing.
func TestSource_ServerLivenessOffDisablesProbing(t *testing.T) {
	src := NewSource(
		WithConnectTimeout(2*time.Second),
		WithRetryBackoff(20*time.Millisecond, 50*time.Millisecond),
		WithLivenessInterval(30*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() {
		if conn, err := serverT.Connect(context.Background()); err == nil {
			fakeLegacyServer(conn)
		}
	}()
	counting := &countingTransport{inner: clientT}
	if err := src.AddServer(context.Background(), "quiet", counting, WithServerLiveness(0)); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "attach to a legacy session", func() bool { return len(src.Tools()) == 1 })

	srv := sourceServer(t, src, "quiet")
	session, err := srv.currentSession()
	if err != nil {
		t.Fatalf("currentSession: %v", err)
	}
	if usesModernProtocol(session) {
		t.Fatal("test session negotiated a modern protocol; the liveness override is not exercised")
	}
	if srv.owner != src {
		t.Fatal("AddServer did not wire the server owner")
	}

	// Several source-level intervals with a healthy peer: the per-server
	// opt-out means no pings, no teardown, and no redial.
	time.Sleep(5 * 30 * time.Millisecond)
	if got := counting.closeCount(); got != 0 {
		t.Fatalf("healthy probe-less session Close calls = %d, want 0", got)
	}
	if got := counting.connectCount(); got != 1 {
		t.Fatalf("connect attempts = %d, want 1 (liveness override ignored)", got)
	}
}

// captureTransport records the connection it hands to a server, so a
// test can kill the peer side at will.
type captureTransport struct {
	inner mcpsdk.Transport
	mu    sync.Mutex
	conn  mcpsdk.Connection
}

func (t *captureTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.conn = conn
	t.mu.Unlock()
	return conn, nil
}

func (t *captureTransport) connection() mcpsdk.Connection {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn
}

// TestSource_CallFailureSchedulesReconnect covers the fallback for
// transports that leave a dead session installed: a tool call that
// finds the connection closed must tear the session down and schedule
// a reconnect even when no watcher is running.
func TestSource_CallFailureSchedulesReconnect(t *testing.T) {
	src := NewSource(
		WithConnectTimeout(2*time.Second),
		WithRetryBackoff(20*time.Millisecond, 50*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "dying", Version: "test"}, nil)
	mcpServer.AddTool(&mcpsdk.Tool{
		Name:        "mem_tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}},
		}, nil
	})
	capture := &captureTransport{inner: serverT}
	go func() { _, _ = mcpServer.Connect(context.Background(), capture, nil) }()

	counting := &countingTransport{inner: &singleShotTransport{inner: clientT}}
	srv := &server{
		name:       "dying",
		prefix:     "dying" + DefaultPrefixSeparator,
		transport:  counting,
		cfg:        &serverConfig{prefix: "dying" + DefaultPrefixSeparator, clientName: "flowcraft", clientVer: "v1"},
		clientName: "flowcraft",
		clientVer:  "v1",
		owner:      src,
	}
	session, err := src.connect(context.Background(), srv, srv.cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := src.attachSession(context.Background(), srv, session); err != nil {
		t.Fatalf("attachSession: %v", err)
	}

	srv.mu.Lock()
	tools := append([]sdktool.Tool(nil), srv.tools...)
	srv.mu.Unlock()
	if len(tools) != 1 {
		t.Fatalf("projected tools = %d, want 1", len(tools))
	}

	// Kill the peer with no watcher running, then wait until the client
	// side has observed the closure.
	waitFor(t, 5*time.Second, "server connection", func() bool { return capture.connection() != nil })
	if err := capture.connection().Close(); err != nil {
		t.Fatalf("close server connection: %v", err)
	}
	closed := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(closed)
	}()
	waitFor(t, 5*time.Second, "client to observe the closure", func() bool {
		select {
		case <-closed:
			return true
		default:
			return false
		}
	})

	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tools[0].Execute(callCtx, "{}"); err == nil {
		t.Fatal("call on a dead session succeeded")
	}
	waitFor(t, 5*time.Second, "call failure to schedule a reconnect", func() bool {
		return counting.connectCount() >= 2
	})
	if _, err := srv.currentSession(); err == nil {
		t.Fatal("srv.session still points at the dead session")
	}
}

// countingTransport wraps a transport, counts Connect attempts, and
// counts Close calls on every connection it hands out, so tests can
// assert exactly when sessions are dialed and torn down.
type countingTransport struct {
	inner    mcpsdk.Transport
	mu       sync.Mutex
	connects int
	closes   int
}

func (t *countingTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	t.mu.Lock()
	t.connects++
	t.mu.Unlock()
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countingConn{Connection: conn, parent: t}, nil
}

func (t *countingTransport) connectCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connects
}

func (t *countingTransport) closeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closes
}

type countingConn struct {
	mcpsdk.Connection
	parent *countingTransport
}

func (c *countingConn) Close() error {
	c.parent.mu.Lock()
	c.parent.closes++
	c.parent.mu.Unlock()
	return c.Connection.Close()
}

// singleShotTransport serves one Connect and blocks every later one
// until its context ends, so a test can observe the first session
// without a scheduled retry replacing it.
type singleShotTransport struct {
	inner mcpsdk.Transport
	used  atomic.Bool
}

func (t *singleShotTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	if t.used.Swap(true) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return t.inner.Connect(ctx)
}

// newInMemoryServer starts an in-memory MCP server exposing mem_tool
// and returns the client-side transport to dial it with. wrapServer may
// wrap the server side before the connection is served; nil uses it as
// is.
func newInMemoryServer(t *testing.T, name string, wrapServer func(mcpsdk.Transport) mcpsdk.Transport) mcpsdk.Transport {
	t.Helper()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: "test"}, nil)
	mcpServer.AddTool(&mcpsdk.Tool{
		Name:        "mem_tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}},
		}, nil
	})
	if wrapServer == nil {
		wrapServer = func(t mcpsdk.Transport) mcpsdk.Transport { return t }
	}
	go func() { _, _ = mcpServer.Connect(context.Background(), wrapServer(serverT), nil) }()
	return clientT
}

// fakeLegacyServer speaks just enough pre-2026-07-28 MCP on conn to
// attach this package: it rejects server/discover (forcing the legacy
// initialize handshake) and ping, and serves tools/list. Rejecting ping
// with a JSON-RPC error while the connection stays healthy is exactly
// the field failure this watch path must survive without leaking the
// session.
func fakeLegacyServer(conn mcpsdk.Connection) {
	ctx := context.Background()
	for {
		msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok || !req.IsCall() {
			continue
		}
		var (
			result json.RawMessage
			rerr   error
		)
		switch req.Method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":false}},"serverInfo":{"name":"fake","version":"test"}}`)
		case "tools/list":
			result = json.RawMessage(`{"tools":[{"name":"mem_tool","inputSchema":{"type":"object"}}]}`)
		case "ping":
			rerr = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "ping is not supported"}
		default:
			rerr = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: req.Method + " is not supported"}
		}
		if err := conn.Write(ctx, &jsonrpc.Response{ID: req.ID, Result: result, Error: rerr}); err != nil {
			return
		}
	}
}

// pingRejectTransport makes a server answer incoming pings with
// MethodNotFound without disturbing anything else, standing in for a
// conformant 2026-07-28 peer whose per-request metadata rules a ping
// can never satisfy (the field failure this package must not probe
// into a reconnect loop).
type pingRejectTransport struct {
	inner mcpsdk.Transport
}

func (t *pingRejectTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pingRejectConn{Connection: conn}, nil
}

type pingRejectConn struct {
	mcpsdk.Connection
}

func (c *pingRejectConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		msg, err := c.Connection.Read(ctx)
		if err != nil {
			return nil, err
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok || req.Method != "ping" {
			return msg, nil
		}
		// Answer the probe directly: the server handler never sees it,
		// so the connection stays healthy, exactly like a peer that
		// rejects the method.
		if err := c.Write(ctx, &jsonrpc.Response{
			ID: req.ID,
			Error: &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: "ping is not supported",
			},
		}); err != nil {
			return nil, err
		}
	}
}

// sourceServer returns the attached server, failing the test when the
// name is unknown.
func sourceServer(t *testing.T, src *Source, name string) *server {
	t.Helper()
	src.mu.Lock()
	defer src.mu.Unlock()
	srv := src.servers[name]
	if srv == nil {
		t.Fatalf("server %q is not attached", name)
	}
	return srv
}

// connectCountingServer spins up an in-memory MCP server and returns a
// connected client session whose transport counts Close calls.
func connectCountingServer(t *testing.T, src *Source, name string) (*server, *mcpsdk.ClientSession, *countingTransport) {
	t.Helper()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: "test"}, nil)
	mcpServer.AddTool(&mcpsdk.Tool{
		Name:        "mem_tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}},
		}, nil
	})
	go func() { _, _ = mcpServer.Connect(context.Background(), serverT, nil) }()

	counting := &countingTransport{inner: clientT}
	srv := &server{
		name:       name,
		prefix:     name + DefaultPrefixSeparator,
		transport:  counting,
		cfg:        &serverConfig{prefix: name + DefaultPrefixSeparator, clientName: "flowcraft", clientVer: "v1"},
		clientName: "flowcraft",
		clientVer:  "v1",
	}
	session, err := src.connect(context.Background(), srv, srv.cfg)
	if err != nil {
		t.Fatalf("connect %q: %v", name, err)
	}
	return srv, session, counting
}

// TestAttachSessionClosesSessionWhenSourceClosed locks in the closed
// branch of attachSession: a session that connected while the Source
// was closing must not be orphaned, because AddServer's retry path is
// a no-op once the source is closed.
func TestAttachSessionClosesSessionWhenSourceClosed(t *testing.T) {
	src := NewSource(WithConnectTimeout(2 * time.Second))
	srv, session, counting := connectCountingServer(t, src, "mem")

	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := src.attachSession(context.Background(), srv, session)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("attachSession after Close = %v, want NotAvailable", err)
	}
	if got := counting.closeCount(); got != 1 {
		t.Fatalf("session Close calls = %d, want 1 (orphaned session)", got)
	}
	srv.mu.Lock()
	current := srv.session
	srv.mu.Unlock()
	if current != nil {
		t.Fatal("srv.session still set after failed attach")
	}
}

// TestAttachSessionClosesSessionWhenAlreadyAttached locks in the
// duplicate branch: a second concurrent attach of the same server name
// must close its own session rather than leak it.
func TestAttachSessionClosesSessionWhenAlreadyAttached(t *testing.T) {
	src := NewSource(WithConnectTimeout(2 * time.Second))
	srv1, session1, _ := connectCountingServer(t, src, "mem")
	if err := src.attachSession(context.Background(), srv1, session1); err != nil {
		t.Fatalf("first attachSession: %v", err)
	}

	_, session2, counting2 := connectCountingServer(t, src, "mem")
	srv2 := &server{
		name:       "mem",
		prefix:     "mem" + DefaultPrefixSeparator,
		transport:  counting2,
		cfg:        &serverConfig{prefix: "mem" + DefaultPrefixSeparator, clientName: "flowcraft", clientVer: "v1"},
		clientName: "flowcraft",
		clientVer:  "v1",
	}
	err := src.attachSession(context.Background(), srv2, session2)
	if !errdefs.IsValidation(err) {
		t.Fatalf("second attachSession = %v, want Validation", err)
	}
	if got := counting2.closeCount(); got != 1 {
		t.Fatalf("duplicate session Close calls = %d, want 1 (orphaned session)", got)
	}
}

// TestAttachSessionClosesSessionOnReconcileFailure verifies the
// reconcile-failure path closes the session exactly once, which is
// what makes retryLoop's own Close redundant.
func TestAttachSessionClosesSessionOnReconcileFailure(t *testing.T) {
	src := NewSource(WithConnectTimeout(2 * time.Second))
	srv, session, counting := connectCountingServer(t, src, "mem")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := src.attachSession(ctx, srv, session)
	if err == nil {
		t.Fatal("attachSession with canceled context returned nil")
	}
	if got := counting.closeCount(); got != 1 {
		t.Fatalf("session Close calls = %d, want 1", got)
	}
	srv.mu.Lock()
	current := srv.session
	srv.mu.Unlock()
	if current != nil {
		t.Fatal("srv.session still set after failed attach")
	}
}

func TestProjectToolsDeduplicatesNames(t *testing.T) {
	srv := &server{name: "s", prefix: "s" + DefaultPrefixSeparator}
	dup := &mcpsdk.Tool{Name: "dup", InputSchema: map[string]any{"type": "object"}}
	list := []*mcpsdk.Tool{
		dup,
		{Name: "other", InputSchema: map[string]any{"type": "object"}},
		nil,
		dup,
		{Name: "", InputSchema: map[string]any{"type": "object"}},
	}
	tools := projectTools(srv, list, false)
	if len(tools) != 2 {
		t.Fatalf("projectTools returned %d tools, want 2 (deduped): %v", len(tools), toolNames(tools))
	}
	if tools[0].Definition().Name != "s"+DefaultPrefixSeparator+"dup" ||
		tools[1].Definition().Name != "s"+DefaultPrefixSeparator+"other" {
		t.Fatalf("projectTools = %v, want [s__dup s__other]", toolNames(tools))
	}

	// A server tool that collides with a resource bridge name wins; the
	// projection must still be unique when resources are enabled.
	withResource := []*mcpsdk.Tool{{Name: "list_resources", InputSchema: map[string]any{"type": "object"}}}
	tools = projectTools(srv, withResource, true)
	want := []string{
		"s" + DefaultPrefixSeparator + "list_resources",
		"s" + DefaultPrefixSeparator + "read_resource",
	}
	if len(tools) != 2 || tools[0].Definition().Name != want[0] || tools[1].Definition().Name != want[1] {
		t.Fatalf("projectTools with resource collision = %v, want %v", toolNames(tools), want)
	}
}

func toolNames(tools []sdktool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Definition().Name)
	}
	return names
}
