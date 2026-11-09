package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/GizClaw/flowcraft/core/tool"
)

// callReadFile executes read_file through the assembled dispatch chain.
func callReadFile(t *testing.T, rt *Runtime) message.ToolResult {
	t.Helper()
	path := filepath.Join(rt.Files().Writable()[0], "doc.txt")
	if err := rt.Files().Write(context.Background(), path, []byte("hello\n")); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	call, err := message.NewToolCall("call-1", "read_file", map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	return rt.Assembly().Execute(rt.Context(context.Background()), call)
}

// The default chain exists so that a tool call is never invisible: without it a
// runtime-assembled tool set produces no span at all.
func TestDefaultMiddlewareInstallsTelemetry(t *testing.T) {
	recorder := telemetrytest.New(t)
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}

	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if res := callReadFile(t, rt); res.IsError {
		t.Fatalf("execute = %q", res.Content.Text())
	}
	span := recorder.RequireNamed(t, "tool.execute")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrRuntimeID, rt.ID())
}

// A deployment that names its chain replaces the default one, including the
// telemetry entry.
func TestMiddlewareChainIsConfigurable(t *testing.T) {
	recorder := telemetrytest.New(t)
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	cfg.Tools.Middleware = []MiddlewareConfig{{Name: MiddlewareRecover}}

	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if res := callReadFile(t, rt); res.IsError {
		t.Fatalf("execute = %q", res.Content.Text())
	}
	if spans := recorder.Named("tool.execute"); len(spans) != 0 {
		t.Errorf("spans = %d, want none: telemetry was not in the chain", len(spans))
	}
}

func TestUnknownMiddlewareIsRejected(t *testing.T) {
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	cfg.Tools.Middleware = []MiddlewareConfig{{Name: "nope"}}

	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("unknown middleware accepted")
	}
	if !strings.Contains(err.Error(), "telemetry") {
		t.Errorf("error %q does not list the offered middleware", err)
	}
}

// Middleware that needs a Go value arrives the same way drivers do.
func TestHostMiddlewareIsRegistered(t *testing.T) {
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	cfg.Tools.Middleware = []MiddlewareConfig{
		{Name: MiddlewareRecover},
		{Name: "refuse"},
	}

	rt, err := New(context.Background(), cfg, WithMiddleware(MiddlewareOption{
		Name: "refuse",
		Build: func(json.RawMessage) (tool.Middleware, error) {
			return func(tool.Dispatch) tool.Dispatch {
				return func(context.Context, message.ToolCall) message.ToolResult {
					return message.NewErrorToolResult("call-1", "refused by the host")
				}
			}, nil
		},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if res := callReadFile(t, rt); !res.IsError {
		t.Error("the host middleware did not run")
	}
}

// result_limit takes its bound from settings; a missing one is a configuration
// error rather than a silent default.
func TestMiddlewareSettingsAreTyped(t *testing.T) {
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	cfg.Tools.Middleware = []MiddlewareConfig{{Name: MiddlewareResultLimit}}
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("result_limit without settings accepted")
	}

	cfg.Tools.Middleware = []MiddlewareConfig{
		{Name: MiddlewareRecover},
		{Name: MiddlewareResultLimit, Settings: settingsJSON(map[string]any{"max_bytes": 4096})},
	}
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New with settings: %v", err)
	}
	defer func() { _ = rt.Close() }()
}
