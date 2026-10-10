package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestCallToolUnknownServer(t *testing.T) {
	source := NewSource()
	defer func() { _ = source.Close() }()
	_, err := source.CallTool(
		context.Background(), "missing", "echo", map[string]any{})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("CallTool error = %v, want NotFound", err)
	}
}

// TestCallToolNotConnected covers a server that is attached but has not
// completed its handshake yet: the call must report NotAvailable
// instead of panicking on a nil session.
func TestCallToolNotConnected(t *testing.T) {
	source := NewSource(
		WithConnectTimeout(50*time.Millisecond),
		WithRetryBackoff(time.Millisecond, time.Millisecond),
	)
	defer func() { _ = source.Close() }()
	if err := source.AddServer(context.Background(), "blocked", &blockingTransport{}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	_, err := source.CallTool(
		context.Background(), "blocked", "probe", map[string]any{})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("CallTool error = %v, want NotAvailable", err)
	}
}

// TestCallToolReturnsPayload covers the result shapes a plugin-provided
// graph node consumes: valid JSON text passes through untouched, plain
// text becomes a JSON string, and a result without content becomes an
// empty object.
func TestCallToolReturnsPayload(t *testing.T) {
	cases := []struct {
		name   string
		result *mcpsdk.CallToolResult
		want   string
	}{{
		name:   "json text passes through",
		result: textResult(`{"writes":{"result":"ok"}}`),
		want:   `{"writes":{"result":"ok"}}`,
	}, {
		name: "text parts concatenate and trim",
		result: &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: `  {"a":`},
			&mcpsdk.TextContent{Text: `1}  `},
		}},
		want: `{"a":1}`,
	}, {
		name:   "plain text becomes a json string",
		result: textResult("hello"),
		want:   `"hello"`,
	}, {
		name: "empty text block carries nothing",
		result: &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: ""},
		}},
		want: `{}`,
	}, {
		name:   "empty result is an object",
		result: &mcpsdk.CallToolResult{},
		want:   `{}`,
	}, {
		name: "structured content substitutes for missing text",
		result: &mcpsdk.CallToolResult{StructuredContent: map[string]any{
			"writes": map[string]any{"result": "ok"},
		}},
		want: `{"writes":{"result":"ok"}}`,
	}, {
		name: "empty text block keeps structured content reachable",
		result: &mcpsdk.CallToolResult{
			Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: ""}},
			StructuredContent: map[string]any{"writes": map[string]any{"n": 1.0}},
		},
		want: `{"writes":{"n":1}}`,
	}, {
		name:   "structured primitives are legal",
		result: &mcpsdk.CallToolResult{StructuredContent: []any{1.0, 2.0}},
		want:   `[1,2]`,
	}, {
		name: "text wins over structured content",
		result: &mcpsdk.CallToolResult{
			Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"writes":{}}`}},
			StructuredContent: map[string]any{"writes": map[string]any{"other": true}},
		},
		want: `{"writes":{}}`,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := newCallToolServer(t, "probe", func(map[string]any) *mcpsdk.CallToolResult {
				return tc.result
			})
			raw, err := source.CallTool(
				context.Background(), "probe", "probe", map[string]any{"text": "hi"})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if string(raw) != tc.want {
				t.Fatalf("CallTool payload = %s, want %s", raw, tc.want)
			}
			if !json.Valid(raw) {
				t.Fatalf("CallTool payload %s is not valid JSON", raw)
			}
		})
	}
}

// TestCallToolReturnsToolError covers the IsError branch: the text (or
// the structured content) a server sends with the failure must reach
// the caller as an error, and no payload is returned.
func TestCallToolReturnsToolError(t *testing.T) {
	cases := []struct {
		name   string
		result *mcpsdk.CallToolResult
		want   string
	}{{
		name: "text is the message",
		result: &mcpsdk.CallToolResult{
			IsError: true,
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "boom"}},
		},
		want: "boom",
	}, {
		name: "structured content is the message",
		result: &mcpsdk.CallToolResult{
			IsError:           true,
			StructuredContent: map[string]any{"message": "nope"},
		},
		want: `{"message":"nope"}`,
	}, {
		name:   "silent error names the tool",
		result: &mcpsdk.CallToolResult{IsError: true},
		want:   `mcp tool "probe" reported an error`,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := newCallToolServer(t, "probe", func(map[string]any) *mcpsdk.CallToolResult {
				return tc.result
			})
			raw, err := source.CallTool(
				context.Background(), "probe", "probe", map[string]any{})
			if err == nil {
				t.Fatalf("CallTool returned %s, want an error", raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CallTool error = %q, want it to contain %q", err, tc.want)
			}
			if raw != nil {
				t.Fatalf("CallTool returned payload %s with the error", raw)
			}
		})
	}
}

// TestCallToolForwardsArguments checks that the arguments reach the
// server and the tool-level result is the one the server produced.
func TestCallToolForwardsArguments(t *testing.T) {
	var got map[string]any
	source := newCallToolServer(t, "probe", func(args map[string]any) *mcpsdk.CallToolResult {
		got = args
		return textResult(`{"writes":{"result":"ok"}}`)
	})
	raw, err := source.CallTool(context.Background(), "probe", "probe",
		map[string]any{"text": "hi", "level": 2.0})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if string(raw) != `{"writes":{"result":"ok"}}` {
		t.Fatalf("CallTool payload = %s", raw)
	}
	if got["text"] != "hi" || got["level"] != 2.0 {
		t.Fatalf("server received %#v, want the caller's arguments", got)
	}
}

func textResult(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
	}
}

// newCallToolServer starts an in-memory server exposing one tool named
// "probe" whose result comes from result, and returns a connected
// Source holding that server under name.
//
// The raw [mcpsdk.Server.AddTool] handler is used on purpose: it passes
// the result through verbatim, the way a foreign SDK serving
// structured output does, instead of adding the JSON text mirror the
// Go SDK's typed handler appends.
func newCallToolServer(
	t *testing.T,
	name string,
	result func(args map[string]any) *mcpsdk.CallToolResult,
) *Source {
	t.Helper()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: name, Version: "test"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "probe",
		InputSchema: map[string]any{"type": "object"},
	}, func(
		_ context.Context,
		req *mcpsdk.CallToolRequest,
	) (*mcpsdk.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
		}
		return result(args), nil
	})
	go func() { _, _ = server.Connect(context.Background(), serverT, nil) }()

	source := NewSource(WithConnectTimeout(2 * time.Second))
	t.Cleanup(func() { _ = source.Close() })
	ctx := context.Background()
	if err := source.AddServer(ctx, name, clientT); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := source.WaitReady(ctx, name, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return source
}
