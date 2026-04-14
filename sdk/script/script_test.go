package script

import (
	"context"
	"errors"
	"testing"
)

func TestSignal_Fields(t *testing.T) {
	s := Signal{Type: "abort", Message: "something went wrong"}
	if s.Type != "abort" {
		t.Errorf("Type = %q, want %q", s.Type, "abort")
	}
	if s.Message != "something went wrong" {
		t.Errorf("Message = %q, want %q", s.Message, "something went wrong")
	}
}

func TestSignal_ZeroValue(t *testing.T) {
	var s Signal
	if s.Type != "" {
		t.Errorf("zero Type = %q, want empty", s.Type)
	}
	if s.Message != "" {
		t.Errorf("zero Message = %q, want empty", s.Message)
	}
}

func TestEnv_NilMaps(t *testing.T) {
	env := &Env{}
	if env.Config != nil {
		t.Error("zero Config should be nil")
	}
	if env.Bindings != nil {
		t.Error("zero Bindings should be nil")
	}
}

func TestEnv_WithConfigAndBindings(t *testing.T) {
	env := &Env{
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

type stubRuntime struct {
	signal *Signal
	err    error
}

func (s *stubRuntime) Exec(_ context.Context, _, _ string, _ *Env) (*Signal, error) {
	return s.signal, s.err
}

func TestRuntime_InterfaceSatisfaction(t *testing.T) {
	var r Runtime = &stubRuntime{
		signal: &Signal{Type: "done"},
	}
	sig, err := r.Exec(context.Background(), "test.js", "console.log(1)", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig.Type != "done" {
		t.Errorf("signal Type = %q, want %q", sig.Type, "done")
	}
}

func TestRuntime_ReturnsError(t *testing.T) {
	var r Runtime = &stubRuntime{
		err: errors.New("syntax error"),
	}
	_, err := r.Exec(context.Background(), "bad.js", "???", &Env{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "syntax error" {
		t.Errorf("error = %q, want %q", err.Error(), "syntax error")
	}
}
