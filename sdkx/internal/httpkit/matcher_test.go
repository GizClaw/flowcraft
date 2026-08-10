package httpkit

import (
	"net/netip"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestMatcher_DenyWinsOverAllow(t *testing.T) {
	m, err := NewMatcher(sandbox.NetPolicy{
		Rules: []sandbox.NetRule{
			{Action: sandbox.NetAllow, Host: "example.com"},
			{Action: sandbox.NetDeny, Host: "*.internal.example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if action, rule, matched := m.Match("api.internal.example.com", 443); !matched || action != sandbox.NetDeny {
		t.Fatalf("deny rule must win: action=%v rule=%q matched=%v", action, rule, matched)
	}
	if action, _, matched := m.Match("example.com", 443); !matched || action != sandbox.NetAllow {
		t.Fatalf("allow rule missing: action=%v matched=%v", action, matched)
	}
}

func TestMatcher_DomainAndWildcardSemantics(t *testing.T) {
	m, err := NewMatcher(sandbox.NetPolicy{
		Rules: []sandbox.NetRule{
			{Action: sandbox.NetAllow, Host: "example.com"},
			{Action: sandbox.NetDeny, Host: "*.blocked.example"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bare domain AND descendants.
	for _, host := range []string{"example.com", "api.example.com", "a.b.example.com"} {
		if action, _, matched := m.Match(host, 80); !matched || action != sandbox.NetAllow {
			t.Errorf("%s: want allow", host)
		}
	}
	// Wildcard: subdomains only, never the bare domain.
	if action, _, matched := m.Match("blocked.example", 80); matched {
		t.Errorf("bare wildcard base must not match: action=%v", action)
	}
	for _, host := range []string{"x.blocked.example", "a.b.blocked.example"} {
		if action, _, matched := m.Match(host, 80); !matched || action != sandbox.NetDeny {
			t.Errorf("%s: want deny", host)
		}
	}
}

func TestMatcher_PortMatching(t *testing.T) {
	m, err := NewMatcher(sandbox.NetPolicy{
		Rules: []sandbox.NetRule{
			{Action: sandbox.NetAllow, Host: "example.com", Port: 443},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, matched := m.Match("example.com", 80); matched {
		t.Fatal("port 80 must not match a 443-only rule")
	}
	if action, _, matched := m.Match("example.com", 443); !matched || action != sandbox.NetAllow {
		t.Fatal("port 443 must match")
	}
}

func TestMatcher_IPAndCIDR(t *testing.T) {
	m, err := NewMatcher(sandbox.NetPolicy{
		Rules: []sandbox.NetRule{
			{Action: sandbox.NetAllow, Host: "10.0.0.0/8", Port: 443},
			{Action: sandbox.NetDeny, Host: "10.1.2.3"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasIPRules() {
		t.Fatal("IP rules must be reported")
	}
	if action, _, matched := m.MatchIP(netip.MustParseAddr("10.9.9.9"), 443); !matched || action != sandbox.NetAllow {
		t.Fatalf("CIDR allow missing: %v %v", action, matched)
	}
	if _, _, matched := m.MatchIP(netip.MustParseAddr("10.9.9.9"), 80); matched {
		t.Fatal("CIDR rule is port-bound; port 80 must not match")
	}
	if action, _, matched := m.MatchIP(netip.MustParseAddr("10.1.2.3"), 443); !matched || action != sandbox.NetDeny {
		t.Fatalf("exact IP deny missing: %v %v", action, matched)
	}
	if _, _, matched := m.MatchIP(netip.MustParseAddr("11.0.0.1"), 443); matched {
		t.Fatal("out-of-prefix IP must not match")
	}
}

func TestMatcher_AllowHostsCompiledAsTrailingAllow(t *testing.T) {
	m, err := NewMatcher(sandbox.NetPolicy{
		AllowHosts: []string{"legacy.example"},
		Rules: []sandbox.NetRule{
			{Action: sandbox.NetDeny, Host: "*.blocked.example"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if action, _, matched := m.Match("api.legacy.example", 80); !matched || action != sandbox.NetAllow {
		t.Fatalf("legacy allow missing for subdomain: %v %v", action, matched)
	}
	if action, _, matched := m.Match("x.blocked.example", 80); !matched || action != sandbox.NetDeny {
		t.Fatalf("explicit deny missing: %v %v", action, matched)
	}
}

func TestMatcher_IDNA(t *testing.T) {
	m, err := NewMatcher(sandbox.NetPolicy{
		Rules: []sandbox.NetRule{{Action: sandbox.NetAllow, Host: "münchen.example"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if action, _, matched := m.Match("xn--mnchen-3ya.example", 80); !matched || action != sandbox.NetAllow {
		t.Fatalf("punycode host must match unicode rule: %v %v", action, matched)
	}
	if action, _, matched := m.Match("MÜNCHEN.EXAMPLE.", 80); !matched || action != sandbox.NetAllow {
		t.Fatalf("case/trailing-dot normalization missing: %v %v", action, matched)
	}
}

func TestMatcher_InvalidRules(t *testing.T) {
	for _, host := range []string{"", "a*b.example", "*.", "*"} {
		_, err := NewMatcher(sandbox.NetPolicy{
			Rules: []sandbox.NetRule{{Action: sandbox.NetAllow, Host: host}},
		})
		if !errdefs.IsValidation(err) {
			t.Errorf("host %q: err = %v, want Validation", host, err)
		}
	}
}
