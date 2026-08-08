package httpkit

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestAllowed(t *testing.T) {
	proxy := &Proxy{cfg: ProxyConfig{
		Mode:       sandbox.NetAllowList,
		AllowHosts: []string{"example.com", "api.internal", "127.0.0.1"},
	}}
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},        // exact
		{"api.example.com", true},    // suffix
		{"deep.a.example.com", true}, // nested suffix
		{"notexample.com", false},    // suffix boundary
		{"example.com.evil.net", false},
		{"api.internal", true},
		{"svc.api.internal", true},
		{"127.0.0.1", true}, // IP literal exact
		{"127.0.0.1:8080", true},
		{"1.127.0.0.1", false}, // IP must not suffix-match
		{"other.com", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := proxy.allowed(tc.host); got != tc.want {
			t.Errorf("allowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestAllowed_ProxyModeAllowsEverything(t *testing.T) {
	proxy := &Proxy{cfg: ProxyConfig{Mode: sandbox.NetProxy, Upstream: "http://127.0.0.1:1"}}
	if !proxy.allowed("anything.example:80") {
		t.Errorf("proxy mode must defer policy to the upstream")
	}
}

// dialProxy connects to the proxy's unix socket and returns the conn.
func dialProxy(t *testing.T, p *Proxy) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", p.SocketPath())
	if err != nil {
		t.Fatalf("dial proxy socket: %v", err)
	}
	return conn
}

func readResponse(t *testing.T, r *bufio.Reader) (*http.Response, string) {
	t.Helper()
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestHTTPForward_Allowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "allowed-ok")
	}))
	defer srv.Close()

	p, err := Start(ProxyConfig{Mode: sandbox.NetAllowList, AllowHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "GET %s/hello HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", srv.URL, strings.TrimPrefix(srv.URL, "http://"))
	resp, body := readResponse(t, bufio.NewReader(conn))
	if resp.StatusCode != http.StatusOK || body != "allowed-ok" {
		t.Errorf("got status=%d body=%q, want 200 allowed-ok", resp.StatusCode, body)
	}
}

func TestHTTPForward_Denied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "should-not-arrive")
	}))
	defer srv.Close()

	p, err := Start(ProxyConfig{Mode: sandbox.NetAllowList, AllowHosts: []string{"example.com"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", srv.URL, strings.TrimPrefix(srv.URL, "http://"))
	resp, _ := readResponse(t, bufio.NewReader(conn))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got status=%d, want 403", resp.StatusCode)
	}
}

func TestConnectTunnel(t *testing.T) {
	// TCP echo server acts as the CONNECT target.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = echo.Close() }()
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn) // echo
	}()

	p, err := Start(ProxyConfig{Mode: sandbox.NetAllowList, AllowHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo.Addr().String(), echo.Addr().String())
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status=%d, want 200", resp.StatusCode)
	}
	// Tunnel is established: send bytes, expect the echo back.
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read tunnel: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("tunnel echo = %q, want %q", buf, "ping")
	}
}

func TestProxyMode_ForwardsToUpstream(t *testing.T) {
	var (
		mu   sync.Mutex
		hit  bool
		path string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = true
		path = r.URL.Path
		mu.Unlock()
		_, _ = fmt.Fprint(w, "upstream-ok")
	}))
	defer upstream.Close()

	p, err := Start(ProxyConfig{Mode: sandbox.NetProxy, Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	// The absolute-form URL targets a port that is never reachable
	// directly; success proves the request was routed via the upstream.
	_, _ = fmt.Fprintf(conn, "GET http://127.0.0.1:1/hello HTTP/1.1\r\nHost: 127.0.0.1:1\r\nConnection: close\r\n\r\n")
	resp, body := readResponse(t, bufio.NewReader(conn))
	if resp.StatusCode != http.StatusOK || body != "upstream-ok" {
		t.Errorf("got status=%d body=%q, want 200 upstream-ok", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if !hit || path != "/hello" {
		t.Errorf("upstream saw hit=%v path=%q, want hit=true path=/hello", hit, path)
	}
}

func TestCloseRemovesSocket(t *testing.T) {
	p, err := Start(ProxyConfig{Mode: sandbox.NetAllowList})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	path := p.SocketPath()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket path %q should be removed after Close, stat err=%v", path, err)
	}
}

// dialTCP connects to a TCP-loopback proxy's bound address.
func dialTCP(t *testing.T, p *Proxy) net.Conn {
	t.Helper()
	addr := p.Addr().(*net.TCPAddr)
	if addr.IP.String() != "127.0.0.1" {
		t.Fatalf("proxy bound %v, want loopback", addr)
	}
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial tcp proxy: %v", err)
	}
	return conn
}

func TestTCPLoopback_AddrAndForward(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "tcp-ok")
	}))
	defer srv.Close()

	p, err := Start(ProxyConfig{
		Mode:        sandbox.NetAllowList,
		AllowHosts:  []string{"127.0.0.1"},
		TCPLoopback: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	// TCP-loopback proxies must not allocate a unix socket path.
	if p.SocketPath() != "" {
		t.Errorf("SocketPath = %q, want empty for TCP loopback", p.SocketPath())
	}
	addr := p.Addr().(*net.TCPAddr)
	if addr.Port == 0 {
		t.Fatalf("bound port 0, want ephemeral")
	}

	conn := dialTCP(t, p)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "GET %s/hello HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", srv.URL, strings.TrimPrefix(srv.URL, "http://"))
	resp, body := readResponse(t, bufio.NewReader(conn))
	if resp.StatusCode != http.StatusOK || body != "tcp-ok" {
		t.Errorf("got status=%d body=%q, want 200 tcp-ok", resp.StatusCode, body)
	}
}

func TestTCPLoopback_Close(t *testing.T) {
	p, err := Start(ProxyConfig{Mode: sandbox.NetAllowList, TCPLoopback: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := p.Addr().String()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Errorf("TCP loopback proxy still accepting after Close at %s", addr)
	}
}
