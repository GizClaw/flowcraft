//go:build linux

package bridge

import (
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsBridgeInvocation(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"marker", []string{"/usr/bin/flowcraft", Marker, "--sock", "x"}, true},
		{"marker plus command", []string{"/usr/bin/flowcraft", Marker, "--sock", "x", "--", "/bin/true"}, true},
		{"no args", nil, false},
		{"one arg", []string{"/usr/bin/flowcraft"}, false},
		{"ordinary flag", []string{"/usr/bin/flowcraft", "--serve"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBridgeInvocation(tc.argv); got != tc.want {
				t.Errorf("isBridgeInvocation(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestChildEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://host-proxy:3128")
	t.Setenv("NO_PROXY", "localhost")
	t.Setenv("KEEP_ME", "kept")
	env := childEnv(43123)

	got := make(map[string]string)
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		got[name] = value
	}
	if got["KEEP_ME"] != "kept" {
		t.Errorf("non-proxy env dropped: %v", got)
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		if got[name] != "http://127.0.0.1:43123" {
			t.Errorf("%s = %q, want bridge loopback", name, got[name])
		}
	}
	if got["NO_PROXY"] != "" {
		t.Errorf("NO_PROXY not stripped: %q", got["NO_PROXY"])
	}
}

func TestRun_InjectsProxyEnv(t *testing.T) {
	var out, errb strings.Builder
	code := run(
		[]string{"--sock", "/nonexistent.sock", "--", "/usr/bin/env"},
		strings.NewReader(""), &out, &errb,
	)
	if code != 0 {
		t.Fatalf("run env: exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{
		"HTTP_PROXY=http://127.0.0.1:", "HTTPS_PROXY=http://127.0.0.1:", "ALL_PROXY=http://127.0.0.1:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("env missing %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "\nNO_PROXY=\n") {
		t.Errorf("NO_PROXY not stripped:\n%s", out.String())
	}
}

func TestRun_PropagatesExitCode(t *testing.T) {
	var out, errb strings.Builder
	code := run(
		[]string{"--sock", "/nonexistent.sock", "--", "/bin/sh", "-c", "exit 7"},
		strings.NewReader(""), &out, &errb,
	)
	if code != 7 {
		t.Errorf("expected exit 7, got %d (stderr=%q)", code, errb.String())
	}
}

func TestRun_ForwardsTCPOverUDS(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not on PATH: %v", err)
	}

	// Fake host proxy: a unix listener that echoes bytes back.
	dir := t.TempDir()
	sock := filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}()

	// Child connects to HTTP_PROXY (the bridge's loopback port) and
	// expects the echo.
	script := `
import os, socket
host, port = os.environ["HTTP_PROXY"].split("//")[1].split(":")
s = socket.create_connection((host, int(port)))
s.sendall(b"ping")
print(s.recv(100).decode())
`
	var out, errb strings.Builder
	code := run(
		[]string{"--sock", sock, "--", "/usr/bin/python3", "-c", script},
		strings.NewReader(""), &out, &errb,
	)
	if code != 0 {
		t.Fatalf("run bridge: exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ping") {
		t.Errorf("forwarded echo missing, out=%q", out.String())
	}
}

// TestRun_RequiresSockAndCommand guards the usage contract that the
// runner always supplies --sock and a command after "--".
func TestRun_RequiresSockAndCommand(t *testing.T) {
	var out, errb strings.Builder
	for _, argv := range [][]string{
		{"--sock", "/x.sock"},
		{"--", "/bin/true"},
		nil,
	} {
		code := run(argv, strings.NewReader(""), &out, &errb)
		if code == 0 {
			t.Errorf("run(%v) expected usage error, got 0", argv)
		}
	}
}
