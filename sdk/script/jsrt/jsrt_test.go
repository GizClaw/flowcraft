package jsrt

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/script"
)

func TestRuntime_ExecSimpleScript(t *testing.T) {
	rt := New(WithPoolSize(2))
	sig, err := rt.Exec(context.Background(), "test", `var x = 1 + 2;`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig != nil {
		t.Fatalf("unexpected signal: %+v", sig)
	}
}

func TestRuntime_ScriptError(t *testing.T) {
	rt := New(WithPoolSize(1))
	_, err := rt.Exec(context.Background(), "bad", `throw new Error("fail")`, nil)
	if err == nil {
		t.Fatal("expected error from failing script")
	}
}

func TestRuntime_ConfigInjection(t *testing.T) {
	rt := New(WithPoolSize(1))
	env := &script.Env{Config: map[string]any{"greeting": "hello"}}
	sig, err := rt.Exec(context.Background(), "cfg", `
		if (config.greeting !== "hello") {
			throw new Error("config not injected");
		}
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
		_, err := rt.Exec(context.Background(), "reuse", `var y = 42;`, nil)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

func TestWrapIIFE_BareCode(t *testing.T) {
	input := `var x = 1; var y = 2;`
	result := wrapIIFE(input)
	if result == input {
		t.Fatal("bare code should be wrapped")
	}
	if !strings.Contains(result, "(function(){") {
		t.Fatalf("expected IIFE wrapper, got %q", result)
	}
	if !strings.Contains(result, input) {
		t.Fatal("original script should be inside the wrapper")
	}
}

func TestWrapIIFE_AlreadyWrappedFunction(t *testing.T) {
	input := `(function(){ var x = 1; })()`
	result := wrapIIFE(input)
	if result != input {
		t.Fatalf("already-wrapped script should not be double-wrapped, got %q", result)
	}
}

func TestWrapIIFE_AlreadyWrappedArrow(t *testing.T) {
	input := `(()=>{ var x = 1; })()`
	result := wrapIIFE(input)
	if result != input {
		t.Fatalf("arrow IIFE should not be double-wrapped, got %q", result)
	}
}

func TestWrapIIFE_WhitespacePrefix(t *testing.T) {
	input := `  (function(){ var x = 1; })()`
	result := wrapIIFE(input)
	if result != input {
		t.Fatal("should detect IIFE even with leading whitespace")
	}
}

func TestRuntime_IIFE_VarIsolation(t *testing.T) {
	rt := New(WithPoolSize(1))

	_, err := rt.Exec(context.Background(), "set-var", `var leaked = "secret";`, nil)
	if err != nil {
		t.Fatalf("first exec: %v", err)
	}

	_, err = rt.Exec(context.Background(), "check-var", `
		if (typeof leaked !== "undefined") {
			throw new Error("var leaked across executions: " + leaked);
		}
	`, nil)
	if err != nil {
		t.Fatalf("var should not leak between IIFE-wrapped executions: %v", err)
	}
}

func TestEnrichError_GojaException(t *testing.T) {
	rt := New(WithPoolSize(1))
	_, err := rt.Exec(context.Background(), "err-test", `
		function foo() { throw new Error("line error"); }
		foo();
	`, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "err-test") {
		t.Fatalf("error should contain script name, got %q", msg)
	}
	if !strings.Contains(msg, "line error") {
		t.Fatalf("error should contain original message, got %q", msg)
	}
}

func TestEnrichError_GenericError(t *testing.T) {
	err := enrichError("myscript", fmt.Errorf("some failure"))
	if !strings.Contains(err.Error(), "myscript") {
		t.Fatalf("error should contain script name, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "some failure") {
		t.Fatalf("error should contain original message, got %q", err.Error())
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
	_, err := rt.Exec(context.Background(), "bind", `host.setVal("hello from js")`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured != "hello from js" {
		t.Fatalf("captured = %q, want %q", captured, "hello from js")
	}
}
