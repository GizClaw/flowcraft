package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// ---------------------------------------------------------------------------
// Recover
// ---------------------------------------------------------------------------

func TestRecover_PanicBecomesErrorResult(t *testing.T) {
	reg := catalogWith(tool.FuncTool(message.ToolDefinition{Name: "panicker"},
		func(_ context.Context, _ string) (string, error) { panic("boom") }))
	exec := tool.NewExecutor(reg, Recover())

	res := exec.Execute(context.Background(), call("panicker"))
	if !res.IsError {
		t.Fatal("expected IsError result for panic")
	}
	if !strings.Contains(res.Content, "panicked") {
		t.Errorf("Content = %q, want to contain 'panicked'", res.Content)
	}
}

func TestRecover_ExecuteAllSurvivesPanic(t *testing.T) {
	reg := catalogWith(
		tool.FuncTool(message.ToolDefinition{Name: "panicker"},
			func(_ context.Context, _ string) (string, error) { panic("boom") }),
		echoTool("fine"),
	)
	exec := tool.NewExecutor(reg, Recover())

	results := exec.ExecuteAll(context.Background(), []message.ToolCall{
		{ID: "c1", Name: "panicker", Arguments: json.RawMessage("{}")},
		{ID: "c2", Name: "fine", Arguments: json.RawMessage("{}")},
	})
	if !results[0].IsError {
		t.Error("panicking call should produce IsError result")
	}
	if results[1].IsError {
		t.Errorf("healthy call should succeed, got %q", results[1].Content)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestConcurrency_CapsInFlight(t *testing.T) {
	var inFlight, maxSeen atomic.Int32
	reg := catalogWith(tool.FuncTool(message.ToolDefinition{Name: "slow"},
		func(_ context.Context, _ string) (string, error) {
			cur := inFlight.Add(1)
			for {
				old := maxSeen.Load()
				if cur <= old || maxSeen.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
			return "ok", nil
		}))
	exec := tool.NewExecutor(reg, Concurrency(2))

	calls := make([]message.ToolCall, 6)
	for i := range calls {
		calls[i] = message.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "slow", Arguments: json.RawMessage("{}")}
	}
	exec.ExecuteAll(context.Background(), calls)
	if got := maxSeen.Load(); got > 2 {
		t.Errorf("max in-flight = %d, want <= 2", got)
	}
}

func TestConcurrency_ContextCancelWhileWaiting(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	reg := catalogWith(tool.FuncTool(message.ToolDefinition{Name: "holder"},
		func(ctx context.Context, _ string) (string, error) {
			close(started)
			select {
			case <-release:
				return "ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}))
	exec := tool.NewExecutor(reg, Concurrency(1))

	first := make(chan message.ToolResult, 1)
	go func() { first <- exec.Execute(context.Background(), call("holder")) }()
	<-started // first call now holds the only slot

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res := exec.Execute(ctx, call("holder"))
	if !res.IsError {
		t.Fatal("expected IsError while waiting on a held slot with cancelled ctx")
	}
	close(release)
	<-first
}

func TestConcurrency_InvalidLimitPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for non-positive limit")
		}
	}()
	Concurrency(0)
}

// ---------------------------------------------------------------------------
// Timeout
// ---------------------------------------------------------------------------

func TestTimeout_SlowToolTimesOut(t *testing.T) {
	reg := catalogWith(tool.FuncTool(message.ToolDefinition{Name: "hang"},
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}))
	exec := tool.NewExecutor(reg, Timeout(50*time.Millisecond, nil))

	res := exec.Execute(context.Background(), call("hang"))
	if !res.IsError {
		t.Fatal("expected IsError for timed-out tool")
	}
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("Content = %q, want to contain 'timed out'", res.Content)
	}
}

func TestTimeout_PerToolOverrideAndExemption(t *testing.T) {
	reg := catalogWith(
		tool.FuncTool(message.ToolDefinition{Name: "slowish"},
			func(ctx context.Context, _ string) (string, error) {
				select {
				case <-time.After(80 * time.Millisecond):
					return "done", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}),
		echoTool("fast"),
	)
	exec := tool.NewExecutor(reg, Timeout(30*time.Millisecond, map[string]time.Duration{
		"slowish": 200 * time.Millisecond, // override: fits
		"fast":    0,                      // exempt
	}))

	if res := exec.Execute(context.Background(), call("slowish")); res.IsError {
		t.Errorf("slowish with generous override should succeed, got %q", res.Content)
	}
	if res := exec.Execute(context.Background(), call("fast")); res.IsError {
		t.Errorf("exempt tool should succeed, got %q", res.Content)
	}
}

// ---------------------------------------------------------------------------
// RateLimit
// ---------------------------------------------------------------------------

type ratedTool struct {
	def  message.ToolDefinition
	rate float64
}

func (r ratedTool) Definition() message.ToolDefinition { return r.def }
func (r ratedTool) Execute(_ context.Context, _ string) (string, error) {
	return "ok", nil
}
func (r ratedTool) Metadata() tool.ToolMeta { return tool.ToolMeta{RateLimit: r.rate} }

func TestRateLimit_PacesCalls(t *testing.T) {
	reg := catalogWith(ratedTool{def: message.ToolDefinition{Name: "api"}, rate: 50})
	exec := tool.NewExecutor(reg, RateLimit(reg))

	start := time.Now()
	for i := 0; i < 3; i++ {
		if res := exec.Execute(context.Background(), call("api")); res.IsError {
			t.Fatalf("call %d: %s", i, res.Content)
		}
	}
	// 3 calls at 50/s: first immediate, slots 2 and 3 wait ~20ms each.
	if elapsed := time.Since(start); elapsed < 35*time.Millisecond {
		t.Errorf("3 paced calls took %v, expected >= ~35ms", elapsed)
	}
}

func TestRateLimit_UndeclaredPassesThrough(t *testing.T) {
	reg := catalogWith(echoTool("plain"))
	exec := tool.NewExecutor(reg, RateLimit(reg))

	start := time.Now()
	for i := 0; i < 5; i++ {
		if res := exec.Execute(context.Background(), call("plain")); res.IsError {
			t.Fatalf("call %d: %s", i, res.Content)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("unlimited calls took %v, expected no pacing", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Approval
// ---------------------------------------------------------------------------

func TestApproval_DeniedShortCircuits(t *testing.T) {
	var executed atomic.Bool
	reg := catalogWith(tool.FuncTool(message.ToolDefinition{Name: "exec"},
		func(_ context.Context, _ string) (string, error) {
			executed.Store(true)
			return "ran", nil
		}))
	approver := ApproverFunc(func(_ context.Context, _ message.ToolCall) error {
		return errors.New("user rejected")
	})
	exec := tool.NewExecutor(reg, Approval(approver, "exec"))

	res := exec.Execute(context.Background(), call("exec"))
	if !res.IsError {
		t.Fatal("expected IsError for denied call")
	}
	if !strings.Contains(res.Content, "denied") {
		t.Errorf("Content = %q, want to contain 'denied'", res.Content)
	}
	if executed.Load() {
		t.Error("denied call reached the tool")
	}
}

func TestApproval_ApprovedAndUngated(t *testing.T) {
	reg := catalogWith(echoTool("exec"), echoTool("other"))
	approver := ApproverFunc(func(_ context.Context, _ message.ToolCall) error { return nil })
	exec := tool.NewExecutor(reg, Approval(approver, "exec"))

	if res := exec.Execute(context.Background(), call("exec")); res.IsError {
		t.Errorf("approved call should succeed, got %q", res.Content)
	}
	if res := exec.Execute(context.Background(), call("other")); res.IsError {
		t.Errorf("ungated tool should skip approval, got %q", res.Content)
	}
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

func TestAudit_RecordsEveryCall(t *testing.T) {
	reg := catalogWith(echoTool("echo"))
	var mu sync.Mutex
	var records []AuditRecord
	sink := AuditSinkFunc(func(_ context.Context, rec AuditRecord) {
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
	})
	exec := tool.NewExecutor(reg, Audit(sink))

	res := exec.Execute(context.Background(), call("echo"))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.Call.Name != "echo" || rec.Result.CallID != res.CallID {
		t.Errorf("record = %+v, want call echo / result %q", rec, res.CallID)
	}
	if rec.Duration < 0 {
		t.Error("record duration should not be negative")
	}
}

// selfTimingTool declares SelfTimeout and sleeps past any deadline the
// middleware might impose, so a wrapped call is distinguishable from an
// exempt one by whether it completes.
type selfTimingTool struct {
	name        string
	selfTimeout bool
	sleep       time.Duration
}

func (s selfTimingTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: s.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (s selfTimingTool) Metadata() tool.ToolMeta {
	return tool.ToolMeta{SelfTimeout: s.selfTimeout}
}

func (s selfTimingTool) Execute(ctx context.Context, _ string) (string, error) {
	select {
	case <-time.After(s.sleep):
		return "finished", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestTimeoutWithCatalog_SelfTimeoutExempt(t *testing.T) {
	registry := catalogWith(
		selfTimingTool{name: "self", selfTimeout: true, sleep: 20 * time.Millisecond},
		selfTimingTool{name: "wrapped", selfTimeout: false, sleep: 20 * time.Millisecond},
	)

	executor := tool.NewExecutor(registry,
		TimeoutWithCatalog(registry, time.Millisecond, nil))

	self := executor.Execute(context.Background(), call("self"))
	if self.IsError {
		t.Errorf("self-timing tool was cut short: %q", self.Content)
	}
	if self.Content != "finished" {
		t.Errorf("self-timing tool content = %q, want %q", self.Content, "finished")
	}

	wrapped := executor.Execute(context.Background(), call("wrapped"))
	if !wrapped.IsError {
		t.Errorf("tool without a SelfTimeout claim should have timed out, got %q", wrapped.Content)
	}
}

// TestTimeoutWithCatalog_PerToolOverridesSelfTimeout pins the precedence
// rule: a tool's claim is advisory, an explicit per-tool entry is host
// policy and wins.
func TestTimeoutWithCatalog_PerToolOverridesSelfTimeout(t *testing.T) {
	registry := catalogWith(
		selfTimingTool{name: "self", selfTimeout: true, sleep: 20 * time.Millisecond},
	)

	executor := tool.NewExecutor(registry, TimeoutWithCatalog(
		registry, 0, map[string]time.Duration{"self": time.Millisecond}))

	res := executor.Execute(context.Background(), call("self"))
	if !res.IsError {
		t.Errorf("per-tool override should bound a self-timing tool, got %q", res.Content)
	}
}

// TestTimeout_IgnoresSelfTimeoutWithoutCatalog documents that the
// catalog-free constructor cannot see the claim, so it keeps its
// original behaviour of wrapping everything.
func TestTimeout_IgnoresSelfTimeoutWithoutCatalog(t *testing.T) {
	registry := catalogWith(
		selfTimingTool{name: "self", selfTimeout: true, sleep: 20 * time.Millisecond},
	)

	executor := tool.NewExecutor(registry, Timeout(time.Millisecond, nil))

	res := executor.Execute(context.Background(), call("self"))
	if !res.IsError {
		t.Errorf("Timeout without a catalog should still wrap, got %q", res.Content)
	}
}
