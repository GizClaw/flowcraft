package windows

import (
	"fmt"
	"strings"
)

// injectProxyEnv forces the child environment onto the host-side
// enforcement proxy's loopback address, mirroring the seatbelt
// backend: HTTP(S)_PROXY / ALL_PROXY point at 127.0.0.1:<port> and
// NO_PROXY is emptied, so proxy-aware clients use the single enforced
// egress. WFP confines the offline account to loopback, making this
// the only route out.
func injectProxyEnv(env []string, proxyPort int) []string {
	if proxyPort <= 0 {
		return env
	}
	values := map[string]string{}
	for _, kv := range env {
		if key, value, ok := splitKV(kv); ok {
			values[key] = value
		}
	}
	proxy := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	for _, name := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		values[name] = proxy
	}
	delete(values, "NO_PROXY")
	delete(values, "no_proxy")
	values["NO_PROXY"] = ""
	values["no_proxy"] = ""
	out := make([]string, 0, len(values))
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func splitKV(kv string) (string, string, bool) {
	index := strings.IndexByte(kv, '=')
	if index <= 0 {
		return "", "", false
	}
	return kv[:index], kv[index+1:], true
}
