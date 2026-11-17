package tool_test

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestCallFrom_AbsentSemantics(t *testing.T) {
	if _, ok := tool.CallFrom(context.Background()); ok {
		t.Fatal("CallFrom outside a tool call should report absent")
	}
	//nolint:staticcheck // deliberate: the accessor documents that it accepts a nil context
	if _, ok := tool.CallFrom(nil); ok {
		t.Fatal("CallFrom on a nil context should report absent")
	}

	ctx := tool.WithCall(context.Background(), tool.Call{ID: "call-42"})
	call, ok := tool.CallFrom(ctx)
	if !ok || call.ID != "call-42" {
		t.Fatalf("CallFrom = %+v, %v; want call-42, true", call, ok)
	}
}

// The first classification wins: the innermost stage is closest to the cause, so
// an outer timeout must not rewrite an inner policy denial.
func TestCall_FirstFailureWins(t *testing.T) {
	ctx := tool.WithCall(context.Background(), tool.Call{ID: "call-1"})
	tool.RecordFailure(ctx, tool.FailurePolicyDenied)
	tool.RecordFailure(ctx, tool.FailureTimeout)

	kind, ok := tool.FailureKindFrom(ctx)
	if !ok || kind != tool.FailurePolicyDenied {
		t.Fatalf("FailureKindFrom = %q, %v; want policy_denied", kind, ok)
	}
	if _, ok := tool.FailureKindFrom(context.Background()); ok {
		t.Fatal("FailureKindFrom outside a tool call should report absent")
	}
}

// A second call must not inherit the first call's box: a leaked classification
// would misclassify the next call, which is why the box is private to this
// package and WithCall installs a fresh one.
func TestWithCall_StartsAFreshBox(t *testing.T) {
	first := tool.WithCall(context.Background(), tool.Call{ID: "call-1"})
	tool.RecordFailure(first, tool.FailureTimeout)

	second := tool.WithCall(first, tool.Call{ID: "call-2"})
	if kind, ok := tool.FailureKindFrom(second); ok {
		t.Fatalf("the next call inherited the classification %q", kind)
	}
	if kind, ok := tool.FailureKindFrom(first); !ok || kind != tool.FailureTimeout {
		t.Fatalf("the first call lost its classification: %q, %v", kind, ok)
	}
}

// The deprecated accessors are forwarders over the domain: WithCallID replaces
// the id and keeps the box the executor installed.
func TestWithCallID_KeepsTheFailureBox(t *testing.T) {
	ctx := tool.WithCall(context.Background(), tool.Call{ID: "call-7"})
	tool.RecordFailure(ctx, tool.FailureTimeout)

	//nolint:staticcheck // deprecated forwarder: pinned so it keeps the box
	next := tool.WithCallID(ctx, "call-8")
	if call, ok := tool.CallFrom(next); !ok || call.ID != "call-8" {
		t.Fatalf("CallFrom = %+v, %v; want call-8", call, ok)
	}
	//nolint:staticcheck // deprecated forwarder: reads the same domain
	if id, ok := tool.CallIDFromContext(next); !ok || id != "call-8" {
		t.Fatalf("CallIDFromContext = %q, %v; want call-8", id, ok)
	}
	if kind, ok := tool.FailureKindFrom(next); !ok || kind != tool.FailureTimeout {
		t.Fatalf("WithCallID dropped the failure box: %q, %v", kind, ok)
	}
}

// WithFailureRecorder returns the call unchanged when it already has a box and
// installs one otherwise, so instrumentation never shadows the executor's box.
func TestWithFailureRecorder_KeepsAnExistingBox(t *testing.T) {
	ctx := tool.WithCall(context.Background(), tool.Call{ID: "call-1"})
	//nolint:staticcheck // deprecated forwarder: pinned so it keeps the box
	if got := tool.WithFailureRecorder(ctx); got != ctx {
		t.Fatal("WithFailureRecorder replaced the executor's box")
	}

	//nolint:staticcheck // deprecated forwarder: installs a box when absent
	fresh := tool.WithFailureRecorder(context.Background())
	tool.RecordFailure(fresh, tool.FailureCanceled)
	if kind, ok := tool.FailureKindFrom(fresh); !ok || kind != tool.FailureCanceled {
		t.Fatalf("WithFailureRecorder did not install a box: %q, %v", kind, ok)
	}
}

// TestExecutor_ExecuteStampsCall verifies the executor stamps the active
// message.ToolCall into the context handed to the tool implementation and to
// middleware.
func TestExecutor_ExecuteStampsCall(t *testing.T) {
	var toolSeen, middlewareSeen string
	reg, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.TextTool(
			message.ToolDefinition{Name: "peek", InputSchema: []byte(`{"type":"object"}`)},
			func(ctx context.Context, _ string) (string, error) {
				call, _ := tool.CallFrom(ctx)
				toolSeen = call.ID
				return "ok", nil
			},
		),
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	mw := func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			active, _ := tool.CallFrom(ctx)
			middlewareSeen = active.ID
			return next(ctx, call)
		}
	}
	exec := tool.NewExecutor(reg, mw)
	res := exec.Execute(context.Background(),
		message.ToolCall{ID: "call-7", Name: "peek", Arguments: message.NewArguments([]byte(`{}`))})
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if toolSeen != "call-7" {
		t.Fatalf("tool saw call id %q, want call-7", toolSeen)
	}
	if middlewareSeen != "call-7" {
		t.Fatalf("middleware saw call id %q, want call-7", middlewareSeen)
	}
}

// TestExecutor_ExecuteAllStampsPerCallIDs verifies concurrent calls each see
// their own call.
func TestExecutor_ExecuteAllStampsPerCallIDs(t *testing.T) {
	reg, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.TextTool(
			message.ToolDefinition{Name: "peek", InputSchema: []byte(`{"type":"object"}`)},
			func(ctx context.Context, _ string) (string, error) {
				call, _ := tool.CallFrom(ctx)
				return call.ID, nil
			},
		),
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	exec := tool.NewExecutor(reg)
	calls := []message.ToolCall{
		{ID: "a", Name: "peek", Arguments: message.NewArguments([]byte(`{}`))},
		{ID: "b", Name: "peek", Arguments: message.NewArguments([]byte(`{}`))},
		{ID: "c", Name: "peek", Arguments: message.NewArguments([]byte(`{}`))},
	}
	results := exec.ExecuteAll(context.Background(), calls)
	var mu sync.Mutex
	got := make(map[string]bool, len(calls))
	for _, res := range results {
		if res.IsError {
			t.Fatalf("result = %+v", res)
		}
		mu.Lock()
		got[res.Content.Text()] = true
		mu.Unlock()
	}
	for _, call := range calls {
		if !got[call.ID] {
			t.Fatalf("call id %q not seen by any concurrent execution", call.ID)
		}
	}
}

// TestExecutor_SecondDispatchStartsWithAFreshBox pins the lifetime of the box: one
// call, one box. A second dispatch on the same caller context must start
// unclassified, or a failure would be reported against work that succeeded.
func TestExecutor_SecondDispatchStartsWithAFreshBox(t *testing.T) {
	reg, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.TextTool(
			message.ToolDefinition{Name: "peek", InputSchema: []byte(`{"type":"object"}`)},
			func(context.Context, string) (string, error) { return "ok", nil },
		),
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var kinds []tool.FailureKind
	var classified []bool
	observer := func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			kind, ok := tool.FailureKindFrom(ctx)
			kinds = append(kinds, kind)
			classified = append(classified, ok)
			return res
		}
	}
	exec := tool.NewExecutor(reg, observer)
	base := context.Background()
	_ = exec.Execute(base, message.ToolCall{ID: "call-1", Name: "missing"})
	res := exec.Execute(base, message.ToolCall{ID: "call-2", Name: "peek",
		Arguments: message.NewArguments([]byte(`{}`))})
	if res.IsError {
		t.Fatalf("second result = %+v, want success", res)
	}
	if len(kinds) != 2 {
		t.Fatalf("observed %d dispatches, want 2", len(kinds))
	}
	if !classified[0] || kinds[0] != tool.FailureNotFound {
		t.Fatalf("first call classified = %q, %v; want not_found", kinds[0], classified[0])
	}
	if classified[1] || kinds[1] != "" {
		t.Fatalf("second call inherited a classification: %q, %v", kinds[1], classified[1])
	}
}

// TestExecutor_ExecuteInstallsAFailureBox pins the phase this domain enables: the
// executor installs the box for every call, so middleware reads a classification
// even when no instrumentation middleware is in the chain.
func TestExecutor_ExecuteInstallsAFailureBox(t *testing.T) {
	reg, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.TextTool(
			message.ToolDefinition{Name: "peek", InputSchema: []byte(`{"type":"object"}`)},
			func(context.Context, string) (string, error) { return "ok", nil },
		),
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var kind tool.FailureKind
	var classified bool
	observer := func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			kind, classified = tool.FailureKindFrom(ctx)
			return res
		}
	}
	exec := tool.NewExecutor(reg, observer)
	res := exec.Execute(context.Background(),
		message.ToolCall{ID: "call-9", Name: "missing", Arguments: message.NewArguments([]byte(`{}`))})
	if !res.IsError {
		t.Fatalf("result = %+v, want an error", res)
	}
	if !classified || kind != tool.FailureNotFound {
		t.Fatalf("FailureKindFrom = %q, %v; want not_found from the executor's box", kind, classified)
	}
}

// TestExecuteAllocationBudget guards the dispatch path the context fold changed: one
// tool.Call value per call — the id and the classification box, which used to be two
// context values installed by two different layers. The budget is the measured floor
// with one slot of slack, the way the registry's own budget is set: a regression that
// installs a node twice shows up here, and a test-only reshuffle does not.
func TestExecuteAllocationBudget(t *testing.T) {
	registry, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{
		tool.TextTool(
			message.ToolDefinition{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
			func(context.Context, string) (string, error) { return "ok", nil },
		),
	}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	exec := tool.NewExecutor(registry)
	call := message.ToolCall{ID: "call-1", Name: "echo",
		Arguments: message.NewArguments([]byte(`{}`))}
	// Warm the once-per-process paths the first dispatch pays for.
	if res := exec.Execute(context.Background(), call); res.IsError {
		t.Fatalf("warm-up result = %+v", res)
	}
	const budget = 7 // measured: six allocations per dispatch
	got := testing.AllocsPerRun(200, func() {
		if res := exec.Execute(context.Background(), call); res.IsError {
			t.Fatalf("result = %+v", res)
		}
	})
	if got > budget {
		t.Fatalf("Execute allocates %.0f times per call, want <= %d: a context node or a "+
			"classification box is being installed twice", got, budget)
	}
}
