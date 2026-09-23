package importer

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBlockedAddressCoversReservedRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.5.4", "192.168.1.1", "169.254.1.1",
		"0.0.0.0", "0.1.2.3", "100.64.0.1", "100.127.255.255",
		"198.18.0.1", "198.19.255.255", "240.0.0.1", "255.255.255.255",
		"224.0.0.1", "239.0.0.1", "239.255.255.255", "::1", "fe80::1", "fc00::1", "ff02::1",
	}
	for _, value := range blocked {
		if !blockedAddress(net.ParseIP(value)) {
			t.Errorf("blockedAddress(%s) = false, want blocked", value)
		}
	}
	allowed := []string{
		"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111",
		"100.63.255.255", "100.128.0.0", "198.17.255.255",
	}
	for _, value := range allowed {
		if blockedAddress(net.ParseIP(value)) {
			t.Errorf("blockedAddress(%s) = true, want allowed", value)
		}
	}
}

func TestFetchBlocksLoopbackAtDialTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("should not be reached"))
	}))
	defer server.Close()
	_, err := Fetch(context.Background(), server.URL, Policy{
		AllowedSchemes: []string{"http"}, AllowedHosts: []string{"127.0.0.1"},
		AllowLoopback: false, Timeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v, want a dial-time address rejection", err)
	}
}

func TestFetchEnforcesRedirectHostPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "http://example.com/", http.StatusFound)
	}))
	defer server.Close()
	_, err := Fetch(context.Background(), server.URL, Policy{
		AllowedSchemes: []string{"http"}, AllowedHosts: []string{"127.0.0.1"},
		AllowLoopback: true, Timeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "redirect host") {
		t.Fatalf("err = %v, want a redirect host rejection", err)
	}
}

func TestFetchAppliesPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte("hello import"))
	}))
	defer server.Close()
	ctx := context.Background()
	policy := Policy{
		AllowedSchemes: []string{"http"},
		AllowedHosts:   []string{"127.0.0.1"},
		AllowLoopback:  true,
		MaxBytes:       64,
		Timeout:        time.Second,
	}
	document, err := Fetch(ctx, server.URL, policy)
	if err != nil {
		t.Fatal(err)
	}
	if string(document.Data) != "hello import" || document.ContentType != "text/plain" {
		t.Fatalf("document = %#v", document)
	}
}

func TestFetchRejectsDisallowedTargets(t *testing.T) {
	ctx := context.Background()
	tests := map[string]struct {
		url    string
		policy Policy
	}{
		"scheme": {url: "file:///etc/passwd", policy: Policy{}},
		"host":   {url: "https://evil.example/doc", policy: Policy{AllowedHosts: []string{"trusted.example"}}},
		"loopback": {
			url:    "http://127.0.0.1/doc",
			policy: Policy{AllowedSchemes: []string{"http"}, AllowedHosts: []string{"127.0.0.1"}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Fetch(ctx, test.url, test.policy); err == nil {
				t.Fatal("expected the fetch to be rejected")
			}
		})
	}
}

func TestFetchEnforcesSizeLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(strings.Repeat("x", 128)))
	}))
	defer server.Close()
	_, err := Fetch(context.Background(), server.URL, Policy{
		AllowedSchemes: []string{"http"},
		AllowedHosts:   []string{"127.0.0.1"},
		AllowLoopback:  true,
		MaxBytes:       32,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("size limit error = %v", err)
	}
}
