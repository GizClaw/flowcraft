// Package importer fetches external documents under an explicit network
// policy. Importing is the main memory attack surface: without a policy a
// resource URL can read local files or reach internal services.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Policy bounds one import fetch.
type Policy struct {
	// AllowedSchemes defaults to ["https"].
	AllowedSchemes []string
	// AllowedHosts matches exact names or ".suffix" entries. Empty allows any
	// public host.
	AllowedHosts []string
	// MaxBytes caps the response body. Zero selects 8 MiB.
	MaxBytes int64
	// AllowLoopback permits loopback targets (tests and local development).
	// It relaxes loopback only: private, CGNAT, link-local (including the
	// cloud metadata address) and reserved ranges stay blocked, because a
	// caller asking for a local test server is not asking to reach the
	// host's other networks.
	AllowLoopback bool
	// Timeout bounds the whole fetch. Zero selects 30s.
	Timeout time.Duration
}

// Document is one fetched external resource.
type Document struct {
	URL         string
	ContentType string
	Data        []byte
}

// Fetch validates rawURL, dials only allowed addresses, and returns the
// bounded body.
func Fetch(ctx context.Context, rawURL string, policy Policy) (Document, error) {
	if ctx == nil {
		return Document{}, errors.New("memory importer: context is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return Document{}, fmt.Errorf("memory importer: parse url: %w", err)
	}
	schemes := policy.AllowedSchemes
	if len(schemes) == 0 {
		schemes = []string{"https"}
	}
	if !containsFold(schemes, parsed.Scheme) {
		return Document{}, fmt.Errorf("memory importer: scheme %q is not allowed", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return Document{}, errors.New("memory importer: url host is required")
	}
	if !hostAllowed(parsed.Hostname(), policy.AllowedHosts) {
		return Document{}, fmt.Errorf("memory importer: host %q is not allowed", parsed.Hostname())
	}
	timeout := policy.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxBytes := policy.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: dialPolicy(policy),
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("memory importer: too many redirects")
			}
			if !containsFold(schemes, request.URL.Scheme) {
				return fmt.Errorf("memory importer: redirect scheme %q is not allowed", request.URL.Scheme)
			}
			if !hostAllowed(request.URL.Hostname(), policy.AllowedHosts) {
				return fmt.Errorf("memory importer: redirect host %q is not allowed", request.URL.Hostname())
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Document{}, fmt.Errorf("memory importer: build request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return Document{}, fmt.Errorf("memory importer: fetch: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return Document{}, fmt.Errorf("memory importer: status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return Document{}, fmt.Errorf("memory importer: read body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return Document{}, fmt.Errorf("memory importer: body exceeds %d bytes", maxBytes)
	}
	contentType := response.Header.Get("Content-Type")
	if separator := strings.IndexByte(contentType, ';'); separator >= 0 {
		contentType = strings.TrimSpace(contentType[:separator])
	}
	return Document{URL: response.Request.URL.String(), ContentType: contentType, Data: data}, nil
}

// dialPolicy resolves a host once, validates every resolved address, and then
// dials the validated addresses directly. Re-resolving the host name at dial
// time would reopen a DNS-rebinding window between the policy check and the
// connection.
func dialPolicy(policy Policy) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("memory importer: dial address: %w", err)
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("memory importer: resolve %q: %w", host, err)
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("memory importer: resolve %q: no addresses", host)
		}
		for _, resolved := range addresses {
			if blockedAddress(resolved.IP, policy.AllowLoopback) {
				return nil, fmt.Errorf("memory importer: address %s is not allowed", resolved.IP)
			}
		}
		var lastErr error
		for _, resolved := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("memory importer: dial %q: %w", host, lastErr)
	}
}

// blockedAddress reports whether the importer refuses to dial ip.
// allowLoopback relaxes the loopback range only: every other blocked range
// (private, CGNAT, link-local and the cloud metadata address, benchmarking,
// reserved) stays blocked even for local development, so the flag cannot be
// used to reach the host's other networks by accident.
func blockedAddress(ip net.IP, allowLoopback bool) bool {
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0: // 0.0.0.0/8 "this network"
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // 100.64.0.0/10 CGNAT
			return true
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // 198.18.0.0/15 benchmarking
			return true
		case ip4[0] >= 240: // 240.0.0.0/4 reserved, including broadcast
			return true
		}
	}
	return false
}

func hostAllowed(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, entry := range allowed {
		entry = strings.ToLower(strings.TrimSpace(entry))
		switch {
		case entry == "":
			continue
		case strings.HasPrefix(entry, "."):
			if host == strings.TrimPrefix(entry, ".") || strings.HasSuffix(host, entry) {
				return true
			}
		case entry == host:
			return true
		}
	}
	return false
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}
