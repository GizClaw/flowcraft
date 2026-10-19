package net

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestUpstreamConnectKeepsBytesAfterTheResponse drives the HTTP-upstream
// path with a proxy that writes its 200 and the first tunnel bytes in one
// write, the way a fast upstream does.
func TestUpstreamConnectKeepsBytesAfterTheResponse(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\npayload")
		// Hold the tunnel open until the proxy closes it, which it does when
		// the client goes away.
		_, _ = io.Copy(io.Discard, br)
	}()

	proxy, err := Start(ProxyConfig{
		Mode:        NetProxy,
		Upstream:    "http://" + upstream.Addr().String(),
		TCPLoopback: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proxy.Close() }()

	client, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read proxy reply: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy reply = %d", resp.StatusCode)
	}
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	got := make([]byte, len("payload"))
	n, err := io.ReadFull(br, got)
	if err != nil {
		t.Fatalf("read tunnel bytes: %v (got %q)", err, got[:n])
	}
	if string(got) != "payload" {
		t.Fatalf("tunnel bytes = %q, want payload", got)
	}
}
