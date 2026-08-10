package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// VersionV1 is the only supported sandbox document version.
const VersionV1 = "v1"

// Network mode names accepted by Defaults.Net.
const (
	NetModeDefault   = "default"
	NetModeDenyAll   = "deny_all"
	NetModeAllowList = "allow_list"
	NetModeProxy     = "proxy"
)

// Document declares independently named sandbox resources.
type Document struct {
	Version   string                  `json:"version"`
	Sandboxes map[string]SandboxEntry `json:"sandboxes"`
}

// SandboxEntry binds a backend to a host-rooted workspace.
type SandboxEntry struct {
	Backend         string          `json:"backend"`
	Workspace       string          `json:"workspace"`
	Settings        json.RawMessage `json:"settings,omitempty"`
	Defaults        Defaults        `json:"defaults,omitempty"`
	AllowedCommands []string        `json:"allowed_commands,omitempty"`
	Approval        *ApprovalConfig `json:"approval,omitempty"`
}

// Duration is a duration string such as "30s" or "2m".
type Duration time.Duration

// UnmarshalJSON rejects unitless numbers and parses Go duration strings.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\"")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	*d = Duration(parsed)
	return nil
}

// Defaults is the daemon-owned subset of sandbox.ExecOptions.
// WorkDir and Stdin deliberately are not configurable here.
type Defaults struct {
	Timeout   Duration         `json:"timeout,omitempty"`
	Env       EnvDefaults      `json:"env,omitempty"`
	Net       NetDefaults      `json:"net,omitempty"`
	Resources ResourceDefaults `json:"resources,omitempty"`
}

// EnvDefaults controls inherited and injected environment values.
type EnvDefaults struct {
	Allow  []string          `json:"allow,omitempty"`
	Inject map[string]string `json:"inject,omitempty"`
}

// NetDefaults configures outbound network policy.
type NetDefaults struct {
	Mode        string        `json:"mode,omitempty"`
	AllowHosts  []string      `json:"allow_hosts,omitempty"`
	Rules       []NetRuleJSON `json:"rules,omitempty"`
	Proxy       string        `json:"proxy,omitempty"`
	UnixSockets []string      `json:"unix_sockets,omitempty"`
	MITM        *MITMJSON     `json:"mitm,omitempty"`
}

// NetRuleJSON is the wire form of sandbox.NetRule. Action is "allow"
// or "deny"; port 0 means any port.
type NetRuleJSON struct {
	Action string `json:"action"`
	Host   string `json:"host"`
	Port   int    `json:"port,omitempty"`
}

// MITMJSON is the wire form of sandbox.MITMPolicy.
type MITMJSON struct {
	Enabled       bool     `json:"enabled,omitempty"`
	InspectBodies bool     `json:"inspect_bodies,omitempty"`
	MaxBodyBytes  int64    `json:"max_body_bytes,omitempty"`
	Hosts         []string `json:"hosts,omitempty"`
	ExcludeHosts  []string `json:"exclude_hosts,omitempty"`
}

// ResourceDefaults configures process resource limits.
type ResourceDefaults struct {
	CPUMillicores  int   `json:"cpu_millicores,omitempty"`
	MemoryBytes    int64 `json:"memory_bytes,omitempty"`
	DiskBytes      int64 `json:"disk_bytes,omitempty"`
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"`
}

// ApprovalConfig selects policy-boundary predicates.
type ApprovalConfig struct {
	OutsideWorkDir    bool     `json:"outside_workdir,omitempty"`
	NonDefaultNetwork bool     `json:"non_default_network,omitempty"`
	Interactive       bool     `json:"interactive,omitempty"`
	SensitiveCommands []string `json:"sensitive_commands,omitempty"`
}

// Validate checks all backend-independent document invariants.
func (d Document) Validate() error {
	if d.Version != VersionV1 {
		return errdefs.Validationf(
			"sandbox config version %q is not supported (want %q)",
			d.Version, VersionV1)
	}
	if d.Sandboxes == nil {
		return errdefs.Validationf("sandbox config sandboxes map is required")
	}
	for name, entry := range d.Sandboxes {
		if name == "" {
			return errdefs.Validationf("sandbox config sandboxes: empty sandbox name")
		}
		if entry.Backend == "" {
			return errdefs.Validationf(
				"sandbox config sandboxes[%q]: backend is required", name)
		}
		if entry.Workspace == "" {
			return errdefs.Validationf(
				"sandbox config sandboxes[%q]: workspace is required", name)
		}
		if err := entry.validate(); err != nil {
			return errdefs.Validationf(
				"sandbox config sandboxes[%q]: %v", name, err)
		}
	}
	return nil
}

func (e SandboxEntry) validate() error {
	if time.Duration(e.Defaults.Timeout) < 0 {
		return fmt.Errorf("defaults.timeout must be non-negative")
	}
	for i, name := range e.Defaults.Env.Allow {
		if name == "" {
			return fmt.Errorf("defaults.env.allow[%d] is empty", i)
		}
	}
	for name := range e.Defaults.Env.Inject {
		if name == "" {
			return fmt.Errorf("defaults.env.inject contains an empty name")
		}
	}
	if err := e.Defaults.Net.validate(); err != nil {
		return err
	}
	if err := e.Defaults.Resources.validate(time.Duration(e.Defaults.Timeout)); err != nil {
		return err
	}
	for i, command := range e.AllowedCommands {
		if command == "" {
			return fmt.Errorf("allowed_commands[%d] is empty", i)
		}
	}
	if e.Approval != nil {
		for i, pattern := range e.Approval.SensitiveCommands {
			if pattern == "" {
				return fmt.Errorf("approval.sensitive_commands[%d] is empty", i)
			}
			if _, err := filepath.Match(pattern, "command"); err != nil {
				return fmt.Errorf(
					"approval.sensitive_commands[%d] invalid pattern %q: %w",
					i, pattern, err)
			}
		}
	}
	return nil
}

func (n NetDefaults) validate() error {
	switch n.Mode {
	case "", NetModeDefault, NetModeDenyAll:
		if len(n.AllowHosts) != 0 || len(n.Rules) != 0 || n.Proxy != "" ||
			len(n.UnixSockets) != 0 || n.MITM != nil {
			return fmt.Errorf(
				"defaults.net mode %q cannot set allow_hosts, rules, proxy, unix_sockets, or mitm",
				defaultNetMode(n.Mode))
		}
	case NetModeAllowList:
		if len(n.AllowHosts) == 0 && len(n.Rules) == 0 {
			return fmt.Errorf("defaults.net allow_list requires allow_hosts or rules")
		}
		if n.Proxy != "" {
			return fmt.Errorf("defaults.net allow_list cannot set proxy")
		}
	case NetModeProxy:
		if n.Proxy == "" {
			return fmt.Errorf("defaults.net proxy mode requires proxy")
		}
		if len(n.AllowHosts) != 0 {
			return fmt.Errorf("defaults.net proxy mode cannot set allow_hosts")
		}
	default:
		return fmt.Errorf("defaults.net mode %q is invalid", n.Mode)
	}
	for i, host := range n.AllowHosts {
		if host == "" {
			return fmt.Errorf("defaults.net.allow_hosts[%d] is empty", i)
		}
	}
	for i, rule := range n.Rules {
		if rule.Action != "allow" && rule.Action != "deny" {
			return fmt.Errorf("defaults.net.rules[%d].action must be \"allow\" or \"deny\"", i)
		}
	}
	// Reuse the core policy validation for the new fields (proxy
	// scheme, rule actions/ports, unix socket paths, MITM caps).
	return toExecOptions(Defaults{Net: n}).Net.Validate()
}

func defaultNetMode(mode string) string {
	if mode == "" {
		return NetModeDefault
	}
	return mode
}

func (r ResourceDefaults) validate(timeout time.Duration) error {
	if r.CPUMillicores < 0 {
		return fmt.Errorf("defaults.resources.cpu_millicores must be non-negative")
	}
	if r.MemoryBytes < 0 {
		return fmt.Errorf("defaults.resources.memory_bytes must be non-negative")
	}
	if r.DiskBytes < 0 {
		return fmt.Errorf("defaults.resources.disk_bytes must be non-negative")
	}
	if r.MaxOutputBytes < 0 {
		return fmt.Errorf("defaults.resources.max_output_bytes must be non-negative")
	}
	if r.CPUMillicores > 0 && timeout <= 0 {
		return fmt.Errorf(
			"defaults.resources.cpu_millicores requires a positive defaults.timeout")
	}
	return nil
}

func (a *ApprovalConfig) hasPredicates() bool {
	return a != nil &&
		(a.OutsideWorkDir || a.NonDefaultNetwork || a.Interactive ||
			len(a.SensitiveCommands) > 0)
}

// Parse strictly decodes and validates exactly one document. YAML and
// JSON are both accepted.
func Parse(data []byte) (Document, error) {
	doc, err := utils.Decode[Document](data)
	if err != nil {
		return Document{}, errdefs.Validationf(
			"decode sandbox config: %v", err)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}
