package policy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// This file is the network half of a grant. Its types are part of the deployment
// document — a Grant is parsed from YAML directly, with no intermediate DTO — so
// every field carries both a json and a yaml tag, and the two enums marshal as
// text rather than as their integer value.
//
// The tags are not cosmetic. Grant.Digest hashes the marshalled form, so a tag is
// part of a session's identity: renaming one invalidates every checkpoint. That is
// why the field names are pinned by a test rather than left to convention.

// NetMode names the network access posture the sandbox should enforce.
type NetMode int

const (
	// NetDefault leaves networking to the host.
	NetDefault NetMode = iota
	// NetDenyAll forbids outbound connections.
	NetDenyAll
	// NetAllowList permits only the destinations listed in AllowHosts and Rules.
	NetAllowList
	// NetProxy routes traffic through Proxy.
	NetProxy
)

// String renders the mode as it appears in a document.
func (m NetMode) String() string {
	switch m {
	case NetDefault:
		return "default"
	case NetDenyAll:
		return "deny_all"
	case NetAllowList:
		return "allow_list"
	case NetProxy:
		return "proxy"
	default:
		return "unknown"
	}
}

func parseNetMode(text string) (NetMode, error) {
	switch text {
	case "default", "":
		return NetDefault, nil
	case "deny_all":
		return NetDenyAll, nil
	case "allow_list":
		return NetAllowList, nil
	case "proxy":
		return NetProxy, nil
	default:
		return NetDefault, errdefs.Validationf(
			"net: unknown mode %q (want default, deny_all, allow_list or proxy)", text)
	}
}

// MarshalText renders the mode for a YAML document.
func (m NetMode) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText parses the mode from a YAML document.
func (m *NetMode) UnmarshalText(text []byte) error {
	parsed, err := parseNetMode(string(text))
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// MarshalJSON renders the mode as a string. encoding/json does not consult
// TextMarshaler for values, and the deployment loader decodes through JSON
// semantics, so this is the method that actually runs there.
func (m NetMode) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }

// UnmarshalJSON parses the mode from a JSON document.
func (m *NetMode) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errdefs.Validationf("net: mode must be a string: %w", err)
	}
	return m.UnmarshalText([]byte(text))
}

// NetPolicy controls outbound networking for the child process.
type NetPolicy struct {
	// Mode is the posture a backend enforces.
	Mode NetMode `json:"mode,omitempty" yaml:"mode,omitempty"`
	// AllowHosts is the host allow-list: hostname suffixes and exact IP
	// literals, with an optional ":port" suffix ("api.example.com:443"). An
	// entry without a port matches any port. Prefer Rules for new
	// configurations.
	AllowHosts []string `json:"allow_hosts,omitempty" yaml:"allow_hosts,omitempty"`
	// Rules are evaluated in the declared order; the first match wins.
	Rules []NetRule `json:"rules,omitempty" yaml:"rules,omitempty"`
	// Proxy is the URL traffic is routed through when Mode is NetProxy.
	Proxy string `json:"proxy,omitempty" yaml:"proxy,omitempty"`
	// UnixSockets lists local sockets a sandboxed process may reach.
	UnixSockets []string `json:"unix_sockets,omitempty" yaml:"unix_sockets,omitempty"`
	// MITM enables TLS termination for CONNECT traffic.
	MITM *MITMPolicy `json:"mitm,omitempty" yaml:"mitm,omitempty"`
}

// NetAction is the verdict of one network rule.
type NetAction int

const (
	// NetDeny refuses the destination. It is the zero value: a rule that does
	// not say "allow" denies.
	NetDeny NetAction = iota
	// NetAllow permits the destination.
	NetAllow
)

// String renders the verdict as it appears in a document.
func (a NetAction) String() string {
	switch a {
	case NetDeny:
		return "deny"
	case NetAllow:
		return "allow"
	default:
		return "unknown"
	}
}

func parseNetAction(text string) (NetAction, error) {
	switch text {
	case "deny", "":
		return NetDeny, nil
	case "allow":
		return NetAllow, nil
	default:
		return NetDeny, errdefs.Validationf("net: unknown action %q (want allow or deny)", text)
	}
}

// MarshalText renders the verdict for a YAML document.
func (a NetAction) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// UnmarshalText parses the verdict from a YAML document.
func (a *NetAction) UnmarshalText(text []byte) error {
	parsed, err := parseNetAction(string(text))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// MarshalJSON renders the verdict as a string, for the JSON-semantics loader.
func (a NetAction) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// UnmarshalJSON parses the verdict from a JSON document.
func (a *NetAction) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errdefs.Validationf("net: action must be a string: %w", err)
	}
	return a.UnmarshalText([]byte(text))
}

// NetRule is one host/port-level allow or deny rule.
type NetRule struct {
	// Action is the verdict. It has no omitempty: deny is the zero value, and a
	// dropped deny rule would invert the policy.
	Action NetAction `json:"action" yaml:"action"`
	// Host is a hostname suffix or an exact IP literal.
	Host string `json:"host" yaml:"host"`
	// Port limits the rule to one port; zero matches any port.
	Port int `json:"port,omitempty" yaml:"port,omitempty"`
	// Reason is an optional model-facing explanation attached to a deny rule.
	// It is surfaced in proxy denial responses and audit records so an agent
	// can see what was blocked and why. Reasons on allow rules are ignored.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// MITMPolicy enables TLS termination and content hooks for CONNECT traffic.
type MITMPolicy struct {
	// Enabled turns interception on.
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// InspectBodies reads request and response bodies, not just headers.
	InspectBodies bool `json:"inspect_bodies,omitempty" yaml:"inspect_bodies,omitempty"`
	// MaxBodyBytes caps how much of a body is inspected; zero means the
	// implementation default.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty" yaml:"max_body_bytes,omitempty"`
	// Hosts are the destinations to intercept.
	Hosts []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	// ExcludeHosts are destinations never to intercept.
	ExcludeHosts []string `json:"exclude_hosts,omitempty" yaml:"exclude_hosts,omitempty"`
}

// Validate checks NetPolicy's cross-backend invariants.
func (p NetPolicy) Validate() error {
	if p.Mode < NetDefault || p.Mode > NetProxy {
		return errdefs.Validationf("net: unknown net mode %d", int(p.Mode))
	}
	if p.Proxy != "" {
		u, err := url.Parse(p.Proxy)
		if err != nil {
			return errdefs.Validationf("net: invalid proxy URL %q: %w", p.Proxy, err)
		}
		if u.Scheme != "http" && u.Scheme != "socks5" {
			return errdefs.Validationf("net: proxy scheme %q must be http or socks5", u.Scheme)
		}
		if u.Host == "" {
			return errdefs.Validationf("net: proxy URL %q has no host", p.Proxy)
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword && u.User.Username() == "" {
				return errdefs.Validationf("net: proxy URL %q has a password but no username", p.Proxy)
			}
		}
	}
	for i, rule := range p.Rules {
		if rule.Action != NetDeny && rule.Action != NetAllow {
			return errdefs.Validationf("net: net rule %d has invalid action %d", i, int(rule.Action))
		}
		if strings.TrimSpace(rule.Host) == "" {
			return errdefs.Validationf("net: net rule %d has an empty host", i)
		}
		if rule.Port < 0 || rule.Port > 65535 {
			return errdefs.Validationf("net: net rule %d port %d out of range", i, rule.Port)
		}
		if strings.TrimSpace(rule.Reason) != rule.Reason {
			return errdefs.Validationf("net: net rule %d reason has leading or trailing whitespace", i)
		}
		if strings.ContainsAny(rule.Reason, "\r\n") {
			return errdefs.Validationf("net: net rule %d reason must be a single line", i)
		}
	}
	for _, path := range p.UnixSockets {
		if !filepath.IsAbs(path) {
			return errdefs.Validationf("net: unix socket path %q must be absolute", path)
		}
	}
	if p.MITM != nil {
		if p.MITM.MaxBodyBytes < 0 {
			return errdefs.Validationf("net: mitm.max_body_bytes must be non-negative")
		}
		for i, host := range p.MITM.Hosts {
			if strings.TrimSpace(host) == "" {
				return errdefs.Validationf("net: mitm.hosts[%d] is empty", i)
			}
		}
		for i, host := range p.MITM.ExcludeHosts {
			if strings.TrimSpace(host) == "" {
				return errdefs.Validationf("net: mitm.exclude_hosts[%d] is empty", i)
			}
		}
	}
	return nil
}

// Normalized returns the canonical form: the two unordered lists are sorted and
// deduplicated so that one policy has one digest, while Rules keep their declared
// order because they are matched in order.
func (p NetPolicy) Normalized() (NetPolicy, error) {
	if err := p.Validate(); err != nil {
		return NetPolicy{}, err
	}
	out := p
	out.AllowHosts = sortedUnique(p.AllowHosts)
	out.UnixSockets = sortedUnique(p.UnixSockets)
	out.Rules = append([]NetRule(nil), p.Rules...)
	if p.MITM != nil {
		mitm := *p.MITM
		mitm.Hosts = sortedUnique(p.MITM.Hosts)
		mitm.ExcludeHosts = sortedUnique(p.MITM.ExcludeHosts)
		out.MITM = &mitm
	}
	return out, nil
}

// IsolatingNetModes returns the postures a backend that confines egress at the OS
// level can enforce: deny everything, permit an allow-list, or pin traffic to an
// enforcement proxy. NetDefault is deliberately absent — it is the absence of a
// policy, not an enforceable posture.
//
// It is a function rather than a package variable so one backend cannot mutate
// what another one reports.
func IsolatingNetModes() []NetMode {
	return []NetMode{NetDenyAll, NetAllowList, NetProxy}
}

// IsZero reports whether the network policy asks for nothing: no mode, no rules,
// no proxy. It is what an unconfigured host produces.
func (p NetPolicy) IsZero() bool {
	return p.Mode == NetDefault && len(p.AllowHosts) == 0 && len(p.Rules) == 0 &&
		p.Proxy == "" && len(p.UnixSockets) == 0 && p.MITM == nil
}

// String renders a rule for an audit record or a model-facing message.
func (r NetRule) String() string {
	host := r.Host
	if r.Port != 0 {
		host = fmt.Sprintf("%s:%d", r.Host, r.Port)
	}
	return fmt.Sprintf("%s %s", r.Action, host)
}
