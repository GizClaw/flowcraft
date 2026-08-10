package httpkit

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit/mitm"
)

func TestRules_DenyOverridesInProxyMode(t *testing.T) {
	var decisions []ProxyDecision
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "upstream-ok")
	}))
	defer upstream.Close()

	p, err := Start(ProxyConfig{
		Mode:       sandbox.NetProxy,
		Rules:      []sandbox.NetRule{{Action: sandbox.NetDeny, Host: "blocked.example"}},
		Upstream:   upstream.URL,
		OnDecision: func(d ProxyDecision) { decisions = append(decisions, d) },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT blocked.example:443 HTTP/1.1\r\nHost: blocked.example:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for deny rule", resp.StatusCode)
	}
	if len(decisions) != 1 || decisions[0].Action != sandbox.NetDeny || !strings.Contains(decisions[0].Rule, "blocked.example") {
		t.Fatalf("decisions = %+v, want one deny for blocked.example", decisions)
	}
}

func TestDecisionAudit_AllowList(t *testing.T) {
	var decisions []ProxyDecision
	echo := startEchoServer(t)
	defer func() { _ = echo.Close() }()

	p, err := Start(ProxyConfig{
		Mode:       sandbox.NetAllowList,
		AllowHosts: []string{"127.0.0.1"},
		OnDecision: func(d ProxyDecision) { decisions = append(decisions, d) },
	})
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
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if len(decisions) != 1 || decisions[0].Action != sandbox.NetAllow || decisions[0].Port == 0 {
		t.Fatalf("decisions = %+v, want one allow with port", decisions)
	}
}

func TestSocks5Upstream_HTTPForward(t *testing.T) {
	socks := startSocks5Server(t, "user", "pass")
	defer func() { _ = socks.Close() }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "socks-ok")
	}))
	defer srv.Close()

	p, err := Start(ProxyConfig{
		Mode:     sandbox.NetProxy,
		Upstream: "socks5://user:pass@" + socks.Addr().String(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "GET %s/hello HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n",
		srv.URL, strings.TrimPrefix(srv.URL, "http://"))
	resp, body := readResponse(t, bufio.NewReader(conn))
	if resp.StatusCode != http.StatusOK || body != "socks-ok" {
		t.Fatalf("status=%d body=%q, want 200 socks-ok", resp.StatusCode, body)
	}
	if !socks.authed() {
		t.Fatal("socks5 server did not receive credentials")
	}
}

func TestSocks5Upstream_CONNECT(t *testing.T) {
	socks := startSocks5Server(t, "", "")
	defer func() { _ = socks.Close() }()
	echo := startEchoServer(t)
	defer func() { _ = echo.Close() }()

	p, err := Start(ProxyConfig{
		Mode:     sandbox.NetProxy,
		Upstream: "socks5://" + socks.Addr().String(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	target := echo.Addr().String()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("tunnel echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
	if !socks.sawTarget(target) {
		t.Fatalf("socks5 server targets = %v, want %q", socks.targets(), target)
	}
}

func TestHTTPUpstream_CONNECT_SendsAuth(t *testing.T) {
	var (
		mu   sync.Mutex
		auth string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "expect CONNECT", http.StatusBadRequest)
			return
		}
		mu.Lock()
		auth = r.Header.Get("Proxy-Authorization")
		mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			_ = client.Close()
			return
		}
		go func() { _, _ = io.Copy(target, client); _ = target.Close() }()
		_, _ = io.Copy(client, target)
	}))
	defer upstream.Close()

	echo := startEchoServer(t)
	defer func() { _ = echo.Close() }()

	u := strings.Replace(upstream.URL, "http://", "http://user:secret@", 1)
	p, err := Start(ProxyConfig{Mode: sandbox.NetProxy, Upstream: u})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	target := echo.Addr().String()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("tunnel echo: %v", err)
	}
	mu.Lock()
	gotAuth := auth
	mu.Unlock()
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("Proxy-Authorization = %q, want Basic", gotAuth)
	}
}

func TestMITM_HooksAndBodyInspection(t *testing.T) {
	targetCA, targetCert := issueTestCert(t, "127.0.0.1")
	target := startTLSOrigin(t, targetCA, targetCert)
	defer func() { _ = target.Close() }()

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(targetCAPEM(t, targetCA))

	hooks := &recordingHooks{}
	p, err := Start(ProxyConfig{
		Mode:          sandbox.NetAllowList,
		AllowHosts:    []string{"127.0.0.1"},
		MITM:          &sandbox.MITMPolicy{Enabled: true, InspectBodies: true},
		Hooks:         hooks,
		OutboundRoots: roots,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(p.CAPEM())
	if len(p.CAPEM()) == 0 {
		t.Fatal("CAPEM empty with MITM enabled")
	}

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	targetAddr := target.Addr().String()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	tlsConn := tls.Client(conn, &tls.Config{
		RootCAs:    caPool,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client handshake through MITM: %v", err)
	}
	_, _ = fmt.Fprintf(tlsConn, "POST /submit HTTP/1.1\r\nHost: %s\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello", targetAddr)
	br := bufio.NewReader(tlsConn)
	resp, body := readResponse(t, br)
	if resp.StatusCode != http.StatusOK || body != "mitm-ok" {
		t.Fatalf("status=%d body=%q, want 200 mitm-ok", resp.StatusCode, body)
	}

	if hooks.connect == 0 {
		t.Fatal("OnConnect not called")
	}
	if len(hooks.requests) != 1 || string(hooks.requests[0].Body) != "hello" || hooks.requests[0].Path != "/submit" {
		t.Fatalf("requests = %+v, want one inspected POST /submit body=hello", hooks.requests)
	}
	if len(hooks.responses) != 1 || string(hooks.responses[0].Body) != "mitm-ok" {
		t.Fatalf("responses = %+v, want one inspected body=mitm-ok", hooks.responses)
	}
}

func TestMITM_HookBlocksRequest(t *testing.T) {
	targetCA, targetCert := issueTestCert(t, "127.0.0.1")
	target := startTLSOrigin(t, targetCA, targetCert)
	defer func() { _ = target.Close() }()

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(targetCAPEM(t, targetCA))
	hooks := &recordingHooks{blockRequest: true}
	p, err := Start(ProxyConfig{
		Mode:          sandbox.NetAllowList,
		AllowHosts:    []string{"127.0.0.1"},
		MITM:          &sandbox.MITMPolicy{Enabled: true},
		Hooks:         hooks,
		OutboundRoots: roots,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	conn := dialProxy(t, p)
	defer func() { _ = conn.Close() }()
	targetAddr := target.Addr().String()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)
	if _, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect}); err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(p.CAPEM())
	tlsConn := tls.Client(conn, &tls.Config{RootCAs: caPool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	_, _ = fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", targetAddr)
	resp, _ := readResponse(t, bufio.NewReader(tlsConn))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from hook", resp.StatusCode)
	}
}

// --- test helpers ---

func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return echo
}

type socks5Server struct {
	ln         net.Listener
	user       string
	pass       string
	mu         sync.Mutex
	authedFlag bool
	targetList []string
}

func startSocks5Server(t *testing.T, user, pass string) *socks5Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socks5Server{ln: ln, user: user, pass: pass}
	go s.acceptLoop()
	return s
}

func (s *socks5Server) Addr() net.Addr { return s.ln.Addr() }
func (s *socks5Server) Close() error   { return s.ln.Close() }

func (s *socks5Server) authed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authedFlag
}

func (s *socks5Server) sawTarget(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, got := range s.targetList {
		if got == target {
			return true
		}
	}
	return false
}

func (s *socks5Server) targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targetList...)
}

func (s *socks5Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *socks5Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	ver, err := br.ReadByte()
	if err != nil || ver != 5 {
		return
	}
	nmethods, err := br.ReadByte()
	if err != nil {
		return
	}
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if s.user != "" {
		if _, err := conn.Write([]byte{5, 2}); err != nil {
			return
		}
		// RFC 1929 username/password subnegotiation.
		header := make([]byte, 2)
		if _, err := io.ReadFull(br, header); err != nil || header[0] != 1 {
			return
		}
		uname := make([]byte, header[1])
		if _, err := io.ReadFull(br, uname); err != nil {
			return
		}
		plen := make([]byte, 1)
		if _, err := io.ReadFull(br, plen); err != nil {
			return
		}
		passwd := make([]byte, plen[0])
		if _, err := io.ReadFull(br, passwd); err != nil {
			return
		}
		if string(uname) != s.user || string(passwd) != s.pass {
			_, _ = conn.Write([]byte{1, 1})
			return
		}
		_, _ = conn.Write([]byte{1, 0})
		s.mu.Lock()
		s.authedFlag = true
		s.mu.Unlock()
	} else {
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[0] != 5 || req[1] != 1 {
		return
	}
	host, err := readSocksAddr(br, req[3])
	if err != nil {
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return
	}
	target := net.JoinHostPort(host, fmt.Sprintf("%d", int(portBytes[0])<<8|int(portBytes[1])))
	s.mu.Lock()
	s.targetList = append(s.targetList, target)
	s.mu.Unlock()

	upstream, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	// Reply: success, IPv4 0.0.0.0:0.
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(upstream, conn) }()
	_, _ = io.Copy(conn, upstream)
}

func readSocksAddr(br *bufio.Reader, atyp byte) (string, error) {
	switch atyp {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	default:
		return "", fmt.Errorf("unknown atyp %d", atyp)
	}
}

type recordingHooks struct {
	blockConnect bool
	blockRequest bool
	connect      int
	requests     []*mitm.RequestInfo
	mu           sync.Mutex
	responses    []*mitm.ResponseInfo
}

func (h *recordingHooks) OnConnect(context.Context, *mitm.ConnectInfo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connect++
	if h.blockConnect {
		return fmt.Errorf("blocked")
	}
	return nil
}

func (h *recordingHooks) OnRequest(_ context.Context, req *mitm.RequestInfo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req)
	if h.blockRequest {
		return fmt.Errorf("blocked")
	}
	return nil
}

func (h *recordingHooks) OnResponse(_ context.Context, resp *mitm.ResponseInfo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.responses = append(h.responses, resp)
	return nil
}

func issueTestCert(t *testing.T, host string) (*x509.Certificate, *tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP(host)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return caCert, &tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey, Leaf: leafCert}
}

func targetCAPEM(t *testing.T, ca *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
}

func startTLSOrigin(t *testing.T, _ *x509.Certificate, cert *tls.Certificate) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprint(w, "mitm-ok")
	})}
	go func() { _ = srv.Serve(tlsLn) }()
	return &closingListener{Listener: tlsLn, closer: func() { _ = srv.Close() }}
}

type closingListener struct {
	net.Listener
	closer func()
}

func (l *closingListener) Close() error {
	l.closer()
	return l.Listener.Close()
}
