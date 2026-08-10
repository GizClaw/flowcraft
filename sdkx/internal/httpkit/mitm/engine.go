package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// DefaultMaxBodyBytes caps buffered bodies when
// MITMPolicy.MaxBodyBytes is zero.
const DefaultMaxBodyBytes = 64 << 10

// Engine terminates TLS for one CONNECT tunnel and runs decrypted
// requests through hooks and an outbound transport. One Engine is
// shared by all tunnels of one proxy instance; it is safe for
// concurrent use.
type Engine struct {
	policy *sandbox.MITMPolicy
	hooks  ProxyHooks
	ca     *CA
	roots  *x509.CertPool
	dial   func(ctx context.Context, hostport string) (net.Conn, error)
	cap    int64
}

// New builds an Engine. dial must return a raw (pre-TLS) connection to
// the target — the enforcement proxy's policy-aware dialer. roots
// overrides the system pool used to verify the target's certificate
// (nil means system roots).
func New(policy *sandbox.MITMPolicy, hooks ProxyHooks, dial func(context.Context, string) (net.Conn, error), roots *x509.CertPool) (*Engine, error) {
	if policy == nil {
		return nil, fmt.Errorf("mitm: nil policy")
	}
	ca, err := NewCA()
	if err != nil {
		return nil, err
	}
	capBytes := policy.MaxBodyBytes
	if capBytes <= 0 {
		capBytes = DefaultMaxBodyBytes
	}
	return &Engine{
		policy: policy,
		hooks:  hooks,
		ca:     ca,
		roots:  roots,
		dial:   dial,
		cap:    capBytes,
	}, nil
}

// PEM exposes the temporary root CA for bundle injection.
func (e *Engine) PEM() []byte { return e.ca.PEM() }

// Serve terminates TLS on client for serverName, forwards decrypted
// HTTP/1.1 requests to dialHostport (TLS-verified against the real
// target), and writes responses back. It returns when the client
// closes the connection or a fatal protocol error occurs.
func (e *Engine) Serve(client net.Conn, serverName, dialHostport string) error {
	leaf, err := e.ca.Leaf(serverName)
	if err != nil {
		return err
	}
	tlsConn := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		// v1 is HTTP/1.1 only: no h2 ALPN, so h2-only clients are
		// rejected at handshake and everything else falls back cleanly.
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("mitm: client handshake: %w", err)
	}
	defer func() { _ = tlsConn.Close() }()

	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return e.dialTLS(ctx, dialHostport, serverName)
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 2,
	}
	defer transport.CloseIdleConnections()

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return nil // client closed or keep-alive ended
		}
		info, err := e.inspectRequest(req)
		if err != nil {
			writeError(tlsConn, http.StatusBadGateway, "mitm: read request body")
			return nil
		}
		if e.hooks != nil && info != nil {
			if err := e.hooks.OnRequest(context.Background(), info); err != nil {
				writeError(tlsConn, http.StatusForbidden, "mitm: request blocked by hook")
				return nil
			}
		}
		outReq := req.Clone(context.Background())
		outReq.RequestURI = ""
		outReq.URL.Scheme = "https"
		outReq.URL.Host = dialHostport
		resp, err := transport.RoundTrip(outReq)
		if err != nil {
			writeError(tlsConn, http.StatusBadGateway, "mitm: upstream request failed")
			return nil
		}

		if info, err := e.inspectResponse(resp); err != nil {
			_ = resp.Body.Close()
			writeError(tlsConn, http.StatusBadGateway, "mitm: read response body")
			return nil
		} else if info != nil {
			if e.hooks != nil {
				if hookErr := e.hooks.OnResponse(context.Background(), info); hookErr != nil {
					_ = resp.Body.Close()
					writeError(tlsConn, http.StatusForbidden, "mitm: response blocked by hook")
					return nil
				}
			}
		}

		// The client side is HTTP/1.1 even when the origin negotiated
		// h2; normalize the status line before writing back.
		resp.Proto = "HTTP/1.1"
		resp.ProtoMajor = 1
		resp.ProtoMinor = 1
		closeConn := req.Close || resp.Close
		if err := resp.Write(tlsConn); err != nil {
			return err
		}
		if closeConn {
			return nil
		}
	}
}

// inspectRequest buffers the body only when inspection is on and
// Content-Length is bounded by the cap; otherwise the body is passed
// through untouched and the hook sees Body nil.
func (e *Engine) inspectRequest(req *http.Request) (*RequestInfo, error) {
	if e.hooks == nil {
		return nil, nil
	}
	info := &RequestInfo{
		Method: req.Method,
		Scheme: "https",
		Host:   req.Host,
		Path:   req.URL.RequestURI(),
		Header: req.Header.Clone(),
	}
	if !e.policy.InspectBodies {
		return info, nil
	}
	if req.Body == nil || req.ContentLength < 0 || req.ContentLength > e.cap {
		return info, nil
	}
	consumed, err := io.ReadAll(io.LimitReader(req.Body, e.cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(consumed)) > e.cap {
		// Content-Length lied; do not inspect, but preserve the bytes
		// we already consumed so the upstream body stays intact.
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(consumed), req.Body))
		return info, nil
	}
	info.Body = consumed
	req.Body = io.NopCloser(bytes.NewReader(consumed))
	return info, nil
}

func (e *Engine) inspectResponse(resp *http.Response) (*ResponseInfo, error) {
	if e.hooks == nil {
		return nil, nil
	}
	info := &ResponseInfo{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
	}
	if !e.policy.InspectBodies {
		return info, nil
	}
	if resp.Body == nil || resp.ContentLength < 0 || resp.ContentLength > e.cap {
		return info, nil
	}
	consumed, err := io.ReadAll(io.LimitReader(resp.Body, e.cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(consumed)) > e.cap {
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(consumed), resp.Body))
		return info, nil
	}
	info.Body = consumed
	resp.Body = io.NopCloser(bytes.NewReader(consumed))
	return info, nil
}

func (e *Engine) dialTLS(ctx context.Context, hostport, serverName string) (net.Conn, error) {
	raw, err := e.dial(ctx, hostport)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: serverName,
		RootCAs:    e.roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("mitm: upstream handshake: %w", err)
	}
	return tlsConn, nil
}

func writeError(w io.Writer, status int, msg string) {
	body := msg + "\n"
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}
