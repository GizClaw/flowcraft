package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	sandboxengine "github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// remoteConfig is a document whose boundary lives behind a listener.
//
// root is shared with the server on purpose: the client sends an absolute WorkDir
// and the server resolves it against its own runner root, so the two sides have
// to agree on the path. That agreement is a deployment requirement (a shared
// mount), not something the wire can arrange.
func remoteConfig(t *testing.T, root, endpoint string) Config {
	t.Helper()
	cfg := validConfig(root)
	cfg.Sandbox.Backend = BackendRemote
	cfg.Sandbox.Remote = RemoteConfig{
		Endpoint: endpoint,
		Token:    "test-token",
		Timeout:  "10s",
		PollHold: "2s",
	}
	cfg.Tools.Enable = []string{ToolExec}
	return cfg
}

// TestRemoteBackendRunsACommand is the wiring test for backend: remote — a
// document, a real listener, a real server, and the built exec tool — with the
// far end being sandbox-local so the test needs no isolation privileges.
func TestRemoteBackendRunsACommand(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("sandbox-local serves no sessions on windows, so there is nothing to proxy")
	}
	root := t.TempDir()
	endpoint := serveLocal(t, root)

	cfg := remoteConfig(t, root, endpoint)
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	// The declaration is the server's, so a caller learns the backend's real
	// capabilities without probing.
	if !rt.Runner().Capabilities().Policy.EnvAllowList {
		t.Error("remote runner does not report the server's env allow-list")
	}

	name := sandboxengine.DefaultShell(goruntime.GOOS).Name
	call, err := message.NewToolCall("call-1", name, map[string]any{"command": "echo hello-remote"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	res := rt.Assembly().Execute(rt.Context(context.Background()), call)
	if res.IsError {
		t.Fatalf("execute = %q", res.Content.Text())
	}
	if !strings.Contains(res.Content.Text(), "hello-remote") {
		t.Fatalf("result = %q, want the command's output", res.Content.Text())
	}
}

// serveLocal puts one sandbox-local runner behind an authenticated listener.
func serveLocal(t *testing.T, root string) string {
	t.Helper()
	inner := local.New(root)
	t.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, remote.WithServerToken("test-token"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)
	return listener.URL
}

// A connection failure has to be reported when the runtime is built, not on the
// first command: a deployment that cannot reach its boundary should not come up.
func TestRemoteBackendFailsWhenTheServerIsUnreachable(t *testing.T) {
	root := t.TempDir()
	endpoint := serveLocal(t, root)
	cfg := remoteConfig(t, root, endpoint)
	cfg.Sandbox.Remote.Token = "wrong-token"

	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("New succeeded against a server that refuses the token")
	}
}

// The remote block describes a connection; on a local backend it is dead
// configuration, and dead configuration is how a deployment runs somewhere the
// author did not intend.
func TestConfigRejectsRemoteSettingsOnALocalBackend(t *testing.T) {
	cfg := validConfig(t.TempDir())
	cfg.Sandbox.Remote = RemoteConfig{Endpoint: "https://example.invalid"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a local backend accepted a remote block")
	}
	if !strings.Contains(err.Error(), "sandbox.remote") {
		t.Errorf("error %q does not name the field", err)
	}
}

func TestConfigValidatesTheRemoteBlock(t *testing.T) {
	cases := map[string]struct {
		mutate func(*RemoteConfig)
		want   string
	}{
		"missing endpoint": {func(c *RemoteConfig) { c.Endpoint = "" }, "sandbox.remote.endpoint"},
		"not a URL":        {func(c *RemoteConfig) { c.Endpoint = "sandbox.internal:8443" }, "not an http(s) URL"},
		"wrong scheme":     {func(c *RemoteConfig) { c.Endpoint = "unix:///run/sandbox.sock" }, "not an http(s) URL"},
		"bad timeout":      {func(c *RemoteConfig) { c.Timeout = "10" }, "sandbox.remote.timeout"},
		"negative hold":    {func(c *RemoteConfig) { c.PollHold = "-1s" }, "sandbox.remote.poll_hold"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t.TempDir())
			cfg.Sandbox.Backend = BackendRemote
			cfg.Sandbox.Remote = RemoteConfig{Endpoint: "https://sandbox.internal:8443"}
			tc.mutate(&cfg.Sandbox.Remote)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("accepted an invalid remote block")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// remoteUnixConfig is a document whose boundary is a socket on this host.
func remoteUnixConfig(t *testing.T, root, socket string) Config {
	t.Helper()
	cfg := validConfig(root)
	cfg.Sandbox.Backend = BackendRemote
	cfg.Sandbox.Remote = RemoteConfig{
		Transport: "unix",
		Socket:    socket,
		Timeout:   "10s",
		PollHold:  "2s",
	}
	cfg.Tools.Enable = []string{ToolExec}
	return cfg
}

// serveLocalUnix puts one sandbox-local runner behind an unauthenticated unix
// socket — the same-host shape: the socket file's permissions are the boundary.
//
// root is shared with the client's document for the same reason as in the HTTP
// case: the client sends an absolute WorkDir and the server resolves it against
// its own runner root.
func serveLocalUnix(t *testing.T, root string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sbx-runtime")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")

	inner := local.New(root)
	t.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, remote.WithoutAuth())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener, err := remote.ListenUnix(path)
	if err != nil {
		t.Skipf("unix sockets are not available here: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	return path
}

// TestRemoteUnixBackendRunsACommand is the same wiring test as the HTTP one, over
// the transport a same-host deployment should use.
func TestRemoteUnixBackendRunsACommand(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("sandbox-local serves no sessions on windows, so there is nothing to proxy")
	}
	root := t.TempDir()
	socket := serveLocalUnix(t, root)

	cfg := remoteUnixConfig(t, root, socket)
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	name := sandboxengine.DefaultShell(goruntime.GOOS).Name
	call, err := message.NewToolCall("call-1", name, map[string]any{"command": "echo hello-unix"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	res := rt.Assembly().Execute(rt.Context(context.Background()), call)
	if res.IsError {
		t.Fatalf("execute = %q", res.Content.Text())
	}
	if !strings.Contains(res.Content.Text(), "hello-unix") {
		t.Fatalf("result = %q", res.Content.Text())
	}
}

// The remote block names a transport, and the field the other transport needs is
// rejected instead of silently ignored.
func TestConfigValidatesTheRemoteTransport(t *testing.T) {
	cases := map[string]struct {
		remote RemoteConfig
		want   string
	}{
		"unix without a socket": {
			remote: RemoteConfig{Transport: "unix"},
			want:   "sandbox.remote.socket",
		},
		"unix with an endpoint": {
			remote: RemoteConfig{Transport: "unix", Socket: "/run/s.sock", Endpoint: "https://x.test"},
			want:   "not used with sandbox.remote.transport: unix",
		},
		"unix with TLS options": {
			remote: RemoteConfig{Transport: "unix", Socket: "/run/s.sock", InsecureSkipVerify: true},
			want:   "insecure_skip_verify",
		},
		"http with a socket": {
			remote: RemoteConfig{Endpoint: "https://sandbox.test", Socket: "/run/s.sock"},
			want:   "sandbox.remote.socket",
		},
		"unknown transport": {
			remote: RemoteConfig{Transport: "quic", Endpoint: "https://sandbox.test"},
			want:   "sandbox.remote.transport",
		},
		"http still needs an endpoint": {
			remote: RemoteConfig{Transport: "http"},
			want:   "sandbox.remote.endpoint",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t.TempDir())
			cfg.Sandbox.Backend = BackendRemote
			cfg.Sandbox.Remote = tc.remote

			err := cfg.Validate()
			if err == nil {
				t.Fatal("accepted an invalid remote transport")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
