package httpkit

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/GizClaw/flowcraft/backends/sandbox/internal/hostmatch"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Matcher evaluates sandbox.NetPolicy rules against a destination.
// Rules are compiled once at construction: hostnames are normalized
// (lowercase, trailing dot removed, IDNA → punycode) and IP/CIDR
// entries are parsed so Match / MatchIP stay pure and IO-free.
//
// Semantics:
//
//   - "example.com": the bare domain and every subdomain
//     (domain-and-descendants, matching the legacy AllowHosts
//     behaviour).
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
	action sandbox.NetAction
	port   int
	desc   string
	host   *hostmatch.Pattern
}

// NewMatcher compiles policy into a Matcher. The policy should already
// have passed NetPolicy.Validate; this constructor re-checks the rule
// forms it needs and returns Validation errors for malformed hosts.
func NewMatcher(policy sandbox.NetPolicy) (*Matcher, error) {
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
		c, err := compileRule(sandbox.NetRule{Action: sandbox.NetAllow, Host: host})
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
// rule string describes the decisive rule ("" when nothing matched).
func (m *Matcher) Match(host string, port int) (sandbox.NetAction, string, bool) {
	host = normalizeHost(host)
	for _, r := range m.rules {
		if r.host.IsIP() || !r.host.Match(host) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		if r.action == sandbox.NetDeny {
			return sandbox.NetDeny, r.desc, true
		}
	}
	for _, r := range m.rules {
		if r.host.IsIP() || !r.host.Match(host) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		return sandbox.NetAllow, r.desc, true
	}
	return sandbox.NetAllow, "", false
}

// MatchIP evaluates IP/CIDR rules against one resolved address.
func (m *Matcher) MatchIP(ip netip.Addr, port int) (sandbox.NetAction, string, bool) {
	for _, r := range m.rules {
		if !r.host.MatchIP(ip) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		if r.action == sandbox.NetDeny {
			return sandbox.NetDeny, r.desc, true
		}
	}
	for _, r := range m.rules {
		if !r.host.MatchIP(ip) {
			continue
		}
		if !portMatches(r.port, port) {
			continue
		}
		return sandbox.NetAllow, r.desc, true
	}
	return sandbox.NetAllow, "", false
}

func compileRule(rule sandbox.NetRule) (compiledRule, error) {
	c := compiledRule{action: rule.Action, port: rule.Port}
	pattern, err := hostmatch.Compile(rule.Host)
	if err != nil {
		return c, err
	}
	c.host = pattern
	c.desc = fmt.Sprintf("%s %s", c.action, rule.Host)
	if c.port != 0 {
		c.desc += fmt.Sprintf(":%d", c.port)
	}
	return c, nil
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
