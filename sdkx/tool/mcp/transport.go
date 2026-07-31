package mcp

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"

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
	cmd := exec.Command(command, args...)
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	return &mcpsdk.CommandTransport{Command: cmd}, nil
}

// StreamableHTTP builds a transport that talks to a remote MCP server
// over the streamable-HTTP binding: POST for requests, a standing SSE
// stream for server-initiated messages such as tools/list_changed.
//
// headers are attached to every outgoing request, which is where
// per-server credentials belong (`Authorization: Bearer ...`). They are
// never logged. Passing a nil client uses http.DefaultClient.
func StreamableHTTP(endpoint string, headers map[string]string, client *http.Client) (mcpsdk.Transport, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("mcp: streamable-http transport endpoint is empty")
	}
	if len(headers) > 0 {
		base := http.DefaultTransport
		if client != nil && client.Transport != nil {
			base = client.Transport
		}
		wrapped := &headerRoundTripper{base: base, headers: headers}
		if client == nil {
			client = &http.Client{Transport: wrapped}
		} else {
			copied := *client
			copied.Transport = wrapped
			client = &copied
		}
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
