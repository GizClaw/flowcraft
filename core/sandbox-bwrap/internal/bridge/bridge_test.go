//go:build linux

package bridge

import (
	"strings"
	"testing"
)

// TestChildEnvProxySplit verifies the bridge injects HTTP_PROXY as an
// http:// URL and ALL_PROXY as a socks5:// URL on the same loopback
// port, strips NO_PROXY, and preserves every other variable.
func TestChildEnvProxySplit(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://corp:1")
	t.Setenv("HTTPS_PROXY", "http://corp:1")
	t.Setenv("ALL_PROXY", "http://corp:1")
	t.Setenv("NO_PROXY", "corp")
	t.Setenv("KEEP", "1")

	env := childEnv(32123)
	got := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("bad env entry %q", kv)
		}
		got[k] = v
	}
	if got["HTTP_PROXY"] != "http://127.0.0.1:32123" {
		t.Errorf("HTTP_PROXY = %q, want http://127.0.0.1:32123", got["HTTP_PROXY"])
	}
	if got["HTTPS_PROXY"] != "http://127.0.0.1:32123" {
		t.Errorf("HTTPS_PROXY = %q, want http://127.0.0.1:32123", got["HTTPS_PROXY"])
	}
	if got["ALL_PROXY"] != "socks5://127.0.0.1:32123" {
		t.Errorf("ALL_PROXY = %q, want socks5://127.0.0.1:32123", got["ALL_PROXY"])
	}
	if got["NO_PROXY"] != "" {
		t.Errorf("NO_PROXY = %q, want empty", got["NO_PROXY"])
	}
	if got["KEEP"] != "1" {
		t.Errorf("KEEP = %q, want 1", got["KEEP"])
	}
	// os.Environ() is read inside childEnv; the t.Setenv values above
	// must not leak into the returned list as stale proxy entries.
	for _, kv := range env {
		if strings.HasPrefix(kv, "HTTP_PROXY=http://corp") ||
			strings.HasPrefix(kv, "NO_PROXY=corp") {
			t.Errorf("stale env entry %q", kv)
		}
	}
}
