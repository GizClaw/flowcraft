package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func stubTool(name string) Tool {
	return FuncTool(model.ToolDefinition{Name: name, Description: name + " desc"}, func(_ context.Context, _ string) (string, error) {
		return "{}", nil
	})
}

func errTool(name string, err error) Tool {
	return FuncTool(model.ToolDefinition{Name: name}, func(_ context.Context, _ string) (string, error) {
		return "", err
	})
}

type selfTimeoutTool struct {
	def    model.ToolDefinition
	called bool
}

func (s *selfTimeoutTool) Definition() model.ToolDefinition { return s.def }
func (s *selfTimeoutTool) Execute(ctx context.Context, _ string) (string, error) {
	s.called = true
	if _, ok := ctx.Deadline(); ok {
		return "", fmt.Errorf("context should not have deadline")
	}
	return "ok", nil
}
func (s *selfTimeoutTool) SelfTimeout() bool { return true }

func TestRegister_DefaultScopeIsAgent(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))

	if got := r.ScopeOf("foo"); got != ScopeAgent {
		t.Errorf("ScopeOf(foo) = %q, want %q", got, ScopeAgent)
	}
}

func TestRegisterWithScope_Platform(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithScope(stubTool("bar"), ScopePlatform)

	if got := r.ScopeOf("bar"); got != ScopePlatform {
		t.Errorf("ScopeOf(bar) = %q, want %q", got, ScopePlatform)
	}
}

func TestScopeOf_UnknownTool_ReturnsAgent(t *testing.T) {
	r := NewRegistry()
	if got := r.ScopeOf("nonexistent"); got != ScopeAgent {
		t.Errorf("ScopeOf(nonexistent) = %q, want %q", got, ScopeAgent)
	}
}

func TestDefinitionsByScope(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("agent_tool_1"))
	r.Register(stubTool("agent_tool_2"))
	r.RegisterWithScope(stubTool("platform_tool_1"), ScopePlatform)
	r.RegisterWithScope(stubTool("platform_tool_2"), ScopePlatform)
	r.RegisterWithScope(stubTool("platform_tool_3"), ScopePlatform)

	agentDefs := r.DefinitionsByScope(ScopeAgent)
	if len(agentDefs) != 2 {
		t.Errorf("DefinitionsByScope(agent) returned %d items, want 2", len(agentDefs))
	}

	platformDefs := r.DefinitionsByScope(ScopePlatform)
	if len(platformDefs) != 3 {
		t.Errorf("DefinitionsByScope(platform) returned %d items, want 3", len(platformDefs))
	}

	allDefs := r.Definitions()
	if len(allDefs) != 5 {
		t.Errorf("Definitions() returned %d items, want 5", len(allDefs))
	}
}

func TestUnregister_RemovesScope(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithScope(stubTool("tmp"), ScopePlatform)

	if !r.Unregister("tmp") {
		t.Fatal("Unregister returned false for existing tool")
	}
	if _, ok := r.Get("tmp"); ok {
		t.Error("tool still found after Unregister")
	}
	if got := r.ScopeOf("tmp"); got != ScopeAgent {
		t.Errorf("ScopeOf(tmp) after Unregister = %q, want %q (default)", got, ScopeAgent)
	}
}

func TestDefinitionsByScope_Empty(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithScope(stubTool("only_platform"), ScopePlatform)

	agentDefs := r.DefinitionsByScope(ScopeAgent)
	if agentDefs != nil {
		t.Errorf("DefinitionsByScope(agent) = %v, want nil", agentDefs)
	}
}

func TestRegister_OverwritePreservesNewScope(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("x"))

	if got := r.ScopeOf("x"); got != ScopeAgent {
		t.Fatalf("initial scope = %q, want %q", got, ScopeAgent)
	}

	r.RegisterWithScope(stubTool("x"), ScopePlatform)
	if got := r.ScopeOf("x"); got != ScopePlatform {
		t.Errorf("after re-register scope = %q, want %q", got, ScopePlatform)
	}
}

func TestWithMaxConcurrency(t *testing.T) {
	r := NewRegistry(WithMaxConcurrency(5))
	if r.maxConcurrency != 5 {
		t.Errorf("maxConcurrency = %d, want 5", r.maxConcurrency)
	}
}

func TestWithMaxConcurrency_IgnoresNonPositive(t *testing.T) {
	r := NewRegistry(WithMaxConcurrency(0))
	if r.maxConcurrency != defaultMaxConcurrency {
		t.Errorf("maxConcurrency = %d, want default %d", r.maxConcurrency, defaultMaxConcurrency)
	}
	r2 := NewRegistry(WithMaxConcurrency(-3))
	if r2.maxConcurrency != defaultMaxConcurrency {
		t.Errorf("maxConcurrency = %d, want default %d", r2.maxConcurrency, defaultMaxConcurrency)
	}
}

func TestWithExecTimeout(t *testing.T) {
	r := NewRegistry(WithExecTimeout(10 * time.Second))
	if r.execTimeout != 10*time.Second {
		t.Errorf("execTimeout = %v, want 10s", r.execTimeout)
	}
}

func TestWithExecTimeout_IgnoresNonPositive(t *testing.T) {
	r := NewRegistry(WithExecTimeout(0))
	if r.execTimeout != defaultExecTimeout {
		t.Errorf("execTimeout = %v, want default %v", r.execTimeout, defaultExecTimeout)
	}
}

func TestNewRegistry_EnvVarConcurrency(t *testing.T) {
	t.Setenv("FLOWCRAFT_TOOL_CONCURRENCY", "3")
	r := NewRegistry()
	if r.maxConcurrency != 3 {
		t.Errorf("maxConcurrency = %d, want 3", r.maxConcurrency)
	}
}

func TestNewRegistry_EnvVarTimeout(t *testing.T) {
	t.Setenv("FLOWCRAFT_TOOL_TIMEOUT", "5s")
	r := NewRegistry()
	if r.execTimeout != 5*time.Second {
		t.Errorf("execTimeout = %v, want 5s", r.execTimeout)
	}
}

func TestNewRegistry_InvalidEnvVarsIgnored(t *testing.T) {
	t.Setenv("FLOWCRAFT_TOOL_CONCURRENCY", "not_a_number")
	t.Setenv("FLOWCRAFT_TOOL_TIMEOUT", "not_a_duration")
	r := NewRegistry()
	if r.maxConcurrency != defaultMaxConcurrency {
		t.Errorf("maxConcurrency = %d, want default %d", r.maxConcurrency, defaultMaxConcurrency)
	}
	if r.execTimeout != defaultExecTimeout {
		t.Errorf("execTimeout = %v, want default %v", r.execTimeout, defaultExecTimeout)
	}
}

func TestNewRegistry_OptionOverridesEnvVar(t *testing.T) {
	t.Setenv("FLOWCRAFT_TOOL_CONCURRENCY", "3")
	r := NewRegistry(WithMaxConcurrency(7))
	if r.maxConcurrency != 7 {
		t.Errorf("maxConcurrency = %d, want 7 (option should override env)", r.maxConcurrency)
	}
}

func TestNames(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("alpha"))
	r.Register(stubTool("beta"))

	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("len(Names) = %d, want 2", len(names))
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["alpha"] || !nameSet["beta"] {
		t.Errorf("Names = %v, want {alpha, beta}", names)
	}
}

func TestLen(t *testing.T) {
	r := NewRegistry()
	if r.Len() != 0 {
		t.Fatalf("empty Len = %d", r.Len())
	}
	r.Register(stubTool("a"))
	r.Register(stubTool("b"))
	if r.Len() != 2 {
		t.Errorf("Len = %d, want 2", r.Len())
	}
}

func TestExecute_Success(t *testing.T) {
	r := NewRegistry()
	r.Register(FuncTool(
		model.ToolDefinition{Name: "echo"},
		func(_ context.Context, args string) (string, error) {
			return "echoed:" + args, nil
		},
	))

	result := r.Execute(context.Background(), model.ToolCall{
		ID: "call-1", Name: "echo", Arguments: "hello",
	})
	if result.ToolCallID != "call-1" {
		t.Errorf("ToolCallID = %q, want %q", result.ToolCallID, "call-1")
	}
	if result.Content != "echoed:hello" {
		t.Errorf("Content = %q, want %q", result.Content, "echoed:hello")
	}
	if result.IsError {
		t.Error("IsError should be false for success")
	}
}

func TestExecute_ToolNotFound(t *testing.T) {
	r := NewRegistry()
	result := r.Execute(context.Background(), model.ToolCall{
		ID: "call-1", Name: "missing", Arguments: "{}",
	})
	if !result.IsError {
		t.Fatal("expected IsError for missing tool")
	}
	if !strings.Contains(result.Content, "not found") {
		t.Errorf("Content = %q, want to contain 'not found'", result.Content)
	}
}

func TestExecute_ToolReturnsError(t *testing.T) {
	r := NewRegistry()
	r.Register(errTool("fail", errors.New("broken")))

	result := r.Execute(context.Background(), model.ToolCall{
		ID: "call-2", Name: "fail", Arguments: "{}",
	})
	if !result.IsError {
		t.Error("IsError should be true")
	}
	if !strings.Contains(result.Content, "broken") {
		t.Errorf("Content = %q, want to contain 'broken'", result.Content)
	}
}

func TestExecute_ContextCancelled(t *testing.T) {
	r := NewRegistry(WithExecTimeout(5 * time.Second))
	r.Register(FuncTool(
		model.ToolDefinition{Name: "slow"},
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := r.Execute(ctx, model.ToolCall{
		ID: "call-3", Name: "slow", Arguments: "{}",
	})
	if !result.IsError {
		t.Error("IsError should be true for cancelled context")
	}
}

func TestExecute_Timeout(t *testing.T) {
	r := NewRegistry(WithExecTimeout(50 * time.Millisecond))
	r.Register(FuncTool(
		model.ToolDefinition{Name: "hang"},
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	))

	result := r.Execute(context.Background(), model.ToolCall{
		ID: "call-4", Name: "hang", Arguments: "{}",
	})
	if !result.IsError {
		t.Error("IsError should be true for timed-out tool")
	}
}

func TestExecute_SelfTimeouter_SkipsTimeout(t *testing.T) {
	r := NewRegistry(WithExecTimeout(50 * time.Millisecond))
	st := &selfTimeoutTool{def: model.ToolDefinition{Name: "self"}}
	r.Register(st)

	result := r.Execute(context.Background(), model.ToolCall{
		ID: "call-5", Name: "self", Arguments: "{}",
	})
	if result.IsError {
		t.Errorf("unexpected tool error: %s", result.Content)
	}
	if !st.called {
		t.Error("tool was not called")
	}
}

func TestExecuteAll_Success(t *testing.T) {
	r := NewRegistry()
	r.Register(FuncTool(
		model.ToolDefinition{Name: "add"},
		func(_ context.Context, args string) (string, error) {
			return "result:" + args, nil
		},
	))

	calls := []model.ToolCall{
		{ID: "c1", Name: "add", Arguments: "1"},
		{ID: "c2", Name: "add", Arguments: "2"},
		{ID: "c3", Name: "add", Arguments: "3"},
	}
	results := r.ExecuteAll(context.Background(), calls)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	for i, res := range results {
		if res.ToolCallID != calls[i].ID {
			t.Errorf("results[%d].ToolCallID = %q, want %q", i, res.ToolCallID, calls[i].ID)
		}
		expected := "result:" + calls[i].Arguments
		if res.Content != expected {
			t.Errorf("results[%d].Content = %q, want %q", i, res.Content, expected)
		}
	}
}

func TestExecuteAll_MixedSuccessAndFailure(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("good"))
	r.Register(errTool("bad", errors.New("fail")))

	calls := []model.ToolCall{
		{ID: "c1", Name: "good", Arguments: "{}"},
		{ID: "c2", Name: "bad", Arguments: "{}"},
		{ID: "c3", Name: "good", Arguments: "{}"},
	}
	results := r.ExecuteAll(context.Background(), calls)
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}
	if results[0].IsError {
		t.Error("results[0] should succeed")
	}
	if !results[1].IsError {
		t.Error("results[1] should be error")
	}
	if results[2].IsError {
		t.Error("results[2] should succeed")
	}
}

func TestExecuteAll_PanicRecovery(t *testing.T) {
	r := NewRegistry()
	r.Register(FuncTool(
		model.ToolDefinition{Name: "panicker"},
		func(_ context.Context, _ string) (string, error) {
			panic("oh no")
		},
	))

	results := r.ExecuteAll(context.Background(), []model.ToolCall{
		{ID: "c1", Name: "panicker", Arguments: "{}"},
	})
	if len(results) != 1 {
		t.Fatalf("len = %d, want 1", len(results))
	}
	if !results[0].IsError {
		t.Error("panicked tool result should be IsError")
	}
	if !strings.Contains(results[0].Content, "panicked") {
		t.Errorf("Content = %q, want to contain 'panicked'", results[0].Content)
	}
}

func TestExecuteAll_SemaphoreContextCancelled(t *testing.T) {
	r := NewRegistry(WithMaxConcurrency(1))
	r.Register(FuncTool(
		model.ToolDefinition{Name: "block"},
		func(ctx context.Context, _ string) (string, error) {
			time.Sleep(200 * time.Millisecond)
			return "done", nil
		},
	))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	calls := make([]model.ToolCall, 5)
	for i := range calls {
		calls[i] = model.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "block", Arguments: "{}"}
	}

	results := r.ExecuteAll(ctx, calls)
	hasError := false
	for _, res := range results {
		if res.IsError {
			hasError = true
			break
		}
	}
	if !hasError {
		t.Error("expected at least one error from semaphore contention with cancelled context")
	}
}

func TestExecuteAll_Empty(t *testing.T) {
	r := NewRegistry()
	results := r.ExecuteAll(context.Background(), nil)
	if len(results) != 0 {
		t.Errorf("len = %d, want 0", len(results))
	}
}

func TestExecuteAll_ToolNotFound(t *testing.T) {
	r := NewRegistry()
	results := r.ExecuteAll(context.Background(), []model.ToolCall{
		{ID: "c1", Name: "nonexistent", Arguments: "{}"},
	})
	if len(results) != 1 {
		t.Fatalf("len = %d, want 1", len(results))
	}
	if !results[0].IsError {
		t.Error("should be error for missing tool")
	}
}

func TestUnregister_NonExistent(t *testing.T) {
	r := NewRegistry()
	if r.Unregister("nope") {
		t.Error("Unregister should return false for non-existent tool")
	}
}
