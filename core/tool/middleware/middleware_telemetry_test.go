package middleware

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

func TestTelemetry_EmitsExecutionSpan(t *testing.T) {
	rec := installTelemetryTestTracer(t)
	exec := tool.NewExecutor(catalogWith(echoTool("echo")), Telemetry())
	tc := call("echo")

	res := exec.Execute(context.Background(), tc)
	if res.IsError {
		t.Fatalf("execute = %q, want success", res.Content.Text())
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	// The name must not vary with the tool: the tool is an attribute, so a
	// dashboard groups by it instead of filtering by name.
	if span.Name() != spanToolExecute {
		t.Errorf("span name = %q, want %q", span.Name(), spanToolExecute)
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("span status = %v, want Ok", span.Status().Code)
	}
	attrs := telemetrySpanAttrs(span)
	if attrs[telemetry.AttrToolName] != "echo" ||
		attrs[telemetry.AttrToolCallID] != tc.ID {
		t.Errorf("span attrs = %#v, want tool name/call id", attrs)
	}
	if _, ok := attrs[telemetry.AttrErrorKind]; ok {
		t.Errorf("successful call carries %q", telemetry.AttrErrorKind)
	}
}

// TestTelemetry_CarriesRuntimeIdentity pins the correlation channel: an identity
// that travels in the context reaches the span, so a trace can be filtered by the
// runtime instance that produced it. The middleware never learns what a runtime
// is; it only copies what it finds.
func TestTelemetry_CarriesRuntimeIdentity(t *testing.T) {
	rec := installTelemetryTestTracer(t)
	exec := tool.NewExecutor(catalogWith(echoTool("echo")), Telemetry())
	ctx := runctx.WithRun(context.Background(), runctx.Run{RuntimeID: "rt-7", TenantID: "acme"})

	if res := exec.Execute(ctx, call("echo")); res.IsError {
		t.Fatalf("execute = %q, want success", res.Content.Text())
	}

	span := rec.Ended()[0]
	telemetrytest.RequireAttrString(t, span, telemetry.AttrRuntimeID, "rt-7")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrTenantID, "acme")
}

// TestTelemetry_RecordsFailureKind pins the classification channel: the stages
// that short-circuit a call tag the reason, and the span reports it instead of
// echoing a message it would have to parse.
func TestTelemetry_RecordsFailureKind(t *testing.T) {
	tests := []struct {
		name     string
		mws      []tool.Middleware
		toolName string
		want     tool.FailureKind
	}{
		{
			name:     "unknown tool",
			mws:      []tool.Middleware{Telemetry()},
			toolName: "missing",
			want:     tool.FailureNotFound,
		},
		{
			name:     "tool returned an error",
			mws:      []tool.Middleware{Telemetry()},
			toolName: "broken",
			want:     tool.FailureToolError,
		},
		{
			name:     "timeout",
			mws:      []tool.Middleware{Telemetry(), Timeout(10*time.Millisecond, nil)},
			toolName: "blocking",
			want:     tool.FailureTimeout,
		},
		{
			name:     "panic is recovered and classified",
			mws:      []tool.Middleware{Telemetry(), Recover()},
			toolName: "panicking",
			want:     tool.FailurePanicked,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := installTelemetryTestTracer(t)
			catalog := catalogWith(
				echoTool("echo"),
				brokenTool("broken"),
				blockingTool("blocking"),
				panickingTool("panicking"),
			)
			exec := tool.NewExecutor(catalog, test.mws...)

			if res := exec.Execute(context.Background(), call(test.toolName)); !res.IsError {
				t.Fatalf("execute = %q, want an error result", res.Content.Text())
			}

			spans := rec.Ended()
			if len(spans) != 1 {
				t.Fatalf("ended spans = %d, want 1", len(spans))
			}
			span := spans[0]
			if span.Status().Code != codes.Error {
				t.Errorf("span status = %v, want Error", span.Status().Code)
			}
			if description := span.Status().Description; description != string(test.want) {
				t.Errorf("status description = %q, want %q", description, test.want)
			}
			attrs := telemetrySpanAttrs(span)
			if attrs[telemetry.AttrErrorKind] != string(test.want) {
				t.Errorf("error.kind = %v, want %q", attrs[telemetry.AttrErrorKind], test.want)
			}
		})
	}
}

// TestTelemetry_ApprovalDenialIsPolicyDenied covers the approval short-circuit,
// which runs before the tool and therefore has to tag the recorder itself.
func TestTelemetry_ApprovalDenialIsPolicyDenied(t *testing.T) {
	rec := installTelemetryTestTracer(t)
	deny := ApproverFunc(func(context.Context, message.ToolCall) error {
		return errors.New("not on the allow list")
	})
	exec := tool.NewExecutor(
		catalogWith(echoTool("echo")),
		Telemetry(),
		Approval(deny, "echo"),
	)

	if res := exec.Execute(context.Background(), call("echo")); !res.IsError {
		t.Fatalf("execute = %q, want a denial", res.Content.Text())
	}
	span := rec.Ended()[0]
	attrs := telemetrySpanAttrs(span)
	if attrs[telemetry.AttrErrorKind] != string(tool.FailurePolicyDenied) {
		t.Errorf("error.kind = %v, want %q", attrs[telemetry.AttrErrorKind], tool.FailurePolicyDenied)
	}
}

// TestFromSettings_TelemetryDefault pins the default-on rule: only an explicit
// false drops the middleware.
func TestFromSettings_TelemetryDefault(t *testing.T) {
	tests := []struct {
		name     string
		settings Settings
		want     int
	}{
		// Recover and the non-text part limiter are always installed.
		{"absent means on", Settings{}, 3},
		{"explicit on", Settings{Telemetry: &TelemetrySettings{Enabled: boolPtr(true)}}, 3},
		{"explicit off", Settings{Telemetry: &TelemetrySettings{Enabled: boolPtr(false)}}, 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mws, err := FromSettings(test.settings)
			if err != nil {
				t.Fatalf("FromSettings: %v", err)
			}
			if len(mws) != test.want {
				t.Fatalf("middlewares = %d, want %d", len(mws), test.want)
			}
		})
	}
}

// installTelemetryTestTracer installs an in-memory TracerProvider and
// restores the previous global provider on cleanup.
func installTelemetryTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

func telemetrySpanAttrs(span sdktrace.ReadOnlySpan) map[string]any {
	attrs := make(map[string]any, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	return attrs
}

func brokenTool(name string) tool.Tool {
	return tooltest.FuncTool(name, "", func(context.Context, string) (string, error) {
		return "", errors.New("tool failed")
	})
}

func blockingTool(name string) tool.Tool {
	return tooltest.FuncTool(name, "", func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
}

func panickingTool(name string) tool.Tool {
	return tooltest.FuncTool(name, "", func(context.Context, string) (string, error) {
		panic("boom")
	})
}

// metaTool lets a fixture declare catalog metadata (a rate limit) without a
// custom Tool implementation.
type metaTool struct {
	tool.Tool
	meta tool.ToolMeta
}

func (t metaTool) Metadata() tool.ToolMeta { return t.meta }

// TestTelemetry_RecordsPolicyAndLimitAnnotations pins that middleware nested
// inside the span annotates it: a policy decision and a shortened result are the
// two facts that explain a call whose output looks wrong.
func TestTelemetry_RecordsPolicyAndLimitAnnotations(t *testing.T) {
	rec := installTelemetryTestTracer(t)
	limiter, err := ResultLimiter(3)
	if err != nil {
		t.Fatalf("ResultLimiter: %v", err)
	}
	approve := ApproverFunc(func(context.Context, message.ToolCall) error { return nil })
	exec := tool.NewExecutor(
		catalogWith(echoTool("echo")),
		Telemetry(),
		Approval(approve, "echo"),
		limiter,
	)

	res := exec.Execute(context.Background(), call("echo"))
	if res.IsError {
		t.Fatalf("execute = %q, want success", res.Content.Text())
	}
	attrs := telemetrySpanAttrs(rec.Ended()[0])
	if attrs[telemetry.AttrPolicyDecision] != "allow" {
		t.Errorf("policy.decision = %v, want allow", attrs[telemetry.AttrPolicyDecision])
	}
	if attrs[telemetry.AttrResultTruncated] != true {
		t.Errorf("result.truncated = %v, want true", attrs[telemetry.AttrResultTruncated])
	}
	dropped, ok := attrs[telemetry.AttrResultDroppedRunes].(int64)
	if !ok || dropped <= 0 {
		t.Errorf("result.truncated.runes = %v, want a positive count", attrs[telemetry.AttrResultDroppedRunes])
	}
}

// TestTelemetry_RecordsRateLimitWait pins that backpressure is visible on the
// call it delayed, which is what separates "slow tool" from "saturated fleet".
func TestTelemetry_RecordsRateLimitWait(t *testing.T) {
	rec := installTelemetryTestTracer(t)
	paced := metaTool{Tool: echoTool("echo"), meta: tool.ToolMeta{RateLimit: 5}}
	catalog := catalogWith(paced)
	exec := tool.NewExecutor(catalog, Telemetry(), RateLimit(catalog))

	if res := exec.Execute(context.Background(), call("echo")); res.IsError {
		t.Fatalf("first execute = %q", res.Content.Text())
	}
	if res := exec.Execute(context.Background(), call("echo")); res.IsError {
		t.Fatalf("second execute = %q", res.Content.Text())
	}

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(spans))
	}
	attrs := telemetrySpanAttrs(spans[1])
	waited, ok := attrs[telemetry.AttrRateLimitBlockedMs].(int64)
	if !ok || waited < 100 {
		t.Fatalf("rate_limit.blocked_ms = %v, want the ~200ms slot wait", attrs[telemetry.AttrRateLimitBlockedMs])
	}
}
