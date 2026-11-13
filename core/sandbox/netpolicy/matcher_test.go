package netpolicy

import (
	"net/netip"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
)

// A single deny wins over any number of allows, wherever it sits in the list.
// This is what "explicit rules override the legacy allow-list" means.
func TestMatcherDenyWins(t *testing.T) {
	m, err := NewMatcher(permission.NetPolicy{
		Mode: permission.NetAllowList,
		Rules: []permission.NetRule{
			{Action: permission.NetAllow, Host: "example.com"},
			{Action: permission.NetAllow, Host: "api.example.com"},
			{Action: permission.NetDeny, Host: "api.example.com", Reason: "no data here"},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	action, rule, reason, matched := m.Match("api.example.com", 443)
	if action != permission.NetDeny || !matched {
		t.Fatalf("deny rule did not win: action=%v matched=%v rule=%q", action, matched, rule)
	}
	if reason != "no data here" {
		t.Errorf("reason = %q, want the rule's explanation", reason)
	}

	// A sibling host still follows its allow rule.
	if action, _, _, _ := m.Match("docs.example.com", 443); action != permission.NetAllow {
		t.Errorf("allow rule did not apply: action=%v", action)
	}
	// Anything else is unmatched, and the caller applies the mode default.
	if _, _, _, matched := m.Match("elsewhere.test", 443); matched {
		t.Error("an unrelated host matched a rule")
	}
}

func TestMatcherPortRestriction(t *testing.T) {
	m, err := NewMatcher(permission.NetPolicy{
		Mode: permission.NetAllowList,
		Rules: []permission.NetRule{
			{Action: permission.NetAllow, Host: "example.com:443"},
			{Action: permission.NetAllow, Host: "any.test"},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	if action, _, _, matched := m.Match("example.com", 443); action != permission.NetAllow || !matched {
		t.Error("the port-qualified rule did not match its own port")
	}
	if _, _, _, matched := m.Match("example.com", 80); matched {
		t.Error("a port-qualified rule matched a different port")
	}
	// No port in the rule means any port.
	if action, _, _, matched := m.Match("any.test", 80); action != permission.NetAllow || !matched {
		t.Error("a portless rule did not match an arbitrary port")
	}
}

// The ":port" suffix has to be split off before the host compiles, including for
// bracketed IPv6 literals.
func TestMatcherRuleHostPortSuffix(t *testing.T) {
	cases := []struct {
		host     string
		port     int
		wantHost string
		wantPort int
	}{
		{host: "example.com:443", wantHost: "example.com", wantPort: 443},
		{host: "[::1]:8443", wantHost: "::1", wantPort: 8443},
		{host: "10.0.0.0/8:53", wantHost: "10.0.0.0/8", wantPort: 53},
		{host: "example.com", port: 8080, wantHost: "example.com", wantPort: 8080},
		{host: "::1", wantHost: "::1", wantPort: 0},
	}
	for _, tc := range cases {
		host, port, err := splitRuleHostPort(tc.host, tc.port)
		if err != nil {
			t.Errorf("splitRuleHostPort(%q): %v", tc.host, err)
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("splitRuleHostPort(%q, %d) = (%q, %d), want (%q, %d)",
				tc.host, tc.port, host, port, tc.wantHost, tc.wantPort)
		}
	}
	if _, _, err := splitRuleHostPort("example.com:notaport", 0); err == nil {
		t.Error("a malformed port suffix was accepted")
	}
}

// AllowHosts is the deprecated list; it still works, and it still loses to an
// explicit deny rule.
func TestMatcherLegacyAllowHosts(t *testing.T) {
	m, err := NewMatcher(permission.NetPolicy{
		Mode:       permission.NetAllowList,
		AllowHosts: []string{"example.com", "10.0.0.0/8"},
		Rules: []permission.NetRule{
			{Action: permission.NetDeny, Host: "api.example.com"},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	if action, _, _, _ := m.Match("example.com", 443); action != permission.NetAllow {
		t.Error("the legacy allow entry stopped matching")
	}
	if action, _, _, _ := m.Match("api.example.com", 443); action != permission.NetDeny {
		t.Error("an explicit deny rule lost to the legacy allow list")
	}
	if !m.HasIPRules() {
		t.Error("HasIPRules = false with a CIDR entry in AllowHosts")
	}
	if action, _, _, matched := m.MatchIP(netip.MustParseAddr("10.1.1.1"), 443); action != permission.NetAllow || !matched {
		t.Error("the legacy CIDR entry did not apply to an address")
	}
}

func TestMatcherRejectsMalformedRules(t *testing.T) {
	if _, err := NewMatcher(permission.NetPolicy{
		Rules: []permission.NetRule{{Action: permission.NetAllow, Host: "bad*pattern"}},
	}); !errdefs.IsValidation(err) {
		t.Fatalf("malformed rule: err = %v, want Validation", err)
	}
	if _, err := NewMatcher(permission.NetPolicy{
		AllowHosts: []string{"bad*pattern"},
	}); !errdefs.IsValidation(err) {
		t.Fatalf("malformed AllowHosts entry: err = %v, want Validation", err)
	}
}
