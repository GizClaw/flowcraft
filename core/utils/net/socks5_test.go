package net

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestReadSocks5Request covers the greeting and CONNECT parsing for
// every address type.
func TestReadSocks5Request(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(server)
		req, err := readSocks5Request(server, br)
		if err != nil {
			t.Errorf("readSocks5Request: %v", err)
			return
		}
		if req.host != "example.com" || req.port != 443 {
			t.Errorf("req = %+v, want example.com:443", req)
		}
	}()

	if err := writeSocks5Greeting(client); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := writeSocks5Connect(client, "example.com", 443); err != nil {
		t.Fatalf("connect request: %v", err)
	}
	<-done
}

func TestReadSocks5RequestIPv6AndCIDRHost(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(server)
		req, err := readSocks5Request(server, br)
		if err != nil {
			t.Errorf("readSocks5Request: %v", err)
			return
		}
		if req.host != "2001:db8::1" || req.port != 8443 {
			t.Errorf("req = %+v, want [2001:db8::1]:8443", req)
		}
	}()

	if err := writeSocks5Greeting(client); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	req := []byte{
		socks5Version, socks5CmdConnect, 0x00, socks5AtypIPv6,
		0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
		0x20, 0xfb, // 8443
	}
	if _, err := client.Write(req); err != nil {
		t.Fatalf("request: %v", err)
	}
	<-done
}

func TestReadSocks5RequestRejectsBadVersion(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(server)
		_, err := readSocks5Request(server, br)
		if err == nil || !strings.Contains(err.Error(), "bad version") {
			t.Errorf("err = %v, want bad version", err)
		}
	}()
	if _, err := client.Write([]byte{0x04, 0x01, 0x00}); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-done
}

func TestReadSocks5RequestRejectsNonConnectCommand(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(server)
		_, err := readSocks5Request(server, br)
		if err == nil || !strings.Contains(err.Error(), "unsupported command") {
			t.Errorf("err = %v, want unsupported command", err)
		}
	}()

	if err := writeSocks5Greeting(client); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if _, err := client.Write([]byte{
		socks5Version, socks5CmdUDP, 0x00, socks5AtypIPv4,
		127, 0, 0, 1, 0, 53,
	}); err != nil {
		t.Fatalf("request: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("read command reply: %v", err)
	}
	if reply[1] != socks5CmdUnsup {
		t.Fatalf("reply code = 0x%02x, want 0x07", reply[1])
	}
	<-done
}

func TestReadSocks5RequestRejectsNoAuthMethod(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(server)
		_, err := readSocks5Request(server, br)
		if err == nil || !strings.Contains(err.Error(), "no acceptable auth") {
			t.Errorf("err = %v, want no acceptable auth", err)
		}
	}()
	if _, err := client.Write([]byte{socks5Version, 0x01, 0x02}); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("read method reply: %v", err)
	}
	if reply[1] != socks5NoAccept {
		t.Fatalf("method reply = 0x%02x, want 0xff", reply[1])
	}
	<-done
}

// TestSocks5ServerTunnel drives a full SOCKS5 session through the
// connect callback and verifies bytes flow both ways.
func TestSocks5ServerTunnel(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer func() { _ = echo.Close() }()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	target := echo.Addr().String()

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	srv := &socks5Server{connect: func(_ context.Context, hostport string) (net.Conn, error) {
		if hostport != target {
			return nil, fmt.Errorf("dial %q, want %q", hostport, target)
		}
		return net.Dial("tcp", target)
	}}
	go srv.Serve(server, bufio.NewReader(server))

	if err := writeSocks5Greeting(client); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := writeSocks5Connect(client, "127.0.0.1", echo.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatalf("connect: %v", err)
	}
	code, err := readSocks5ReplyCode(client)
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if code != socks5Success {
		t.Fatalf("reply code = 0x%02x, want 0x00", code)
	}

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
}

// TestSocks5ServerTunnelPipelinedData sends the greeting, CONNECT
// request, and the first payload bytes in a single write, exactly as a
// client that does not wait for the success reply would. The payload
// must reach the upstream — regresses the classifier-buffered-byte
// drop where the tunnel copied from conn instead of br.
func TestSocks5ServerTunnelPipelinedData(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer func() { _ = echo.Close() }()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	srv := &socks5Server{connect: func(_ context.Context, hostport string) (net.Conn, error) {
		return net.Dial("tcp", hostport)
	}}
	go srv.Serve(server, bufio.NewReader(server))

	payload := []byte("pipelined-payload")
	host, portStr, err := net.SplitHostPort(echo.Addr().String())
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("echo port: %v", err)
	}
	req := []byte{socks5Version, 0x01, socks5NoAuth}
	req = append(req, socks5Version, socks5CmdConnect, 0x00, socks5AtypIPv4)
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	req = append(req, payload...)
	if _, err := client.Write(req); err != nil {
		t.Fatalf("write pipelined request: %v", err)
	}

	greet := make([]byte, 2)
	if _, err := io.ReadFull(client, greet); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	if greet[1] != socks5NoAuth {
		t.Fatalf("greeting reply = 0x%02x, want 0x00", greet[1])
	}
	code, err := readSocks5ReplyCode(client)
	if err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if code != socks5Success {
		t.Fatalf("reply code = 0x%02x, want 0x00", code)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
}

// TestSocks5ServerDenied verifies a failed connect callback surfaces
// as a generic SOCKS5 failure reply.
func TestSocks5ServerDenied(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	srv := &socks5Server{connect: func(_ context.Context, _ string) (net.Conn, error) {
		return nil, errDestinationNotAllowed
	}}
	go srv.Serve(server, bufio.NewReader(server))

	if err := writeSocks5Greeting(client); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := writeSocks5Connect(client, "blocked.example", 443); err != nil {
		t.Fatalf("connect: %v", err)
	}
	code, err := readSocks5ReplyCode(client)
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if code != socks5Failure {
		t.Fatalf("reply code = 0x%02x, want 0x01", code)
	}
}

func TestSocks5ServerHandshakeTimeout(t *testing.T) {
	old := socks5HandshakeTimeout
	socks5HandshakeTimeout = 100 * time.Millisecond
	defer func() { socks5HandshakeTimeout = old }()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	srv := &socks5Server{connect: func(_ context.Context, _ string) (net.Conn, error) {
		return nil, errors.New("must not dial")
	}}
	done := make(chan struct{})
	go func() {
		srv.Serve(server, bufio.NewReader(server))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not close an idle connection")
	}
}

// writeSocks5Greeting sends a no-auth greeting and reads the method
// selection reply.
func writeSocks5Greeting(c net.Conn) error {
	if _, err := c.Write([]byte{socks5Version, 0x01, socks5NoAuth}); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(c, reply); err != nil {
		return err
	}
	if reply[0] != socks5Version || reply[1] != socks5NoAuth {
		return fmt.Errorf("greeting reply = %v", reply)
	}
	return nil
}

// writeSocks5Connect sends a domain or IPv4 CONNECT request.
func writeSocks5Connect(c net.Conn, host string, port int) error {
	var req []byte
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		req = []byte{socks5Version, socks5CmdConnect, 0x00, socks5AtypIPv4}
		req = append(req, ip.To4()...)
	} else {
		req = []byte{socks5Version, socks5CmdConnect, 0x00, socks5AtypDomain, byte(len(host))}
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	_, err := c.Write(req)
	return err
}

func readSocks5ReplyCode(c net.Conn) (byte, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		return 0, err
	}
	if buf[0] != socks5Version {
		return 0, fmt.Errorf("reply version = 0x%02x", buf[0])
	}
	atyp := buf[3]
	addrLen := 4
	switch atyp {
	case socks5AtypIPv6:
		addrLen = 16
	case socks5AtypDomain:
		addrLen = 1
	}
	skip := make([]byte, addrLen+2)
	if _, err := io.ReadFull(c, skip); err != nil {
		return 0, err
	}
	return buf[1], nil
}

// TestWriteSocks5ReplyShape is a tiny sanity check for the reply
// layout used by readSocks5ReplyCode in the session tests.
func TestWriteSocks5ReplyShape(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = c.Close() }()
	defer func() { _ = s.Close() }()
	done := make(chan byte, 1)
	go func() {
		code, err := readSocks5ReplyCode(c)
		if err != nil {
			t.Errorf("read reply: %v", err)
		}
		done <- code
	}()
	if err := writeSocks5Reply(s, socks5Success, nil); err != nil {
		t.Fatalf("write reply: %v", err)
	}
	if code := <-done; code != socks5Success {
		t.Fatalf("code = 0x%02x, want 0x00", code)
	}
}
