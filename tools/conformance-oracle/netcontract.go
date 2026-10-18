package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"

	"github.com/GizClaw/flowcraft/core/utils/net"
)

// The net contract: host-pattern compilation, the sets built from patterns, and
// the Matcher's verdicts over NetPolicy rules.
//
// This is the half of core/utils/net that decides egress: what a pattern means,
// which name it matches, and which rule wins when several do. It is pure --
// Compile, Match and MatchIP do no I/O and no DNS -- so every row here is a
// deterministic function of its inputs, which is what makes it worth pinning
// byte for byte.
//
// The other half (the host-side proxy in netproxy.go and socks5.go, the
// certificate authority in net/mitm) is not here: it is sockets, listeners and
// tunnels, and it is a later slice.

const netContractNote = "core/utils/net: host patterns (domain-and-descendants, leading *. wildcards, " +
	"IP literals, CIDR prefixes, IDNA), Set, and Matcher verdicts. Patterns are asked about every " +
	"host in host_queries and every address in ip_queries; matcher rows carry their own policy and queries."

type netContract struct {
	Note     string          `json:"note"`
	Hosts    []string        `json:"host_queries"`
	IPs      []string        `json:"ip_queries"`
	Patterns []netPatternRow `json:"patterns"`
	Sets     []netSetRow     `json:"sets"`
	Matchers []netMatcherRow `json:"matchers"`
}

type netPatternRow struct {
	Raw     string `json:"raw"`
	Error   string `json:"error,omitempty"`
	IsIP    bool   `json:"is_ip"`
	Match   []bool `json:"match"`
	MatchIP []bool `json:"match_ip"`
}

type netSetRow struct {
	Hosts   []string `json:"hosts"`
	Error   string   `json:"error,omitempty"`
	Empty   bool     `json:"empty"`
	Match   []bool   `json:"match"`
	MatchIP []bool   `json:"match_ip"`
}

type netRuleRow struct {
	Action string `json:"action"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Reason string `json:"reason,omitempty"`
}

type netHostQueryRow struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Action  string `json:"action"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason,omitempty"`
	Matched bool   `json:"matched"`
}

type netIPQueryRow struct {
	IP      string `json:"ip"`
	Port    int    `json:"port"`
	Action  string `json:"action"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason,omitempty"`
	Matched bool   `json:"matched"`
}

type netMatcherRow struct {
	Name       string            `json:"name"`
	AllowHosts []string          `json:"allow_hosts,omitempty"`
	Rules      []netRuleRow      `json:"rules,omitempty"`
	Error      string            `json:"error,omitempty"`
	HasIPRules bool              `json:"has_ip_rules"`
	Hosts      []netHostQueryRow `json:"hosts,omitempty"`
	IPs        []netIPQueryRow   `json:"ips,omitempty"`
}

func writeNetContract(w io.Writer) error {
	// The query lists are parsed once, here: a sample that does not parse is a
	// bug in the sample file, not something to record as an answer.
	ips := make([]netip.Addr, 0, len(netIPQueries))
	for _, raw := range netIPQueries {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return fmt.Errorf("ip query %q: %w", raw, err)
		}
		ips = append(ips, addr)
	}

	contract := netContract{
		Note:     netContractNote,
		Hosts:    netHostQueries,
		IPs:      netIPQueries,
		Patterns: make([]netPatternRow, 0, len(netPatternSamples)),
		Sets:     make([]netSetRow, 0, len(netSetSamples)),
		Matchers: make([]netMatcherRow, 0, len(netMatcherSamples)),
	}

	for _, raw := range netPatternSamples {
		// A refused pattern carries its sentence and no answers: the vectors
		// are built only for a pattern that exists, so "has an error" and "has
		// answers" stay mutually exclusive in the file.
		row := netPatternRow{Raw: raw}
		pattern, err := net.Compile(raw)
		if err != nil {
			row.Error = err.Error()
			contract.Patterns = append(contract.Patterns, row)
			continue
		}
		row.IsIP = pattern.IsIP()
		row.Match = make([]bool, 0, len(netHostQueries))
		for _, host := range netHostQueries {
			row.Match = append(row.Match, pattern.Match(host))
		}
		row.MatchIP = make([]bool, 0, len(ips))
		for _, addr := range ips {
			row.MatchIP = append(row.MatchIP, pattern.MatchIP(addr))
		}
		contract.Patterns = append(contract.Patterns, row)
	}

	for _, hosts := range netSetSamples {
		row := netSetRow{Hosts: hosts}
		set, err := net.New(hosts)
		if err != nil {
			row.Error = err.Error()
			contract.Sets = append(contract.Sets, row)
			continue
		}
		row.Empty = set.Empty()
		row.Match = make([]bool, 0, len(netHostQueries))
		for _, host := range netHostQueries {
			row.Match = append(row.Match, set.Match(host))
		}
		row.MatchIP = make([]bool, 0, len(ips))
		for _, addr := range ips {
			row.MatchIP = append(row.MatchIP, set.MatchIP(addr))
		}
		contract.Sets = append(contract.Sets, row)
	}

	for _, sample := range netMatcherSamples {
		policy := net.NetPolicy{
			Mode:       net.NetAllowList,
			AllowHosts: sample.allowHosts,
		}
		row := netMatcherRow{Name: sample.name, AllowHosts: sample.allowHosts}
		for _, rule := range sample.rules {
			action := net.NetAllow
			if rule.deny {
				action = net.NetDeny
			}
			policy.Rules = append(policy.Rules, net.NetRule{
				Action: action,
				Host:   rule.host,
				Port:   rule.port,
				Reason: rule.reason,
			})
			row.Rules = append(row.Rules, netRuleRow{
				Action: action.String(),
				Host:   rule.host,
				Port:   rule.port,
				Reason: rule.reason,
			})
		}
		matcher, err := net.NewMatcher(policy)
		if err != nil {
			row.Error = err.Error()
			contract.Matchers = append(contract.Matchers, row)
			continue
		}
		row.HasIPRules = matcher.HasIPRules()
		for _, query := range sample.hosts {
			action, rule, reason, matched := matcher.Match(query.host, query.port)
			row.Hosts = append(row.Hosts, netHostQueryRow{
				Host:    query.host,
				Port:    query.port,
				Action:  action.String(),
				Rule:    rule,
				Reason:  reason,
				Matched: matched,
			})
		}
		for _, query := range sample.ips {
			addr, err := netip.ParseAddr(query.ip)
			if err != nil {
				return fmt.Errorf("matcher %s: ip query %q: %w", sample.name, query.ip, err)
			}
			action, rule, reason, matched := matcher.MatchIP(addr, query.port)
			row.IPs = append(row.IPs, netIPQueryRow{
				IP:      query.ip,
				Port:    query.port,
				Action:  action.String(),
				Rule:    rule,
				Reason:  reason,
				Matched: matched,
			})
		}
		contract.Matchers = append(contract.Matchers, row)
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}
