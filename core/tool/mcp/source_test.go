package mcp

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	sdktool "github.com/GizClaw/flowcraft/core/tool"
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
	if !reg.has("mem__mem_tool") {
		t.Fatalf("mem__mem_tool not published; registrar has %v", reg.added)
	}
	tools := src.Tools()
	if len(tools) != 1 || tools[0].Definition().Name != "mem__mem_tool" {
		t.Fatalf("Tools() = %v, want [mem__mem_tool]", tools)
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
	if err := src.AddServer(context.Background(), "dying", tport); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "first connect", func() bool {
		return len(src.Tools()) == 1
	})
	tool := src.Tools()[0]

	execute := func() (string, error) {
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
		return err == nil && out == "ok"
	})
}

// countingTransport wraps a transport and counts Close calls on every
// connection it hands out, so tests can assert exactly when sessions
// are torn down.
type countingTransport struct {
	inner  mcpsdk.Transport
	mu     sync.Mutex
	closes int
}

func (t *countingTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countingConn{Connection: conn, parent: t}, nil
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
