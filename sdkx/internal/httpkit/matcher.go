package httpkit

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"golang.org/x/net/idna"
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

	host   string // normalized hostname (domain / wildcard base)
	wild   bool
	addr   netip.Addr
	prefix netip.Prefix
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
		if c.addr.IsValid() || c.prefix.IsValid() {
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
		if c.addr.IsValid() || c.prefix.IsValid() {
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
		if r.addr.IsValid() || r.prefix.IsValid() || !hostMatches(r, host) {
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
		if r.addr.IsValid() || r.prefix.IsValid() || !hostMatches(r, host) {
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
		if !ipMatches(r, ip) {
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
		if !ipMatches(r, ip) {
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
	host := strings.TrimSpace(rule.Host)
	if host == "" {
		return c, fmt.Errorf("empty host")
	}
	if strings.Contains(host, "*") && !strings.HasPrefix(host, "*.") {
		return c, fmt.Errorf("host %q: \"*\" is only allowed as a leading \"*.\" wildcard", rule.Host)
	}
	c.wild = strings.HasPrefix(host, "*.")
	if c.wild {
		host = strings.TrimPrefix(host, "*.")
		if host == "" || strings.Contains(host, "*") {
			return c, fmt.Errorf("wildcard host %q must have form \"*.example.com\"", rule.Host)
		}
	}
	host = normalizeHost(host)
	c.host = host

	if !c.wild {
		if addr, err := netip.ParseAddr(host); err == nil {
			c.addr = addr.Unmap()
			c.desc = fmt.Sprintf("%s %s", c.action, c.addr.String())
		} else if prefix, err := netip.ParsePrefix(host); err == nil {
			c.prefix = prefix.Masked()
			c.addr = netip.Addr{} // prefix rules use prefix only
			c.desc = fmt.Sprintf("%s %s", c.action, c.prefix.String())
		}
	}
	if c.desc == "" {
		pattern := c.host
		if c.wild {
			pattern = "*." + c.host
		}
		c.desc = fmt.Sprintf("%s %s", c.action, pattern)
	}
	if c.port != 0 {
		c.desc += fmt.Sprintf(":%d", c.port)
	}
	return c, nil
}

func hostMatches(r compiledRule, host string) bool {
	if r.wild {
		return strings.HasSuffix(host, "."+r.host)
	}
	return host == r.host || strings.HasSuffix(host, "."+r.host)
}

func ipMatches(r compiledRule, ip netip.Addr) bool {
	ip = ip.Unmap()
	if r.addr.IsValid() {
		return ip == r.addr
	}
	if r.prefix.IsValid() {
		return r.prefix.Contains(ip)
	}
	return false
}

func portMatches(rulePort, port int) bool {
	return rulePort == 0 || rulePort == port
}

// normalizeHost lowercases, strips the trailing dot, and converts
// unicode hostnames to punycode. IDNA failures leave the input
// unchanged so the proxy can still reject it via the allow-list
// default; rules themselves are normalized at compile time.
func normalizeHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if ascii := isASCII(host); ascii {
		return host
	}
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		return ascii
	}
	return host
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
