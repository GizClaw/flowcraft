package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// The wire contract: the half of core/utils/net that only a socket can answer.
//
// The policy half (proxy-contract.json) is a pure function of its input. The
// wire half is not, so it is pinned by driving the real proxy over a loopback
// socket and recording what came back:
//
//	socks5   -- what the server writes on a scripted SOCKS5 session, and the
//	            audit records that session produced.
//	http     -- the `CONNECT` answer: the exact head for an established tunnel,
//	            and the status and body of every refusal.
//	upstream -- what an upstream receives (the `CONNECT` request line by line,
//	            including `Proxy-Authorization`, and the SOCKS5 handshake) and
//	            what the dialer gets back from it.
//
// Three things are deliberately not rows:
//
//   - The bind address in a SOCKS5 success reply is the *local* address of the
//     outbound socket, so its port is ephemeral. maskBind zeroes BND.ADDR and
//     BND.PORT and keeps ATYP, which is the part that is a contract.
//   - A first byte other than 0x05 is handed to net/http, and the 400 it writes
//     is net/http's answer rather than netproxy's. The classifier is a table the
//     port tests on its own.
//   - A non-CONNECT request's refusal is written through net/http too, which
//     stamps a Date header: the verbatim head is not reproducible. Those paths
//     are covered by the CONNECT rows, which share the same decision.
const wireContractNote = "core/utils/net: the wire half -- the bytes a SOCKS5 or HTTP CONNECT " +
	"session answers, the audit records it produces, and what an upstream receives and returns. " +
	"SOCKS5 bind addresses are masked (BND.ADDR and BND.PORT zeroed, ATYP kept) because they " +
	"report an ephemeral local port."

type wireContract struct {
	Note     string            `json:"note"`
	Socks5   []wireSocksRow    `json:"socks5"`
	HTTP     []wireHTTPRow     `json:"http"`
	Upstream []wireUpstreamRow `json:"upstream"`
}

// wireAudit is one ProxyDecision, the record every decision path emits.
type wireAudit struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Action   string `json:"action"`
	Mode     string `json:"mode"`
	Rule     string `json:"rule"`
	Reason   string `json:"reason,omitempty"`
	DialHost string `json:"dial_host"`
}

// wireUpstreamSpec is the far end a row dials through, expressed without the
// port it happens to listen on: the writer substitutes its own.
type wireUpstreamSpec struct {
	// Scheme is "http" or "socks5".
	Scheme string `json:"scheme"`
	// User and Password are the URL's userinfo, if any.
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	// RequireAuth makes a SOCKS5 upstream demand the username/password method.
	RequireAuth bool `json:"require_auth,omitempty"`
	// Script is what an HTTP upstream answers after the request head, hex.
	Script string `json:"script,omitempty"`
	// Echo makes the upstream relay whatever it reads after the head back.
	Echo bool `json:"echo,omitempty"`
}

// wireSocksRow is one scripted SOCKS5 session.
type wireSocksRow struct {
	Name       string            `json:"name"`
	Mode       string            `json:"mode"`
	Rules      []netRuleRow      `json:"rules,omitempty"`
	AllowHosts []string          `json:"allow_hosts,omitempty"`
	Upstream   *wireUpstreamSpec `json:"upstream,omitempty"`
	Hooks      bool              `json:"hooks,omitempty"`
	// Client is everything the client writes, hex, one write.
	Client string `json:"client"`
	// Reply is everything the server wrote before the session went quiet, hex.
	Reply string `json:"reply"`
	// MaskBind marks a reply whose BND.ADDR/BND.PORT were zeroed.
	MaskBind bool `json:"mask_bind,omitempty"`
	// UpstreamRequest is what the upstream received, hex.
	UpstreamRequest string      `json:"upstream_request,omitempty"`
	Audit           []wireAudit `json:"audit"`
}

// wireHTTPRow is one `CONNECT` answered by the proxy.
type wireHTTPRow struct {
	Name         string            `json:"name"`
	Mode         string            `json:"mode"`
	Rules        []netRuleRow      `json:"rules,omitempty"`
	Upstream     *wireUpstreamSpec `json:"upstream,omitempty"`
	Hooks        bool              `json:"hooks,omitempty"`
	Request      string            `json:"request"`
	Payload      string            `json:"payload,omitempty"`
	Status       int               `json:"status"`
	Head         string            `json:"head,omitempty"`
	Body         string            `json:"body,omitempty"`
	UpstreamSeen string            `json:"upstream_request,omitempty"`
	Audit        []wireAudit       `json:"audit"`
}

// wireUpstreamRow is one dial through an upstream, without a client in front.
type wireUpstreamRow struct {
	Name        string `json:"name"`
	Scheme      string `json:"scheme"`
	User        string `json:"user,omitempty"`
	HasPassword bool   `json:"has_password,omitempty"`
	Password    string `json:"password,omitempty"`
	RequireAuth bool   `json:"require_auth,omitempty"`
	Target      string `json:"target"`
	// Script is what an HTTP upstream answers, hex.
	Script string `json:"script,omitempty"`
	// Request is what the upstream received, hex.
	Request string `json:"request"`
	// Tunnel is what the dialer read back through the tunnel, hex.
	Tunnel string `json:"tunnel,omitempty"`
}

const (
	wireSocks5Version  = 0x05
	wireSocks5NoAuth   = 0x00
	wireSocks5UserPass = 0x02
	wireSocks5AtypIPv4 = 0x01
	wireSocks5AtypName = 0x03
	wireSocks5AtypIPv6 = 0x04
	wireSocks5CmdBind  = 0x02
)

const (
	// wireQuietWindow is how long a session is allowed to say nothing before
	// the reply is called complete. Every row is loopback, so the only thing
	// this waits for is a session that has nothing more to say.
	wireQuietWindow = 150 * time.Millisecond
	// wireMaxReply caps what one row records, so a runaway session fails the
	// row instead of the dump.
	wireMaxReply = 1 << 16
)

// The head an HTTP upstream answers with when the row only cares that the
// tunnel opened.
var wireUpstreamOK = []byte("HTTP/1.1 200 OK\r\n\r\n")

// wireHTTPSamples are the CONNECT rows.
var wireHTTPSamples = []wireHTTPSample{
	{
		name:     "connect-allowed-through-an-upstream",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf(wireUpstreamOK), Echo: true},
		request:  "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		payload:  []byte("ping"),
	},
	{
		name:    "connect-denied",
		mode:    corenet.NetAllowList,
		rules:   []corenet.NetRule{{Action: corenet.NetDeny, Host: "blocked.example", Reason: "blocked for the demo"}},
		request: "CONNECT blocked.example:443 HTTP/1.1\r\nHost: blocked.example:443\r\n\r\n",
	},
	{
		name:    "connect-not-allow-listed",
		mode:    corenet.NetAllowList,
		rules:   []corenet.NetRule{{Action: corenet.NetAllow, Host: "allowed.example"}},
		request: "CONNECT other.example:443 HTTP/1.1\r\nHost: other.example:443\r\n\r\n",
	},
	{
		name:     "connect-blocked-by-a-hook",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf(wireUpstreamOK), Echo: true},
		hooks:    true,
		request:  "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
	},
	{
		name:    "connect-the-policy-cannot-judge",
		mode:    corenet.NetProxy,
		rules:   []corenet.NetRule{{Action: corenet.NetAllow, Host: "exa*mple.com"}},
		request: "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
	},
	{
		// The 200 is written before the dial, so a refusing upstream is
		// invisible to a CONNECT client: it sees an established tunnel that
		// then closes. Pinned because it looks like a bug until you know.
		name:     "connect-to-an-upstream-that-refuses",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))},
		request:  "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
	},
}

// wireUpstreamSamples are the rows that watch an upstream directly.
var wireUpstreamSamples = []wireUpstreamSample{
	{name: "http-upstream-sees-the-connect", scheme: "http", target: "example.com:443"},
	{
		name:     "http-upstream-sees-credentials",
		scheme:   "http",
		user:     "user",
		password: "pass",
		target:   "example.com:443",
	},
	{
		// url.Parse accepts a user without a password, and core still writes
		// the colon: the pair is "user:".
		name:   "http-upstream-sees-a-user-without-a-password",
		scheme: "http",
		user:   "user",
		target: "example.com:443",
	},
	{
		name:        "socks5-upstream-sees-the-connect",
		scheme:      "socks5",
		target:      "example.com:443",
		requireAuth: false,
	},
	{
		name:        "socks5-upstream-sees-the-handshake",
		scheme:      "socks5",
		user:        "user",
		password:    "pass",
		target:      "example.com:443",
		requireAuth: true,
	},
}

type wireHTTPSample struct {
	name     string
	mode     corenet.NetMode
	rules    []corenet.NetRule
	upstream *wireUpstreamSpec
	hooks    bool
	request  string
	payload  []byte
}

type wireUpstreamSample struct {
	name        string
	scheme      string
	user        string
	password    string
	target      string
	requireAuth bool
}

func writeWireContract(w io.Writer) error {
	contract := wireContract{Note: wireContractNote}
	for _, sample := range wireSocksSamples {
		row, err := wireSocksRowOf(sample)
		if err != nil {
			return fmt.Errorf("socks5 %s: %w", sample.name, err)
		}
		contract.Socks5 = append(contract.Socks5, row)
	}
	for _, sample := range wireHTTPSamples {
		row, err := wireHTTPRowOf(sample)
		if err != nil {
			return fmt.Errorf("http %s: %w", sample.name, err)
		}
		contract.HTTP = append(contract.HTTP, row)
	}
	for _, sample := range wireUpstreamSamples {
		row, err := wireUpstreamRowOf(sample)
		if err != nil {
			return fmt.Errorf("upstream %s: %w", sample.name, err)
		}
		contract.Upstream = append(contract.Upstream, row)
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}

// ---------------------------------------------------------------- SOCKS5 rows

func wireSocksRowOf(sample wireSocksSample) (row wireSocksRow, err error) {
	row = wireSocksRow{
		Name:       sample.name,
		Mode:       proxyModeName(sample.mode),
		Rules:      wireRuleRows(sample.rules),
		AllowHosts: sample.allowHosts,
		Hooks:      sample.hooks,
		Client:     hexOf(sample.client),
	}
	var stub *upstreamStub
	if sample.upstream != nil {
		stub, err = startUpstream(*sample.upstream)
		if err != nil {
			return row, err
		}
		defer stub.stop()
		row.Upstream = sample.upstream
	}
	var decisions recorder
	proxy, err := corenet.Start(wireProxyConfig(sample.mode, sample.rules, sample.allowHosts, stub, sample.hooks, &decisions))
	if err != nil {
		return row, fmt.Errorf("start: %w", err)
	}
	defer func() {
		if cerr := proxy.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close: %w", cerr)
		}
	}()

	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		return row, fmt.Errorf("dial: %w", err)
	}
	reply, err := wireSession(conn, sample.client)
	if err != nil {
		return row, err
	}
	if sample.maskBind {
		reply = maskBind(reply)
		row.MaskBind = true
	}
	row.Reply = hexOf(reply)
	row.Audit = decisions.rows()
	if stub != nil {
		seen, err := stub.waitRequest()
		if err != nil {
			return row, err
		}
		row.UpstreamRequest = hexOf(seen)
	}
	return row, nil
}

// wireSession writes the client's bytes, then reads until the session closes or
// goes quiet, and closes both ends.
func wireSession(conn net.Conn, client []byte) ([]byte, error) {
	if _, err := conn.Write(client); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write: %w", err)
	}
	reply := make([]byte, 0, 64)
	buf := make([]byte, 4096)
	for len(reply) < wireMaxReply {
		if err := conn.SetReadDeadline(time.Now().Add(wireQuietWindow)); err != nil {
			_ = conn.Close()
			return reply, fmt.Errorf("deadline: %w", err)
		}
		read, err := conn.Read(buf)
		reply = append(reply, buf[:read]...)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				break
			}
			_ = conn.Close()
			return reply, fmt.Errorf("read: %w", err)
		}
	}
	if err := conn.Close(); err != nil {
		return reply, fmt.Errorf("close client: %w", err)
	}
	return reply, nil
}

// maskBind zeroes BND.ADDR and BND.PORT in a success reply and leaves ATYP in
// place: the address is the outbound socket's local one, i.e. an ephemeral
// port, while ATYP is core choosing IPv4 or IPv6 for it.
func maskBind(reply []byte) []byte {
	const methodReply = 2
	if len(reply) < methodReply+4 || reply[0] != wireSocks5Version {
		return reply
	}
	request := reply[methodReply:]
	if request[0] != wireSocks5Version || request[1] != 0x00 {
		return reply
	}
	var address int
	switch request[3] {
	case wireSocks5AtypIPv4:
		address = 4
	case wireSocks5AtypIPv6:
		address = 16
	default:
		return reply
	}
	start := methodReply + 4
	end := start + address + 2
	if len(reply) < end {
		return reply
	}
	masked := append([]byte(nil), reply...)
	for i := start; i < end; i++ {
		masked[i] = 0
	}
	return masked
}

// ---------------------------------------------------------------- HTTP rows

func wireHTTPRowOf(sample wireHTTPSample) (row wireHTTPRow, err error) {
	row = wireHTTPRow{
		Name:    sample.name,
		Mode:    proxyModeName(sample.mode),
		Rules:   wireRuleRows(sample.rules),
		Hooks:   sample.hooks,
		Request: sample.request,
		Payload: hexOf(sample.payload),
	}
	var stub *upstreamStub
	if sample.upstream != nil {
		stub, err = startUpstream(*sample.upstream)
		if err != nil {
			return row, err
		}
		defer stub.stop()
		row.Upstream = sample.upstream
	}
	var decisions recorder
	proxy, err := corenet.Start(wireProxyConfig(sample.mode, sample.rules, nil, stub, sample.hooks, &decisions))
	if err != nil {
		return row, fmt.Errorf("start: %w", err)
	}
	defer func() {
		if cerr := proxy.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close: %w", cerr)
		}
	}()

	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		return row, fmt.Errorf("dial: %w", err)
	}
	answer, err := wireConnectSession(conn, sample.request, sample.payload)
	if err != nil {
		return row, err
	}
	row.Status = answer.status
	row.Head = hexOf(answer.head)
	row.Body = hexOf(answer.body)
	row.Audit = decisions.rows()
	if stub != nil && answer.status == 200 {
		seen, err := stub.waitRequest()
		if err != nil {
			return row, err
		}
		row.UpstreamSeen = hexOf(seen)
	}
	return row, nil
}

type httpAnswer struct {
	status int
	head   []byte
	body   []byte
}

// wireConnectSession writes one CONNECT and reads the answer. A hijacked
// answer (200) is followed by the tunnel, so the payload goes out after the
// head and whatever comes back is the tunnel's bytes.
func wireConnectSession(conn net.Conn, request string, payload []byte) (httpAnswer, error) {
	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return httpAnswer{}, fmt.Errorf("write request: %w", err)
	}
	head, err := wireReadHead(conn)
	if err != nil {
		_ = conn.Close()
		return httpAnswer{}, err
	}
	answer := httpAnswer{status: wireStatus(head)}
	// Only a hijacked answer has a head that is netproxy's own: net/http
	// stamps a Date header on every response it writes itself.
	if answer.status == 200 {
		answer.head = head
	}
	if answer.status == 200 && len(payload) > 0 {
		if _, err := conn.Write(payload); err != nil {
			_ = conn.Close()
			return answer, fmt.Errorf("write payload: %w", err)
		}
	}
	rest, err := wireReadQuiet(conn)
	if err != nil {
		_ = conn.Close()
		return answer, err
	}
	// A hijacked connection carries the tunnel, not a body; a refused one
	// carries the error text.
	answer.body = rest
	return answer, nil
}

// wireReadHead reads up to and including the blank line that ends a response
// head.
func wireReadHead(conn net.Conn) ([]byte, error) {
	head := make([]byte, 0, 256)
	buf := make([]byte, 1)
	for len(head) < wireMaxReply {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return head, fmt.Errorf("deadline: %w", err)
		}
		read, err := conn.Read(buf)
		if read > 0 {
			head = append(head, buf[0])
			if n := len(head); n >= 4 && string(head[n-4:]) == "\r\n\r\n" {
				return head, nil
			}
		}
		if err != nil {
			return head, fmt.Errorf("read head: %w", err)
		}
	}
	return head, fmt.Errorf("head longer than %d bytes", wireMaxReply)
}

// wireStatus reads the status code out of a response head's first line.
func wireStatus(head []byte) int {
	line := string(head)
	if end := strings.Index(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	parts := strings.Split(line, " ")
	if len(parts) < 2 {
		return 0
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return status
}

// wireReadQuiet reads until the peer closes or says nothing for one window.
func wireReadQuiet(conn net.Conn) ([]byte, error) {
	rest := make([]byte, 0, 64)
	buf := make([]byte, 4096)
	for len(rest) < wireMaxReply {
		if err := conn.SetReadDeadline(time.Now().Add(wireQuietWindow)); err != nil {
			return rest, fmt.Errorf("deadline: %w", err)
		}
		read, err := conn.Read(buf)
		rest = append(rest, buf[:read]...)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				break
			}
			return rest, fmt.Errorf("read: %w", err)
		}
	}
	if err := conn.Close(); err != nil {
		return rest, fmt.Errorf("close: %w", err)
	}
	return rest, nil
}

// ------------------------------------------------------------ upstream rows

func wireUpstreamRowOf(sample wireUpstreamSample) (row wireUpstreamRow, err error) {
	row = wireUpstreamRow{
		Name:        sample.name,
		Scheme:      sample.scheme,
		User:        sample.user,
		HasPassword: sample.password != "",
		Password:    sample.password,
		RequireAuth: sample.requireAuth,
		Target:      sample.target,
	}
	spec := wireUpstreamSpec{
		Scheme:      sample.scheme,
		User:        sample.user,
		Password:    sample.password,
		RequireAuth: sample.requireAuth,
		Script:      hexOf(wireUpstreamOK),
		Echo:        true,
	}
	stub, err := startUpstream(spec)
	if err != nil {
		return row, err
	}
	defer stub.stop()
	row.Script = spec.Script

	// Through the proxy's own dial path: a SOCKS5 session reaches dialTarget
	// with the same target the row names, and the tunnel carries whatever the
	// upstream sends first.
	var decisions recorder
	proxy, err := corenet.Start(wireProxyConfig(corenet.NetProxy, nil, nil, stub, false, &decisions))
	if err != nil {
		return row, fmt.Errorf("start: %w", err)
	}
	defer func() {
		if cerr := proxy.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close: %w", cerr)
		}
	}()
	target := sample.target
	host, port := splitTarget(target)
	client := append(socks5Greeting(wireSocks5NoAuth), socks5Connect(host, port)...)
	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		return row, fmt.Errorf("dial: %w", err)
	}
	reply, err := wireSession(conn, client)
	if err != nil {
		return row, err
	}
	row.Tunnel = hexOf(wireTunnel(reply))
	seen, err := stub.waitRequest()
	if err != nil {
		return row, err
	}
	row.Request = hexOf(seen)
	return row, nil
}

// wireTunnel is what a session wrote after the SOCKS5 handshake: the method
// reply, the request reply, and then the tunnel's own bytes.
func wireTunnel(reply []byte) []byte {
	index := 2
	if len(reply) < index+4 {
		return nil
	}
	switch reply[index+3] {
	case wireSocks5AtypIPv4:
		index += 4 + 4 + 2
	case wireSocks5AtypIPv6:
		index += 4 + 16 + 2
	default:
		return nil
	}
	if len(reply) < index {
		return nil
	}
	return reply[index:]
}

// ------------------------------------------------------------------ plumbing

// recorder collects the audit records of one proxy.
type recorder struct {
	mu     sync.Mutex
	audits []corenet.ProxyDecision
}

func (r *recorder) record(decision corenet.ProxyDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, decision)
}

func (r *recorder) rows() []wireAudit {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := make([]wireAudit, 0, len(r.audits))
	for _, audit := range r.audits {
		rows = append(rows, wireAudit{
			Host:     audit.Host,
			Port:     audit.Port,
			Action:   audit.Action.String(),
			Mode:     proxyModeName(audit.Mode),
			Rule:     audit.Rule,
			Reason:   audit.Reason,
			DialHost: audit.DialHost,
		})
	}
	return rows
}

// wireProxyConfig is the config every row shares, with the upstream URL built
// from the stub's own address so no port reaches the dump.
func wireProxyConfig(
	mode corenet.NetMode,
	rules []corenet.NetRule,
	allowHosts []string,
	stub *upstreamStub,
	hooks bool,
	decisions *recorder,
) corenet.ProxyConfig {
	config := corenet.ProxyConfig{
		Mode:        mode,
		Rules:       rules,
		AllowHosts:  allowHosts,
		TCPLoopback: true,
		OnDecision:  decisions.record,
	}
	if stub != nil {
		config.Upstream = stub.url()
	}
	if hooks {
		config.Hooks = refuseHooks{}
	}
	return config
}

// refuseHooks blocks every CONNECT, which is what a hook is for.
type refuseHooks struct{}

func (refuseHooks) OnConnect(context.Context, *corenet.MITMConnectInfo) error {
	return errors.New("refused by the conformance hook")
}

func (refuseHooks) OnRequest(context.Context, *corenet.MITMRequestInfo) error { return nil }

func (refuseHooks) OnResponse(context.Context, *corenet.MITMResponseInfo) error { return nil }

// upstreamStub is the far end a row dials through. It answers the way the row
// asks and records the bytes it received before answering.
type upstreamStub struct {
	ln     net.Listener
	spec   wireUpstreamSpec
	mu     sync.Mutex
	got    []byte
	done   chan struct{}
	closed sync.Once
}

func startUpstream(spec wireUpstreamSpec) (*upstreamStub, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen upstream: %w", err)
	}
	stub := &upstreamStub{ln: ln, spec: spec, done: make(chan struct{})}
	switch spec.Scheme {
	case "http":
		go stub.serveHTTP()
	case "socks5":
		go stub.serveSOCKS5()
	default:
		_ = ln.Close()
		return nil, fmt.Errorf("unsupported upstream scheme %q", spec.Scheme)
	}
	return stub, nil
}

// url is the upstream URL a proxy config points at. The port is the stub's,
// which is why no row records it.
func (s *upstreamStub) url() string {
	host, port, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		return s.spec.Scheme + "://" + s.ln.Addr().String()
	}
	userinfo := ""
	if s.spec.User != "" {
		userinfo = s.spec.User + ":" + s.spec.Password + "@"
	}
	return s.spec.Scheme + "://" + userinfo + host + ":" + port
}

func (s *upstreamStub) stop() {
	s.closed.Do(func() { _ = s.ln.Close() })
}

// waitRequest returns the bytes recorded so far, once the stub has finished
// reading the request it answers.
func (s *upstreamStub) waitRequest() ([]byte, error) {
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		return nil, errors.New("the upstream never received a request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.got...), nil
}

func (s *upstreamStub) note(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, chunk...)
}

func (s *upstreamStub) serveHTTP() {
	defer close(s.done)
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	head := make([]byte, 0, 256)
	buf := make([]byte, 1)
	for len(head) < wireMaxReply {
		read, err := conn.Read(buf)
		if read > 0 {
			head = append(head, buf[0])
			if n := len(head); n >= 4 && string(head[n-4:]) == "\r\n\r\n" {
				break
			}
		}
		if err != nil {
			return
		}
	}
	s.note(head)
	if _, err := conn.Write(hexBytes(s.spec.Script)); err != nil {
		return
	}
	if s.spec.Echo {
		_, _ = io.Copy(conn, conn)
	}
}

func (s *upstreamStub) serveSOCKS5() {
	defer close(s.done)
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	// Greeting: VER, NMETHODS, METHODS[].
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	s.note(head)
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	s.note(methods)
	offersAuth := false
	for _, method := range methods {
		if method == wireSocks5UserPass {
			offersAuth = true
		}
	}
	chooses := byte(wireSocks5NoAuth)
	if s.spec.RequireAuth && offersAuth {
		chooses = wireSocks5UserPass
	}
	if _, err := conn.Write([]byte{wireSocks5Version, chooses}); err != nil {
		return
	}
	if chooses == wireSocks5UserPass {
		// RFC 1929: VER, ULEN, UNAME, PLEN, PASSWD.
		auth := make([]byte, 2)
		if _, err := io.ReadFull(conn, auth); err != nil {
			return
		}
		s.note(auth)
		username := make([]byte, int(auth[1]))
		if _, err := io.ReadFull(conn, username); err != nil {
			return
		}
		s.note(username)
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		s.note(length)
		password := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, password); err != nil {
			return
		}
		s.note(password)
		if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	}
	// Request: VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT.
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	s.note(request)
	var address []byte
	switch request[3] {
	case wireSocks5AtypIPv4:
		address = make([]byte, 4)
	case wireSocks5AtypIPv6:
		address = make([]byte, 16)
	default:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		s.note(length)
		address = make([]byte, int(length[0]))
	}
	if _, err := io.ReadFull(conn, address); err != nil {
		return
	}
	s.note(address)
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	s.note(port)
	reply := []byte{wireSocks5Version, 0x00, 0x00, wireSocks5AtypIPv4, 0, 0, 0, 0, 0, 0}
	if _, err := conn.Write(reply); err != nil {
		return
	}
	if s.spec.Echo {
		_, _ = io.Copy(conn, conn)
	}
}

// -------------------------------------------------------------- the samples

// wireSocksSamples are the SOCKS5 rows, in the order they are dumped.
var wireSocksSamples = []wireSocksSample{
	{
		name:     "connect-through-an-http-upstream",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf(wireUpstreamOK), Echo: true},
		client:   append(socks5Greeting(wireSocks5NoAuth), append(socks5Connect("example.com", 443), []byte("ping")...)...),
		maskBind: true,
	},
	{
		// The name is recorded as the client wrote it; only the match is
		// normalized. An audit trail that lowercased it would be answering a
		// question nobody asked.
		name:     "connect-keeps-the-name-as-written",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf(wireUpstreamOK), Echo: true},
		client:   append(socks5Greeting(wireSocks5NoAuth), socks5Connect("Example.COM", 443)...),
		maskBind: true,
	},
	{
		name:     "connect-an-ipv4-literal",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf(wireUpstreamOK), Echo: true},
		client:   append(socks5Greeting(wireSocks5NoAuth), socks5Connect("203.0.113.9", 443)...),
		maskBind: true,
	},
	{
		name:     "connect-an-ipv6-literal",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf(wireUpstreamOK), Echo: true},
		client:   append(socks5Greeting(wireSocks5NoAuth), socks5Connect("2001:db8::1", 8443)...),
		maskBind: true,
	},
	{
		name:   "connect-denied",
		mode:   corenet.NetAllowList,
		rules:  []corenet.NetRule{{Action: corenet.NetDeny, Host: "blocked.example", Reason: "blocked for the demo"}},
		client: append(socks5Greeting(wireSocks5NoAuth), socks5Connect("blocked.example", 443)...),
	},
	{
		name:   "connect-not-allow-listed",
		mode:   corenet.NetAllowList,
		rules:  []corenet.NetRule{{Action: corenet.NetAllow, Host: "allowed.example"}},
		client: append(socks5Greeting(wireSocks5NoAuth), socks5Connect("other.example", 443)...),
	},
	{
		name:   "connect-blocked-by-a-hook",
		mode:   corenet.NetProxy,
		hooks:  true,
		client: append(socks5Greeting(wireSocks5NoAuth), socks5Connect("example.com", 443)...),
	},
	{
		// The SOCKS5 path audits the deny it cannot judge, where the HTTP path
		// refuses before auditing. That asymmetry is core's.
		name:   "connect-the-policy-cannot-judge",
		mode:   corenet.NetProxy,
		rules:  []corenet.NetRule{{Action: corenet.NetAllow, Host: "exa*mple.com"}},
		client: append(socks5Greeting(wireSocks5NoAuth), socks5Connect("example.com", 443)...),
	},
	{
		name:     "connect-to-an-upstream-that-speaks-first",
		mode:     corenet.NetProxy,
		upstream: &wireUpstreamSpec{Scheme: "http", Script: hexOf([]byte("HTTP/1.1 200 OK\r\n\r\nbanner"))},
		client:   append(socks5Greeting(wireSocks5NoAuth), socks5Connect("example.com", 443)...),
		maskBind: true,
	},
	{
		name:   "no-acceptable-method",
		mode:   corenet.NetProxy,
		client: socks5Greeting(wireSocks5UserPass),
	},
	{
		name:   "unsupported-command",
		mode:   corenet.NetProxy,
		client: append(socks5Greeting(wireSocks5NoAuth), socks5Command(wireSocks5CmdBind, "example.com", 443)...),
	},
	{
		name:   "unsupported-address-type",
		mode:   corenet.NetProxy,
		client: append(socks5Greeting(wireSocks5NoAuth), 0x05, 0x01, 0x00, 0x09),
	},
	{
		name:   "empty-domain",
		mode:   corenet.NetProxy,
		client: append(socks5Greeting(wireSocks5NoAuth), 0x05, 0x01, 0x00, wireSocks5AtypName, 0x00),
	},
	{
		name:   "zero-destination-port",
		mode:   corenet.NetProxy,
		client: append(socks5Greeting(wireSocks5NoAuth), socks5Connect("example.com", 0)...),
	},
	{
		name:   "bad-request-version",
		mode:   corenet.NetProxy,
		client: append(socks5Greeting(wireSocks5NoAuth), 0x04, 0x01, 0x00, wireSocks5AtypIPv4, 127, 0, 0, 1, 0, 80),
	},
	{
		name:   "nothing-after-the-greeting",
		mode:   corenet.NetProxy,
		client: socks5Greeting(wireSocks5NoAuth),
	},
}

type wireSocksSample struct {
	name       string
	mode       corenet.NetMode
	rules      []corenet.NetRule
	allowHosts []string
	upstream   *wireUpstreamSpec
	hooks      bool
	client     []byte
	maskBind   bool
}

// --------------------------------------------------------------- the scripts

// socks5Greeting is the client's method offer: VER, NMETHODS, METHODS[].
func socks5Greeting(methods ...byte) []byte {
	out := []byte{wireSocks5Version, byte(len(methods))}
	return append(out, methods...)
}

// socks5Connect is a CONNECT request for one destination.
func socks5Connect(host string, port int) []byte {
	return socks5Command(0x01, host, port)
}

// socks5Command frames one request, choosing ATYP the way a client would.
func socks5Command(command byte, host string, port int) []byte {
	out := []byte{wireSocks5Version, command, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, wireSocks5AtypIPv4)
			out = append(out, v4...)
		} else {
			out = append(out, wireSocks5AtypIPv6)
			out = append(out, ip.To16()...)
		}
	} else {
		out = append(out, wireSocks5AtypName, byte(len(host)))
		out = append(out, host...)
	}
	return append(out, byte(port>>8), byte(port))
}

// splitTarget splits a "host:port" the way the SOCKS5 rows write it.
func splitTarget(target string) (string, int) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return target, 443
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return host, 443
	}
	return host, number
}

// wireRuleRows renders a rule the way proxy-contract.json does.
func wireRuleRows(rules []corenet.NetRule) []netRuleRow {
	rows := make([]netRuleRow, 0, len(rules))
	for _, rule := range rules {
		rows = append(rows, netRuleRow{
			Action: rule.Action.String(),
			Host:   rule.Host,
			Port:   rule.Port,
			Reason: rule.Reason,
		})
	}
	return rows
}

func hexOf(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return hex.EncodeToString(data)
}

func hexBytes(encoded string) []byte {
	if encoded == "" {
		return nil
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return decoded
}
