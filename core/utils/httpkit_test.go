package utils

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHTTP2TransportMapping pins the Config-to-transport mapping: the
// knobs the x/net HTTP/2 transport used to own (ReadIdleTimeout,
// PingTimeout, WriteByteTimeout, IdleConnTimeout) live on
// http.Transport in the standard library.
func TestHTTP2TransportMapping(t *testing.T) {
	roundTripper := NewRoundTripper(
		WithHTTP2(),
		WithoutRetry(),
		WithHTTP2Timeouts(11*time.Second, 22*time.Second, 33*time.Second),
		WithConnectionPool(4, 2, 44*time.Second),
	)
	transport, ok := roundTripper.(*http.Transport)
	if !ok {
		t.Fatalf("round tripper = %T, want *http.Transport", roundTripper)
	}
	if transport.HTTP2 == nil {
		t.Fatal("HTTP2Config is nil: the health checks would be dropped")
	}
	if got := transport.HTTP2.SendPingTimeout; got != 11*time.Second {
		t.Errorf("SendPingTimeout = %v, want 11s (the old ReadIdleTimeout)", got)
	}
	if got := transport.HTTP2.PingTimeout; got != 22*time.Second {
		t.Errorf("PingTimeout = %v, want 22s", got)
	}
	if got := transport.HTTP2.WriteByteTimeout; got != 33*time.Second {
		t.Errorf("WriteByteTimeout = %v, want 33s", got)
	}
	if got := transport.IdleConnTimeout; got != 44*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 44s", got)
	}
}

// TestProtocolSelection runs real requests against an HTTP/2-capable TLS
// server: the HTTP/2 mode must negotiate h2 over ALPN, and the HTTP/1.1
// mode must stay on HTTP/1.1.
func TestProtocolSelection(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	// One config for both subtests on purpose: NewRoundTripper clones it,
	// so neither client may leak its ALPN choice into the other.
	tlsConfig := &tls.Config{RootCAs: pool}

	for _, test := range []struct {
		name  string
		proto int
		opts  []Option
	}{
		{name: "http2", proto: 2, opts: []Option{WithHTTP2()}},
		{name: "http1", proto: 1, opts: []Option{WithHTTP1()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewHttpClient(append(test.opts, WithTLSClientConfig(tlsConfig))...)
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(body) != "ok" {
				t.Fatalf("body = %q, want ok", body)
			}
			if resp.ProtoMajor != test.proto {
				t.Fatalf("protocol = %s, want HTTP/%d", resp.Proto, test.proto)
			}
		})
	}
}

// TestRetryBackoffClampsServerHint is the regression test for an
// unbounded Retry-After: the header is a server's suggestion, so it must
// not outlast the transport's own delay ceiling. Without the clamp the
// caller sleeps for whatever the response asked for — up to a year for a
// hostile or misconfigured upstream. The hint still gets a wider ceiling
// than the exponential path: a real 429's "wait a minute" must be
// honored, not answered with a two-second sleep.
func TestRetryBackoffClampsServerHint(t *testing.T) {
	transport := func(maxDelay, maxHintDelay time.Duration) *retryTransport {
		return &retryTransport{config: RetryConfig{
			MaxAttempts:  3,
			BaseDelay:    200 * time.Millisecond,
			MaxDelay:     maxDelay,
			MaxHintDelay: maxHintDelay,
		}}
	}
	responseWith := func(hint string) *http.Response {
		header := http.Header{}
		if hint != "" {
			header.Set("Retry-After", hint)
		}
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header}
	}

	cases := []struct {
		name         string
		maxDelay     time.Duration
		maxHintDelay time.Duration
		hint         string
		want         time.Duration
	}{
		{"hint beyond the hint ceiling is clamped", 2 * time.Second, 30 * time.Second, "31536000", 30 * time.Second},
		{"hint above the exponential ceiling is honored", 2 * time.Second, 30 * time.Second, "20", 20 * time.Second},
		{"hint within both ceilings is honored", 2 * time.Second, 30 * time.Second, "1", time.Second},
		{"no hint ceiling falls back to the exponential one", 2 * time.Second, 0, "31536000", 2 * time.Second},
		{"no ceilings at all means no sleep", 0, 0, "60", 0},
		{"hint ceiling alone still sleeps", 0, 30 * time.Second, "10", 10 * time.Second},
		{"garbage hint falls back to the exponential path", 2 * time.Second, 30 * time.Second, "soon", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transport(tc.maxDelay, tc.maxHintDelay).backoff(1, responseWith(tc.hint))
			if tc.hint == "soon" {
				// The exponential path is jittered: the first step draws
				// from [0, BaseDelay], so a value bounded by BaseDelay —
				// not MaxDelay — is what proves the header was not parsed.
				if got < 0 || got > 200*time.Millisecond {
					t.Fatalf("backoff = %v, want within [0, 200ms]", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("backoff = %v, want %v", got, tc.want)
			}
		})
	}

	// Without a Retry-After the exponential path is used and stays
	// bounded by the ceiling it grows against.
	if got := transport(time.Second, 30*time.Second).backoff(9, responseWith("")); got < 0 || got > time.Second {
		t.Fatalf("backoff without hint = %v, want within [0, 1s]", got)
	}
}
