//go:build integration_seatbelt && darwin

package seatbelt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// TestIntegrationHelper is re-executed by the MITM integration test
// as the sandboxed child. It is inert unless the helper env is set.
func TestIntegrationHelper(t *testing.T) {
	if os.Getenv("FLOWCRAFT_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("FLOWCRAFT_TEST_HELPER_MODE") {
	case "https-client":
		target := os.Getenv("FLOWCRAFT_TEST_TARGET")
		// Use the proxy explicitly: Go's ProxyFromEnvironment bypasses
		// loopback targets by design, but a real proxy-aware client
		// (curl, browsers) honors HTTPS_PROXY here. The transport still
		// verifies the MITM leaf through SSL_CERT_FILE.
		proxyURL, err := url.Parse(os.Getenv("HTTPS_PROXY"))
		if err != nil {
			t.Fatalf("parse HTTPS_PROXY: %v", err)
		}
		// Load the injected bundle explicitly: on macOS Go's system
		// root store is the Security framework, which both ignores
		// SSL_CERT_FILE and is unreachable under SBPL (empty pool).
		// This is the --cacert behaviour curl would use.
		pool := x509.NewCertPool()
		pemData, err := os.ReadFile(os.Getenv("SSL_CERT_FILE"))
		if err != nil {
			t.Fatalf("read SSL_CERT_FILE: %v", err)
		}
		if !pool.AppendCertsFromPEM(pemData) {
			t.Fatalf("SSL_CERT_FILE contains no usable certificates")
		}
		client := &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}}
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		fmt.Print(string(body))
	}
}

// TestIntegration_MITM_HTTPS is the real-environment MITM assertion:
// a Go client inside the Seatbelt sandbox trusts the injected
// SSL_CERT_FILE bundle, reaches a local HTTPS origin through the
// enforcement proxy's TLS termination, and receives the origin body.
func TestIntegration_MITM_HTTPS(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "mitm-real-ok")
	}))
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	runner, err := New(t.TempDir(), WithOutboundRoots(roots))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := runner.Exec(
		context.Background(),
		os.Args[0],
		[]string{"-test.run", "^TestIntegrationHelper$"},
		sandbox.ExecOptions{
			Timeout: 30 * time.Second,
			Env: sandbox.EnvPolicy{
				Allow: []string{"PATH", "HOME"},
				Inject: map[string]string{
					"FLOWCRAFT_TEST_HELPER":      "1",
					"FLOWCRAFT_TEST_HELPER_MODE": "https-client",
					"FLOWCRAFT_TEST_TARGET":      srv.URL,
				},
			},
			Net: sandbox.NetPolicy{
				Mode:       sandbox.NetAllowList,
				AllowHosts: []string{"127.0.0.1"},
				MITM:       &sandbox.MITMPolicy{Enabled: true},
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "mitm-real-ok") {
		t.Fatalf("MITM response missing origin body: %q", result.Stdout)
	}
}

// TestIntegration_NetDenyAll proves the generated SBPL profile blocks
// even loopback connections at the OS level. A local listener avoids
// coupling the test to internet or DNS availability.
func TestIntegration_NetDenyAll(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/usr/bin/nc",
		[]string{"-z", "-w", "1", "127.0.0.1", strconv.Itoa(port)},
		sandbox.ExecOptions{
			Timeout: 3 * time.Second,
			Net:     sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("NetDenyAll allowed loopback connection to %s: %+v",
			fmt.Sprintf("127.0.0.1:%d", port), result)
	}
}

// TestIntegration_NetDefaultAllowsNetwork proves the generated SBPL
// profile for NetDefault leaves the host network posture intact: a
// loopback connection that NetDenyAll blocks must succeed under
// NetDefault. This is the regression guard for the upstream Codex bug
// (openai/codex#10390) where the Seatbelt profile unconditionally
// disabled network even when network access was requested.
func TestIntegration_NetDefaultAllowsNetwork(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/usr/bin/nc",
		[]string{"-z", "-w", "1", "127.0.0.1", strconv.Itoa(port)},
		sandbox.ExecOptions{
			Timeout: 3 * time.Second,
			Net:     sandbox.NetPolicy{Mode: sandbox.NetDefault},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("NetDefault blocked loopback connection to %s (exit %d): %+v",
			fmt.Sprintf("127.0.0.1:%d", port), result.ExitCode, result)
	}
}

// requireCurl skips the test when curl is not on PATH.
func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skipf("curl not on PATH: %v", err)
	}
}

// TestIntegration_NetAllowListAllowsListedHost proves the loopback
// enforcement proxy + SBPL single-hole profile lets a listed host
// through and delivers the response.
func TestIntegration_NetAllowListAllowsListedHost(t *testing.T) {
	requireCurl(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "allowed-ok")
	}))
	defer srv.Close()

	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/usr/bin/curl",
		[]string{"-s", srv.URL},
		sandbox.ExecOptions{
			Timeout: 15 * time.Second,
			Net: sandbox.NetPolicy{
				Mode:       sandbox.NetAllowList,
				AllowHosts: []string{"127.0.0.1"},
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, result.Stderr)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", result.ExitCode, result.Stderr, result.Stdout)
	}
	if !strings.Contains(result.Stdout, "allowed-ok") {
		t.Errorf("response not delivered through the proxy: stdout=%q", result.Stdout)
	}
}

// TestIntegration_NetAllowListDeniesUnlisted proves a non-listed
// destination fails closed: curl -f turns the proxy's HTTP 403 into a
// non-zero exit, and the response body never reaches the child.
func TestIntegration_NetAllowListDeniesUnlisted(t *testing.T) {
	requireCurl(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "should-not-arrive")
	}))
	defer srv.Close()

	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/usr/bin/curl",
		[]string{"-s", "-f", srv.URL},
		sandbox.ExecOptions{
			Timeout: 15 * time.Second,
			Net: sandbox.NetPolicy{
				Mode:       sandbox.NetAllowList,
				AllowHosts: []string{"example.com"},
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Errorf("unlisted destination must fail, got exit=0 stdout=%q", result.Stdout)
	}
	if strings.Contains(result.Stdout, "should-not-arrive") {
		t.Errorf("denied destination received the response body: %q", result.Stdout)
	}
}

// TestIntegration_NetProxyForwardsToUpstream proves proxy mode routes
// through the configured upstream: the target port is never reachable
// directly, so success means the host proxy forwarded to the upstream.
func TestIntegration_NetProxyForwardsToUpstream(t *testing.T) {
	requireCurl(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "upstream-ok")
	}))
	defer upstream.Close()

	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/usr/bin/curl",
		[]string{"-s", "http://127.0.0.1:1/hello"},
		sandbox.ExecOptions{
			Timeout: 15 * time.Second,
			Net: sandbox.NetPolicy{
				Mode:  sandbox.NetProxy,
				Proxy: upstream.URL,
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v (stderr=%q)", err, result.Stderr)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", result.ExitCode, result.Stderr, result.Stdout)
	}
	if !strings.Contains(result.Stdout, "upstream-ok") {
		t.Errorf("upstream response not delivered: stdout=%q", result.Stdout)
	}
}
