// This file carries the sandbox enforcement proxy (host side) that
// turns sandbox.NetAllowList / sandbox.NetProxy into a single enforced
// network path. It runs on the host (outside any sandbox) and is the
// sandboxed child's only network egress. It supports two listening
// surfaces:
//
//   - unix socket (default): the bwrap backend bind-mounts the socket
//     into the sandbox, because its netns hides the host loopback and
//     a bridge process pipes the netns loopback to the socket;
//   - TCP loopback (ProxyConfig.TCPLoopback): the seatbelt backend,
//     whose child shares the host network stack and dials
//     127.0.0.1:<port> directly under an SBPL loopback-only rule.
//
// It is part of httpkit because the proxy is plain HTTP machinery: an
// http.Handler with allow-list / upstream policy over a listener.
//
// Trust boundary: the proxy is spawned by the host application and is
// the sandboxed child's only network egress. A compromised child can
// reach exactly the destinations the proxy's allow-list (or upstream)
// permits — that is the configured policy, not a sandbox escape. The
// proxy must therefore be pinned and audited like any trust anchor.
package httpkit

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// ProxyConfig configures one enforcement proxy instance. It is named
// ProxyConfig (not Config) because httpkit already owns Config for the
// transport builder.
type ProxyConfig struct {
	// Mode is sandbox.NetAllowList or sandbox.NetProxy.
	Mode sandbox.NetMode
	// AllowHosts is the allow-list: hostname suffixes ("example.com"
	// matches "api.example.com") and exact IP literals. Ignored in
	// proxy mode.
	AllowHosts []string
	// Upstream is the proxy-mode upstream URL ("http://host:port").
	// Ignored in allow-list mode.
	Upstream string
	// TCPLoopback binds 127.0.0.1:0 (an ephemeral loopback TCP port)
	// instead of a unix socket. The seatbelt backend uses this because
	// its child shares the host network stack and reaches the proxy
	// through the single SBPL-allowed loopback port. The bwrap backend
	// leaves it false and uses the unix socket for its bridge.
	TCPLoopback bool
}

// Proxy is a host-side enforcement proxy listening on a unix socket
// (default) or TCP loopback (ProxyConfig.TCPLoopback). Create it with
// Start and stop it with Close.
type Proxy struct {
	cfg       ProxyConfig
	ln        net.Listener
	srv       *http.Server
	transport *http.Transport
	dir       string
	path      string
}

// Start serves the enforcement proxy in the background and returns
// immediately. With ProxyConfig.TCPLoopback it binds an ephemeral
// loopback TCP port (Addr reports it); otherwise it binds a unix
// socket in a fresh per-run temp directory (mode 0600) whose path is
// available via SocketPath for bind-mounting into a sandbox.
func Start(cfg ProxyConfig) (*Proxy, error) {
	p := &Proxy{
		cfg:       cfg,
		transport: buildTransport(cfg),
	}
	if cfg.TCPLoopback {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("netproxy: listen tcp loopback: %w", err)
		}
		p.ln = ln
	} else {
		dir, err := os.MkdirTemp("", "flowcraft-netproxy-")
		if err != nil {
			return nil, fmt.Errorf("netproxy: temp dir: %w", err)
		}
		path := filepath.Join(dir, "proxy.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("netproxy: listen: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			_ = ln.Close()
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("netproxy: chmod socket: %w", err)
		}
		p.ln = ln
		p.dir = dir
		p.path = path
	}
	p.srv = &http.Server{Handler: p}
	go func() { _ = p.srv.Serve(p.ln) }()
	return p, nil
}

// SocketPath returns the unix socket path to bind into the sandbox.
// It is empty when the proxy was started with TCPLoopback.
func (p *Proxy) SocketPath() string { return p.path }

// Addr returns the bound listener address. For a TCP-loopback proxy
// this is the 127.0.0.1:<port> the sandboxed child must be allowed to
// dial; for a unix-socket proxy it is the socket path address.
func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

// Close stops the server, closes the listener, and removes the temp
// directory (including the socket file) when one was created.
func (p *Proxy) Close() error {
	_ = p.srv.Close()
	_ = p.ln.Close()
	p.transport.CloseIdleConnections()
	if p.dir != "" {
		return os.RemoveAll(p.dir)
	}
	return nil
}

// buildTransport returns the outbound transport. Proxy is set
// explicitly in both modes so Go's ProxyFromEnvironment never leaks
// the host application's proxy settings into the sandbox path: direct
// dial for allow-list mode, the configured upstream for proxy mode.
func buildTransport(cfg ProxyConfig) *http.Transport {
	if cfg.Upstream != "" {
		if u, err := url.Parse(cfg.Upstream); err == nil {
			return &http.Transport{Proxy: http.ProxyURL(u)}
		}
	}
	return &http.Transport{Proxy: nil}
}

// ServeHTTP implements http.Handler for both HTTP absolute-form
// requests and CONNECT tunnels.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	p.serveHTTP(w, r)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.allowed(r.URL.Host) {
		http.Error(w, "netproxy: destination not allowed", http.StatusForbidden)
		return
	}
	resp, err := p.transport.RoundTrip(r)
	if err != nil {
		http.Error(w, "netproxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	if !p.allowed(r.Host) {
		http.Error(w, "netproxy: destination not allowed", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "netproxy: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()

	target, err := p.dialTarget(r.Host)
	if err != nil {
		return
	}
	defer func() { _ = target.Close() }()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	go func() { _, _ = io.Copy(target, client); _ = target.Close() }()
	_, _ = io.Copy(client, target)
}

// dialTarget dials the CONNECT target directly (allow-list mode) or
// establishes a CONNECT tunnel through the upstream (proxy mode).
func (p *Proxy) dialTarget(hostport string) (net.Conn, error) {
	if p.cfg.Upstream != "" {
		u, err := url.Parse(p.cfg.Upstream)
		if err != nil {
			return nil, err
		}
		up, err := net.Dial("tcp", u.Host)
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Fprintf(up, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostport, hostport); err != nil {
			_ = up.Close()
			return nil, err
		}
		br := bufio.NewReader(up)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			_ = up.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = up.Close()
			return nil, fmt.Errorf("netproxy: upstream refused CONNECT: %d", resp.StatusCode)
		}
		return up, nil
	}
	return net.Dial("tcp", hostport)
}

// allowed evaluates the allow-list. Hostname entries are suffixes
// ("example.com" matches "api.example.com"); IP literals match exactly
// so a dotted IP cannot suffix-match a longer string.
func (p *Proxy) allowed(hostport string) bool {
	if p.cfg.Mode == sandbox.NetProxy {
		return true // upstream is the policy
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	for _, entry := range p.cfg.AllowHosts {
		if net.ParseIP(entry) != nil {
			if entry == host {
				return true
			}
			continue
		}
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
