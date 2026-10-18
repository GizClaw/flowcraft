package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	corenet "github.com/GizClaw/flowcraft/core/utils/net"
	"github.com/GizClaw/flowcraft/core/utils/net/mitm"
)

// The proxy contract: the half of core/utils/net that answers "what would the
// proxy do" without opening a socket.
//
// Five questions are dumped, each a place a port can be plausibly wrong while
// still looking right:
//
//	urls        -- url.Parse's answer, including its error spelling, for the
//	               proxy-URL grammar NetPolicy.Validate reads.
//	validations -- NetPolicy.Validate's refusals, in core's order and wording.
//	decisions   -- Proxy.Decide: the verdict, the decisive rule, the deny
//	               reason, the split of the destination, and the address the
//	               dial is pinned to.
//	mitm        -- ShouldMITM's include/exclude selection.
//
// What is not here: the listener, the HTTP and SOCKS5 wire, the tunnels, and
// TLS termination. Those are not functions of their input; the contracts that
// pin them drive a real proxy, which is a later slice.
//
// Two Go branches have no row because a Rust enum cannot hold the input: an
// unknown NetMode and an unknown NetAction. Both are refusals in Validate, and
// both are unreachable through the port's typed API.

const proxyContractNote = "core/utils/net: the proxy's policy half -- the url.Parse grammar " +
	"NetPolicy.Validate reads, its refusals, Proxy.Decide verdicts (split, rule, reason, pinned " +
	"dial address), and the MITM include/exclude selection. Sockets and tunnels are not here."

type proxyContract struct {
	Note        string             `json:"note"`
	URLs        []proxyURLRow      `json:"urls"`
	Validations []proxyPolicyRow   `json:"validations"`
	Decisions   []proxyDecisionRow `json:"decisions"`
	MITM        []proxyMITMRow     `json:"mitm"`
}

// proxyURLRow is one url.Parse answer. The flags are unconditional so that
// "no userinfo" and "an empty username" stay distinguishable; the strings are
// omitted when empty, which keeps an error row to one field.
type proxyURLRow struct {
	Raw         string `json:"raw"`
	Error       string `json:"error,omitempty"`
	Scheme      string `json:"scheme,omitempty"`
	Host        string `json:"host,omitempty"`
	HasUser     bool   `json:"has_user"`
	User        string `json:"user,omitempty"`
	HasPassword bool   `json:"has_password"`
	Password    string `json:"password,omitempty"`
	Path        string `json:"path,omitempty"`
	RawQuery    string `json:"raw_query,omitempty"`
	Fragment    string `json:"fragment,omitempty"`
	Opaque      string `json:"opaque,omitempty"`
}

// proxyPolicyRow is one policy and Validate's answer for it.
type proxyPolicyRow struct {
	Name        string              `json:"name"`
	Mode        string              `json:"mode"`
	Proxy       string              `json:"proxy,omitempty"`
	AllowHosts  []string            `json:"allow_hosts,omitempty"`
	Rules       []netRuleRow        `json:"rules,omitempty"`
	UnixSockets []string            `json:"unix_sockets,omitempty"`
	MITM        *proxyMITMPolicyRow `json:"mitm,omitempty"`
	Error       string              `json:"error,omitempty"`
}

type proxyMITMPolicyRow struct {
	Enabled       bool     `json:"enabled"`
	InspectBodies bool     `json:"inspect_bodies"`
	MaxBodyBytes  int64    `json:"max_body_bytes"`
	Hosts         []string `json:"hosts,omitempty"`
	ExcludeHosts  []string `json:"exclude_hosts,omitempty"`
}

type proxyDecisionRow struct {
	Name        string       `json:"name"`
	Mode        string       `json:"mode"`
	Upstream    string       `json:"upstream,omitempty"`
	AllowHosts  []string     `json:"allow_hosts,omitempty"`
	Rules       []netRuleRow `json:"rules,omitempty"`
	Request     string       `json:"request"`
	DefaultPort int          `json:"default_port"`
	Error       string       `json:"error,omitempty"`
	Host        string       `json:"host"`
	Port        int          `json:"port"`
	Action      string       `json:"action"`
	Rule        string       `json:"rule"`
	Reason      string       `json:"reason,omitempty"`
	DialHost    string       `json:"dial_host"`
}

type proxyMITMRow struct {
	Name     string   `json:"name"`
	Hosts    []string `json:"hosts,omitempty"`
	Excludes []string `json:"exclude_hosts,omitempty"`
	Error    string   `json:"error,omitempty"`
	Queries  []string `json:"queries,omitempty"`
	Answers  []bool   `json:"answers,omitempty"`
}

func proxyModeName(mode corenet.NetMode) string {
	switch mode {
	case corenet.NetDefault:
		return "default"
	case corenet.NetDenyAll:
		return "deny_all"
	case corenet.NetAllowList:
		return "allow_list"
	case corenet.NetProxy:
		return "proxy"
	}
	return fmt.Sprintf("unknown(%d)", int(mode))
}

func proxyNetRule(sample netRuleSample) corenet.NetRule {
	action := corenet.NetAllow
	if sample.deny {
		action = corenet.NetDeny
	}
	return corenet.NetRule{
		Action: action,
		Host:   sample.host,
		Port:   sample.port,
		Reason: sample.reason,
	}
}

func proxyNetRules(samples []netRuleSample) []corenet.NetRule {
	rules := make([]corenet.NetRule, 0, len(samples))
	for _, sample := range samples {
		rules = append(rules, proxyNetRule(sample))
	}
	return rules
}

func proxyRuleRows(samples []netRuleSample) []netRuleRow {
	rows := make([]netRuleRow, 0, len(samples))
	for _, sample := range samples {
		rows = append(rows, netRuleRow{
			Action: proxyNetRule(sample).Action.String(),
			Host:   sample.host,
			Port:   sample.port,
			Reason: sample.reason,
		})
	}
	return rows
}

func proxyMITMPolicyRowOf(policy *proxyMITMPolicySample) *proxyMITMPolicyRow {
	if policy == nil {
		return nil
	}
	return &proxyMITMPolicyRow{
		Enabled:       policy.enabled,
		InspectBodies: policy.inspectBodies,
		MaxBodyBytes:  policy.maxBodyBytes,
		Hosts:         policy.hosts,
		ExcludeHosts:  policy.excludeHosts,
	}
}

func proxyNetPolicy(sample proxyPolicySample) corenet.NetPolicy {
	policy := corenet.NetPolicy{
		Mode:        sample.mode,
		AllowHosts:  sample.allowHosts,
		Rules:       proxyNetRules(sample.rules),
		Proxy:       sample.proxy,
		UnixSockets: sample.unixSockets,
	}
	if sample.mitm != nil {
		policy.MITM = &corenet.MITMPolicy{
			Enabled:       sample.mitm.enabled,
			InspectBodies: sample.mitm.inspectBodies,
			MaxBodyBytes:  sample.mitm.maxBodyBytes,
			Hosts:         sample.mitm.hosts,
			ExcludeHosts:  sample.mitm.excludeHosts,
		}
	}
	return policy
}

func writeProxyContract(w io.Writer) error {
	contract := proxyContract{
		Note:        proxyContractNote,
		URLs:        make([]proxyURLRow, 0, len(proxyURLSamples)),
		Validations: make([]proxyPolicyRow, 0, len(proxyValidateSamples)),
		Decisions:   make([]proxyDecisionRow, 0, len(proxyDecisionSamples)),
		MITM:        make([]proxyMITMRow, 0, len(proxyMITMSamples)),
	}

	for _, raw := range proxyURLSamples {
		contract.URLs = append(contract.URLs, proxyURLRowOf(raw))
	}

	for _, sample := range proxyValidateSamples {
		row := proxyPolicyRow{
			Name:        sample.name,
			Mode:        proxyModeName(sample.mode),
			Proxy:       sample.proxy,
			AllowHosts:  sample.allowHosts,
			Rules:       proxyRuleRows(sample.rules),
			UnixSockets: sample.unixSockets,
			MITM:        proxyMITMPolicyRowOf(sample.mitm),
		}
		if err := proxyNetPolicy(sample).Validate(); err != nil {
			row.Error = err.Error()
		}
		contract.Validations = append(contract.Validations, row)
	}

	for _, sample := range proxyDecisionSamples {
		row, err := proxyDecisionRowOf(sample)
		if err != nil {
			return err
		}
		contract.Decisions = append(contract.Decisions, row)
	}

	for _, sample := range proxyMITMSamples {
		row, err := proxyMITMRowOf(sample)
		if err != nil {
			return err
		}
		contract.MITM = append(contract.MITM, row)
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}

func proxyURLRowOf(raw string) proxyURLRow {
	row := proxyURLRow{Raw: raw}
	parsed, err := url.Parse(raw)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Scheme = parsed.Scheme
	row.Host = parsed.Host
	row.Path = parsed.Path
	row.RawQuery = parsed.RawQuery
	row.Fragment = parsed.Fragment
	row.Opaque = parsed.Opaque
	if parsed.User != nil {
		row.HasUser = true
		row.User = parsed.User.Username()
		password, has := parsed.User.Password()
		row.HasPassword = has
		row.Password = password
	}
	return row
}

func proxyDecisionRowOf(sample proxyDecisionSample) (proxyDecisionRow, error) {
	row := proxyDecisionRow{
		Name:        sample.name,
		Mode:        proxyModeName(sample.mode),
		Upstream:    sample.upstream,
		AllowHosts:  sample.allowHosts,
		Rules:       proxyRuleRows(sample.rules),
		Request:     sample.request,
		DefaultPort: sample.defaultPort,
	}
	proxy, err := corenet.Start(corenet.ProxyConfig{
		Mode:        sample.mode,
		Upstream:    sample.upstream,
		AllowHosts:  sample.allowHosts,
		Rules:       proxyNetRules(sample.rules),
		TCPLoopback: true,
	})
	if err != nil {
		return row, fmt.Errorf("decision %s: start: %w", sample.name, err)
	}
	defer func() {
		if err := proxy.Close(); err != nil {
			panic(fmt.Sprintf("decision %s: close: %v", sample.name, err))
		}
	}()
	decision, err := proxy.Decide(context.Background(), sample.request, sample.defaultPort)
	if err != nil {
		row.Error = err.Error()
	}
	row.Host = decision.Host
	row.Port = decision.Port
	row.Action = decision.Action.String()
	row.Rule = decision.Rule
	row.Reason = decision.Reason
	row.DialHost = decision.DialHost
	return row, nil
}

func proxyMITMRowOf(sample proxyMITMSample) (proxyMITMRow, error) {
	row := proxyMITMRow{
		Name:     sample.name,
		Hosts:    sample.hosts,
		Excludes: sample.excludes,
		Queries:  sample.queries,
	}
	engine, err := mitm.New(&corenet.MITMPolicy{
		Enabled:      true,
		Hosts:        sample.hosts,
		ExcludeHosts: sample.excludes,
	}, nil, nil, nil)
	if err != nil {
		row.Error = err.Error()
		row.Queries = nil
		return row, nil
	}
	row.Answers = make([]bool, 0, len(sample.queries))
	for _, query := range sample.queries {
		row.Answers = append(row.Answers, engine.ShouldMITM(query))
	}
	return row, nil
}
