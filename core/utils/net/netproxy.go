package net

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	"golang.org/x/net/proxy"
)

// MITMConnectInfo describes one CONNECT attempt before tunnel or TLS
// termination.
type MITMConnectInfo struct {
	Host string
	Port int
}

// MITMRequestInfo is a decrypted request as seen by hooks.
type MITMRequestInfo struct {
	Method string
	Scheme string
	Host   string
	Path   string
	Header http.Header
	Body   []byte
}

// MITMResponseInfo is a decrypted response as seen by hooks.
type MITMResponseInfo struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// MITMHooks observes decrypted traffic.
type MITMHooks interface {
	OnConnect(ctx context.Context, info *MITMConnectInfo) error
	OnRequest(ctx context.Context, req *MITMRequestInfo) error
	OnResponse(ctx context.Context, resp *MITMResponseInfo) error
}

// MITMEngine is the MITM termination seam injected by a backend.
type MITMEngine interface {
	PEM() []byte
	ShouldMITM(host string) bool
	Serve(client net.Conn, serverName, dialHostport string) error
}

// MITMFactory builds a MITMEngine.
type MITMFactory func(
	policy *MITMPolicy,
	hooks MITMHooks,
	dial func(context.Context, string) (net.Conn, error),
	roots *x509.CertPool,
) (MITMEngine, error)

// ProxyConfig configures one enforcement proxy instance. It is named
// ProxyConfig (not Config) because httpkit already owns Config for the
// transport builder.
type ProxyConfig struct {
	// Mode is NetAllowList or NetProxy.
	Mode NetMode
	// AllowHosts is the deprecated allow-list: hostname suffixes and
	// exact IP literals. Compiled as trailing allow rules (see
	// Matcher); explicit Rules deny rules always win.
	AllowHosts []string
	// Rules are explicit host/port allow or deny rules. In
	// NetAllowList mode they define what is reachable; in NetProxy
	// mode deny rules override the upstream and everything else is
	// delegated to the upstream.
	Rules []NetRule
	// Upstream is the proxy-mode upstream URL: "http://host:port" or
	// "socks5://[user:pass@]host:port". Ignored in allow-list mode.
	Upstream string
	// TCPLoopback binds 127.0.0.1:0 (an ephemeral loopback TCP port)
	// instead of a unix socket. The seatbelt backend uses this because
	// its child shares the host network stack and reaches the proxy
	// through the single SBPL-allowed loopback port. The bwrap backend
	// leaves it false and uses the unix socket for its bridge.
	TCPLoopback bool
	// MITM enables TLS termination for CONNECT tunnels (opt-in).
	MITM *MITMPolicy
	// OnDecision receives one audit record per allow/deny decision.
	// It must not block the proxy; keep it fast and non-throwing.
	OnDecision func(ProxyDecision)
	// Hooks observe/block traffic. OnConnect applies to every CONNECT;
	// OnRequest / OnResponse run only on MITM-decrypted traffic.
	Hooks       MITMHooks
	MITMFactory MITMFactory
	// OutboundRoots overrides the roots used to verify the real
	// target's TLS certificate during MITM. Nil means system roots.
	OutboundRoots *x509.CertPool
}

// ProxyDecision is one audit record: which destination was decided,
// with which action, and which rule ("" = mode default) decided it.
type ProxyDecision struct {
	Host   string
	Port   int
	Action NetAction
	Mode   NetMode
	Rule   string
}

// Proxy is a host-side enforcement proxy listening on a unix socket
// (default) or TCP loopback (ProxyConfig.TCPLoopback). Create it with
// Start and stop it with Close.
type Proxy struct {
	cfg       ProxyConfig
	ln        net.Listener
	srv       *http.Server
	transport *http.Transport
	upstream  *url.URL
	socksDial proxy.ContextDialer
	mitm      MITMEngine

	matcherOnce sync.Once
	compiled    *Matcher
	matcherErr  error

	dir  string
	path string
}

// Start serves the enforcement proxy in the background and returns
// immediately. With ProxyConfig.TCPLoopback it binds an ephemeral
// loopback TCP port (Addr reports it); otherwise it binds a unix
// socket in a fresh per-run temp directory (mode 0600) whose path is
// available via SocketPath for bind-mounting into a
func Start(cfg ProxyConfig) (*Proxy, error) {
	if cfg.Mode != NetAllowList && cfg.Mode != NetProxy {
		return nil, fmt.Errorf(
			"netproxy: mode must be allow_list or proxy, got %d", int(cfg.Mode))
	}
	if err := (NetPolicy{
		Mode:       cfg.Mode,
		AllowHosts: cfg.AllowHosts,
		Rules:      cfg.Rules,
		Proxy:      cfg.Upstream,
		MITM:       cfg.MITM,
	}).Validate(); err != nil {
		return nil, err
	}

	p := &Proxy{cfg: cfg}
	if cfg.Upstream != "" {
		u, err := url.Parse(cfg.Upstream)
		if err != nil {
			return nil, fmt.Errorf("netproxy: parse upstream %q: %w", cfg.Upstream, err)
		}
		switch u.Scheme {
		case "http":
			p.transport = &http.Transport{Proxy: http.ProxyURL(u)}
		case "socks5":
			dialer, err := socks5Dialer(u)
			if err != nil {
				return nil, err
			}
			p.socksDial = dialer
			p.transport = &http.Transport{DialContext: dialer.DialContext}
		default:
			return nil, fmt.Errorf("netproxy: unsupported upstream scheme %q", u.Scheme)
		}
		p.upstream = u
	} else {
		p.transport = &http.Transport{Proxy: nil}
	}

	if cfg.MITM != nil && cfg.MITM.Enabled {
		if cfg.MITMFactory == nil {
			return nil, fmt.Errorf("netproxy: MITM enabled but no MITMFactory injected")
		}
		engine, err := cfg.MITMFactory(cfg.MITM, cfg.Hooks, p.dialTarget, cfg.OutboundRoots)
		if err != nil {
			return nil, err
		}
		p.mitm = engine
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
			if rerr := os.RemoveAll(dir); rerr != nil {
				telemetry.WarnErr(context.Background(),
					"netproxy: remove temp dir after listen failure", rerr)
			}
			return nil, fmt.Errorf("netproxy: listen: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			if cerr := ln.Close(); cerr != nil {
				telemetry.WarnErr(context.Background(),
					"netproxy: close listener after chmod failure", cerr)
			}
			if rerr := os.RemoveAll(dir); rerr != nil {
				telemetry.WarnErr(context.Background(),
					"netproxy: remove temp dir after chmod failure", rerr)
			}
			return nil, fmt.Errorf("netproxy: chmod socket: %w", err)
		}
		p.ln = ln
		p.dir = dir
		p.path = path
	}
	p.srv = &http.Server{Handler: p}
	go func() {
		if err := p.srv.Serve(p.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			telemetry.WarnErr(context.Background(), "netproxy: serve failed", err)
		}
	}()
	return p, nil
}

// SocketPath returns the unix socket path to bind into the
// It is empty when the proxy was started with TCPLoopback.
func (p *Proxy) SocketPath() string { return p.path }

// Addr returns the bound listener address. For a TCP-loopback proxy
// this is the 127.0.0.1:<port> the sandboxed child must be allowed to
// dial; for a unix-socket proxy it is the socket path address.
func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

// CAPEM returns the temporary MITM root CA in PEM form, for bundle
// injection into the  Nil when MITM is disabled.
func (p *Proxy) CAPEM() []byte {
	if p.mitm == nil {
		return nil
	}
	return p.mitm.PEM()
}

// Close stops the server, closes the listener, and removes the temp
// directory (including the socket file) when one was created.
func (p *Proxy) Close() error {
	if err := p.srv.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		telemetry.WarnErr(context.Background(), "netproxy: close server failed", err)
	}
	if err := p.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		telemetry.WarnErr(context.Background(), "netproxy: close listener failed", err)
	}
	p.transport.CloseIdleConnections()
	if p.dir != "" {
		err := os.RemoveAll(p.dir)
		if err != nil {
			telemetry.WarnErr(context.Background(), "netproxy: remove temp dir failed", err)
		}
		return err
	}
	return nil
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
	host, port := splitHostPort(r.URL.Host, 80)
	action, rule, dialHost, err := p.decide(r.Context(), r.URL.Host, 80)
	if err != nil {
		http.Error(w, "netproxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	p.audit(host, port, action, rule)
	if action != NetAllow {
		http.Error(w, "netproxy: destination not allowed", http.StatusForbidden)
		return
	}
	out := r
	if dialHost != r.URL.Host {
		clone := r.Clone(r.Context())
		clone.Host = r.Host
		if clone.Host == "" {
			clone.Host = r.URL.Host
		}
		clone.URL.Host = dialHost
		out = clone
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "netproxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			telemetry.WarnErr(r.Context(), "netproxy: close upstream response body failed", err)
		}
	}()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	host, port := splitHostPort(r.Host, 443)
	action, rule, dialHost, err := p.decide(r.Context(), r.Host, 443)
	if err != nil {
		http.Error(w, "netproxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	p.audit(host, port, action, rule)
	if action != NetAllow {
		http.Error(w, "netproxy: destination not allowed", http.StatusForbidden)
		return
	}
	if p.cfg.Hooks != nil {
		if err := p.cfg.Hooks.OnConnect(r.Context(), &MITMConnectInfo{Host: host, Port: port}); err != nil {
			http.Error(w, "netproxy: connect blocked by hook", http.StatusForbidden)
			return
		}
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
	defer func() {
		if err := client.Close(); err != nil {
			telemetry.WarnErr(r.Context(), "netproxy: close hijacked client failed", err)
		}
	}()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	if p.mitm != nil && p.mitm.ShouldMITM(host) {
		if err := p.mitm.Serve(client, host, dialHost); err != nil {
			return
		}
		return
	}

	target, err := p.dialTarget(r.Context(), dialHost)
	if err != nil {
		return
	}
	defer func() {
		if err := target.Close(); err != nil {
			telemetry.WarnErr(r.Context(), "netproxy: close tunnel target failed", err)
		}
	}()
	go func() {
		if _, err := io.Copy(target, client); err != nil {
			telemetry.Debug(r.Context(), "netproxy: tunnel upstream copy ended with error",
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
		}
		if err := target.Close(); err != nil {
			telemetry.WarnErr(r.Context(), "netproxy: close tunnel target after copy failed", err)
		}
	}()
	_, _ = io.Copy(client, target)
}

// decide evaluates the policy for one destination. It returns the
// verdict, the decisive rule description ("" = mode default), and the
// concrete dial target. When IP/CIDR rules exist the hostname is
// resolved locally and the dial target is pinned to a validated IP so
// neither the upstream nor a rebinding DNS response can bypass the
// rule set.
func (p *Proxy) decide(ctx context.Context, hostport string, defaultPort int) (NetAction, string, string, error) {
	host, port := splitHostPort(hostport, defaultPort)
	dialHost := hostport
	m := p.matcher()
	if m == nil {
		return NetDeny, "", dialHost, fmt.Errorf("netproxy: policy matcher unavailable")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		action, rule, matched := m.MatchIP(ip.Unmap(), port)
		if action == NetDeny {
			return action, rule, dialHost, nil
		}
		if matched {
			return action, rule, dialHost, nil
		}
		if p.cfg.Mode == NetAllowList {
			return NetDeny, "", dialHost, nil
		}
		return NetAllow, "", dialHost, nil
	}

	action, rule, matched := m.Match(host, port)
	if action == NetDeny {
		return action, rule, dialHost, nil
	}
	if m.HasIPRules() {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			// With IP rules present we cannot prove the destination is
			// not denied, so we fail closed.
			return NetDeny, "", dialHost, fmt.Errorf(
				"netproxy: resolve %s for IP rules: %w", host, err)
		}
		chosen := netip.Addr{}
		ipAllow := false
		for _, addr := range addrs {
			addr = addr.Unmap()
			if ipAction, ipRule, ipMatched := m.MatchIP(addr, port); ipMatched && ipAction == NetDeny {
				return NetDeny, ipRule, dialHost, nil
			}
			if !chosen.IsValid() {
				chosen = addr
			}
			if ipAction, _, ipMatched := m.MatchIP(addr, port); ipMatched && ipAction == NetAllow {
				ipAllow = true
			}
		}
		if chosen.IsValid() {
			dialHost = net.JoinHostPort(chosen.String(), strconv.Itoa(port))
		}
		if matched || ipAllow {
			return NetAllow, rule, dialHost, nil
		}
		if p.cfg.Mode == NetAllowList {
			return NetDeny, "", dialHost, nil
		}
		return NetAllow, "", dialHost, nil
	}
	if matched {
		return NetAllow, rule, dialHost, nil
	}
	if p.cfg.Mode == NetAllowList {
		return NetDeny, "", dialHost, nil
	}
	return NetAllow, "", dialHost, nil
}

// audit emits one decision record when OnDecision is configured.
func (p *Proxy) audit(host string, port int, action NetAction, rule string) {
	if p.cfg.OnDecision != nil {
		p.cfg.OnDecision(ProxyDecision{
			Host:   host,
			Port:   port,
			Action: action,
			Mode:   p.cfg.Mode,
			Rule:   rule,
		})
	}
}

// dialTarget dials the CONNECT target directly (allow-list mode),
// establishes a CONNECT tunnel through an HTTP upstream, or dials
// through a SOCKS5 upstream (proxy mode).
func (p *Proxy) dialTarget(ctx context.Context, hostport string) (net.Conn, error) {
	if p.upstream != nil {
		switch p.upstream.Scheme {
		case "http":
			up, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.upstream.Host)
			if err != nil {
				return nil, err
			}
			req := "CONNECT " + hostport + " HTTP/1.1\r\nHost: " + hostport
			if p.upstream.User != nil {
				user := p.upstream.User.Username()
				pass, _ := p.upstream.User.Password()
				req += "\r\nProxy-Authorization: Basic " +
					base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
			}
			req += "\r\n\r\n"
			if _, err := io.WriteString(up, req); err != nil {
				if cerr := up.Close(); cerr != nil {
					telemetry.WarnErr(ctx, "netproxy: close upstream after write failure", cerr)
				}
				return nil, err
			}
			br := bufio.NewReader(up)
			resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
			if err != nil {
				if cerr := up.Close(); cerr != nil {
					telemetry.WarnErr(ctx, "netproxy: close upstream after response failure", cerr)
				}
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				if cerr := up.Close(); cerr != nil {
					telemetry.WarnErr(ctx, "netproxy: close upstream after refusal", cerr)
				}
				return nil, fmt.Errorf("netproxy: upstream refused CONNECT: %d", resp.StatusCode)
			}
			return up, nil
		case "socks5":
			return p.socksDial.DialContext(ctx, "tcp", hostport)
		}
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", hostport)
}

func (p *Proxy) matcher() *Matcher {
	p.matcherOnce.Do(func() {
		p.compiled, p.matcherErr = NewMatcher(NetPolicy{
			Mode:       p.cfg.Mode,
			AllowHosts: p.cfg.AllowHosts,
			Rules:      p.cfg.Rules,
		})
	})
	return p.compiled
}

func socks5Dialer(u *url.URL) (proxy.ContextDialer, error) {
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{User: u.User.Username()}
		if pass, ok := u.User.Password(); ok {
			auth.Password = pass
		}
	}
	dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("netproxy: socks5 dialer: %w", err)
	}
	cd, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("netproxy: socks5 dialer does not implement DialContext")
	}
	return cd, nil
}

func splitHostPort(hostport string, defaultPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.Trim(hostport, "[]"), defaultPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		port = defaultPort
	}
	return strings.Trim(host, "[]"), port
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
