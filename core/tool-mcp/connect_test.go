package mcp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/goccy/go-json"
)

// TestConnectHandshakeOverStdio is the end-to-end path the config rewrite
// introduced: a JSON declaration is decoded, connected, and then used for a real
// MCP handshake over stdio.
//
// The other tests drive Source.AddServer directly, so nothing else proves that the
// entry point an application actually calls — Decode then Connect — reaches a live
// server, publishes its tools and can invoke one.
func TestConnectHandshakeOverStdio(t *testing.T) {
	recorder := telemetrytest.New(t)

	declaration := Spec{Servers: []ServerSpec{{
		Name:      "helper",
		Transport: TransportStdio,
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPHelperProcess"},
		Env:       map[string]string{"FC_MCP_HELPER": "1"},
	}}}
	// Round-trip through JSON so the test exercises the decoder an application uses,
	// not just the struct.
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

	waitFor(t, 5*time.Second, "helper tool published", func() bool {
		return len(source.Tools()) == 1
	})
	tools := source.Tools()
	if got := tools[0].Definition().Name; got != "helper__helper_tool" {
		t.Fatalf("tool name = %q, want helper__helper_tool", got)
	}

	payload, err := source.CallTool(ctx, "helper", "helper_tool", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !json.Valid(payload) {
		t.Fatalf("CallTool payload %s is not valid JSON", payload)
	}

	// The connection is its own story: it happens before any tool call, and a
	// failure here is invisible to a tool span that never starts.
	// One span per attempt: the transport retries a failed dial, so the thing
	// this test pins is that the connection is its own story with the server and
	// transport on it — not that a loaded machine never retried once.
	connects := recorder.Named("mcp.connect")
	if len(connects) == 0 {
		t.Fatalf("no mcp.connect span among %d", len(recorder.Spans()))
	}
	span := connects[len(connects)-1]
	telemetrytest.RequireAttrString(t, span, telemetry.AttrMCPServer, "helper")
	telemetrytest.RequireAttrString(t, span, "mcp.transport", "*mcp.reconnectableStdio")
}
