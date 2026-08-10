package hostmatch

import (
	"net/netip"
	"testing"
)

func TestSet_MatchSemantics(t *testing.T) {
	s, err := New([]string{
		"example.com",
		"*.blocked.example",
		"10.0.0.0/8",
		"1.2.3.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"example.com", "api.example.com", "a.b.example.com"} {
		if !s.Match(host) {
			t.Errorf("%s: bare domain must match descendants", host)
		}
	}
	if s.Match("blocked.example") {
		t.Error("wildcard base must not match bare domain")
	}
	for _, host := range []string{"x.blocked.example", "a.b.blocked.example"} {
		if !s.Match(host) {
			t.Errorf("%s: wildcard subdomain must match", host)
		}
	}
	if !s.MatchIP(netip.MustParseAddr("10.9.9.9")) {
		t.Error("CIDR match failed")
	}
	if !s.MatchIP(netip.MustParseAddr("1.2.3.4")) {
		t.Error("exact IP match failed")
	}
	if s.MatchIP(netip.MustParseAddr("11.0.0.1")) {
		t.Error("out-of-prefix IP must not match")
	}
}

func TestSet_EmptyAndNormalization(t *testing.T) {
	empty, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Empty() || empty.Match("anything.example") {
		t.Fatal("empty set must never match")
	}

	s, err := New([]string{"MÜNCHEN.EXAMPLE."})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Match("xn--mnchen-3ya.example") {
		t.Fatal("unicode rule must match punycode host")
	}
}

func TestCompile_Invalid(t *testing.T) {
	for _, host := range []string{"", "a*b.example", "*.", "*"} {
		if _, err := Compile(host); err == nil {
			t.Errorf("host %q: expected error", host)
		}
	}
}
