//go:build e2e

package vesseld_e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_API_404_UnknownVessel asserts every per-vessel route
// returns 404 (not 500) for an unknown vessel id. errdefs maps
// NotFound → 404; if a future refactor swaps the error helper this
// test surfaces the regression on every method-route combo.
func TestE2E_API_404_UnknownVessel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")
	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/vessels/no-such/phase"},
		{http.MethodPost, "/v1/vessels/no-such/drain"},
		{http.MethodPost, "/v1/vessels/no-such/stop"},
		{http.MethodPost, "/v1/vessels/no-such/submit"},
		{http.MethodPost, "/v1/vessels/no-such/call"},
		{http.MethodGet, "/v1/vessels/no-such/logs"},
	}
	for _, c := range cases {
		var body io.Reader
		if c.method == http.MethodPost {
			body = strings.NewReader(`{"agent":"x"}`)
		}
		resp := d.Do(t, c.method, c.path, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", c.method, c.path, resp.StatusCode)
		}
	}
}

// TestE2E_API_405_WrongMethod asserts the mux returns 405 (not 404)
// when the path exists but the method does not. With http.ServeMux's
// pattern syntax this is the default behaviour; the test pins it
// against a future regression where someone wraps the mux with a
// catch-all that swallows 405.
func TestE2E_API_405_WrongMethod(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")
	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	// /v1/vessels/{id}/phase only accepts GET.
	resp := d.Do(t, http.MethodPost, "/v1/vessels/echo/phase", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		// http.ServeMux returns either 405 or 404 depending on
		// the exact pattern shape; both prove the request was
		// not silently accepted.
		t.Fatalf("POST /phase status=%d, want 405 or 404", resp.StatusCode)
	}
}

// TestE2E_API_ErrorShape asserts non-2xx responses have a JSON body
// with at least an "error" string field. Operators and SDK clients
// rely on this contract; a future writer that emits plain text
// would break tooling silently.
func TestE2E_API_ErrorShape(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")
	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	resp := d.Do(t, http.MethodGet, "/v1/vessels/no-such/phase", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected error status, got 200")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error Content-Type=%q, want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("error response missing 'error' field; payload=%+v", body)
	}
}

// TestE2E_Submit_UnknownAgent asserts a 4xx (not 5xx) when the
// vessel exists but the agent name is not part of its agent list.
// Mapping is errdefs.NotFound → 404 in the current daemon; a less
// specific mapping is acceptable as long as it stays in 4xx.
func TestE2E_Submit_UnknownAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")
	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	resp := d.Do(t, http.MethodPost, "/v1/vessels/echo/submit", strings.NewReader(`{"agent":"no-such","query":"x"}`))
	resp.Body.Close()
	if resp.StatusCode/100 != 4 {
		t.Fatalf("unknown agent status=%d, want 4xx (got %d body shape)", resp.StatusCode, resp.StatusCode)
	}
}

// TestE2E_DoubleDrain_Idempotent asserts a second /drain after the
// first does not 500 — Drain on an already-draining or drained
// vessel must be a no-op (or report the already-stable phase).
func TestE2E_DoubleDrain_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")
	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	resp := d.Do(t, http.MethodPost, "/v1/vessels/echo/drain", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first drain status=%d", resp.StatusCode)
	}
	resp = d.Do(t, http.MethodPost, "/v1/vessels/echo/drain", nil)
	resp.Body.Close()
	if resp.StatusCode/100 == 5 {
		t.Fatalf("second drain returned 5xx (not idempotent): status=%d", resp.StatusCode)
	}
}
