package net

import (
	"net/netip"
	"testing"
)

// TestMatcherPortSuffixAndReason covers the srt-style ":port" suffix
// grammar on both AllowHosts entries and explicit rules, plus
// model-facing deny reasons.
func TestMatcherPortSuffixAndReason(t *testing.T) {
	m, err := NewMatcher(NetPolicy{
		Mode: NetAllowList,
		AllowHosts: []string{
			"example.com:443",
			"[::1]:8443",
			"10.0.0.0/8:53",
			"bare.example.com",
		},
		Rules: []NetRule{
			{Action: NetDeny, Host: "evil.example", Reason: "known malware domain"},
			{Action: NetDeny, Host: "example.com", Port: 22, Reason: "ssh egress is blocked"},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	action, rule, reason, matched := m.Match("example.com", 443)
	if !matched || action != NetAllow || rule != "allow example.com:443" || reason != "" {
		t.Fatalf("example.com:443 = (%v, %q, %q, %v)", action, rule, reason, matched)
	}
	action, _, reason, matched = m.Match("www.example.com", 443)
	if !matched || action != NetAllow || reason != "" {
		t.Fatalf("www.example.com:443 (domain-and-descendants) = (%v, %q, %v)", action, reason, matched)
	}
	action, _, _, matched = m.Match("example.com", 80)
	if matched || action != NetAllow {
		t.Fatalf("example.com:80 should not match the :443 allow rule (matched=%v)", matched)
	}
	action, _, reason, matched = m.Match("example.com", 22)
	if !matched || action != NetDeny || reason != "ssh egress is blocked" {
		t.Fatalf("example.com:22 = (%v, %q, %v)", action, reason, matched)
	}
	action, _, reason, matched = m.Match("evil.example", 80)
	if !matched || action != NetDeny || reason != "known malware domain" {
		t.Fatalf("evil.example:80 = (%v, %q, %v)", action, reason, matched)
	}
	action, _, _, matched = m.Match("bare.example.com", 9999)
	if !matched || action != NetAllow {
		t.Fatalf("bare.example.com:9999 should match with any port")
	}

	action, _, _, matched = m.MatchIP(netip.MustParseAddr("10.1.2.3"), 53)
	if !matched || action != NetAllow {
		t.Fatalf("10.1.2.3:53 should match the CIDR:port rule")
	}
	_, _, _, matched = m.MatchIP(netip.MustParseAddr("10.1.2.3"), 54)
	if matched {
		t.Fatalf("10.1.2.3:54 should not match the CIDR:53 rule")
	}
	action, _, _, matched = m.MatchIP(netip.MustParseAddr("::1"), 8443)
	if !matched || action != NetAllow {
		t.Fatalf("[::1]:8443 should match")
	}
}

// TestMatcherAllowHostsPortSuffixValidation rejects malformed numeric
// ports instead of silently treating them as hostnames.
func TestMatcherAllowHostsPortSuffixValidation(t *testing.T) {
	_, err := NewMatcher(NetPolicy{
		Mode:       NetAllowList,
		AllowHosts: []string{"example.com:not-a-port"},
	})
	if err == nil {
		t.Fatal("NewMatcher accepted a non-numeric port suffix")
	}
}
