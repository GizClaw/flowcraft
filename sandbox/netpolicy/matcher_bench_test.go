package netpolicy_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/sandbox/netpolicy"
)

// BenchmarkMatch is one egress decision: a proxied connection, under an allow-list
// with explicit denies ahead of it. The matcher runs per connection, so its cost is
// paid on the network path rather than once per session.
func BenchmarkMatch(b *testing.B) {
	matcher, err := netpolicy.NewMatcher(policy.NetPolicy{
		Mode: policy.NetAllowList,
		Rules: []policy.NetRule{
			{Action: policy.NetDeny, Host: "telemetry.example.com"},
			{Action: policy.NetDeny, Host: "*.internal.example.com"},
			{Action: policy.NetAllow, Host: "api.example.com", Port: 443},
			{Action: policy.NetAllow, Host: "cdn.example.com"},
		},
		AllowHosts: []string{"docs.example.com", "pypi.org:443"},
	})
	if err != nil {
		b.Fatalf("NewMatcher: %v", err)
	}
	if _, _, _, matched := matcher.Match("api.example.com", 443); !matched {
		b.Fatal("the fixture rule does not match")
	}

	b.ReportAllocs()
	for b.Loop() {
		matcher.Match("api.example.com", 443)
	}
}

// BenchmarkMatchMiss is the denial path: every rule is tried before the allow-list
// default answers. A deny rule is the expensive case because it cannot stop early.
func BenchmarkMatchMiss(b *testing.B) {
	matcher, err := netpolicy.NewMatcher(policy.NetPolicy{
		Mode: policy.NetAllowList,
		Rules: []policy.NetRule{
			{Action: policy.NetAllow, Host: "api.example.com", Port: 443},
			{Action: policy.NetAllow, Host: "cdn.example.com"},
		},
	})
	if err != nil {
		b.Fatalf("NewMatcher: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		matcher.Match("blocked.example.net", 443)
	}
}
