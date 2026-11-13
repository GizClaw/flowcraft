package netpolicy

import (
	"net/netip"
	"testing"
)

// The interpretation of a host pattern is shared by the rule matcher and the
// MITM selector, so it is pinned once, here.
func TestCompileAndMatchHostnames(t *testing.T) {
	cases := []struct {
		pattern string
		host    string
		want    bool
		why     string
	}{
		{"example.com", "example.com", true, "bare domain matches itself"},
		{"example.com", "api.example.com", true, "bare domain covers its subdomains"},
		{"example.com", "a.b.example.com", true, "any depth"},
		{"example.com", "notexample.com", false, "a suffix is not a subdomain"},
		{"example.com", "example.com.evil.test", false, "the suffix must be on the host side"},
		{"*.example.com", "api.example.com", true, "wildcard matches subdomains"},
		{"*.example.com", "a.b.example.com", true, "wildcard is any depth"},
		{"*.example.com", "example.com", false, "wildcard never matches the bare domain"},
		{"EXAMPLE.com.", "api.example.COM.", true, "case and trailing dots normalize"},
		{"  example.com  ", "example.com", true, "whitespace normalizes"},
		{"bücher.example", "xn--bcher-kva.example", true, "unicode compiles to punycode"},
		{"example.com", "", false, "an empty host matches nothing"},
	}
	for _, tc := range cases {
		p, err := Compile(tc.pattern)
		if err != nil {
			t.Fatalf("Compile(%q): %v", tc.pattern, err)
		}
		if got := p.Match(tc.host); got != tc.want {
			t.Errorf("Compile(%q).Match(%q) = %v, want %v (%s)", tc.pattern, tc.host, got, tc.want, tc.why)
		}
	}
}

func TestCompileRejectsMalformedPatterns(t *testing.T) {
	for _, raw := range []string{
		"",
		" ",
		"foo*bar",    // "*" outside a leading wildcard
		"*",          // a bare star is not a host
		"*.",         // a wildcard with nothing under it
		"*.*.a.test", // nested wildcards
	} {
		if _, err := Compile(raw); err == nil {
			t.Errorf("Compile(%q) accepted a malformed pattern", raw)
		}
	}
}

func TestIPPatterns(t *testing.T) {
	addr := mustCompile(t, "1.2.3.4")
	if !addr.IsIP() {
		t.Fatal("an address literal is not reported as an IP pattern")
	}
	if !addr.MatchIP(netip.MustParseAddr("1.2.3.4")) {
		t.Error("exact address did not match itself")
	}
	if addr.MatchIP(netip.MustParseAddr("1.2.3.5")) {
		t.Error("exact address matched a different address")
	}
	if addr.Match("1.2.3.4") {
		t.Error("an IP pattern matched a hostname lookup; it is evaluated by MatchIP only")
	}

	cidr := mustCompile(t, "10.0.0.0/8")
	if !cidr.IsIP() {
		t.Fatal("a CIDR is not reported as an IP pattern")
	}
	if !cidr.MatchIP(netip.MustParseAddr("10.255.0.1")) {
		t.Error("CIDR did not match an address inside it")
	}
	if cidr.MatchIP(netip.MustParseAddr("11.0.0.1")) {
		t.Error("CIDR matched an address outside it")
	}

	// A v4-mapped v6 address is the same destination; the pattern unmaps it.
	if !addr.MatchIP(netip.MustParseAddr("::ffff:1.2.3.4")) {
		t.Error("a v4-mapped address did not match the v4 literal")
	}
}

func TestSetMatching(t *testing.T) {
	set, err := New([]string{"example.com", "10.0.0.0/8", "*.internal.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if set.Empty() {
		t.Fatal("a compiled set reports empty")
	}
	for _, host := range []string{"example.com", "a.internal.test"} {
		if !set.Match(host) {
			t.Errorf("Set.Match(%q) = false", host)
		}
	}
	if set.Match("elsewhere.test") {
		t.Error("Set.Match matched an unrelated host")
	}
	if !set.MatchIP(netip.MustParseAddr("10.1.2.3")) {
		t.Error("Set.MatchIP missed a CIDR entry")
	}

	empty, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if !empty.Empty() || empty.Match("example.com") || empty.MatchIP(netip.MustParseAddr("10.0.0.1")) {
		t.Error("an empty set matched something")
	}

	if _, err := New([]string{"example.com", "bad*pattern"}); err == nil {
		t.Error("New accepted a malformed entry")
	}
}

func mustCompile(t *testing.T, raw string) *Pattern {
	t.Helper()
	p, err := Compile(raw)
	if err != nil {
		t.Fatalf("Compile(%q): %v", raw, err)
	}
	return p
}
