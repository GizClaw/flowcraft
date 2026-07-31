package config

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	yamlv3 "gopkg.in/yaml.v3"
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
	Version   string                  `yaml:"version"`
	Sandboxes map[string]SandboxEntry `yaml:"sandboxes"`
}

// SandboxEntry binds a backend to a host-rooted workspace.
type SandboxEntry struct {
	Backend         string          `yaml:"backend"`
	Workspace       string          `yaml:"workspace"`
	Settings        *Opaque         `yaml:"settings,omitempty"`
	Defaults        Defaults        `yaml:"defaults,omitempty"`
	AllowedCommands []string        `yaml:"allowed_commands,omitempty"`
	Approval        *ApprovalConfig `yaml:"approval,omitempty"`
}

// Opaque preserves backend-owned YAML until its factory decodes it.
type Opaque yamlv3.Node

// UnmarshalYAML captures an opaque YAML subtree.
func (o *Opaque) UnmarshalYAML(node *yamlv3.Node) error {
	*o = Opaque(*node)
	return nil
}

// Node returns the captured settings subtree.
func (o *Opaque) Node() *yamlv3.Node {
	if o == nil {
		return nil
	}
	return (*yamlv3.Node)(o)
}

// Duration is a YAML duration string such as "30s" or "2m".
type Duration time.Duration

// UnmarshalYAML rejects unitless numbers and parses Go duration strings.
func (d *Duration) UnmarshalYAML(node *yamlv3.Node) error {
	if node.Kind != yamlv3.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a string like \"30s\"")
	}
	value, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(value)
	return nil
}

// Defaults is the daemon-owned subset of sandbox.ExecOptions.
// WorkDir and Stdin deliberately are not configurable here.
type Defaults struct {
	Timeout   Duration         `yaml:"timeout,omitempty"`
	Env       EnvDefaults      `yaml:"env,omitempty"`
	Net       NetDefaults      `yaml:"net,omitempty"`
	Resources ResourceDefaults `yaml:"resources,omitempty"`
}

// EnvDefaults controls inherited and injected environment values.
type EnvDefaults struct {
	Allow  []string          `yaml:"allow,omitempty"`
	Inject map[string]string `yaml:"inject,omitempty"`
}

// NetDefaults configures outbound network policy.
type NetDefaults struct {
	Mode       string   `yaml:"mode,omitempty"`
	AllowHosts []string `yaml:"allow_hosts,omitempty"`
	Proxy      string   `yaml:"proxy,omitempty"`
}

// ResourceDefaults configures process resource limits.
type ResourceDefaults struct {
	CPUMillicores  int   `yaml:"cpu_millicores,omitempty"`
	MemoryBytes    int64 `yaml:"memory_bytes,omitempty"`
	DiskBytes      int64 `yaml:"disk_bytes,omitempty"`
	MaxOutputBytes int64 `yaml:"max_output_bytes,omitempty"`
}

// ApprovalConfig selects policy-boundary predicates.
type ApprovalConfig struct {
	OutsideWorkDir    bool     `yaml:"outside_workdir,omitempty"`
	NonDefaultNetwork bool     `yaml:"non_default_network,omitempty"`
	SensitiveCommands []string `yaml:"sensitive_commands,omitempty"`
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
		if len(n.AllowHosts) != 0 || n.Proxy != "" {
			return fmt.Errorf(
				"defaults.net mode %q cannot set allow_hosts or proxy",
				defaultNetMode(n.Mode))
		}
	case NetModeAllowList:
		if len(n.AllowHosts) == 0 {
			return fmt.Errorf("defaults.net allow_list requires allow_hosts")
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
	return nil
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
		(a.OutsideWorkDir || a.NonDefaultNetwork || len(a.SensitiveCommands) > 0)
}

// Parse strictly decodes and validates exactly one YAML document.
func Parse(data []byte) (Document, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, errdefs.Validationf(
			"decode sandbox config YAML: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return Document{}, errdefs.Validationf(
			"decode sandbox config YAML: %v", err)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// DecodeSettings strictly decodes an opaque settings node into T.
func DecodeSettings[T any](node *yamlv3.Node) (T, error) {
	var out T
	if node == nil {
		return out, nil
	}
	raw, err := yamlv3.Marshal(node)
	if err != nil {
		return out, fmt.Errorf("re-encode settings node: %w", err)
	}
	decoder := yamlv3.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}
