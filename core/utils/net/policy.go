package net

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// NetMode names the network access posture the sandbox should enforce.
type NetMode int

const (
	// NetDefault leaves networking to the host.
	NetDefault NetMode = iota
	// NetDenyAll forbids outbound connections.
	NetDenyAll
	// NetAllowList permits only destinations listed in AllowHosts.
	NetAllowList
	// NetProxy routes traffic through Proxy.
	NetProxy
)

// NetPolicy controls outbound networking for the child process.
type NetPolicy struct {
	Mode NetMode
	// AllowHosts is the legacy allow-list: hostname suffixes and exact
	// IP literals. Entries support an optional ":port" suffix
	// ("api.example.com:443", "[::1]:8443"); entries without a suffix
	// match any port. Prefer Rules for new configurations.
	AllowHosts  []string
	Rules       []NetRule
	Proxy       string
	UnixSockets []string
	MITM        *MITMPolicy
}

// NetAction is the verdict of one network rule.
type NetAction int

const (
	NetDeny NetAction = iota
	NetAllow
)

func (a NetAction) String() string {
	switch a {
	case NetDeny:
		return "deny"
	case NetAllow:
		return "allow"
	default:
		return "unknown"
	}
}

// NetRule is one host/port-level allow or deny rule.
type NetRule struct {
	Action NetAction
	Host   string
	Port   int
	// Reason is an optional model-facing explanation attached to a
	// deny rule. It is surfaced in proxy denial responses and audit
	// records so an agent can see what was blocked and why (mirrors
	// srt's deniedDomainReasons). Allow-rule reasons are ignored.
	Reason string
}

// MITMPolicy enables TLS termination and content hooks for CONNECT
// traffic.
type MITMPolicy struct {
	Enabled       bool
	InspectBodies bool
	MaxBodyBytes  int64
	Hosts         []string
	ExcludeHosts  []string
}

// Validate checks NetPolicy's cross-backend invariants.
func (p NetPolicy) Validate() error {
	if p.Mode < NetDefault || p.Mode > NetProxy {
		return errdefs.Validationf("net: unknown net mode %d", int(p.Mode))
	}
	if p.Proxy != "" {
		u, err := url.Parse(p.Proxy)
		if err != nil {
			return errdefs.Validationf("net: invalid proxy URL %q: %v", p.Proxy, err)
		}
		if u.Scheme != "http" && u.Scheme != "socks5" {
			return errdefs.Validationf("net: proxy scheme %q must be http or socks5", u.Scheme)
		}
		if u.Host == "" {
			return errdefs.Validationf("net: proxy URL %q has no host", p.Proxy)
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword && u.User.Username() == "" {
				return errdefs.Validationf("net: proxy URL %q has a password but no username", p.Proxy)
			}
		}
	}
	for i, rule := range p.Rules {
		if rule.Action != NetDeny && rule.Action != NetAllow {
			return errdefs.Validationf("net: net rule %d has invalid action %d", i, int(rule.Action))
		}
		if strings.TrimSpace(rule.Host) == "" {
			return errdefs.Validationf("net: net rule %d has an empty host", i)
		}
		if rule.Port < 0 || rule.Port > 65535 {
			return errdefs.Validationf("net: net rule %d port %d out of range", i, rule.Port)
		}
		if strings.TrimSpace(rule.Reason) != rule.Reason {
			return errdefs.Validationf("net: net rule %d reason has leading or trailing whitespace", i)
		}
		if strings.ContainsAny(rule.Reason, "\r\n") {
			return errdefs.Validationf("net: net rule %d reason must be a single line", i)
		}
	}
	for _, path := range p.UnixSockets {
		if !filepath.IsAbs(path) {
			return errdefs.Validationf("net: unix socket path %q must be absolute", path)
		}
	}
	if p.MITM != nil {
		if p.MITM.MaxBodyBytes < 0 {
			return errdefs.Validationf("net: mitm.max_body_bytes must be non-negative")
		}
		for i, host := range p.MITM.Hosts {
			if strings.TrimSpace(host) == "" {
				return errdefs.Validationf("net: mitm.hosts[%d] is empty", i)
			}
		}
		for i, host := range p.MITM.ExcludeHosts {
			if strings.TrimSpace(host) == "" {
				return errdefs.Validationf("net: mitm.exclude_hosts[%d] is empty", i)
			}
		}
	}
	return nil
}
