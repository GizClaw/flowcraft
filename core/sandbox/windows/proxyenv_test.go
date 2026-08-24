package windows

import (
	"strings"
	"testing"
)

func TestInjectProxyEnv(t *testing.T) {
	env := injectProxyEnv([]string{"PATH=C:\\bin", "NO_PROXY=proxy.internal"}, 4321)
	values := map[string]string{}
	for _, kv := range env {
		if k, v, ok := splitKV(kv); ok {
			values[k] = v
		}
	}
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "ALL_PROXY"} {
		if values[name] != "http://127.0.0.1:4321" {
			t.Fatalf("%s = %q, want proxy url", name, values[name])
		}
	}
	if values["NO_PROXY"] != "" {
		t.Fatalf("NO_PROXY = %q, want empty", values["NO_PROXY"])
	}
	if strings.Contains(env[0], "proxy.internal") {
		t.Fatalf("old NO_PROXY leaked: %v", env)
	}
}

func TestInjectProxyEnvPassthrough(t *testing.T) {
	env := []string{"A=1"}
	if got := injectProxyEnv(env, 0); len(got) != 1 || got[0] != "A=1" {
		t.Fatalf("proxyPort 0 changed env: %v", got)
	}
}
