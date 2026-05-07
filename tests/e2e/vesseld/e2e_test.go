//go:build e2e

package vesseld_e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// configTemplate is the minimum vesseld YAML that still exercises
// the full startup path: one Daemon, one LLMProfile pointing at
// the mock OpenAI server, one Vessel with one graph-llm Agent.
//
// __SOCKET__ and __OPENAI_URL__ are placeholder substitutions
// performed by the helpers / template-fill calls in each test.
const configTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-e2e
spec:
  control:
    socket: __SOCKET__
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

// fillConfig substitutes the mock OpenAI base URL into the
// template; the daemon helper does the __SOCKET__ substitution
// itself because it picks the temp socket path.
func fillConfig(openaiURL string) string {
	return strings.ReplaceAll(configTemplate, "__OPENAI_URL__", openaiURL)
}

// TestE2E_Lifecycle covers the headline path: build → run → bind
// socket → answer healthz → list vessels → call → SIGTERM → drain
// → exit. A failure anywhere in that chain is the most useful
// signal a daemon test can produce.
func TestE2E_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "hello from mock"
	defer mock.Close()

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	cli := d.HTTPClient()

	// /v1/vessels returns our one vessel.
	resp, err := cli.Get("http://vesseld/v1/vessels")
	if err != nil {
		t.Fatalf("GET vessels: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vessels status = %d", resp.StatusCode)
	}
	var list []map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode vessels: %v", err)
	}
	resp.Body.Close()
	if len(list) != 1 || list[0]["name"] != "echo" {
		t.Fatalf("vessels payload = %+v", list)
	}

	// POST /call dispatches synchronously and waits for the result.
	body := strings.NewReader(`{"agent":"responder","query":"ping"}`)
	resp, err = cli.Post("http://vesseld/v1/vessels/echo/call", "application/json", body)
	if err != nil {
		t.Fatalf("POST call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := readAll(resp.Body)
		t.Fatalf("call status = %d, body = %s\nstderr:\n%s", resp.StatusCode, buf, d.Stderr())
	}

	var call struct {
		RunID    string `json:"run_id"`
		Status   string `json:"status"`
		Error    string `json:"error,omitempty"`
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&call); err != nil {
		t.Fatalf("decode call: %v", err)
	}
	if call.Status != "completed" {
		raw, _ := json.Marshal(call)
		t.Fatalf("status = %q\nfull payload: %s\nmock calls: %d\nstderr:\n%s",
			call.Status, raw, mock.CallCount.Load(), d.Stderr())
	}
	if mock.CallCount.Load() == 0 {
		t.Fatalf("mock OpenAI never called — dispatch path broken")
	}

	// SIGTERM → drain → exit cleanly.
	if err := d.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v\nstderr:\n%s", err, d.Stderr())
	}

	// Socket file should be gone after Stop returns (api/server.go
	// removes it on Shutdown).
	if _, err := os.Stat(d.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("socket %s still present after Stop (err=%v)", d.SocketPath, err)
	}
}

// TestE2E_HealthzAndReadyz validates the two liveness endpoints
// return their advertised semantics: /healthz is unconditional
// 200, /readyz only flips after the fleet finishes Launch.
//
// The current daemon flips ready before the http handler accepts
// any requests (Launch returns sync), so /readyz should already be
// 200 by the time waitForHealthz returned in StartDaemon. This
// test would catch a regression where Launch becomes async without
// a corresponding readiness gate update.
func TestE2E_HealthzAndReadyz(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	cli := d.HTTPClient()
	for _, route := range []string{"/healthz", "/readyz"} {
		resp, err := cli.Get("http://vesseld" + route)
		if err != nil {
			t.Fatalf("%s: %v", route, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d (stderr:\n%s)", route, resp.StatusCode, d.Stderr())
		}
	}
}

// TestE2E_NotFound asserts the errdefs → HTTP status mapping
// survives the binary boundary (regression guard: a future
// rewrite that swaps the api package's writeError shouldn't
// silently start returning 500 for missing-vessel lookups).
func TestE2E_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	resp, err := d.HTTPClient().Get("http://vesseld/v1/vessels/nonexistent/phase")
	if err != nil {
		t.Fatalf("GET phase: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestE2E_SignalDrainTimeout asserts SIGTERM-driven shutdown
// completes well under the daemon's drainTimeout (5s in our
// fixture). helpers.Stop sends SIGTERM and waits for Wait to
// return, so wall-clock here is the actual SIGTERM-to-exit
// latency.
func TestE2E_SignalDrainTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	start := time.Now()
	if err := d.Stop(10 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("shutdown took %s, expected well under 8s", took)
	}
}

// readAll is a tiny helper avoiding the io/ioutil deprecation
// warning while keeping the test file's import list short.
func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), err
		}
	}
}
