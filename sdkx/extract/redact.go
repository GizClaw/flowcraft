package extract

import (
	"net/url"
	"strings"
)

// redactURL returns a log-safe form of raw (scheme + host only)
// so userinfo, query parameters, paths, and fragments never leak.
func redactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Host == "" && !strings.Contains(raw, "://") {
		u, err = url.Parse("//" + raw)
		if err != nil {
			return ""
		}
	}
	if u.Host == "" {
		return ""
	}
	if u.Scheme == "" {
		return u.Host
	}
	return u.Scheme + "://" + u.Host
}
