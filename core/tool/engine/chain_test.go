package engine_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/sandbox"
	sandboxengine "github.com/GizClaw/flowcraft/core/sandbox/engine"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/GizClaw/flowcraft/core/tool"
	toolengine "github.com/GizClaw/flowcraft/core/tool/engine"
	"github.com/GizClaw/flowcraft/core/tool/middleware"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestChainJoinsUnderOneRoot is the one assertion the per-package tests cannot
// make: that the layers actually nest. Each of them proves its own span exists;
// only a single root with a real tool call below it proves the context is carried
// all the way down.
//
// The tree it builds:
//
//	agent.run (root)
//	├── tool.execute{think}        → llm.route → llm.generate
//	├── tool.execute{run_command}  → sandbox.exec
//	└── tool.execute{deferred}     → tool.load
func TestChainJoinsUnderOneRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandbox/local sessions require a unix platform")
	}
	recorder := telemetrytest.New(t)

	runner := local.New(t.TempDir())
	t.Cleanup(func() { _ = runner.Close() })

	router, err := route.New([]route.Target{{
		Name: "primary",
		// A real bound driver, not a canned generator: the funnel span only
		// exists if the call actually goes through protocol.
		Provider: inference.Provider{Generator: boundGenerator(t)},
	}}, route.Policy{})
	if err != nil {
		t.Fatalf("route.New: %v", err)
	}
	request := inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "stub", Name: "m"}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hi"),
		},
	}

	think := tooltest.FuncTool("think", "", func(ctx context.Context, _ string) (string, error) {
		if _, err := router.Generate(ctx, request); err != nil {
			return "", err
		}
		return "ok", nil
	})
	runCommand := tooltest.FuncTool("run_command", "", func(ctx context.Context, _ string) (string, error) {
		result, err := sandboxengine.Exec(ctx, runner, "echo", []string{"hi"}, sandbox.ExecOptions{})
		if err != nil {
			return "", err
		}
		return result.Stdout, nil
	})
	deferred := tooltest.LazyTool("deferred", func(context.Context) (tool.Tool, error) {
		return tooltest.FuncTool("deferred", "", func(context.Context, string) (string, error) {
			return "loaded", nil
		}), nil
	})

	assembly, err := toolengine.NewAssembly(
		[]tool.Source{
			tooltest.Source(think, runCommand),
			tooltest.LazySource(deferred),
		},
		toolengine.WithMiddleware(middleware.Telemetry()),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	ctx, root := telemetry.Tracer().Start(context.Background(), "agent.run")

	for _, name := range []string{"think", "run_command", "deferred"} {
		call, err := message.NewToolCall(name+"-call", name, map[string]any{})
		if err != nil {
			t.Fatalf("NewToolCall(%s): %v", name, err)
		}
		if result := assembly.Execute(ctx, call); result.IsError {
			t.Fatalf("%s: %s", name, result.Content.Text())
		}
	}
	root.End()

	rootID := root.SpanContext().SpanID()

	thinkSpan := requireToolSpan(t, recorder, "think")
	routeSpan := recorder.RequireNamed(t, "llm.route")
	generateSpan := recorder.RequireNamed(t, "llm.generate")
	requireChild(t, thinkSpan, rootID)
	requireChild(t, routeSpan, thinkSpan.SpanContext().SpanID())
	requireChild(t, generateSpan, routeSpan.SpanContext().SpanID())

	commandSpan := requireToolSpan(t, recorder, "run_command")
	execSpan := recorder.RequireNamed(t, "sandbox.exec")
	requireChild(t, commandSpan, rootID)
	requireChild(t, execSpan, commandSpan.SpanContext().SpanID())

	lazySpan := requireToolSpan(t, recorder, "deferred")
	loadSpan := recorder.RequireNamed(t, "tool.load")
	requireChild(t, lazySpan, rootID)
	requireChild(t, loadSpan, lazySpan.SpanContext().SpanID())
}

// boundGenerator binds a unary driver over a stub transport: the cheapest way to
// exercise the real protocol funnel (and therefore its span) without a network.
func boundGenerator(t *testing.T) inference.Generator {
	t.Helper()
	driver, err := protocol.BindGenerate(
		"stub",
		func(_ context.Context, request inference.GenerateRequest, _ protocol.Shape) (protocol.Compiled[string], error) {
			return protocol.Compiled[string]{Wire: "wire", Fields: request.ActiveFields()}, nil
		},
		func(context.Context, inference.GenerateRequest, string) (string, error) {
			return "raw", nil
		},
		func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{
				Message:      message.NewTextMessage(message.RoleAssistant, "ok"),
				FinishReason: inference.FinishStop,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	return protocol.BindGenerator(driver, nil)
}

// requireToolSpan returns the tool.execute span for one tool name; three calls
// share the span name by design.
func requireToolSpan(
	t *testing.T,
	recorder *telemetrytest.Recorder,
	name string,
) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range recorder.Named("tool.execute") {
		attr, ok := telemetrytest.Attrs(span)[telemetry.AttrToolName]
		if ok && attr.AsString() == name {
			return span
		}
	}
	t.Fatalf("no tool.execute span for %q", name)
	return nil
}

func requireChild(t *testing.T, child sdktrace.ReadOnlySpan, parent trace.SpanID) {
	t.Helper()
	if got := child.Parent().SpanID(); got != parent {
		t.Fatalf("%s parent = %s, want %s", child.Name(), got, parent)
	}
	if !child.Parent().IsValid() {
		t.Fatalf("%s has no parent", child.Name())
	}
}
