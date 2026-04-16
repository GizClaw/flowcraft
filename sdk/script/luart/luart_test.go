package luart

import (
	"context"
	"fmt"
	"strings"
	"testing"

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

func TestRuntime_VMDiscardOnError_SignalDoesNotDiscard(t *testing.T) {
	rt := New(WithPoolSize(1))

	_, err := rt.Exec(context.Background(), "set-global", `
		_G.persist_marker = 42
		signal.interrupt("pause")
	`, nil)
	if err != nil {
		t.Fatalf("signal should not cause error: %v", err)
	}

	_, err = rt.Exec(context.Background(), "check-persist", `
		if _G.persist_marker ~= 42 then
			error("expected marker to persist after signal (VM not discarded)")
		end
	`, nil)
	if err != nil {
		t.Fatalf("VM should NOT be discarded on signal: %v", err)
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
