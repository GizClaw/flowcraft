package main

// The samples the net contract is computed from: host patterns, the sets built
// from them, and the policies a Matcher is compiled from.
//
// The shape is two shared query lists plus one row per pattern, so the dump
// answers "which of these hosts does this pattern match" for every
// (pattern, host) pair without repeating either side. That is the question a
// port can get plausibly wrong: domain-and-descendants versus wildcard,
// normalization on the pattern side versus the query side, and IDNA converting
// one and not the other.

// netHostQueries are the hostnames every pattern is matched against. They are
// deliberately redundant: the same name in ASCII, punycode, uppercase, with a
// trailing dot, and as a subdomain, so a port that normalizes only one side
// shows up as a disagreement rather than as an invalid host.
var netHostQueries = []string{
	"example.com",
	"Example.COM",
	"example.com.",
	"www.example.com",
	"a.b.example.com",
	"example.com.evil",
	"notexample.com",
	"example.co",
	"com",
	"",
	" ",
	" example.com",
	"example.com ",
	"example.com..",
	"localhost",
	"127.0.0.1",
	"10.1.2.3",
	"::1",
	"[::1]",
	// Punycode and its unicode spelling, both directions.
	"xn--e1afmkfd.xn--p1ai",
	"例え.テスト",
	"sub.例え.テスト",
	"ЕХАМПЛЕ.РФ",
	"ехампле.рф",
	// The names UTS #46 maps rather than rejects.
	"faß.de",
	"fass.de",
	"Bücher.example",
	"bücher.example",
	"xn--bcher-kva.example",
	// Names STD3 or the hyphen rules refuse: no conversion, compared raw.
	"a_b.example.com",
	"-ab.example.com",
	"ab--cd.example.com",
	"a..example.com",
	"a:b",
}

// netIPQueries are the addresses every pattern is matched against, in Go's
// spelling. All of them parse; the ones that do not are patterns (below), where
// the fall-through to hostname treatment is the behavior worth pinning.
var netIPQueries = []string{
	"1.2.3.4",
	"1.2.3.5",
	"10.0.0.1",
	"10.1.2.3",
	"10.255.255.255",
	"11.0.0.1",
	"127.0.0.1",
	"192.168.1.1",
	"194.0.0.1",
	"10.0.0.0",
	"0.0.0.0",
	"::1",
	// The same address spelled out: parsing is what carries the equivalence,
	// and a port that compares text instead of addresses shows up here.
	"0:0:0:0:0:0:0:1",
	"::",
	"2001:db8::1",
	"fe80::1",
	"fe80::1%eth0",
	// IPv4-mapped: BitLen 128, and Unmap turns it into the IPv4 address.
	"::ffff:1.2.3.4",
	"::ffff:10.1.2.3",
	// IPv4-compatible, which Unmap leaves alone.
	"::1.2.3.4",
}

// netPatternSamples are the raw pattern strings compiled by core's Compile.
var netPatternSamples = []string{
	// The four forms the doc comment names.
	"example.com",
	"*.example.com",
	"1.2.3.4",
	"10.0.0.0/8",
	// Domain-and-descendants: a bare domain matches itself and every
	// subdomain, so the ports that differ are the near misses.
	"example",
	"example.co",
	"sub.example.com",
	// Wildcards never match the bare domain, and only the leading "*." is a
	// wildcard.
	"*.example",
	"*.com",
	"*",
	"*example.com",
	"a.*.com",
	"example.*",
	"*.",
	"*..example.com",
	"*.*.example.com",
	// Normalization on the pattern side: case, trailing dots, and whitespace.
	"Example.COM",
	"example.com.",
	"example.com..",
	" example.com ",
	"   10.0.0.0/8   ",
	"10.0.0.0/8 ",
	// IDNA: converted when Lookup accepts the name, left raw when it does
	// not.
	"例え.テスト",
	"*.例え.テスト",
	"xn--e1afmkfd.xn--p1ai",
	"ЕХАМПЛЕ.РФ",
	"faß.de",
	"Bücher.example",
	// What STD3 and the hyphen checks refuse stays a raw string.
	"a_b.example.com",
	"-ab.example.com",
	"ab--cd.example.com",
	"a..example.com",
	"example.com:443",
	// Addresses and prefixes: what parses decides the kind, and what does not
	// falls through to hostname treatment.
	"1.2.3.4",
	"1.2.3",
	"1.2.3.4.5",
	"010.1.2.3",
	"10.0.0.0/8",
	"10.0.0.0/33",
	"10.0.0.0/",
	"10.0.0.0/-1",
	"10.0.0.0/8/8",
	"194.0.0.1/32",
	"0.0.0.0/0",
	"::",
	"::1",
	"::/0",
	"2001:db8::/32",
	"::ffff:1.2.3.4",
	"::ffff:10.0.0.0/104",
	"::ffff:10.0.0.0/96",
	"fe80::1",
	"fe80::1%eth0",
	"fe80::1%",
	"1.2.3.4%eth0",
	"::1::2",
	"[::1]",
	"[::1]:8443",
	// Empty is refused, and the refusal is a sentence the port prints.
	"",
	"   ",
}

// netSetSamples are the host lists a Set is built from.
var netSetSamples = [][]string{
	{},
	{"example.com"},
	{"*.example.com", "10.0.0.0/8", "1.2.3.4"},
	{"example.com", "*.example.com", "example.co"},
	// One bad entry fails the whole set, naming nothing: New returns the
	// compile error as-is.
	{"example.com", "*bad.example"},
	{"", "example.com"},
}

type netHostPort struct {
	host string
	port int
}

type netIPPort struct {
	ip   string
	port int
}

// netMatcherSample is one policy plus the queries asked of its Matcher.
type netMatcherSample struct {
	name       string
	allowHosts []string
	rules      []netRuleSample
	hosts      []netHostPort
	ips        []netIPPort
}

// netRuleSample is a NetRule in the sample file, so this file does not import
// the package it is measuring.
type netRuleSample struct {
	deny   bool
	host   string
	port   int
	reason string
}

var netMatcherSamples = []netMatcherSample{
	{
		// core's matcher_test.go policy: the ":port" suffix grammar, deny
		// reasons, and the three allow-list entry forms.
		name:       "core-matcher-test",
		allowHosts: []string{"example.com:443", "[::1]:8443", "10.0.0.0/8:53", "bare.example.com"},
		rules: []netRuleSample{
			{deny: true, host: "evil.example", reason: "known malware domain"},
			{deny: true, host: "example.com", port: 22, reason: "ssh egress is blocked"},
		},
		hosts: []netHostPort{
			{"example.com", 443},
			{"www.example.com", 443},
			{"example.com", 80},
			{"example.com", 22},
			{"evil.example", 80},
			{"bare.example.com", 9999},
			{"EXAMPLE.com.", 443},
			{"10.1.2.3", 53},
		},
		ips: []netIPPort{
			{"10.1.2.3", 53},
			{"10.1.2.3", 54},
			{"::1", 8443},
			{"::1", 443},
			{"10.1.2.3", 0},
		},
	},
	{
		// Deny wins over allow regardless of order: the deny is listed second
		// and the allow first, and the allow is the one that names a port.
		name: "deny-first-across-the-set",
		rules: []netRuleSample{
			{host: "example.com"},
			{host: "*.example.com", deny: true, reason: "subdomains are not allowed"},
			{host: "api.example.com", reason: "the allow reason comes back too"},
		},
		hosts: []netHostPort{
			{"example.com", 443},
			{"api.example.com", 443},
			{"www.example.com", 443},
			{"a.b.example.com", 443},
			{"other.example", 443},
		},
	},
	{
		// A ":port"-less deny rule matches every port, and the legacy
		// allow-list is appended after the explicit rules, so an explicit deny
		// still wins over an allow_hosts entry with the same host.
		name:       "legacy-allow-list-loses-to-deny",
		allowHosts: []string{"example.com", "*.cdn.example"},
		rules: []netRuleSample{
			{host: "example.com", port: 25, deny: true, reason: "smtp is never allowed"},
		},
		hosts: []netHostPort{
			{"example.com", 25},
			{"example.com", 443},
			{"img.cdn.example", 80},
			{"cdn.example", 80},
		},
	},
	{
		// The ":port" suffix grammar, one case per policy so a refusal does not
		// hide the cases behind it. A suffix of 0 means any port, and the
		// description drops it.
		name:       "port-suffix-any",
		allowHosts: []string{"example.com:0"},
		hosts: []netHostPort{
			{"example.com", 0},
			{"example.com", 8080},
		},
	},
	{
		name:       "port-suffix-max",
		allowHosts: []string{"example.com:65535"},
		hosts: []netHostPort{
			{"example.com", 65535},
			{"example.com", 65534},
		},
	},
	{
		// A space after the colon is not a port: SplitHostPort accepts the
		// shape, Atoi refuses the text, and the rule is a validation error.
		name:       "port-suffix-space",
		allowHosts: []string{"host.example: 443"},
		hosts:      []netHostPort{{"host.example", 443}},
	},
	{
		name:       "port-suffix-empty",
		allowHosts: []string{"example.com:"},
		hosts:      []netHostPort{{"example.com", 443}},
	},
	{
		name:       "port-suffix-negative",
		allowHosts: []string{"example.com:-1"},
		hosts:      []netHostPort{{"example.com", 443}},
	},
	{
		name:       "port-suffix-out-of-range",
		allowHosts: []string{"example.com:99999"},
		hosts:      []netHostPort{{"example.com", 443}},
	},
	{
		// Two colons: SplitHostPort refuses the shape, so the whole thing falls
		// through to hostname treatment instead of being an error.
		name:       "port-suffix-too-many-colons",
		allowHosts: []string{"colon.example:443:80"},
		hosts: []netHostPort{
			{"colon.example", 443},
			{"colon.example:443:80", 443},
		},
	},
	{
		// A bare IPv6 literal needs no brackets: SplitHostPort refuses it, and
		// what lands is the address itself. Bracketed, it is a hostname.
		name: "bare-and-bracketed-v6",
		rules: []netRuleSample{
			{host: "::1", deny: true, reason: "loopback"},
			{host: "[::1]"},
		},
		ips: []netIPPort{{"::1", 443}},
		hosts: []netHostPort{
			{"[::1]", 443},
			{"::1", 443},
		},
	},
	{
		// A CIDR with a port suffix applies to the resolved address.
		name: "cidr-with-port",
		rules: []netRuleSample{
			{host: "10.0.0.0/8:53", deny: true, reason: "dns"},
		},
		ips: []netIPPort{
			{"10.1.2.3", 53},
			{"10.1.2.3", 5353},
			{"11.1.2.3", 53},
		},
	},
	{
		// Refusals: an empty rule host, a malformed port suffix on both sides,
		// and a bad pattern. Each is a sentence the port must print.
		name:       "refusals",
		allowHosts: []string{"example.com:not-a-port"},
		rules: []netRuleSample{
			{host: ""},
			{host: "example.com:99999", deny: true},
		},
		hosts: []netHostPort{{"example.com", 443}},
	},
	{
		name:       "refusal-bad-pattern",
		allowHosts: []string{"*bad.example"},
		hosts:      []netHostPort{{"bad.example", 443}},
	},
	{
		name: "refusal-empty-allow-host",
		allowHosts: []string{
			"",
		},
		hosts: []netHostPort{{"example.com", 443}},
	},
	{
		// HasIPRules, and the IP half of the decision: a deny on one address in
		// a range, an allow on another, and a v6 literal.
		name: "ip-rules",
		rules: []netRuleSample{
			{host: "10.1.2.3", deny: true, reason: "one bad host"},
			{host: "10.0.0.0/8"},
			{host: "192.168.0.0/16", deny: true, reason: "private range"},
			{host: "::1"},
		},
		hosts: []netHostPort{
			// A name that matches no host rule falls through to the mode's
			// default, which the caller applies; Matcher.Match says so with
			// matched=false.
			{"example.com", 443},
			{"10.1.2.3", 443},
		},
		ips: []netIPPort{
			{"10.1.2.3", 443},
			{"10.2.3.4", 443},
			{"192.168.1.1", 443},
			{"192.169.1.1", 443},
			{"::1", 443},
			{"::2", 443},
			{"::ffff:10.1.2.3", 443},
		},
	},
	{
		// An allow rule's reason comes back from Matcher.Match even though
		// core's docs say allow reasons are ignored: what the field means and
		// what the code returns are two different things, and only one of them
		// is observable.
		name: "an-allow-reason-comes-back-too",
		rules: []netRuleSample{
			{host: "allow.example", reason: "a CDN host"},
			{host: "allow.example", port: 22, deny: true, reason: "but ssh is not"},
		},
		hosts: []netHostPort{
			{"allow.example", 443},
			{"allow.example", 22},
		},
	},
	{
		// No rules at all: every query is unmatched, and the answer is allow
		// because the caller applies the mode.
		name:  "empty-policy",
		hosts: []netHostPort{{"example.com", 443}},
		ips:   []netIPPort{{"10.1.2.3", 443}},
	},
	{
		// A policy whose only rule is a v6 CIDR, to pin BitLen matching between
		// a 4-in-6 query and a v6 prefix.
		name: "v6-prefix",
		rules: []netRuleSample{
			{host: "2001:db8::/32"},
			{host: "::/0", deny: true, reason: "all v6"},
		},
		ips: []netIPPort{
			{"2001:db8::1", 443},
			{"2001:db9::1", 443},
			{"::1", 443},
			{"::ffff:10.1.2.3", 443},
		},
	},
}
