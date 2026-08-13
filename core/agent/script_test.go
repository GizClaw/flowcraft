package agent

import (
	"context"
	"errors"
	"testing"
)

func TestScriptSignal_Fields(t *testing.T) {
	s := ScriptSignal{
		Type:    "error",
		Kind:    "validation",
		Message: "something went wrong",
		Detail:  map[string]any{"field": "model"},
	}
	if s.Type != "error" {
		t.Errorf("Type = %q, want %q", s.Type, "error")
	}
	if s.Kind != "validation" {
		t.Errorf("Kind = %q, want %q", s.Kind, "validation")
	}
	if s.Message != "something went wrong" {
		t.Errorf("Message = %q, want %q", s.Message, "something went wrong")
	}
	if s.Detail["field"] != "model" {
		t.Errorf("Detail[field] = %v, want %q", s.Detail["field"], "model")
	}
}

func TestScriptSignal_ZeroValue(t *testing.T) {
	var s ScriptSignal
	if s.Type != "" {
		t.Errorf("zero Type = %q, want empty", s.Type)
	}
	if s.Kind != "" {
		t.Errorf("zero Kind = %q, want empty", s.Kind)
	}
	if s.Message != "" {
		t.Errorf("zero Message = %q, want empty", s.Message)
	}
	if s.Detail != nil {
		t.Errorf("zero Detail = %v, want nil", s.Detail)
	}
}

func TestScriptEnv_NilMaps(t *testing.T) {
	env := &ScriptEnv{}
	if env.Config != nil {
		t.Error("zero Config should be nil")
	}
	if env.Bindings != nil {
		t.Error("zero Bindings should be nil")
	}
}

func TestScriptEnv_WithConfigAndBindings(t *testing.T) {
	env := &ScriptEnv{
		Config: map[string]any{
			"timeout": 30,
			"verbose": true,
		},
		Bindings: map[string]any{
			"http": map[string]any{"get": "stub"},
		},
	}
	if env.Config["timeout"] != 30 {
		t.Errorf("Config[timeout] = %v, want 30", env.Config["timeout"])
	}
	if _, ok := env.Bindings["http"]; !ok {
		t.Error("Bindings missing 'http' key")
	}
}

type stubScriptRuntime struct {
	signal *ScriptSignal
	err    error
}

func (s *stubScriptRuntime) Exec(_ context.Context, _, _ string, _ *ScriptEnv) (*ScriptSignal, error) {
	return s.signal, s.err
}

func TestScriptRuntime_InterfaceSatisfaction(t *testing.T) {
	var r ScriptRuntime = &stubScriptRuntime{
		signal: &ScriptSignal{Type: "done"},
	}
	sig, err := r.Exec(context.Background(), "test.js", "console.log(1)", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig.Type != "done" {
		t.Errorf("signal Type = %q, want %q", sig.Type, "done")
	}
}

func TestScriptRuntime_ReturnsError(t *testing.T) {
	var r ScriptRuntime = &stubScriptRuntime{
		err: errors.New("syntax error"),
	}
	_, err := r.Exec(context.Background(), "bad.js", "???", &ScriptEnv{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "syntax error" {
		t.Errorf("error = %q, want %q", err.Error(), "syntax error")
	}
}
