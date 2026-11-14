package netpolicy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox/netpolicy"
	"github.com/GizClaw/flowcraft/core/sandbox/netpolicy/mitm"
)

// The two enforcement paths this file covers are the ones a policy decision
// cannot be trusted without: TLS termination, where the proxy is the only thing
// that can see the request, and an upstream proxy, where the proxy is not the
// only hop. Both are tested through real TLS and real sockets — a test that
// disabled certificate verification would prove nothing about the CA injection,
// which is the only reason the feature exists.

const allowLocalhost = "127.0.0.1"

// tlsOrigin is an HTTPS server whose certificate the proxy is told to trust as an
// outbound root, plus a counter of the requests that actually reached it.
func tlsOrigin(t *testing.T) (addr string, roots *x509.CertPool, hits *atomic.Int64) {
	t.Helper()
	hits = &atomic.Int64{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "origin:"+r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	transport, ok := srv.Client().Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("httptest server has no client root pool")
	}
	return strings.TrimPrefix(srv.URL, "https://"), transport.TLSClientConfig.RootCAs, hits
}

// trustPool builds the client-side trust the child would receive: the CA the
// proxy injected into it.
func trustPool(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("the proxy's CA PEM does not parse")
	}
	return pool
}

// startProxy builds a loopback proxy for the test.
func startProxy(t *testing.T, cfg netpolicy.ProxyConfig) *netpolicy.Proxy {
	t.Helper()
	cfg.TCPLoopback = true
	proxy, err := netpolicy.Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy
}

// connect opens a CONNECT tunnel through the proxy and returns the raw
// connection when the proxy accepted it.
func connect(t *testing.T, proxy *netpolicy.Proxy, target string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn
}

// recordingHooks observes decrypted traffic and can refuse a request.
type recordingHooks struct {
	mu       sync.Mutex
	connects int
	requests int
	paths    []string
	block    bool
}

func (h *recordingHooks) OnConnect(context.Context, *netpolicy.MITMConnectInfo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connects++
	return nil
}

func (h *recordingHooks) OnRequest(_ context.Context, req *netpolicy.MITMRequestInfo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.block {
		return errors.New("blocked by policy")
	}
	h.requests++
	h.paths = append(h.paths, req.Path)
	return nil
}

func (h *recordingHooks) OnResponse(context.Context, *netpolicy.MITMResponseInfo) error { return nil }

func (h *recordingHooks) counts() (connects, requests int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connects, h.requests
}

// A CONNECT to a host the policy terminates is decrypted: the client verifies the
// proxy's leaf against the CA the proxy injected, and the origin sees the request.
func TestMITMTerminatesAndInspects(t *testing.T) {
	origin, roots, hits := tlsOrigin(t)
	hooks := &recordingHooks{}
	proxy := startProxy(t, netpolicy.ProxyConfig{
		Mode:          permission.NetAllowList,
		Rules:         []permission.NetRule{{Action: permission.NetAllow, Host: allowLocalhost}},
		MITM:          &permission.MITMPolicy{Enabled: true},
		MITMFactory:   mitm.New,
		Hooks:         hooks,
		OutboundRoots: roots,
	})

	conn := connect(t, proxy, origin)
	// The only thing that can make this handshake succeed is the proxy's own CA:
	// the origin's certificate is not in this pool.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: allowLocalhost,
		RootCAs:    trustPool(t, proxy.CAPEM()),
		MinVersion: tls.VersionTLS12,
	})
	defer func() { _ = tlsConn.Close() }()
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake against the injected CA: %v", err)
	}

	if _, err := io.WriteString(tlsConn, "GET /seen HTTP/1.1\r\nHost: "+allowLocalhost+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "origin:/seen" {
		t.Fatalf("response = %d %q, want the origin's answer", resp.StatusCode, body)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("origin saw %d requests, want 1", got)
	}
	connects, requests := hooks.counts()
	if connects == 0 || requests == 0 {
		t.Fatalf("hooks saw %d connects and %d requests, want both", connects, requests)
	}
}

// A host the MITM policy does not name is tunnelled raw: the proxy must not
// terminate a connection it was not asked to inspect.
func TestMITMLeavesUnlistedHostsRaw(t *testing.T) {
	echo := tcpEcho(t)
	proxy := startProxy(t, netpolicy.ProxyConfig{
		Mode:        permission.NetAllowList,
		Rules:       []permission.NetRule{{Action: permission.NetAllow, Host: allowLocalhost}},
		MITM:        &permission.MITMPolicy{Enabled: true, Hosts: []string{"example.com"}},
		MITMFactory: mitm.New,
	})

	conn := connect(t, proxy, echo)
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v (the proxy terminated a connection it should have tunnelled)", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echoed %q, want ping", buf)
	}
}

// A hook refusal is the enforcement point: the request must not reach the origin.
func TestMITMHookBlockIsEnforced(t *testing.T) {
	origin, roots, hits := tlsOrigin(t)
	hooks := &recordingHooks{block: true}
	proxy := startProxy(t, netpolicy.ProxyConfig{
		Mode:          permission.NetAllowList,
		Rules:         []permission.NetRule{{Action: permission.NetAllow, Host: allowLocalhost}},
		MITM:          &permission.MITMPolicy{Enabled: true},
		MITMFactory:   mitm.New,
		Hooks:         hooks,
		OutboundRoots: roots,
	})

	conn := connect(t, proxy, origin)
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: allowLocalhost,
		RootCAs:    trustPool(t, proxy.CAPEM()),
		MinVersion: tls.VersionTLS12,
	})
	defer func() { _ = tlsConn.Close() }()
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if _, err := io.WriteString(tlsConn, "GET /blocked HTTP/1.1\r\nHost: "+allowLocalhost+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from the hook", resp.StatusCode)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("origin saw %d requests for a blocked request", got)
	}
}

// In proxy mode everything the policy does not deny is delegated to the upstream,
// and the upstream really carries the tunnel.
func TestUpstreamHTTPProxyCarriesTraffic(t *testing.T) {
	echo := tcpEcho(t)
	upstream := newHTTPUpstream(t, "up-user", "up-pass")
	proxy := startProxy(t, netpolicy.ProxyConfig{
		Mode:     permission.NetProxy,
		Upstream: upstream.url("up-user", "up-pass"),
		Rules:    []permission.NetRule{{Action: permission.NetAllow, Host: allowLocalhost}},
	})

	conn := connect(t, proxy, echo)
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through the upstream: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echoed %q", buf)
	}
	if connects, auth := upstream.stats(); connects != 1 || !auth {
		t.Fatalf("upstream saw %d CONNECTs (auth=%v), want 1 with proxy credentials", connects, auth)
	}
}

// A deny rule wins over the upstream: the proxy must decide, and must not hand the
// destination to a hop that would not check it again.
func TestUpstreamHTTPProxyNotUsedWhenDenied(t *testing.T) {
	echo := tcpEcho(t)
	upstream := newHTTPUpstream(t, "", "")
	proxy := startProxy(t, netpolicy.ProxyConfig{
		Mode:     permission.NetProxy,
		Upstream: upstream.url("", ""),
		Rules:    []permission.NetRule{{Action: permission.NetDeny, Host: allowLocalhost}},
	})

	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo, echo); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if connects, _ := upstream.stats(); connects != 0 {
		t.Fatalf("upstream saw %d CONNECTs for a denied destination", connects)
	}
}

// The socks5 upstream path uses its own dialer and credentials; a wrong password
// must fail rather than fall through to a direct dial.
func TestUpstreamSOCKS5WithAuth(t *testing.T) {
	echo := tcpEcho(t)
	upstream := newSOCKS5Upstream(t, "sock-user", "sock-pass")
	proxy := startProxy(t, netpolicy.ProxyConfig{
		Mode:     permission.NetProxy,
		Upstream: upstream.url("sock-user", "sock-pass"),
		Rules:    []permission.NetRule{{Action: permission.NetAllow, Host: allowLocalhost}},
	})

	conn := connect(t, proxy, echo)
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through the socks5 upstream: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echoed %q", buf)
	}
	if dials, user := upstream.stats(); dials != 1 || user != "sock-user" {
		t.Fatalf("socks5 upstream saw %d dials as %q", dials, user)
	}
}

// Absolute-URI requests (no CONNECT) take the same decision path, and a denial is
// a 403 rather than a forwarded request.
func TestPlainHTTPForwarding(t *testing.T) {
	hits := &atomic.Int64{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "plain:"+r.URL.Path)
	}))
	t.Cleanup(origin.Close)

	proxy := startProxy(t, netpolicy.ProxyConfig{
		Mode:  permission.NetAllowList,
		Rules: []permission.NetRule{{Action: permission.NetAllow, Host: allowLocalhost}},
	})

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL(t, proxy))}}
	defer client.CloseIdleConnections()

	resp, err := client.Get(origin.URL + "/forwarded")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "plain:/forwarded" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	if hits.Load() != 1 {
		t.Fatalf("origin saw %d requests, want 1", hits.Load())
	}

	// The same request to a denied host never leaves the proxy.
	denied := startProxy(t, netpolicy.ProxyConfig{
		Mode:  permission.NetAllowList,
		Rules: []permission.NetRule{{Action: permission.NetDeny, Host: allowLocalhost}},
	})
	client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL(t, denied))}}
	defer client.CloseIdleConnections()
	resp, err = client.Get(origin.URL + "/denied")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d for a denied host, want 403", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("origin saw %d requests after a denial, want still 1", hits.Load())
	}
}

func proxyURL(t *testing.T, proxy *netpolicy.Proxy) *url.URL {
	t.Helper()
	u, err := url.Parse("http://" + proxy.Addr().String())
	if err != nil {
		t.Fatalf("parse proxy address: %v", err)
	}
	return u
}

// tcpEcho is a TCP echo server: the raw-tunnel assertion needs a destination that
// is not TLS, so a terminated connection fails loudly instead of quietly working.
func tcpEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// httpUpstream is a minimal HTTP forward proxy: it records CONNECTs (and whether
// the proxy authenticated), answers 200, and tunnels.
type httpUpstream struct {
	ln       net.Listener
	mu       sync.Mutex
	connects int
	auth     bool
}

func newHTTPUpstream(t *testing.T, _, _ string) *httpUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &httpUpstream{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go u.serve(conn)
		}
	}()
	return u
}

func (u *httpUpstream) url(user, pass string) string {
	if user == "" {
		return "http://" + u.ln.Addr().String()
	}
	return fmt.Sprintf("http://%s:%s@%s", user, pass, u.ln.Addr().String())
}

func (u *httpUpstream) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	target := ""
	if fields := strings.Fields(line); len(fields) >= 2 {
		target = fields[1]
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if h == "\r\n" || h == "\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(h), "proxy-authorization:") {
			u.mu.Lock()
			u.auth = true
			u.mu.Unlock()
		}
	}
	u.mu.Lock()
	u.connects++
	u.mu.Unlock()

	dial, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = dial.Close() }()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	go func() { _, _ = io.Copy(dial, br) }()
	_, _ = io.Copy(conn, dial)
}

func (u *httpUpstream) stats() (int, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.connects, u.auth
}

// socks5Upstream is a minimal SOCKS5 server with username/password auth.
type socks5Upstream struct {
	ln   net.Listener
	user string
	pass string

	mu    sync.Mutex
	dials int
	seen  string
}

func newSOCKS5Upstream(t *testing.T, user, pass string) *socks5Upstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &socks5Upstream{ln: ln, user: user, pass: pass}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go u.serve(conn)
		}
	}()
	return u
}

func (u *socks5Upstream) url(user, pass string) string {
	return fmt.Sprintf("socks5://%s:%s@%s", user, pass, u.ln.Addr().String())
}

func (u *socks5Upstream) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	// Greeting: version, method count, methods. Require username/password.
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x02}); err != nil { // 0x02 = username/password
		return
	}

	// Auth: version, ulen, user, plen, pass.
	ver := make([]byte, 2)
	if _, err := io.ReadFull(br, ver); err != nil {
		return
	}
	user := make([]byte, int(ver[1]))
	if _, err := io.ReadFull(br, user); err != nil {
		return
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(br, plen); err != nil {
		return
	}
	pass := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(br, pass); err != nil {
		return
	}
	if string(user) != u.user || string(pass) != u.pass {
		_, _ = conn.Write([]byte{0x01, 0x01})
		return
	}
	if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
		return
	}

	// Request: version, connect, reserved, atyp, addr, port.
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(br, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return
		}
		addr := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, addr); err != nil {
			return
		}
		host = string(addr)
	case 0x04:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(br, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))

	u.mu.Lock()
	u.dials++
	u.seen = string(user)
	u.mu.Unlock()

	dial, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = dial.Close() }()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	go func() { _, _ = io.Copy(dial, br) }()
	_, _ = io.Copy(conn, dial)
}

func (u *socks5Upstream) stats() (int, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dials, u.seen
}
