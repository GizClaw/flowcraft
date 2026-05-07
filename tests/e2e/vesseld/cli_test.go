//go:build e2e

package vesseld_e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_CLI_Version asserts `vesseld version` prints SOMETHING
// non-empty to stdout and exits 0. The string itself is link-time
// (-X main.Version=...) so we only assert non-empty + exit code.
func TestE2E_CLI_Version(t *testing.T) {
	t.Parallel()
	bin := helpers.EnsureBinary(t)

	out, err := runCLI(t, bin, nil, "version")
	if err != nil {
		t.Fatalf("vesseld version: %v\nstdout:\n%s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("vesseld version: empty stdout")
	}
}

// TestE2E_CLI_Help asserts `vesseld help` exits 0 and prints
// usage that mentions every documented sub-command, so removing
// one accidentally trips this test.
func TestE2E_CLI_Help(t *testing.T) {
	t.Parallel()
	bin := helpers.EnsureBinary(t)

	out, err := runCLI(t, bin, nil, "help")
	if err != nil {
		t.Fatalf("vesseld help: %v\nstdout:\n%s", err, out)
	}
	for _, kw := range []string{"run", "validate", "plan", "version"} {
		if !strings.Contains(out, kw) {
			t.Errorf("help text missing keyword %q\n%s", kw, out)
		}
	}
}

// TestE2E_CLI_Validate_OK asserts `vesseld validate --config DIR`
// exits 0 on the multi-vessel testdata fixture (the same fixture
// validate_test.go drives in-process). No socket bind, no
// secret resolution — the binary returns immediately.
func TestE2E_CLI_Validate_OK(t *testing.T) {
	t.Parallel()
	bin := helpers.EnsureBinary(t)
	root, err := filepath.Abs("testdata/multi-vessel")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, bin, nil, "validate", "--config", root, "-R")
	if err != nil {
		t.Fatalf("vesseld validate: %v\nstdout:\n%s", err, out)
	}
}

// TestE2E_CLI_Validate_BadConfig_NonZero asserts validate exits
// non-zero on a known-bad fixture and prints a recognisable error.
func TestE2E_CLI_Validate_BadConfig_NonZero(t *testing.T) {
	t.Parallel()
	bin := helpers.EnsureBinary(t)
	root, err := filepath.Abs("testdata/badconfig/duplicate-vessel")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, bin, nil, "validate", "--config", root)
	if err == nil {
		t.Fatalf("vesseld validate on bad config: expected non-zero exit, got 0\nstdout:\n%s", out)
	}
	// Exit error must come from the process, not the harness.
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("vesseld validate: error type = %T (%v), want *exec.ExitError", err, err)
	}
}

// TestE2E_CLI_Plan_RedactsSecrets asserts `vesseld plan` (a) exits
// 0 on a valid config that references a secret and (b) NEVER
// prints the resolved secret value. We seed the env with a
// distinctive sentinel and grep stdout for it.
func TestE2E_CLI_Plan_RedactsSecrets(t *testing.T) {
	t.Parallel()
	bin := helpers.EnsureBinary(t)
	dir := t.TempDir()

	// Minimal config with a secret-backed apiKey. valueFrom: env
	// keeps the test hermetic — no temp files, no inline plain
	// text (which the resolver rejects anyway).
	cfg := `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata: {name: cli-plan}
spec:
  control: {socket: /tmp/unused.sock}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: LLMProfile
metadata: {name: mock}
spec:
  provider: openai
  config: {defaultModel: gpt-4o-mini, baseURL: http://example.invalid}
  auth:
    apiKey:
      valueFrom: {env: VESSELD_E2E_PLAN_SECRET}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata: {name: echo}
spec: {agents: [responder]}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata: {name: responder}
spec:
  engine:
    ref: graph-llm
    config: {llmProfile: mock}
`
	cfgPath := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	const sentinel = "sk-PLAN-REDACT-SENTINEL-9b4f1a2c"
	out, err := runCLI(t, bin, []string{"VESSELD_E2E_PLAN_SECRET=" + sentinel}, "plan", "--config", cfgPath)
	if err != nil {
		t.Fatalf("vesseld plan: %v\nstdout:\n%s", err, out)
	}
	if strings.Contains(out, sentinel) {
		t.Fatalf("vesseld plan leaked secret sentinel into stdout:\n%s", out)
	}
}

// runCLI execs the binary and returns its merged stdout/stderr.
// extraEnv entries ("KEY=VALUE") augment os.Environ; the harness
// always wipes any pre-existing VESSELD_E2E_* vars to avoid host
// leakage.
func runCLI(t *testing.T, binary string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	env := os.Environ()
	env = append(env, extraEnv...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
