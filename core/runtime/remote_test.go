package runtime

import (
	"context"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"testing"

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
