package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

const testServerName = "example.com"

func TestH1RoundTrip(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	endpoint := startMITM(t, upstream)
	resp := h1Request(t, endpoint, "GET /ok HTTP/1.1\r\nHost: "+testServerName+"\r\nConnection: close\r\n\r\n")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}

func TestH1RejectsOversizedHeader(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "unreachable")
	}))
	defer upstream.Close()

	endpoint := startMITM(t, upstream)
	req := "GET / HTTP/1.1\r\nHost: " + testServerName + "\r\nX-Big: " +
		strings.Repeat("a", mitmMaxRequestHeaderBytes+4096) + "\r\n\r\n"
	resp, err := h1RawResponse(endpoint, req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("status = %d, want 431", resp.StatusCode)
		}
		return
	}
	// The server may close the connection while rejecting the header;
	// either way the request must never reach the upstream handler.
	t.Fatalf("oversized header error = %v (want 431 or close)", err)
}

func TestH1ClientDisconnectCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(10 * time.Second):
		}
	}))
	defer upstream.Close()

	endpoint := startMITM(t, upstream)
	conn := dialH1(t, endpoint)
	if _, err := fmt.Fprintf(conn, "GET /slow HTTP/1.1\r\nHost: %s\r\n\r\n", testServerName); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received the request")
	}
	_ = conn.Close()

	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request context was not cancelled after client disconnect")
	}
}

type mitmEndpoint struct {
	addr       string
	ca         *CA
	serverName string
}

func startMITM(t *testing.T, upstream *httptest.Server) *mitmEndpoint {
	t.Helper()
	certPool := x509.NewCertPool()
	cert, err := x509.ParseCertificate(upstream.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse upstream cert: %v", err)
	}
	certPool.AddCert(cert)

	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}
	impl, err := New(&corenet.MITMPolicy{}, nil, dial, certPool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	engine := impl.(*Engine)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	endpoint := &mitmEndpoint{
		addr:       ln.Addr().String(),
		ca:         engine.ca,
		serverName: testServerName,
	}
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				_ = engine.Serve(conn, testServerName, upstream.Listener.Addr().String())
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return endpoint
}

func dialH1(t *testing.T, endpoint *mitmEndpoint) net.Conn {
	t.Helper()
	raw, err := net.Dial("tcp", endpoint.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tlsConn, err := handshakeH1(raw, endpoint)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("client handshake: %v", err)
	}
	return tlsConn
}

func h1Request(t *testing.T, endpoint *mitmEndpoint, req string) *http.Response {
	t.Helper()
	resp, err := h1RawResponse(endpoint, req)
	if err != nil {
		t.Fatalf("h1 request: %v", err)
	}
	return resp
}

func h1RawResponse(endpoint *mitmEndpoint, req string) (*http.Response, error) {
	raw, err := net.Dial("tcp", endpoint.addr)
	if err != nil {
		return nil, err
	}
	tlsConn, err := handshakeH1(raw, endpoint)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	if _, err := io.WriteString(tlsConn, req); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: "GET"})
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	return resp, nil
}

func handshakeH1(raw net.Conn, endpoint *mitmEndpoint) (*tls.Conn, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(endpoint.ca.PEM()) {
		return nil, fmt.Errorf("append leaf CA PEM")
	}
	conn := tls.Client(raw, &tls.Config{
		RootCAs:    pool,
		ServerName: endpoint.serverName,
		NextProtos: []string{"http/1.1"},
	})
	if err := conn.Handshake(); err != nil {
		return nil, err
	}
	return conn, nil
}
