package main

import corenet "github.com/GizClaw/flowcraft/core/utils/net"

// The samples the proxy contract is computed from: the URL grammar the proxy
// settings are read with, the policies Validate is asked about, the
// destinations Decide is asked about, and the MITM host selection.
//
// Every row here is answerable without a socket. That is the point of the
// split: the proxy itself is listeners and tunnels, but what it *decides* is a
// function of its configuration, and a wrong decision is the failure that
// matters. The rows aim at the places a plausible port goes wrong --
// url.Parse's error spelling, the difference between "no userinfo" and an
// empty username, a destination with no port, a port that is not a number,
// IDNA on the query side, and an allowed destination that the rule set pins to
// a different address than the name.

// proxyURLSamples are fed to url.Parse. They cover the grammar Validate leans
// on and the four ways a proxy URL is refused, plus the shapes that are easy
// to get wrong: a bracketed literal versus a bare one, userinfo with escapes,
// a password with no username, a scheme that is not allowed, and the escapes
// that are legal in one component and refused in another (a %-escape in a
// query is left alone; the same escape in a path is an error).
var proxyURLSamples = []string{
	"http://127.0.0.1:3128",
	"http://proxy.example:8080",
	"http://proxy.example",
	"http://proxy.example/",
	"socks5://proxy.example:1080",
	"socks5://user:pass@proxy.example:1080",
	"socks5://user@proxy.example:1080",
	"socks5://:pass@proxy.example:1080",
	"socks5://:@proxy.example:1080",
	"HTTP://Proxy.Example:8080",
	"https://proxy.example:443",
	"proxy.example:3128",
	"//proxy.example:3128",
	"proxy.example",
	"",
	"*",
	"http://",
	"http:///path",
	"http://proxy.example:bad",
	"http://proxy.example:",
	"http://proxy.example:65536",
	"http://proxy.example:80:90",
	"http://[::1]:8080",
	"http://[::1]",
	"http://[::1",
	"http://::1:8080",
	"http://[127.0.0.1]:8080",
	"http://[::ffff:127.0.0.1]:8080",
	"http://[fe80::1%25en0]:8080",
	"http://[fe80::1%en0]:8080",
	"http://user%20name:p%40ss@proxy.example:8080",
	"http://u%zz@proxy.example",
	"http://user:pa:ss@proxy.example:8080",
	"http://a:b@proxy.example:8080",
	"http://proxy.example/%zz",
	"http://proxy.example/a%2Fb",
	"http://proxy.example?q=%zz",
	"http://proxy.example#f%zz",
	"http://proxy.example#frag",
	"http://proxy.example?",
	"http://proxy.example/?a=b&c",
	"http://pro xy.example:8080",
	"http://proxy.example:8080\x01",
	"http://proxy.example:8080/pa\x7fth",
	"1http://proxy.example",
	"cache_object:foo/bar",
	"a:b",
	"mailto:user@example.com",
	"file:///tmp/x",
	"http://@proxy.example:8080",
	"http://proxy.example:8080/path?q=1#frag",
}

// proxyPolicySample is one NetPolicy, in the shape both tables spell it with.
type proxyPolicySample struct {
	name        string
	mode        corenet.NetMode
	proxy       string
	allowHosts  []string
	rules       []netRuleSample
	unixSockets []string
	mitm        *proxyMITMPolicySample
}

type proxyMITMPolicySample struct {
	enabled       bool
	inspectBodies bool
	maxBodyBytes  int64
	hosts         []string
	excludeHosts  []string
}

// proxyValidateSamples are NetPolicy.Validate's inputs. The modes and actions
// are the valid ones only: a Rust enum cannot hold the others, so the two
// branches that refuse them are unreachable there and stay covered by core's
// own tests (the module docs say so).
var proxyValidateSamples = []proxyPolicySample{
	{name: "zero-policy"},
	{
		name:       "allow-list-with-rules",
		mode:       corenet.NetAllowList,
		allowHosts: []string{"api.example.com:443"},
		rules: []netRuleSample{
			{host: "example.com"},
			{deny: true, host: "*.internal", reason: "internal hosts are not reachable from the sandbox"},
			{host: "10.0.0.0/8"},
			{host: "[::1]:8443"},
		},
	},
	{
		name:  "proxy-mode-http-upstream",
		mode:  corenet.NetProxy,
		proxy: "http://127.0.0.1:3128",
	},
	{
		name:  "proxy-mode-socks-upstream",
		mode:  corenet.NetProxy,
		proxy: "socks5://user:pass@proxy.example:1080",
	},
	{
		name:  "proxy-mode-https-upstream",
		mode:  corenet.NetProxy,
		proxy: "https://proxy.example:443",
	},
	{
		name:  "proxy-mode-scheme-less",
		mode:  corenet.NetProxy,
		proxy: "proxy.example:3128",
	},
	{
		name:  "proxy-mode-empty-host",
		mode:  corenet.NetProxy,
		proxy: "http://",
	},
	{
		name:  "proxy-mode-unparsable",
		mode:  corenet.NetProxy,
		proxy: "://proxy.example",
	},
	{
		name:  "proxy-mode-password-no-username",
		mode:  corenet.NetProxy,
		proxy: "socks5://:pass@proxy.example:1080",
	},
	{
		name:  "proxy-mode-bad-port",
		mode:  corenet.NetProxy,
		proxy: "http://proxy.example:bad",
	},
	{
		name: "rule-empty-host",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{host: ""},
		},
	},
	{
		name: "rule-blank-host",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{host: "   "},
		},
	},
	{
		name: "rule-negative-port",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{host: "example.com", port: -1},
		},
	},
	{
		name: "rule-port-out-of-range",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{host: "example.com", port: 65536},
		},
	},
	{
		name: "rule-reason-padded",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{deny: true, host: "example.com", reason: " padded"},
		},
	},
	{
		name: "rule-reason-two-lines",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{deny: true, host: "example.com", reason: "first\nsecond"},
		},
	},
	{
		// The rules loop runs before the socket loop, so the rule's refusal
		// is the one reported even though the socket is wrong too.
		name: "rule-and-socket-both-wrong",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{deny: true, host: "example.com", reason: "fine"},
			{host: "example.com", port: 70000},
		},
		unixSockets: []string{"relative/path.sock"},
	},
	{
		name:        "socket-relative",
		mode:        corenet.NetAllowList,
		unixSockets: []string{"/tmp/ok.sock", "relative.sock"},
	},
	{
		name: "mitm-negative-body",
		mode: corenet.NetAllowList,
		mitm: &proxyMITMPolicySample{enabled: true, maxBodyBytes: -1},
	},
	{
		name: "mitm-empty-host",
		mode: corenet.NetAllowList,
		mitm: &proxyMITMPolicySample{enabled: true, hosts: []string{"example.com", " "}},
	},
	{
		name: "mitm-empty-exclude",
		mode: corenet.NetAllowList,
		mitm: &proxyMITMPolicySample{enabled: true, excludeHosts: []string{""}},
	},
	{
		name: "mitm-valid",
		mode: corenet.NetAllowList,
		mitm: &proxyMITMPolicySample{
			enabled:       true,
			inspectBodies: true,
			maxBodyBytes:  1024,
			hosts:         []string{"*.example.com"},
			excludeHosts:  []string{"secret.example.com"},
		},
	},
}

// proxyDecisionSample is one Proxy.Decide question: a whole proxy
// configuration and one destination.
type proxyDecisionSample struct {
	name        string
	mode        corenet.NetMode
	upstream    string
	allowHosts  []string
	rules       []netRuleSample
	request     string
	defaultPort int
}

// proxyDecisionSamples are what Proxy.Decide is asked.
//
// A hostname row must carry no IP/CIDR rule: that combination is the one path
// that consults the resolver, and a resolver's answer is not a contract. The
// rows below therefore probe the split, the hostname rules, the IP-literal
// path and both mode defaults, and the address-pinning half of the decision is
// pinned by the port's own tests against core's (documented) rule rather than
// by a byte here.
var proxyDecisionSamples = []proxyDecisionSample{
	// -- the split, observed through the record's Host and Port --
	{name: "split-host-and-port", mode: corenet.NetAllowList, request: "example.com:443", defaultPort: 80},
	{name: "split-no-port", mode: corenet.NetAllowList, request: "example.com", defaultPort: 80},
	{name: "split-empty-port", mode: corenet.NetAllowList, request: "example.com:", defaultPort: 80},
	{name: "split-non-numeric-port", mode: corenet.NetAllowList, request: "example.com:bad", defaultPort: 80},
	{name: "split-overflowing-port", mode: corenet.NetAllowList, request: "example.com:999999999999999999999", defaultPort: 80},
	{name: "split-ipv6", mode: corenet.NetAllowList, request: "[::1]:8443", defaultPort: 80},
	{name: "split-ipv6-no-port", mode: corenet.NetAllowList, request: "[::1]", defaultPort: 80},
	{name: "split-bare-ipv6", mode: corenet.NetAllowList, request: "::1:80", defaultPort: 80},
	{name: "split-doubled-brackets", mode: corenet.NetAllowList, request: "[[::1]]:80", defaultPort: 80},
	{name: "split-empty-brackets", mode: corenet.NetAllowList, request: "[]:80", defaultPort: 80},
	{name: "split-empty-host", mode: corenet.NetAllowList, request: ":80", defaultPort: 80},
	{name: "split-empty", mode: corenet.NetAllowList, request: "", defaultPort: 443},
	{name: "split-leading-space", mode: corenet.NetAllowList, request: " [::1]:80", defaultPort: 80},
	{name: "split-two-colons", mode: corenet.NetAllowList, request: "a:b:c", defaultPort: 80},
	{name: "split-zero-port", mode: corenet.NetAllowList, request: "127.0.0.1:0", defaultPort: 443},
	{name: "split-port-out-of-range", mode: corenet.NetAllowList, request: "example.com:99999", defaultPort: 80},

	// -- hostname rules --
	{
		name:    "host-rule-allows",
		mode:    corenet.NetAllowList,
		rules:   []netRuleSample{{host: "example.com"}},
		request: "example.com:443",
	},
	{
		name:        "host-rule-default-port",
		mode:        corenet.NetAllowList,
		rules:       []netRuleSample{{host: "example.com"}},
		request:     "example.com",
		defaultPort: 443,
	},
	{
		name:       "legacy-allow-hosts",
		mode:       corenet.NetAllowList,
		allowHosts: []string{"example.com"},
		request:    "sub.example.com:443",
	},
	{
		name:       "legacy-allow-hosts-port-suffix",
		mode:       corenet.NetAllowList,
		allowHosts: []string{"example.com:443"},
		request:    "example.com:8443",
	},
	{
		name: "deny-beats-allow",
		mode: corenet.NetAllowList,
		rules: []netRuleSample{
			{host: "example.com"},
			{deny: true, host: "*.example.com", reason: "no subdomains"},
		},
		request: "www.example.com:443",
	},
	{
		name:    "idna-query-matches-punycode-rule",
		mode:    corenet.NetAllowList,
		rules:   []netRuleSample{{host: "xn--bcher-kva.example"}},
		request: "bücher.example:443",
	},

	// -- IP literals --
	{name: "uncovered-literal", mode: corenet.NetAllowList, request: "93.184.216.34:443"},
	{
		name:    "covered-literal",
		mode:    corenet.NetAllowList,
		rules:   []netRuleSample{{host: "93.184.216.34"}},
		request: "93.184.216.34:80",
	},
	{
		name:    "denied-range-carries-reason",
		mode:    corenet.NetAllowList,
		rules:   []netRuleSample{{deny: true, host: "10.0.0.0/8", reason: "metadata range"}},
		request: "10.1.2.3:80",
	},
	{
		name:    "ipv6-literal",
		mode:    corenet.NetAllowList,
		rules:   []netRuleSample{{host: "[::1]:8443"}},
		request: "[::1]:8443",
	},
	{
		name:    "ipv6-literal-uncovered-port",
		mode:    corenet.NetAllowList,
		rules:   []netRuleSample{{host: "[::1]:8443"}},
		request: "[::1]:443",
	},

	// -- the mode defaults --
	{
		name:     "proxy-mode-delegates",
		mode:     corenet.NetProxy,
		upstream: "http://127.0.0.1:9",
		request:  "anything.example:443",
	},
	{
		name:     "proxy-mode-deny-subdomain",
		mode:     corenet.NetProxy,
		upstream: "http://127.0.0.1:9",
		rules:    []netRuleSample{{deny: true, host: "*.internal", reason: "internal"}},
		request:  "db.internal:5432",
	},

	// -- refusals that are not about the destination --
	{
		name:    "matcher-refuses-the-rules",
		mode:    corenet.NetAllowList,
		rules:   []netRuleSample{{host: "*"}},
		request: "example.com:443",
	},
}

// proxyMITMSample is one ShouldMITM question.
type proxyMITMSample struct {
	name     string
	hosts    []string
	excludes []string
	queries  []string
}

// proxyMITMSamples are ShouldMITM's inputs: the include/exclude sets and the
// hosts asked about.
var proxyMITMSamples = []proxyMITMSample{
	{
		name:    "no-selection",
		queries: []string{"example.com", "sub.example.com", "127.0.0.1", "10.1.2.3", ""},
	},
	{
		name:    "includes-only",
		hosts:   []string{"example.com"},
		queries: []string{"example.com", "sub.example.com", "other.example", "example.com.evil"},
	},
	{
		name:     "excludes-only",
		excludes: []string{"internal.example"},
		queries:  []string{"internal.example", "db.internal.example", "example.com"},
	},
	{
		// Excludes win over includes.
		name:     "exclude-inside-include",
		hosts:    []string{"example.com"},
		excludes: []string{"*.example.com"},
		queries:  []string{"example.com", "www.example.com"},
	},
	{
		name:    "ip-pattern",
		hosts:   []string{"10.0.0.0/8"},
		queries: []string{"10.1.2.3", "127.0.0.1", "mapped.example"},
	},
	{
		name:    "empty-include-list",
		hosts:   []string{},
		queries: []string{"example.com"},
	},
	{
		name:    "invalid-pattern",
		hosts:   []string{"*"},
		queries: []string{"example.com"},
	},
}
