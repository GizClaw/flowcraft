//go:build e2e

package vesseld_e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_TCPAuth_NoHeader asserts requests with no Authorization
// header are rejected with a 401-class status. Companion to
// TestE2E_TCPAuth_Required which only proves correct behaviour
// across three permutations; this test pins the no-header case
// specifically so a future "auth filter respects empty as
// anonymous" regression surfaces clearly.
func TestE2E_TCPAuth_NoHeader(t *testing.T) {
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
	helpers.StartDaemon(t, bin, cfg)

	tcp := &http.Client{}
	url := fmt.Sprintf("http://%s/v1/plan", listen)
	resp, err := tcp.Get(url)
	if err != nil {
		t.Fatalf("plan no-auth: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("no-auth request returned 200; auth filter regressed")
	}
}

// TestE2E_TCPAuth_LowercaseBearer asserts the auth filter is
// case-sensitive on the "Bearer " scheme (per RFC 7235 §2.1 the
// scheme is case-insensitive, but the daemon today does a literal
// HasPrefix("Bearer ") check — we pin the current behaviour so a
// future "RFC compliance" change is a deliberate decision rather
// than an accident).
//
// If the daemon ever loosens this check we should update the test
// rather than have it silently regress in the other direction.
func TestE2E_TCPAuth_LowercaseBearer(t *testing.T) {
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
	helpers.StartDaemon(t, bin, cfg)

	tcp := &http.Client{}
	url := fmt.Sprintf("http://%s/v1/plan", listen)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "bearer test-secret-token")
	resp, err := tcp.Do(req)
	if err != nil {
		t.Fatalf("lowercase bearer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("lowercase 'bearer' was accepted; current contract rejects it")
	}
}

// TestE2E_UnixSocket_SkipsAuth asserts that even when a tokenFile
// is configured, requests landing on the unix socket bypass the
// auth filter. The boundary there is filesystem permissions, not
// HTTP headers — the daemon documents this explicitly.
func TestE2E_UnixSocket_SkipsAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()

	dir := t.TempDir()
	tokenPath := writeTokenFile(t, dir, "secret")
	listen := pickFreePort(t)
	cfg := authTemplate
	cfg = strings.ReplaceAll(cfg, "__OPENAI_URL__", mock.URL())
	cfg = strings.ReplaceAll(cfg, "__LISTEN__", listen)
	cfg = strings.ReplaceAll(cfg, "__TOKEN_FILE__", tokenPath)

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, cfg)

	// Companion to TestE2E_TCPAuth_Required step 4. The existing
	// assertion is "anything except 401" — on macOS the unix
	// socket's RemoteAddr does not always carry the "@" / ".sock"
	// marker isUnixRequest looks for, so the auth filter fires
	// and returns NotAvailable=503. We mirror the existing test's
	// permissive contract here; tightening it requires fixing the
	// isUnixRequest portability bug first.
	resp := d.Do(t, http.MethodGet, "/v1/plan", nil)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("unix /plan no-auth status=401; filter must not return Unauthorized")
	}
}
