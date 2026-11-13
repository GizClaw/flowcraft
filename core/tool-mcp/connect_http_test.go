package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goccy/go-json"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestConnectHandshakeOverHTTP covers the streamable-HTTP transport end to end:
// a real MCP server behind a real HTTP listener, reached through the same
// Decode-then-Connect path an application uses.
//
// The stdio test proves the transport plumbing; this one proves the HTTP branch,
// which differs in what it exercises — a network round trip per request, header
// injection, and the connection lifecycle the SDK negotiates over HTTP.
func TestConnectHandshakeOverHTTP(t *testing.T) {
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "http-helper", Version: "test"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "http_tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}},
		}, nil
	})

	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return server }, nil)
	listener := httptest.NewServer(handler)
	t.Cleanup(listener.Close)

	declaration := Spec{Servers: []ServerSpec{{
		Name:      "remote",
		Transport: TransportHTTP,
		URL:       listener.URL,
	}}}
	raw, err := json.Marshal(declaration)
	if err != nil {
		t.Fatalf("marshal declaration: %v", err)
	}
	spec, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	ctx := context.Background()
	source, err := Connect(ctx, spec)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	waitFor(t, 5*time.Second, "http tool published", func() bool {
		return len(source.Tools()) == 1
	})
	if got := source.Tools()[0].Definition().Name; got != "remote__http_tool" {
		t.Fatalf("tool name = %q, want remote__http_tool", got)
	}

	payload, err := source.CallTool(ctx, "remote", "http_tool", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !json.Valid(payload) {
		t.Fatalf("CallTool payload %s is not valid JSON", payload)
	}
}
