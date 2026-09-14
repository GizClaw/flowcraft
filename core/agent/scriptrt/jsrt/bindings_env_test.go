package jsrt_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
)

// buildEnv assembles a script env for the bridge tests; assembly failure
// is a test failure.
func buildEnv(t *testing.T, config map[string]any, fns ...bindings.BindingFunc) *agent.ScriptEnv {
	t.Helper()
	env, err := bindings.BuildEnv(context.Background(), config, fns...)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	return env
}
