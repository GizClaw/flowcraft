package netpolicy

import (
	"net"
	"net/netip"
	"testing"
)

// decideProxy builds a Proxy whose rule set is validated exactly as
// Start would validate it, so the tests exercise realistic policies
// without binding a listener or resolving anything.
func decideProxy(t *testing.T, cfg ProxyConfig) *Proxy {
	t.Helper()
	if err := (NetPolicy{
		Mode:       cfg.Mode,
		AllowHosts: cfg.AllowHosts,
		Rules:      cfg.Rules,
		Proxy:      cfg.Upstream,
	}).Validate(); err != nil {
		t.Fatalf("policy %+v does not validate: %v", cfg.Rules, err)
	}
	return &Proxy{cfg: cfg}
}

// TestDecideAddrsPinsTheAllowedAddress is the regression test for the
// DNS-order bypass: when the answer carries an address the rule set
// does not cover ahead of the one an allow rule covers, the dial must
// go to the validated address — not to whatever the resolver listed
// first.
func TestDecideAddrsPinsTheAllowedAddress(t *testing.T) {
	proxy := decideProxy(t, ProxyConfig{
		Mode: NetAllowList,
		Rules: []NetRule{
			{Action: NetAllow, Host: "allowed.example"},
			{Action: NetAllow, Host: "93.184.216.34/32"},
		},
	})
	addrs := []netip.Addr{
		netip.MustParseAddr("10.0.0.5"),      // uncovered, listed first
		netip.MustParseAddr("93.184.216.34"), // covered by the allow rule
	}

	action, rule, reason, dialHost := proxy.decideAddrs(443, "allowed.example:443", addrs,
		hostVerdict{matched: true, rule: "allow allowed.example"})
	if action != NetAllow {
		t.Fatalf("action = %v, want allow", action)
	}
	if dialHost != "93.184.216.34:443" {
		t.Fatalf("dial target = %q, want 93.184.216.34:443", dialHost)
	}
	if rule != "allow allowed.example" || reason != "" {
		t.Fatalf("decisive rule = (%q, %q), want the hostname allow rule", rule, reason)
	}
}

// TestDecideAddrsDenyBeatsEveryAddress proves a deny match anywhere in
// the answer denies the destination: the dial would otherwise pick an
// allowed sibling and hide the denial.
func TestDecideAddrsDenyBeatsEveryAddress(t *testing.T) {
	proxy := decideProxy(t, ProxyConfig{
		Mode: NetAllowList,
		Rules: []NetRule{
			{Action: NetAllow, Host: "allowed.example"},
			{Action: NetDeny, Host: "10.0.0.0/8", Reason: "metadata range"},
		},
	})
	addrs := []netip.Addr{
		netip.MustParseAddr("93.184.216.34"),
		netip.MustParseAddr("10.0.0.5"),
	}

	action, rule, reason, _ := proxy.decideAddrs(443, "allowed.example:443", addrs,
		hostVerdict{matched: true, rule: "allow allowed.example"})
	if action != NetDeny {
		t.Fatalf("action = %v, want deny", action)
	}
	if reason != "metadata range" {
		t.Fatalf("reason = %q, want the deny rule's reason", reason)
	}
	if rule == "" {
		t.Fatal("deny verdict carries no decisive rule")
	}
}

// TestDecideAddrsUncoveredAnswerPinsFirstAddress covers the policy
// where the operator declared only deny rules: nothing in the rule set
// constrains the host's addresses, so the hostname rule decides and the
// first address is pinned — resolving once, dialing a checked address,
// never re-resolving at dial time.
func TestDecideAddrsUncoveredAnswerPinsFirstAddress(t *testing.T) {
	proxy := decideProxy(t, ProxyConfig{
		Mode: NetAllowList,
		Rules: []NetRule{
			{Action: NetAllow, Host: "allowed.example"},
			{Action: NetDeny, Host: "10.0.0.0/8"},
		},
	})
	addrs := []netip.Addr{
		netip.MustParseAddr("93.184.216.34"),
		netip.MustParseAddr("2001:db8::1"),
	}

	action, rule, _, dialHost := proxy.decideAddrs(443, "allowed.example:443", addrs,
		hostVerdict{matched: true, rule: "allow allowed.example"})
	if action != NetAllow {
		t.Fatalf("action = %v, want allow", action)
	}
	if dialHost != "93.184.216.34:443" {
		t.Fatalf("dial target = %q, want the first answer address", dialHost)
	}
	if rule != "allow allowed.example" {
		t.Fatalf("rule = %q, want the hostname rule", rule)
	}
}

// TestDecideAddrsIPRuleAloneAllowsAndNames itself covers the audit
// trail: with no hostname rule matched, an address an allow rule covers
// is what decides, and the record says so.
func TestDecideAddrsIPRuleAloneAllowsAndNames(t *testing.T) {
	proxy := decideProxy(t, ProxyConfig{
		Mode:  NetAllowList,
		Rules: []NetRule{{Action: NetAllow, Host: "93.184.216.34:443"}},
	})
	addrs := []netip.Addr{
		netip.MustParseAddr("203.0.113.9"),
		netip.MustParseAddr("93.184.216.34"),
	}

	action, rule, _, dialHost := proxy.decideAddrs(443, "cdn.example:443", addrs, hostVerdict{})
	if action != NetAllow {
		t.Fatalf("action = %v, want allow", action)
	}
	if rule != "allow 93.184.216.34:443" {
		t.Fatalf("rule = %q, want the allow rule that covered the pinned address", rule)
	}
	if dialHost != "93.184.216.34:443" {
		t.Fatalf("dial target = %q, want the covered address", dialHost)
	}

	// The same answer on a port no rule covers falls back to the mode
	// default, which allow-list mode reads as a denial.
	action, _, _, _ = proxy.decideAddrs(8443, "cdn.example:8443", addrs, hostVerdict{})
	if action != NetDeny {
		t.Fatalf("action on uncovered port = %v, want deny", action)
	}
}

// TestDecideAddrsProxyModeDelegatesToUpstream proves NetProxy keeps its
// mode default: an address no rule covers is delegated, not denied.
func TestDecideAddrsProxyModeDelegatesToUpstream(t *testing.T) {
	proxy := decideProxy(t, ProxyConfig{
		Mode:     NetProxy,
		Upstream: "http://127.0.0.1:3128",
		Rules:    []NetRule{{Action: NetDeny, Host: "10.0.0.0/8"}},
	})
	addrs := []netip.Addr{netip.MustParseAddr("93.184.216.34")}

	action, _, _, dialHost := proxy.decideAddrs(443, "any.example:443", addrs, hostVerdict{})
	if action != NetAllow {
		t.Fatalf("action = %v, want allow by mode default", action)
	}
	if dialHost != "93.184.216.34:443" {
		t.Fatalf("dial target = %q, want the pinned address", dialHost)
	}
}

// TestDecideAddrsPinsIPv6BracketedForm guards the join: an IPv6 pin
// must render as [addr]:port, or the dial target parses as a bare host.
func TestDecideAddrsPinsIPv6BracketedForm(t *testing.T) {
	proxy := decideProxy(t, ProxyConfig{
		Mode:  NetAllowList,
		Rules: []NetRule{{Action: NetAllow, Host: "2001:db8::/32"}},
	})
	// A v4-mapped answer is unmapped before matching, so the rule above
	// only covers the real v6 entry.
	addrs := []netip.Addr{
		netip.MustParseAddr("::ffff:93.184.216.34"),
		netip.MustParseAddr("2001:db8::1"),
	}

	action, _, _, dialHost := proxy.decideAddrs(443, "v6.example:443", addrs, hostVerdict{})
	if action != NetAllow {
		t.Fatalf("action = %v, want allow", action)
	}
	if dialHost != "[2001:db8::1]:443" {
		t.Fatalf("dial target = %q, want [2001:db8::1]:443", dialHost)
	}
}

// TestDecideIPLiteralUnchanged pins the IP-literal branch of decide:
// rules are evaluated against the literal, and allow-list mode denies
// what they do not cover.
func TestDecideIPLiteralUnchanged(t *testing.T) {
	proxy := decideProxy(t, ProxyConfig{
		Mode:  NetAllowList,
		Rules: []NetRule{{Action: NetAllow, Host: "93.184.216.34"}},
	})

	action, _, _, dialHost, err := proxy.decide(t.Context(), "93.184.216.34:443", 443)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if action != NetAllow || dialHost != "93.184.216.34:443" {
		t.Fatalf("allowed literal = (%v, %q), want (allow, 93.184.216.34:443)", action, dialHost)
	}

	action, _, _, _, err = proxy.decide(t.Context(), "203.0.113.9:443", 443)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if action != NetDeny {
		t.Fatalf("uncovered literal = %v, want deny", action)
	}
}

// TestDecideCarriesTheHostnameVerdictPastResolution pins the decide()
// assembly the decideAddrs unit tests cannot see: when IP rules force a
// local resolution, a matched hostname rule must survive the address
// pass as the decisive rule, and the dial target must be an address the
// resolution actually returned.
func TestDecideCarriesTheHostnameVerdictPastResolution(t *testing.T) {
	if _, err := net.DefaultResolver.LookupNetIP(t.Context(), "ip", "localhost"); err != nil {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	proxy := decideProxy(t, ProxyConfig{
		Mode: NetAllowList,
		Rules: []NetRule{
			{Action: NetAllow, Host: "localhost"},
			{Action: NetDeny, Host: "10.0.0.0/8"},
		},
	})

	action, rule, reason, dialHost, err := proxy.decide(t.Context(), "localhost:443", 443)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if action != NetAllow || rule != "allow localhost" || reason != "" {
		t.Fatalf("verdict = (%v, %q, %q), want (allow, allow localhost, \"\")", action, rule, reason)
	}
	host, _, err := net.SplitHostPort(dialHost)
	if err != nil {
		t.Fatalf("dial target %q carries no port: %v", dialHost, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		t.Fatalf("dial target = %q, want a resolved loopback address", dialHost)
	}
}

// TestDecideAppliesIPRulesToResolvedAddrs runs the whole hostname path
// end to end: with no hostname rule deciding, the local resolution has
// to feed the IP/CIDR rules, and the dial target must be pinned to the
// address an allow rule covers — never to an uncovered sibling.
func TestDecideAppliesIPRulesToResolvedAddrs(t *testing.T) {
	addrs, err := net.DefaultResolver.LookupNetIP(t.Context(), "ip", "localhost")
	if err != nil {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	hasV4Loopback := false
	for _, addr := range addrs {
		if addr.Unmap() == netip.MustParseAddr("127.0.0.1") {
			hasV4Loopback = true
		}
	}
	if !hasV4Loopback {
		t.Skip("localhost does not resolve to 127.0.0.1 here")
	}

	proxy := decideProxy(t, ProxyConfig{
		Mode:  NetAllowList,
		Rules: []NetRule{{Action: NetAllow, Host: "127.0.0.1/32"}},
	})
	action, rule, reason, dialHost, err := proxy.decide(t.Context(), "localhost:443", 443)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if action != NetAllow || dialHost != "127.0.0.1:443" {
		t.Fatalf("decide = (%v, %q), want (allow, 127.0.0.1:443)", action, dialHost)
	}
	if rule != "allow 127.0.0.1/32" || reason != "" {
		t.Fatalf("decisive rule = (%q, %q), want the covering allow rule", rule, reason)
	}
}
