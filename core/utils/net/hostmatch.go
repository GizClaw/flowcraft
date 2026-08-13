// Host-pattern compilation: domains, wildcards, IP literals, and CIDR
// prefixes are compiled into a pure matching set. The net-rule matcher
// and the MITM host selector share this interpretation so that
// "example.com", "*.example.com", "1.2.3.4", and "10.0.0.0/8" behave
// identically wherever a policy is evaluated.
package net

import (
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/net/idna"
)

// Pattern is one compiled host pattern.
//
//   - "example.com": the bare domain AND every subdomain
//     (domain-and-descendants).
//   - "*.example.com": subdomains only, any depth; never the bare
//     domain.
//   - "1.2.3.4" / "10.0.0.0/8": IP / CIDR, evaluated via MatchIP.
type Pattern struct {
	wild   bool
	host   string
	addr   netip.Addr
	prefix netip.Prefix
}

// Compile normalizes one host pattern. Unicode names are converted to
// punycode; case and trailing dots are normalized. A literal "*" is
// only allowed as the leading "*." wildcard.
func Compile(raw string) (*Pattern, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	if strings.Contains(host, "*") && !strings.HasPrefix(host, "*.") {
		return nil, fmt.Errorf("host %q: \"*\" is only allowed as a leading \"*.\" wildcard", raw)
	}
	p := &Pattern{wild: strings.HasPrefix(host, "*.")}
	if p.wild {
		host = strings.TrimPrefix(host, "*.")
		if host == "" || strings.Contains(host, "*") {
			return nil, fmt.Errorf("wildcard host %q must have form \"*.example.com\"", raw)
		}
	}
	host = normalize(host)
	p.host = host

	if !p.wild {
		if addr, err := netip.ParseAddr(host); err == nil {
			p.addr = addr.Unmap()
		} else if prefix, err := netip.ParsePrefix(host); err == nil {
			p.prefix = prefix.Masked()
		}
	}
	return p, nil
}

// IsIP reports whether the pattern is an IP literal or CIDR prefix
// (evaluated against resolved addresses, never hostnames).
func (p *Pattern) IsIP() bool { return p.addr.IsValid() || p.prefix.IsValid() }

// Match evaluates a hostname against the pattern.
func (p *Pattern) Match(host string) bool {
	if p.IsIP() {
		return false
	}
	host = normalize(host)
	if p.wild {
		return strings.HasSuffix(host, "."+p.host)
	}
	return host == p.host || strings.HasSuffix(host, "."+p.host)
}

// MatchIP evaluates a resolved address against the pattern.
func (p *Pattern) MatchIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if p.addr.IsValid() {
		return ip == p.addr
	}
	if p.prefix.IsValid() {
		return p.prefix.Contains(ip)
	}
	return false
}

// Set is an ordered collection of patterns; Match returns true when
// any pattern matches.
type Set struct {
	patterns []*Pattern
}

// New compiles a host list into a Set. An empty list yields an empty
// (never-matching) set; malformed entries are Validation errors.
func New(hosts []string) (*Set, error) {
	s := &Set{}
	for _, raw := range hosts {
		p, err := Compile(raw)
		if err != nil {
			return nil, err
		}
		s.patterns = append(s.patterns, p)
	}
	return s, nil
}

// Empty reports whether the set has no patterns.
func (s *Set) Empty() bool { return len(s.patterns) == 0 }

// Match reports whether any pattern matches the hostname.
func (s *Set) Match(host string) bool {
	for _, p := range s.patterns {
		if p.Match(host) {
			return true
		}
	}
	return false
}

// MatchIP reports whether any IP/CIDR pattern matches the address.
func (s *Set) MatchIP(ip netip.Addr) bool {
	for _, p := range s.patterns {
		if p.MatchIP(ip) {
			return true
		}
	}
	return false
}

// normalize lowercases, strips the trailing dot, and converts unicode
// hostnames to punycode. IDNA failures leave the input unchanged so
// callers can still reject it via their own defaults.
func normalize(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if isASCII(host) {
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
