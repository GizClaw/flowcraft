//go:build darwin

package seatbelt

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// TestBuildEnvProxySplit verifies allow-list / proxy modes inject
// HTTP_PROXY (http://) and ALL_PROXY (socks5://) to the same loopback
// port and strip NO_PROXY.
func TestBuildEnvProxySplit(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://corp:1")
	t.Setenv("HTTPS_PROXY", "http://corp:1")
	t.Setenv("ALL_PROXY", "http://corp:1")
	t.Setenv("NO_PROXY", "corp")
	t.Setenv("KEEP", "1")

	env := buildEnv(sandbox.EnvPolicy{Allow: []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "KEEP",
	}}, 12345)
	got := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("bad env entry %q", kv)
		}
		got[k] = v
	}
	if got["HTTP_PROXY"] != "http://127.0.0.1:12345" {
		t.Errorf("HTTP_PROXY = %q, want http://127.0.0.1:12345", got["HTTP_PROXY"])
	}
	if got["ALL_PROXY"] != "socks5://127.0.0.1:12345" {
		t.Errorf("ALL_PROXY = %q, want socks5://127.0.0.1:12345", got["ALL_PROXY"])
	}
	if got["NO_PROXY"] != "" {
		t.Errorf("NO_PROXY = %q, want empty", got["NO_PROXY"])
	}
	if got["KEEP"] != "1" {
		t.Errorf("KEEP = %q, want 1", got["KEEP"])
	}
}
