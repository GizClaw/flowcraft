package net

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Matcher evaluates NetPolicy rules against a destination.
// Rules are compiled once at construction: hostnames are normalized
// (lowercase, trailing dot removed, IDNA → punycode) and IP/CIDR
// entries are parsed so Match / MatchIP stay pure and IO-free.
//
// Semantics:
//
//   - "example.com": the bare domain and every subdomain
//     (domain-and-descendants, matching the legacy AllowHosts
//     behaviour).
//   - "example.com:443" / "[::1]:8443": an optional ":port" suffix
//     restricts the rule to one destination port; entries without a
//     suffix match any port (same grammar as srt's allowedDomains).
//   - "*.example.com": subdomains only, any depth; never the bare
//     domain.
//   - "1.2.3.4" / "10.0.0.0/8": IP / CIDR, evaluated by MatchIP
//     against locally resolved addresses.
//   - Deny rules are evaluated first across the whole rule set; a
//     single deny match wins over any allow.
//   - Port 0 matches any port.
//
// AllowHosts is compiled as trailing allow rules, so explicit deny
// rules always take precedence over the legacy allow-list.
type Matcher struct {
	rules      []compiledRule
	hasIPRules bool
}

type compiledRule struct {
	action NetAction
	port   int
	desc   string
	reason string
	host   *Pattern
}

// NewMatcher compiles policy into a Matcher. The policy should already
// have passed NetPolicy.Validate; this constructor re-checks the rule
// forms it needs and returns Validation errors for malformed hosts.
func NewMatcher(policy NetPolicy) (*Matcher, error) {
	m := &Matcher{}
	for i, rule := range policy.Rules {
		c, err := compileRule(rule)
		if err != nil {
			return nil, errdefs.Validationf("sandbox: net rule %d: %v", i, err)
		}
		if c.host.IsIP() {
			m.hasIPRules = true
		}
		m.rules = append(m.rules, c)
	}
	// Legacy allow-list entries become allow rules appended after the
	// explicit rules, preserving "explicit deny wins" while keeping
	// old configurations byte-for-byte equivalent.
	for _, host := range policy.AllowHosts {
		c, err := compileRule(NetRule{Action: NetAllow, Host: host})
		if err != nil {
			return nil, errdefs.Validationf("sandbox: allow_hosts entry %q: %v", host, err)
		}
		if c.host.IsIP() {
			m.hasIPRules = true
		}
		m.rules = append(m.rules, c)
	}
	return m, nil
}

// HasIPRules reports whether any rule needs local DNS resolution
// (exact IP or CIDR entries).
func (m *Matcher) HasIPRules() bool { return m.hasIPRules }

// Match evaluates hostname rules for host:port. It never resolves DNS;
// callers apply IP/CIDR rules separately via MatchIP. The returned
// rule string describes the decisive rule ("" when nothing matched)
// and reason is the optional model-facing explanation attached to a
// deny rule.
func (m *Matcher) Match(host string, port int) (NetAction, string, string, bool) {
	host = normalizeHost(host)
	for _, r := range m.rules {
		if r.host.IsIP() || !r.host.Match(host) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		if r.action == NetDeny {
			return NetDeny, r.desc, r.reason, true
		}
	}
	for _, r := range m.rules {
		if r.host.IsIP() || !r.host.Match(host) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		return NetAllow, r.desc, r.reason, true
	}
	return NetAllow, "", "", false
}

// MatchIP evaluates IP/CIDR rules against one resolved address.
func (m *Matcher) MatchIP(ip netip.Addr, port int) (NetAction, string, string, bool) {
	for _, r := range m.rules {
		if !r.host.MatchIP(ip) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		if r.action == NetDeny {
			return NetDeny, r.desc, r.reason, true
		}
	}
	for _, r := range m.rules {
		if !r.host.MatchIP(ip) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		return NetAllow, r.desc, r.reason, true
	}
	return NetAllow, "", "", false
}

func compileRule(rule NetRule) (compiledRule, error) {
	host, port, err := splitRuleHostPort(rule.Host, rule.Port)
	if err != nil {
		return compiledRule{}, err
	}
	c := compiledRule{action: rule.Action, port: port, reason: rule.Reason}
	pattern, err := Compile(host)
	if err != nil {
		return c, err
	}
	c.host = pattern
	c.desc = fmt.Sprintf("%s %s", c.action, host)
	if c.port != 0 {
		c.desc += fmt.Sprintf(":%d", c.port)
	}
	return c, nil
}

// splitRuleHostPort parses an optional ":port" suffix on a rule host
// ("example.com:443", "[::1]:8443", "10.0.0.0/8:53"). Bare hosts and
// bare IPv6 literals without brackets fall through unchanged; a
// malformed numeric port is a validation error. An explicit suffix
// overrides the rule's Port field.
func splitRuleHostPort(host string, port int) (string, int, error) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return "", 0, fmt.Errorf("invalid port suffix %q", host)
		}
		return h, n, nil
	}
	return host, port, nil
}

func portMatches(rulePort, port int) bool {
	return rulePort == 0 || rulePort == port
}

// normalizeHost mirrors hostmatch's normalization for request-side
// lookups (no IDNA failure surfacing — mismatches fall through to the
// mode default).
func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
