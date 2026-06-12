package luart

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/script"
)

func TestRuntime_ExecSimpleScript(t *testing.T) {
	rt := New(WithPoolSize(2))
	sig, err := rt.Exec(context.Background(), "test", `local x = 1 + 2`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig != nil {
		t.Fatalf("unexpected signal: %+v", sig)
	}
}

func TestRuntime_ScriptError(t *testing.T) {
	rt := New(WithPoolSize(1))
	_, err := rt.Exec(context.Background(), "bad", `error("fail")`, nil)
	if err == nil {
		t.Fatal("expected error from failing script")
	}
}

func TestRuntime_ConfigInjection(t *testing.T) {
	rt := New(WithPoolSize(1))
	env := &script.Env{Config: map[string]any{"greeting": "hello"}}
	sig, err := rt.Exec(context.Background(), "cfg", `
		if config.greeting ~= "hello" then
			error("config not injected")
		end
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig != nil {
		t.Fatalf("unexpected signal: %+v", sig)
	}
}

func TestRuntime_SignalInterrupt(t *testing.T) {
	rt := New(WithPoolSize(1))
	sig, err := rt.Exec(context.Background(), "int", `signal.interrupt("need approval")`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil {
		t.Fatal("expected signal")
	}
	if sig.Type != "interrupt" {
		t.Fatalf("signal.Type = %q, want %q", sig.Type, "interrupt")
	}
	if sig.Message != "need approval" {
		t.Fatalf("signal.Message = %q", sig.Message)
	}
	// Bare-string form keeps Kind empty.
	if sig.Kind != "" {
		t.Errorf("bare-string interrupt should leave Kind empty, got %q", sig.Kind)
	}
}

func TestRuntime_SignalError_TableFormCarriesKind(t *testing.T) {
	rt := New(WithPoolSize(1))
	sig, err := rt.Exec(context.Background(), "sig-err-tbl", `
		signal.error({
			kind = "validation",
			message = "model is required",
			detail = { field = "model" }
		})
	`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil || sig.Type != "error" {
		t.Fatalf("signal = %+v, want error", sig)
	}
	if sig.Kind != "validation" {
		t.Errorf("Kind = %q, want %q", sig.Kind, "validation")
	}
	if sig.Message != "model is required" {
		t.Errorf("Message = %q", sig.Message)
	}
	if sig.Detail["field"] != "model" {
		t.Errorf("Detail[field] = %v, want %q", sig.Detail["field"], "model")
	}
}

func TestRuntime_SignalInterrupt_TableFormCarriesCause(t *testing.T) {
	rt := New(WithPoolSize(1))
	sig, err := rt.Exec(context.Background(), "sig-int-tbl", `
		signal.interrupt({ kind = "user_input", message = "barge" })
	`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil || sig.Type != "interrupt" {
		t.Fatalf("signal = %+v, want interrupt", sig)
	}
	if sig.Kind != "user_input" {
		t.Errorf("Kind = %q, want %q", sig.Kind, "user_input")
	}
	if sig.Message != "barge" {
		t.Errorf("Message = %q", sig.Message)
	}
}

func TestRuntime_PoolReuse(t *testing.T) {
	rt := New(WithPoolSize(1))
	for i := 0; i < 5; i++ {
		_, err := rt.Exec(context.Background(), "reuse", `local y = 42`, nil)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

func TestWrapChunk_BareCode(t *testing.T) {
	input := `local x = 1; local y = 2`
	result := wrapChunk(input)
	if result == input {
		t.Fatal("bare code should be wrapped")
	}
	if !strings.Contains(result, "return (function()") {
		t.Fatalf("expected wrapper, got %q", result)
	}
	if !strings.Contains(result, input) {
		t.Fatal("original script should be inside the wrapper")
	}
}

func TestWrapChunk_AlreadyWrapped(t *testing.T) {
	input := `return (function() local x = 1 end)()`
	result := wrapChunk(input)
	if result != input {
		t.Fatalf("already-wrapped script should not be double-wrapped, got %q", result)
	}
}

func TestWrapChunk_WhitespacePrefix(t *testing.T) {
	input := `  return (function() local x = 1 end)()`
	result := wrapChunk(input)
	if result != input {
		t.Fatal("should detect wrapper even with leading whitespace")
	}
}

func TestRuntime_IIFE_LocalIsolation(t *testing.T) {
	rt := New(WithPoolSize(1))

	_, err := rt.Exec(context.Background(), "set-var", `local leaked = "secret"`, nil)
	if err != nil {
		t.Fatalf("first exec: %v", err)
	}

	_, err = rt.Exec(context.Background(), "check-var", `
		if leaked ~= nil then
			error("unexpected global leaked")
		end
	`, nil)
	if err != nil {
		t.Fatalf("local should not become global across runs: %v", err)
	}
}

func TestRuntime_GlobalObjectDoesNotLeakBetweenExecs(t *testing.T) {
	rt := New(WithPoolSize(1))
	var oldHostCalled bool
	env := &script.Env{
		Bindings: map[string]any{
			"host": map[string]any{
				"mark": func() { oldHostCalled = true },
			},
		},
	}

	_, err := rt.Exec(context.Background(), "pollute-global", `
		_G.old_host = host
		_G.leaked_value = "secret"
	`, env)
	if err != nil {
		t.Fatalf("first exec: %v", err)
	}

	_, err = rt.Exec(context.Background(), "check-global", `
		if _G.old_host ~= nil then
			_G.old_host.mark()
			error("old host leaked across executions")
		end
		if _G.leaked_value ~= nil then
			error("global value leaked across executions: " .. tostring(_G.leaked_value))
		end
	`, nil)
	if err != nil {
		t.Fatalf("global object should not leak between executions: %v", err)
	}
	if oldHostCalled {
		t.Fatal("old host capability was callable from a later execution")
	}
}

func TestRuntime_Bindings(t *testing.T) {
	rt := New(WithPoolSize(1))
	var captured string
	env := &script.Env{
		Bindings: map[string]any{
			"host": map[string]any{
				"setVal": func(v string) { captured = v },
			},
		},
	}
	_, err := rt.Exec(context.Background(), "bind", `host.setVal("hello from lua")`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured != "hello from lua" {
		t.Fatalf("captured = %q, want %q", captured, "hello from lua")
	}
}

func TestRuntime_BindingsTwoReturns(t *testing.T) {
	rt := New(WithPoolSize(1))
	env := &script.Env{
		Bindings: map[string]any{
			"demo": map[string]any{
				"pair": func() (int, error) { return 7, nil },
			},
		},
	}
	_, err := rt.Exec(context.Background(), "tworet", `
		local a, err = demo.pair()
		if err ~= nil then error("expected nil err") end
		if a ~= 7 then error("bad a") end
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnrichPath_GenericError(t *testing.T) {
	err := fmt.Errorf("luart: script %q: %w", "myscript", fmt.Errorf("some failure"))
	if !strings.Contains(err.Error(), "myscript") {
		t.Fatalf("error should contain script name, got %q", err.Error())
	}
}

// ── Close idempotency & closed guard ──

func TestRuntime_CloseIdempotent(t *testing.T) {
	rt := New(WithPoolSize(2))
	if err := rt.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRuntime_ExecAfterClose(t *testing.T) {
	rt := New(WithPoolSize(1))
	_, _ = rt.Exec(context.Background(), "warmup", `local x = 1`, nil)
	rt.Close()

	_, err := rt.Exec(context.Background(), "after-close", `local x = 1`, nil)
	if err == nil {
		t.Fatal("expected error from Exec after Close")
	}
	if err != ErrRuntimeClosed {
		t.Fatalf("err = %v, want ErrRuntimeClosed", err)
	}
}

// ── VM discard on error ──

func TestRuntime_VMDiscardOnError_NoStateLeak(t *testing.T) {
	rt := New(WithPoolSize(1))

	_, err := rt.Exec(context.Background(), "pollute", `
		_G.leaked_global = "dirty"
		error("intentional failure")
	`, nil)
	if err == nil {
		t.Fatal("expected error from failing script")
	}

	_, err = rt.Exec(context.Background(), "check", `
		if _G.leaked_global ~= nil then
			error("global state leaked between executions: " .. tostring(_G.leaked_global))
		end
	`, nil)
	if err != nil {
		t.Fatalf("global state leaked after error: %v", err)
	}
}

func TestRuntime_VMDiscardAfterSignal_NoStateLeak(t *testing.T) {
	rt := New(WithPoolSize(1))

	_, err := rt.Exec(context.Background(), "set-global", `
		_G.persist_marker = 42
		signal.interrupt("pause")
	`, nil)
	if err != nil {
		t.Fatalf("signal should not cause error: %v", err)
	}

	_, err = rt.Exec(context.Background(), "check-persist", `
		if _G.persist_marker ~= nil then
			error("global state leaked after signal: " .. tostring(_G.persist_marker))
		end
	`, nil)
	if err != nil {
		t.Fatalf("VM should be discarded after signal: %v", err)
	}
}

// ── pushGoValue integer key map ──

func TestPushGoValue_IntKeyMap(t *testing.T) {
	rt := New(WithPoolSize(1))
	env := &script.Env{
		Bindings: map[string]any{
			"data": map[string]any{
				"intmap": map[int]string{1: "one", 2: "two", 3: "three"},
			},
		},
	}
	_, err := rt.Exec(context.Background(), "intmap", `
		if data.intmap[1] ~= "one" then error("key 1 = " .. tostring(data.intmap[1])) end
		if data.intmap[2] ~= "two" then error("key 2 = " .. tostring(data.intmap[2])) end
		if data.intmap[3] ~= "three" then error("key 3 = " .. tostring(data.intmap[3])) end
	`, env)
	if err != nil {
		t.Fatalf("integer key map: %v", err)
	}
}

func TestPushGoValue_UintKeyMap(t *testing.T) {
	rt := New(WithPoolSize(1))
	env := &script.Env{
		Bindings: map[string]any{
			"data": map[string]any{
				"umap": map[uint]string{10: "ten", 20: "twenty"},
			},
		},
	}
	_, err := rt.Exec(context.Background(), "uintmap", `
		if data.umap[10] ~= "ten" then error("key 10 = " .. tostring(data.umap[10])) end
		if data.umap[20] ~= "twenty" then error("key 20 = " .. tostring(data.umap[20])) end
	`, env)
	if err != nil {
		t.Fatalf("uint key map: %v", err)
	}
}

func TestRuntime_MaxExecTime_RuntimeEnforced(t *testing.T) {
	rt := New(WithPoolSize(1), WithMaxExecTime(50*time.Millisecond))
	defer rt.Close()
	start := time.Now()
	_, err := rt.Exec(context.Background(), "loop", `while true do end`, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout from runtime-enforced cap")
	}
	if !errdefs.IsTimeout(err) {
		t.Errorf("expected errdefs.IsTimeout, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("cap did not enforce, elapsed=%v", elapsed)
	}
}

func TestRuntime_MaxExecTime_CallerCtxStillWins(t *testing.T) {
	rt := New(WithPoolSize(1), WithMaxExecTime(10*time.Second))
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := rt.Exec(ctx, "loop", `while true do end`, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout from caller ctx")
	}
	if elapsed > 2*time.Second {
		t.Errorf("caller ctx did not win, elapsed=%v", elapsed)
	}
}
