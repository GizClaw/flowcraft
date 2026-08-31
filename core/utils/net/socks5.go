package net

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKS5 protocol constants (RFC 1928), kept in one place because the
// proxy multiplexes SOCKS5 and HTTP on the same listener.
const (
	socks5Version    = 0x05
	socks5NoAuth     = 0x00
	socks5NoAccept   = 0xff
	socks5CmdConnect = 0x01
	socks5CmdBind    = 0x02
	socks5CmdUDP     = 0x03
	socks5AtypIPv4   = 0x01
	socks5AtypDomain = 0x03
	socks5AtypIPv6   = 0x04
	socks5Success    = 0x00
	socks5Failure    = 0x01
	socks5CmdUnsup   = 0x07
)

// socks5HandshakeTimeout bounds how long a client may take to send the
// greeting and CONNECT request. Once the request is parsed the deadline
// is cleared: the tunnel itself is unbounded like HTTP CONNECT. It is a
// variable (not a const) so tests can shrink it.
var socks5HandshakeTimeout = 15 * time.Second

// socks5Request is one parsed CONNECT request.
type socks5Request struct {
	host string
	port int
}

// socks5Server serves one SOCKS5 session: no-auth greeting, CONNECT
// only, policy enforcement delegated to the connect callback. It is
// protocol-only — allow/deny, auditing, upstream selection, and the
// HTTP OnConnect hook all live in the proxy's connect closure.
type socks5Server struct {
	// connect dials a policy-allowed CONNECT target. A non-nil error
	// is reported to the client as a generic SOCKS5 failure.
	connect func(ctx context.Context, hostport string) (net.Conn, error)
}

// Serve runs the SOCKS5 handshake on conn (reading through br, which
// may already hold classifier-buffered bytes) and proxies the tunnel.
// conn is always closed before Serve returns.
func (s *socks5Server) Serve(conn net.Conn, br *bufio.Reader) {
	defer func() { _ = conn.Close() }()
	if s.connect == nil {
		return
	}
	if br == nil {
		br = bufio.NewReader(conn)
	}
	if err := conn.SetDeadline(time.Now().Add(socks5HandshakeTimeout)); err != nil {
		return
	}
	req, err := readSocks5Request(conn, br)
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	hostport := net.JoinHostPort(req.host, strconv.Itoa(req.port))
	up, err := s.connect(context.Background(), hostport)
	if err != nil {
		_ = writeSocks5Reply(conn, socks5Failure, nil)
		return
	}
	defer func() { _ = up.Close() }()
	if err := writeSocks5Reply(conn, socks5Success, up.LocalAddr()); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(up, conn)
		_ = up.Close()
	}()
	_, _ = io.Copy(conn, up)
}

// readSocks5Request performs the no-auth SOCKS5 greeting and parses
// the CONNECT request. Non-CONNECT commands get an explicit
// "command not supported" reply before the connection is dropped.
func readSocks5Request(conn net.Conn, br *bufio.Reader) (socks5Request, error) {
	var req socks5Request

	// Greeting: VER, NMETHODS, METHODS[].
	ver, err := br.ReadByte()
	if err != nil {
		return req, err
	}
	if ver != socks5Version {
		return req, fmt.Errorf("socks5: bad version 0x%02x", ver)
	}
	nmethods, err := br.ReadByte()
	if err != nil {
		return req, err
	}
	methods := make([]byte, int(nmethods))
	if _, err := io.ReadFull(br, methods); err != nil {
		return req, err
	}
	reply := []byte{socks5Version, socks5NoAccept}
	for _, m := range methods {
		if m == socks5NoAuth {
			reply = []byte{socks5Version, socks5NoAuth}
			break
		}
	}
	if _, err := conn.Write(reply); err != nil {
		return req, err
	}
	if reply[1] != socks5NoAuth {
		return req, errors.New("socks5: no acceptable auth method")
	}

	// Request: VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT.
	head := make([]byte, 3)
	if _, err := io.ReadFull(br, head); err != nil {
		return req, err
	}
	if head[0] != socks5Version {
		return req, fmt.Errorf("socks5: bad request version 0x%02x", head[0])
	}
	if head[1] != socks5CmdConnect {
		_ = writeSocks5Reply(conn, socks5CmdUnsup, nil)
		return req, fmt.Errorf("socks5: unsupported command 0x%02x", head[1])
	}
	atyp, err := br.ReadByte()
	if err != nil {
		return req, err
	}
	switch atyp {
	case socks5AtypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return req, err
		}
		req.host = net.IPv4(b[0], b[1], b[2], b[3]).String()
	case socks5AtypDomain:
		n, err := br.ReadByte()
		if err != nil {
			return req, err
		}
		if n == 0 {
			return req, errors.New("socks5: empty domain")
		}
		b := make([]byte, int(n))
		if _, err := io.ReadFull(br, b); err != nil {
			return req, err
		}
		req.host = string(b)
	case socks5AtypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return req, err
		}
		req.host = net.IP(b).String()
	default:
		return req, fmt.Errorf("socks5: unsupported address type 0x%02x", atyp)
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return req, err
	}
	req.port = int(binary.BigEndian.Uint16(portBytes))
	if req.port == 0 {
		return req, errors.New("socks5: zero destination port")
	}
	return req, nil
}

// writeSocks5Reply sends one SOCKS5 reply. When bind is non-nil its
// TCP address is reported as the bound address; otherwise 0.0.0.0:0
// is used (clients ignore BND.ADDR for CONNECT).
func writeSocks5Reply(w io.Writer, code byte, bind net.Addr) error {
	atyp := byte(socks5AtypIPv4)
	addr := []byte{0, 0, 0, 0}
	port := uint16(0)
	if tcp, ok := bind.(*net.TCPAddr); ok {
		if ip4 := tcp.IP.To4(); ip4 != nil {
			addr = ip4
		} else if ip6 := tcp.IP.To16(); ip6 != nil {
			atyp = socks5AtypIPv6
			addr = ip6
		}
		port = uint16(tcp.Port)
	}
	buf := make([]byte, 0, 10)
	buf = append(buf, socks5Version, code, 0x00, atyp)
	buf = append(buf, addr...)
	buf = append(buf, byte(port>>8), byte(port))
	_, err := w.Write(buf)
	return err
}
