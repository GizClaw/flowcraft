//go:build e2e

package vesseld_e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// sidecarRWTemplate carries a Vessel that hosts both a primary
// agent (Submit-triggered) and a Sidecar agent that — illegally —
// requests HistoryAccess=read_write. The vessel runtime now
// rejects this combination at buildEntries because sidecar
// dispatches always run with empty ContextID and would scatter
// transcript fragments across one-shot conversations.
//
// We assert the daemon refuses to come up: the binary exits with a
// non-zero status before /healthz ever returns 200, and stderr
// captures the rejection reason so an operator sees "fix this YAML"
// rather than "daemon mysteriously dead".
const sidecarRWTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-sidecar-rw
spec:
  control:
    socket: __SOCKET__
  shutdown:
    drainTimeout: 2s
  logging:
    format: text
    level: info
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: LLMProfile
metadata:
  name: mock-openai
spec:
  provider: openai
  config:
    defaultModel: gpt-4o-mini
    baseURL: __OPENAI_URL__
  auth:
    apiKey:
      valueFrom:
        env: VESSELD_E2E_API_KEY
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: HistoryStore
metadata:
  name: shared-history
spec:
  ref: buffer
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: bad
spec:
  agents: [primary, side]
  history:
    ref: shared-history
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: primary
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: side
spec:
  sidecar: true
  subscribeTo: agent.run.completed
  historyAccess: read_write
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
`

// TestE2E_Sidecar_RejectReadWriteHistory invokes the daemon with
// the illegal config and asserts startup fails inside the
// configured budget. The actual rejection text varies between vet
// and runtime errors, so we look for the substring "sidecar" plus
// "read_write" — the v0.1.0 message contains both words.
func TestE2E_Sidecar_RejectReadWriteHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)

	dir := helpers.ShortTempDir(t)
	socket := filepath.Join(dir, "v.sock")
	cfgPath := filepath.Join(dir, "daemon.yaml")
	cfg := strings.ReplaceAll(sidecarRWTemplate, "__OPENAI_URL__", mock.URL())
	cfg = strings.ReplaceAll(cfg, "__SOCKET__", socket)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run", "--config", cfgPath)
	out, _ := cmd.CombinedOutput()

	// Daemon MUST exit non-zero. ProcessState is non-nil iff the
	// process started and exited (not the timeout case).
	if cmd.ProcessState == nil {
		t.Fatalf("daemon never exited within timeout (likely hanging start)\noutput:\n%s", out)
	}
	if cmd.ProcessState.ExitCode() == 0 {
		t.Fatalf("daemon exited 0 with illegal sidecar+read_write config; should refuse\noutput:\n%s", out)
	}

	body := strings.ToLower(string(out))
	if !strings.Contains(body, "sidecar") || !strings.Contains(body, "readwrite") {
		t.Fatalf("rejection log did not mention sidecar+ReadWrite; output:\n%s", out)
	}
}
