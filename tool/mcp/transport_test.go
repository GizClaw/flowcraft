package mcp

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestReconnectableStdioRereadsEnvironment verifies that each
// connection attempt rebuilds the child environment from the current
// process environment instead of a snapshot taken when Stdio was
// called.
func TestReconnectableStdioRereadsEnvironment(t *testing.T) {
	t.Setenv("FC_MCP_STDIO_TEST", "first")
	tport, err := Stdio("true", nil, map[string]string{"FC_MCP_STDIO_EXTRA": "x"})
	if err != nil {
		t.Fatalf("Stdio: %v", err)
	}
	rs, ok := tport.(*reconnectableStdio)
	if !ok {
		t.Fatalf("Stdio returned %T, want *reconnectableStdio", tport)
	}

	assertEnv := func(wantValue string) {
		t.Helper()
		cmd := rs.newCommand()
		if !slices.Contains(cmd.Env, "FC_MCP_STDIO_TEST="+wantValue) {
			t.Fatalf("child env missing FC_MCP_STDIO_TEST=%s (env %v)", wantValue, cmd.Env)
		}
		if !slices.Contains(cmd.Env, "FC_MCP_STDIO_EXTRA=x") {
			t.Fatalf("child env missing caller-supplied FC_MCP_STDIO_EXTRA=x (env %v)", cmd.Env)
		}
	}

	assertEnv("first")
	t.Setenv("FC_MCP_STDIO_TEST", "second")
	assertEnv("second")
}

// TestReconnectableStdioNilEnvInheritsAtSpawn keeps the no-extra-env
// contract: with no caller env, the child command carries a nil Env,
// so the exec layer inherits the environment at spawn time.
func TestReconnectableStdioNilEnvInheritsAtSpawn(t *testing.T) {
	tport, err := Stdio("true", nil, nil)
	if err != nil {
		t.Fatalf("Stdio: %v", err)
	}
	cmd := tport.(*reconnectableStdio).newCommand()
	if cmd.Env != nil {
		t.Fatalf("child env = %v, want nil (inherit at spawn)", cmd.Env)
	}
	if base := filepath.Base(cmd.Path); base != "true" && base != "true.exe" {
		t.Fatalf("command path = %q, want a true binary", cmd.Path)
	}
}
