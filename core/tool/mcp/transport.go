package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"

	"github.com/GizClaw/flowcraft/core/utils"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Stdio builds a transport that spawns command as a child process and
// speaks MCP over its stdin/stdout, the standard layout for local
// servers (`npx -y @modelcontextprotocol/server-filesystem /root`).
//
// The child inherits this process's stderr so server diagnostics land
// in the host's log stream rather than vanishing. Environment is the
// caller's business: env entries are appended to the current
// environment in "KEY=VALUE" form, so a caller can pass credentials
// without exporting them process-wide.
//
// Closing the session created from this transport closes the child's
// stdin, waits, then escalates to SIGTERM and SIGKILL — the shutdown
// sequence the MCP stdio spec prescribes. Callers therefore never need
// to reap the process themselves.
func Stdio(command string, args []string, env map[string]string) (mcpsdk.Transport, error) {
	if command == "" {
		return nil, fmt.Errorf("mcp: stdio transport command is empty")
	}
	var envList []string
	if len(env) > 0 {
		envList = os.Environ()
		for key, value := range env {
			envList = append(envList, key+"="+value)
		}
	}
	return &reconnectableStdio{command: command, args: args, env: envList}, nil
}

// reconnectableStdio is a [mcpsdk.Transport] that spawns a fresh child
// process on every Connect call. The SDK's CommandTransport wraps a
// single *exec.Cmd and can therefore only be connected once; the
// background reconnect path needs a transport that can be retried, so
// each attempt builds a brand-new command and delegates to a fresh
// CommandTransport (which owns the pipes and the SIGTERM/SIGKILL
// shutdown sequence).
type reconnectableStdio struct {
	command string
	args    []string
	env     []string
}

func (t *reconnectableStdio) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	cmd := exec.Command(t.command, t.args...)
	cmd.Stderr = os.Stderr
	if t.env != nil {
		cmd.Env = t.env
	}
	return (&mcpsdk.CommandTransport{Command: cmd}).Connect(ctx)
}

var _ mcpsdk.Transport = (*reconnectableStdio)(nil)

// StreamableHTTP builds a transport that talks to a remote MCP server
// over the streamable-HTTP binding: POST for requests, a standing SSE
// stream for server-initiated messages such as tools/list_changed.
//
// headers are attached to every outgoing request, which is where
// per-server credentials belong (`Authorization: Bearer ...`). They are
// never logged. Passing a nil client uses the hardened core/utils
// transport without an overall request timeout (MCP streamable-HTTP may
// keep a standing SSE connection open for server-initiated messages).
func StreamableHTTP(endpoint string, headers map[string]string, client *http.Client) (mcpsdk.Transport, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("mcp: streamable-http transport endpoint is empty")
	}
	if client == nil {
		client = &http.Client{Transport: utils.NewRoundTripper()}
	}
	if len(headers) > 0 {
		base := http.DefaultTransport
		if client != nil && client.Transport != nil {
			base = client.Transport
		}
		wrapped := &headerRoundTripper{base: base, headers: headers}
		copied := *client
		copied.Transport = wrapped
		client = &copied
	}
	return &mcpsdk.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: client,
	}, nil
}

// headerRoundTripper injects static headers into every request. It
// clones the request before mutating so a retry of the same *Request by
// a higher layer never sees doubled headers.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for key, value := range rt.headers {
		clone.Header.Set(key, value)
	}
	return rt.base.RoundTrip(clone)
}
