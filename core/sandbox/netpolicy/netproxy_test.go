package netpolicy

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/permission"
)

// countingUpstream is a destination the proxy may or may not be allowed to
// reach. It accepts connections, echoes what it reads, and counts both — which
// is what lets a test assert "refused" as "no connection was ever made" instead
// of "the error text looked right".
type countingUpstream struct {
	ln net.Listener

	mu       sync.Mutex
	accepted int
	conns    []net.Conn
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &countingUpstream{ln: ln}
	go u.accept()
	t.Cleanup(u.close)
	return u
}

func (u *countingUpstream) accept() {
	for {
		conn, err := u.ln.Accept()
		if err != nil {
			return
		}
		u.mu.Lock()
		u.accepted++
		u.conns = append(u.conns, conn)
		u.mu.Unlock()
		go func(c net.Conn) { _, _ = io.Copy(c, c) }(conn)
	}
}

func (u *countingUpstream) port() int { return u.ln.Addr().(*net.TCPAddr).Port }

func (u *countingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.accepted
}

func (u *countingUpstream) close() {
	_ = u.ln.Close()
	u.mu.Lock()
	conns := u.conns
	u.conns = nil
	u.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// startProxy builds a loopback proxy for the test and closes it afterwards.
func startProxy(t *testing.T, cfg ProxyConfig) *Proxy {
	t.Helper()
	cfg.TCPLoopback = true
	proxy, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy
}

// connectThroughProxy performs one HTTP CONNECT and returns the status and the
// hijacked tunnel (nil when the proxy refused).
func connectThroughProxy(t *testing.T, proxy *Proxy, target string) (int, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return resp.StatusCode, nil
	}
	return resp.StatusCode, conn
}

// The assertion this file exists for: a denied destination must not be dialed.
// Checking the status code alone would pass a proxy that connected first and
// refused after.
func TestProxyDenyDoesNotDial(t *testing.T) {
	upstream := newCountingUpstream(t)
	decisions := make(chan ProxyDecision, 8)
	proxy := startProxy(t, ProxyConfig{
		Mode:        permission.NetAllowList,
		TCPLoopback: true,
		Rules: []permission.NetRule{
			{Action: permission.NetAllow, Host: "localhost"},
			{Action: permission.NetDeny, Host: "localhost", Reason: "blocked by policy"},
		},
		OnDecision: func(d ProxyDecision) { decisions <- d },
	})

	status, tunnel := connectThroughProxy(t, proxy, fmt.Sprintf("localhost:%d", upstream.port()))
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if got := upstream.count(); got != 0 {
		t.Fatalf("upstream accepted %d connection(s) for a denied destination", got)
	}

	select {
	case last := <-decisions:
		if last.Action != permission.NetDeny || last.Reason != "blocked by policy" {
			t.Fatalf("audit = %+v, want a deny with the rule's reason", last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision was audited")
	}
}

// The positive control: an allowed destination is dialed once and the tunnel
// carries bytes both ways.
func TestProxyAllowTunnels(t *testing.T) {
	upstream := newCountingUpstream(t)
	proxy := startProxy(t, ProxyConfig{
		Mode:        permission.NetAllowList,
		TCPLoopback: true,
		Rules:       []permission.NetRule{{Action: permission.NetAllow, Host: "localhost"}},
	})

	status, tunnel := connectThroughProxy(t, proxy, fmt.Sprintf("localhost:%d", upstream.port()))
	if status != http.StatusOK || tunnel == nil {
		t.Fatalf("CONNECT = %d tunnel=%v, want 200 and a tunnel", status, tunnel)
	}
	defer func() { _ = tunnel.Close() }()

	if _, err := tunnel.Write([]byte("ping")); err != nil {
		t.Fatalf("tunnel write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(tunnel, buf); err != nil {
		t.Fatalf("tunnel read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echoed %q, want the tunnel to carry bytes both ways", buf)
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("upstream accepted %d connection(s), want 1", got)
	}
}

// An allow-list with no matching rule refuses: the mode default is deny.
func TestProxyAllowListDeniesUnlisted(t *testing.T) {
	upstream := newCountingUpstream(t)
	proxy := startProxy(t, ProxyConfig{
		Mode:        permission.NetAllowList,
		TCPLoopback: true,
		AllowHosts:  []string{"example.com"},
	})

	status, tunnel := connectThroughProxy(t, proxy, fmt.Sprintf("localhost:%d", upstream.port()))
	if tunnel != nil {
		_ = tunnel.Close()
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an unlisted destination", status)
	}
	if got := upstream.count(); got != 0 {
		t.Fatalf("upstream accepted %d connection(s) for an unlisted destination", got)
	}
}

// SOCKS5 shares the decision path; this pins that it also refuses without
// dialing, and that an allowed CONNECT gets a success reply.
func TestProxySOCKS5(t *testing.T) {
	upstream := newCountingUpstream(t)

	t.Run("deny", func(t *testing.T) {
		proxy := startProxy(t, ProxyConfig{
			Mode:        permission.NetAllowList,
			TCPLoopback: true,
			Rules:       []permission.NetRule{{Action: permission.NetDeny, Host: "127.0.0.1"}},
		})
		code, conn := socks5Connect(t, proxy, "127.0.0.1", upstream.port())
		_ = conn.Close()
		if code == 0 {
			t.Fatal("SOCKS5 CONNECT to a denied address succeeded")
		}
		if got := upstream.count(); got != 0 {
			t.Fatalf("upstream accepted %d connection(s) for a denied address", got)
		}
	})

	t.Run("allow", func(t *testing.T) {
		proxy := startProxy(t, ProxyConfig{
			Mode:        permission.NetAllowList,
			TCPLoopback: true,
			Rules:       []permission.NetRule{{Action: permission.NetAllow, Host: "127.0.0.1"}},
		})
		code, conn := socks5Connect(t, proxy, "127.0.0.1", upstream.port())
		if code != 0 {
			t.Fatalf("SOCKS5 CONNECT reply code = %d, want 0", code)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf) != "ping" {
			t.Fatalf("echoed %q", buf)
		}
		if got := upstream.count(); got != 1 {
			t.Fatalf("upstream accepted %d connection(s), want 1", got)
		}
	})
}

// socks5Connect performs a no-auth SOCKS5 CONNECT and returns the reply code and
// the connection (positioned at the start of any tunnelled bytes).
func socks5Connect(t *testing.T, proxy *Proxy, host string, port int) (byte, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	// greeting: version 5, one method, "no authentication".
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read greeting reply: %v", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		t.Fatalf("greeting reply = %v, want [5 0]", reply)
	}

	ip := net.ParseIP(host).To4()
	if ip == nil {
		t.Fatalf("test helper only speaks IPv4 literals, got %q", host)
	}
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, ip...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write connect: %v", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	// Consume the bound address so the caller sees tunnel bytes only.
	switch head[3] {
	case 0x01:
		_, _ = io.ReadFull(conn, make([]byte, 4+2))
	case 0x04:
		_, _ = io.ReadFull(conn, make([]byte, 16+2))
	}
	_ = conn.SetDeadline(time.Time{})
	return head[1], conn
}

// Start rejects what it cannot serve, before any listener exists.
func TestStartRejectsBadConfig(t *testing.T) {
	cases := map[string]ProxyConfig{
		"wrong mode":          {Mode: permission.NetDenyAll},
		"malformed rule":      {Mode: permission.NetAllowList, Rules: []permission.NetRule{{Host: "bad*pattern"}}},
		"mitm without engine": {Mode: permission.NetAllowList, MITM: &permission.MITMPolicy{Enabled: true}},
		"bad upstream scheme": {Mode: permission.NetProxy, Upstream: "ftp://127.0.0.1:1"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			proxy, err := Start(cfg)
			if err == nil {
				_ = proxy.Close()
				t.Fatal("Start accepted an unserviceable config")
			}
		})
	}
}

// The default listener is a unix socket with restrictive permissions, because
// it is the only thing standing between a sandboxed child and the proxy.
func TestStartUnixSocketIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows permission bits are advisory: the directory ACL is the boundary")
	}
	proxy, err := Start(ProxyConfig{Mode: permission.NetAllowList})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proxy.Close() }()

	path := proxy.SocketPath()
	if path == "" {
		t.Fatal("no socket path for the default listener")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}
