package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"

	"github.com/GizClaw/flowcraft/backends/sandbox/internal/hostmatch"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"golang.org/x/net/http2"
)

// DefaultMaxBodyBytes caps buffered bodies when
// MITMPolicy.MaxBodyBytes is zero.
const DefaultMaxBodyBytes = 64 << 10

// Engine terminates TLS for one CONNECT tunnel and runs decrypted
// requests through hooks and an outbound transport. One Engine is
// shared by all tunnels of one proxy instance; it is safe for
// concurrent use.
type Engine struct {
	policy   *sandbox.MITMPolicy
	hooks    ProxyHooks
	ca       *CA
	roots    *x509.CertPool
	dial     func(ctx context.Context, hostport string) (net.Conn, error)
	cap      int64
	includes *hostmatch.Set
	excludes *hostmatch.Set
}

// errBlocked marks a hook refusal: the caller writes 403 and ends the
// tunnel.
var errBlocked = errors.New("mitm: blocked by hook")

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
	includes, err := hostmatch.New(policy.Hosts)
	if err != nil {
		return nil, fmt.Errorf("mitm: hosts: %w", err)
	}
	excludes, err := hostmatch.New(policy.ExcludeHosts)
	if err != nil {
		return nil, fmt.Errorf("mitm: exclude_hosts: %w", err)
	}
	return &Engine{
		policy:   policy,
		hooks:    hooks,
		ca:       ca,
		roots:    roots,
		dial:     dial,
		cap:      capBytes,
		includes: includes,
		excludes: excludes,
	}, nil
}

// PEM exposes the temporary root CA for bundle injection.
func (e *Engine) PEM() []byte { return e.ca.PEM() }

// ShouldMITM reports whether a CONNECT host is subject to TLS
// termination under the policy's host selection. Excludes always win;
// an empty include list means "all hosts". Hosts that bypass MITM get
// a raw tunnel (allow/deny rules still apply; content hooks do not).
func (e *Engine) ShouldMITM(host string) bool {
	if e.excludes != nil && mitmSetMatches(e.excludes, host) {
		return false
	}
	if e.includes != nil && !e.includes.Empty() && !mitmSetMatches(e.includes, host) {
		return false
	}
	return true
}

// mitmSetMatches evaluates a host against a pattern set, routing IP
// literals through MatchIP so "127.0.0.1" entries work as expected.
func mitmSetMatches(s *hostmatch.Set, host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return s.MatchIP(ip.Unmap())
	}
	return s.Match(host)
}

// Serve terminates TLS on client for serverName and forwards decrypted
// HTTP/1.1 or HTTP/2 requests — the client's ALPN choice decides — to
// dialHostport (TLS-verified against the real target). It returns when
// the client closes the connection or a fatal protocol error occurs.
func (e *Engine) Serve(client net.Conn, serverName, dialHostport string) error {
	leaf, err := e.ca.Leaf(serverName)
	if err != nil {
		return err
	}
	tlsConn := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		// h2 and http/1.1 are both offered; the client picks. h2-only
		// clients (gRPC etc.) work; everything else falls back to h1.
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("mitm: client handshake: %w", err)
	}
	defer func() { _ = tlsConn.Close() }()

	transport := e.newTransport(serverName, dialHostport)
	defer transport.CloseIdleConnections()

	if tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
		h2 := &http2.Server{}
		h2.ServeConn(tlsConn, &http2.ServeConnOpts{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				e.serveH2(w, r, transport, serverName, dialHostport)
			}),
		})
		return nil
	}
	return e.serveH1(tlsConn, transport, serverName, dialHostport)
}

func (e *Engine) newTransport(serverName, dialHostport string) *http.Transport {
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return e.dialTLS(ctx, dialHostport, serverName)
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 2,
	}
}

func (e *Engine) serveH1(tlsConn *tls.Conn, transport *http.Transport, serverName, dialHostport string) error {
	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return nil // client closed or keep-alive ended
		}
		resp, err := e.forward(req, transport, serverName, dialHostport)
		if err == errBlocked {
			writeError(tlsConn, http.StatusForbidden, "mitm: blocked by hook")
			return nil
		}
		if err != nil {
			writeError(tlsConn, http.StatusBadGateway, "mitm: upstream request failed")
			return nil
		}
		closeConn := req.Close || resp.Close
		if err := resp.Write(tlsConn); err != nil {
			return err
		}
		if closeConn {
			return nil
		}
	}
}

func (e *Engine) serveH2(w http.ResponseWriter, r *http.Request, transport *http.Transport, serverName, dialHostport string) {
	resp, err := e.forward(r, transport, serverName, dialHostport)
	if err == errBlocked {
		http.Error(w, "mitm: blocked by hook", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "mitm: upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyResponse(w, resp)
}

// forward runs hooks and the outbound round trip for one decrypted
// request, returning the response ready to write. errBlocked means a
// hook refused the traffic.
func (e *Engine) forward(req *http.Request, transport *http.Transport, serverName, dialHostport string) (*http.Response, error) {
	info, err := e.inspectRequest(req)
	if err != nil {
		return nil, err
	}
	if e.hooks != nil && info != nil {
		if err := e.hooks.OnRequest(context.Background(), info); err != nil {
			return nil, errBlocked
		}
	}
	outReq := req.Clone(context.Background())
	outReq.RequestURI = ""
	outReq.URL.Scheme = "https"
	outReq.URL.Host = dialHostport
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		return nil, err
	}
	respInfo, err := e.inspectResponse(resp)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if e.hooks != nil && respInfo != nil {
		if err := e.hooks.OnResponse(context.Background(), respInfo); err != nil {
			_ = resp.Body.Close()
			return nil, errBlocked
		}
	}
	// The client-facing protocol is whatever the client negotiated;
	// the response framing is produced by resp.Write (h1) or
	// copyResponse (h2), so normalize the status line only.
	resp.Proto = "HTTP/1.1"
	resp.ProtoMajor = 1
	resp.ProtoMinor = 1
	return resp, nil
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	h := w.Header()
	for k, vs := range resp.Trailer {
		for _, v := range vs {
			h.Add(http.TrailerPrefix+k, v)
		}
	}
	copyHeader(h, resp.Header)
	w.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		_, _ = io.Copy(w, resp.Body)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
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
