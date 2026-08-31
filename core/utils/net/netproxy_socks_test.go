package net

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestProxySocks5AndHTTPShareListener proves the enforcement proxy
// multiplexes SOCKS5 and HTTP on one address: a SOCKS5 CONNECT
// tunnel, an HTTP GET through the same port, and the unix-socket
// listener (the bwrap bridge path) all enforce the same allow-list.
func TestProxySocks5AndHTTPShareListener(t *testing.T) {
	echo := newEchoServer(t)
	defer echo.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()
	upURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	upHost, upPort := splitHostPort(upURL.Host, 80)
	echoHost, echoPort := splitHostPort(echo.Addr().String(), 0)

	var decisions []ProxyDecision
	var mu sync.Mutex
	proxy, err := Start(ProxyConfig{
		Mode: NetAllowList,
		Rules: []NetRule{
			{Action: NetAllow, Host: upHost, Port: upPort},
			{Action: NetAllow, Host: echoHost, Port: echoPort},
			{Action: NetDeny, Host: "blocked.example", Reason: "denied by test"},
		},
		TCPLoopback: true,
		OnDecision: func(d ProxyDecision) {
			mu.Lock()
			decisions = append(decisions, d)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer proxy.Close()

	// SOCKS5 tunnel to the allow-listed upstream.
	conn, err := socks5Dial(proxy.Addr().String(), echo.Addr().String())
	if err != nil {
		t.Fatalf("socks5 dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("socks-ok")); err != nil {
		t.Fatalf("socks write: %v", err)
	}
	got := make([]byte, len("socks-ok"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("socks read: %v", err)
	}
	if string(got) != "socks-ok" {
		t.Fatalf("echo = %q, want socks-ok", got)
	}

	// SOCKS5 CONNECT to a denied host must be refused.
	denied, err := socks5DialCode(proxy.Addr().String(), "blocked.example:443")
	if err != nil {
		t.Fatalf("denied dial: %v", err)
	}
	defer denied.Close()
	code, err := readSocks5ReplyCode(denied)
	if err != nil {
		t.Fatalf("denied reply: %v", err)
	}
	if code != socks5Failure {
		t.Fatalf("denied code = 0x%02x, want 0x01", code)
	}

	// Plain HTTP through the same listener.
	httpProxy, err := url.Parse("http://" + proxy.Addr().String())
	if err != nil {
		t.Fatalf("parse http proxy: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(httpProxy)}}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("http through proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "upstream-ok" {
		t.Fatalf("http body = %q, want upstream-ok", body)
	}

	// Unix-socket listener (bwrap bridge path) serves SOCKS5 too.
	udsProxy, err := Start(ProxyConfig{
		Mode: NetAllowList,
		Rules: []NetRule{
			{Action: NetAllow, Host: echoHost, Port: echoPort},
		},
		OnDecision: func(ProxyDecision) {},
	})
	if err != nil {
		t.Fatalf("Start uds: %v", err)
	}
	defer udsProxy.Close()
	udsConn, err := socks5DialUnix(udsProxy.SocketPath(), echo.Addr().String())
	if err != nil {
		t.Fatalf("uds socks5 dial: %v", err)
	}
	defer udsConn.Close()
	if _, err := udsConn.Write([]byte("uds-ok")); err != nil {
		t.Fatalf("uds write: %v", err)
	}
	gotUDS := make([]byte, len("uds-ok"))
	if _, err := io.ReadFull(udsConn, gotUDS); err != nil {
		t.Fatalf("uds read: %v", err)
	}
	if string(gotUDS) != "uds-ok" {
		t.Fatalf("uds echo = %q, want uds-ok", gotUDS)
	}

	// The deny decision carried the rule's model-facing reason.
	mu.Lock()
	defer mu.Unlock()
	var deny *ProxyDecision
	for i := range decisions {
		if decisions[i].Action == NetDeny && decisions[i].Rule != "" {
			deny = &decisions[i]
		}
	}
	if deny == nil {
		t.Fatalf("no deny decision recorded: %+v", decisions)
	}
	if deny.Reason != "denied by test" {
		t.Fatalf("deny reason = %q, want denied by test", deny.Reason)
	}
}

// TestProxySocks5DenyReasonBody proves an HTTP denial response carries
// the deny rule's reason (srt's deniedDomainReasons behavior).
func TestProxySocks5DenyReasonBody(t *testing.T) {
	proxy, err := Start(ProxyConfig{
		Mode: NetAllowList,
		Rules: []NetRule{
			{Action: NetDeny, Host: "blocked.example", Reason: "blocked for the demo"},
		},
		TCPLoopback: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer proxy.Close()

	httpProxy, err := url.Parse("http://" + proxy.Addr().String())
	if err != nil {
		t.Fatalf("parse proxy: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(httpProxy)},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get("http://blocked.example/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "blocked for the demo") {
		t.Fatalf("deny body = %q, want reason", body)
	}
}

// socks5Dial opens a SOCKS5 tunnel to hostport through the proxy at
// proxyAddr, asserting the server accepted it.
func socks5Dial(proxyAddr, hostport string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := writeSocks5Greeting(conn); err != nil {
		conn.Close()
		return nil, err
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		conn.Close()
		return nil, err
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bad port %q: %w", port, err)
	}
	if err := writeSocks5Connect(conn, host, portNum); err != nil {
		conn.Close()
		return nil, err
	}
	code, err := readSocks5ReplyCode(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if code != socks5Success {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect refused: code 0x%02x", code)
	}
	return conn, nil
}

// socks5DialCode is socks5Dial without the success assertion: the
// caller reads the reply code itself.
func socks5DialCode(proxyAddr, hostport string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := writeSocks5Greeting(conn); err != nil {
		conn.Close()
		return nil, err
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		conn.Close()
		return nil, err
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bad port %q: %w", port, err)
	}
	if err := writeSocks5Connect(conn, host, portNum); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// socks5DialUnix is socks5Dial over a unix socket.
func socks5DialUnix(sockPath, hostport string) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if err := writeSocks5Greeting(conn); err != nil {
		conn.Close()
		return nil, err
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		conn.Close()
		return nil, err
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bad port %q: %w", port, err)
	}
	if err := writeSocks5Connect(conn, host, portNum); err != nil {
		conn.Close()
		return nil, err
	}
	code, err := readSocks5ReplyCode(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if code != socks5Success {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect refused: code 0x%02x", code)
	}
	return conn, nil
}

// newEchoServer starts a TCP echo server on loopback.
func newEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln
}
