package webhook

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

// SSRFGuard rejects URLs pointing at private / loopback / link-local /
// cloud-metadata endpoints. Hostnames in the deny suffix list are rejected
// without a DNS lookup; otherwise the resolver is consulted and any
// forbidden IP causes a deny.
type SSRFGuard struct {
	resolver  *net.Resolver
	denyHosts []string
}

// NewSSRFGuard builds a guard with the standard deny set.
func NewSSRFGuard() *SSRFGuard {
	return &SSRFGuard{
		resolver: net.DefaultResolver,
		denyHosts: []string{
			".local", ".internal",
			"metadata.google.internal", "instance-data",
		},
	}
}

// ErrSSRFBlocked is returned when SSRFGuard rejects a request.
var ErrSSRFBlocked = errors.New("ssrf_blocked")

// Check returns nil if the URL is safe to call from this process.
func (g *SSRFGuard) Check(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ErrSSRFBlocked
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrSSRFBlocked
	}
	host := u.Hostname()
	if host == "" {
		return ErrSSRFBlocked
	}
	lower := strings.ToLower(host)
	for _, deny := range g.denyHosts {
		if strings.HasSuffix(lower, deny) || lower == deny {
			return ErrSSRFBlocked
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if isForbiddenIP(ip) {
			return ErrSSRFBlocked
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return ErrSSRFBlocked
	}
	for _, a := range addrs {
		if isForbiddenIP(a.IP) {
			return ErrSSRFBlocked
		}
	}
	return nil
}

func isForbiddenIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
			return true
		}
		return false
	}
	if ip[0]&0xfe == 0xfc { // IPv6 ULA fc00::/7
		return true
	}
	return false
}
