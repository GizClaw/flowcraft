//go:build e2e

package vesseld_e2e

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// authTemplate adds a TCP listener + tokenFile auth on top of the
// single-vessel fixture. The daemon binds BOTH the unix socket
// (helpers.StartDaemon's healthz channel) and the TCP listener
// (the auth surface under test).
const authTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-auth
spec:
  control:
    socket: __SOCKET__
    listen: __LISTEN__
    auth:
      tokenFile: __TOKEN_FILE__
  shutdown:
    drainTimeout: 5s
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
kind: Vessel
metadata:
  name: echo
spec:
  agents: [responder]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: responder
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
`

// pickFreePort opens and immediately closes a TCP listener on
// 127.0.0.1:0; the OS-assigned port is then re-used as the
// daemon's listen address. Race-prone in theory (another process
// could grab the port between Close and the daemon binding it),
// in practice fine on a CI host with a private socket pool.
func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func writeTokenFile(t *testing.T, dir, token string) string {
	t.Helper()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

// TestE2E_TCPAuth_Required asserts:
//   - request without Authorization header → 401-ish (NotAvailable)
//   - request with the right bearer token → 200
//   - request with the wrong token         → 401-ish
func TestE2E_TCPAuth_Required(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()

	dir := t.TempDir()
	tokenPath := writeTokenFile(t, dir, "test-secret-token")
	listen := pickFreePort(t)

	cfg := authTemplate
	cfg = strings.ReplaceAll(cfg, "__OPENAI_URL__", mock.URL())
	cfg = strings.ReplaceAll(cfg, "__LISTEN__", listen)
	cfg = strings.ReplaceAll(cfg, "__TOKEN_FILE__", tokenPath)

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, cfg)

	// We use a plain http.Client (NOT d.HTTPClient(), which dials
	// the unix socket) so the request hits the TCP listener and
	// the auth filter actually fires.
	tcp := &http.Client{}
	url := fmt.Sprintf("http://%s/v1/vessels", listen)

	// 1. No auth → reject.
	resp, err := tcp.Get(url)
	if err != nil {
		t.Fatalf("GET tcp (no auth): %v\nstderr:\n%s", err, d.Stderr())
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 for unauthenticated TCP, got 200")
	}

	// 2. Wrong token → reject.
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = tcp.Do(req)
	if err != nil {
		t.Fatalf("GET tcp (wrong token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 for wrong token, got 200")
	}

	// 3. Right token → 200.
	req, _ = http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer test-secret-token")
	resp, err = tcp.Do(req)
	if err != nil {
		t.Fatalf("GET tcp (good token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for valid token, got %d\nstderr:\n%s", resp.StatusCode, d.Stderr())
	}

	// 4. Unix socket path on the same daemon does NOT require
	//    a bearer token (filesystem perms are the boundary). The
	//    fleet may answer 5xx briefly during launch, so we accept
	//    any non-401-style response — the assertion is purely
	//    "auth filter did not fire".
	resp, err = d.HTTPClient().Get("http://vesseld/v1/vessels")
	if err != nil {
		t.Fatalf("GET unix: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("unix socket unexpectedly returned 401 (auth must skip unix transport)")
	}
}
