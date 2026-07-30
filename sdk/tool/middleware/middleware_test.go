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

	"github.com/GizClaw/flowcraft/sdk/tool"
)

func catalogWith(tools ...tool.Tool) *tool.Registry {
	r := tool.NewRegistry()
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

func echoTool(name string) tool.Tool {
	return tool.FuncTool(tool.Definition{Name: name},
		func(_ context.Context, args string) (string, error) {
			return "echo:" + args, nil
		})
}

func call(name string) tool.Call {
	return tool.Call{ID: "call-1", Name: name, Arguments: json.RawMessage(`{"x":1}`)}
}

// ---------------------------------------------------------------------------
// Recover
// ---------------------------------------------------------------------------

func TestRecover_PanicBecomesErrorResult(t *testing.T) {
	reg := catalogWith(tool.FuncTool(tool.Definition{Name: "panicker"},
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
		tool.FuncTool(tool.Definition{Name: "panicker"},
			func(_ context.Context, _ string) (string, error) { panic("boom") }),
		echoTool("fine"),
	)
	exec := tool.NewExecutor(reg, Recover())

	results := exec.ExecuteAll(context.Background(), []tool.Call{
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
	reg := catalogWith(tool.FuncTool(tool.Definition{Name: "slow"},
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

	calls := make([]tool.Call, 6)
	for i := range calls {
		calls[i] = tool.Call{ID: fmt.Sprintf("c%d", i), Name: "slow", Arguments: json.RawMessage("{}")}
	}
	exec.ExecuteAll(context.Background(), calls)
	if got := maxSeen.Load(); got > 2 {
		t.Errorf("max in-flight = %d, want <= 2", got)
	}
}

func TestConcurrency_ContextCancelWhileWaiting(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	reg := catalogWith(tool.FuncTool(tool.Definition{Name: "holder"},
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

	first := make(chan tool.Result, 1)
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
	reg := catalogWith(tool.FuncTool(tool.Definition{Name: "hang"},
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
		tool.FuncTool(tool.Definition{Name: "slowish"},
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
	def  tool.Definition
	rate float64
}

func (r ratedTool) Definition() tool.Definition { return r.def }
func (r ratedTool) Execute(_ context.Context, _ string) (string, error) {
	return "ok", nil
}
func (r ratedTool) Metadata() tool.ToolMeta { return tool.ToolMeta{RateLimit: r.rate} }

func TestRateLimit_PacesCalls(t *testing.T) {
	reg := catalogWith(ratedTool{def: tool.Definition{Name: "api"}, rate: 50})
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
	reg := catalogWith(tool.FuncTool(tool.Definition{Name: "exec"},
		func(_ context.Context, _ string) (string, error) {
			executed.Store(true)
			return "ran", nil
		}))
	approver := ApproverFunc(func(_ context.Context, _ tool.Call) error {
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
	approver := ApproverFunc(func(_ context.Context, _ tool.Call) error { return nil })
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
	if rec.Duration <= 0 {
		t.Error("record duration should be positive")
	}
}
