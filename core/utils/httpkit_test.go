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
