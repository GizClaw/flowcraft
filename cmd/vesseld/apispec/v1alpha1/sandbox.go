package v1alpha1

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Sandbox is the wire form of a sandbox.Runner template. One
// Sandbox document materialises one daemon-wide [sandbox.Runner]
// that Agents reference by name via Agent.spec.sandbox; the
// resolver layers a [sandbox.WithDefaults] decorator over the
// chosen backend so the policy fields below act as the floor that
// per-call ExecOptions cannot widen.
//
// # Mental model
//
// Sandbox stands to Agents the way kubernetes NetworkPolicy stands
// to Pods: it is a standalone resource that any number of agents
// may reference. Keeping it independent of Agent means a single
// operator-managed "production-sandbox" can serve N agents
// without each agent re-declaring the same env / net / resource
// floor.
type Sandbox struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       SandboxSpec `json:"spec" yaml:"spec"`
}

// SandboxSpec configures the runner. v0.2.0 ships two backends:
//
//   - "local"  — [sandbox.LocalRunner]. No isolation; the policy
//     fields are advisory. Suitable for dev / trusted code.
//   - "nsjail" — sdkx-side nsjail.Runner (Linux only). Honours
//     EnvPolicy and NetPolicy.Mode = deny-all; richer modes
//     surface NotAvailable at exec time.
//
// Future backends (container, microVM) will slot in here additively.
type SandboxSpec struct {
	// Backend selects the runner implementation. Required.
	// "local" | "nsjail".
	Backend string `json:"backend" yaml:"backend"`

	// RootDir is the physical confinement root the runner uses
	// when resolving per-call WorkDir. Required: both
	// sandbox.LocalRunner and the nsjail runner take rootDir at
	// construction time and refuse Exec paths that escape it.
	// Relative paths in tool-level WorkDir resolve against this
	// root; absolute paths and `..` escapes are rejected.
	RootDir string `json:"rootDir" yaml:"rootDir"`

	// Env is the default EnvPolicy layered in. The fields map
	// 1:1 to sandbox.EnvPolicy.
	Env *SandboxEnv `json:"env,omitempty" yaml:"env,omitempty"`

	// Net is the default NetPolicy layered in.
	Net *SandboxNet `json:"net,omitempty" yaml:"net,omitempty"`

	// Resources is the default ResourceLimits layered in.
	Resources *SandboxResources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// Nsjail collects backend-specific knobs that only apply when
	// Backend == "nsjail". Setting any field here while Backend
	// != "nsjail" is a validation error so a misconfigured switch
	// of backend never silently drops settings.
	Nsjail *SandboxNsjail `json:"nsjail,omitempty" yaml:"nsjail,omitempty"`
}

// SandboxEnv mirrors sandbox.EnvPolicy. A nil Allow slice means
// "no allow-list configured" (default — inherit nothing from
// the host); an empty non-nil slice means "explicitly allow no
// host env vars" which is the same outcome but documented in
// YAML as the operator's intent.
type SandboxEnv struct {
	Allow  []string          `json:"allow,omitempty" yaml:"allow,omitempty"`
	Inject map[string]string `json:"inject,omitempty" yaml:"inject,omitempty"`
}

// SandboxNet mirrors sandbox.NetPolicy.
//
// Mode values:
//
//	"default"    — inherit host networking.
//	"deny-all"   — block egress entirely.
//	"allow-list" — allow only entries in Allow (backend-dependent).
//	"proxy"      — route through Proxy.
//
// "" defaults to "default" at resolve time.
type SandboxNet struct {
	Mode  string   `json:"mode,omitempty" yaml:"mode,omitempty"`
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Proxy string   `json:"proxy,omitempty" yaml:"proxy,omitempty"`
}

// SandboxResources mirrors sandbox.ResourceLimits. All fields are
// advisory on the "local" backend; "nsjail" enforces CPUMillicores
// and MemoryBytes via cgroups, with the rest surfaced as
// NotAvailable at exec time.
type SandboxResources struct {
	CPUMillicores  int   `json:"cpuMillicores,omitempty" yaml:"cpuMillicores,omitempty"`
	MemoryBytes    int64 `json:"memoryBytes,omitempty" yaml:"memoryBytes,omitempty"`
	DiskBytes      int64 `json:"diskBytes,omitempty" yaml:"diskBytes,omitempty"`
	MaxOutputBytes int64 `json:"maxOutputBytes,omitempty" yaml:"maxOutputBytes,omitempty"`
}

// SandboxNsjail collects optional knobs that only the nsjail
// backend reads. Keeping them in a typed sub-struct (rather than a
// free-form map[string]any) means the validator can reject typos
// at boot.
type SandboxNsjail struct {
	// Binary overrides the nsjail executable path. Empty falls
	// back to $PATH lookup at runner-construction time.
	Binary string `json:"binary,omitempty" yaml:"binary,omitempty"`

	// ExtraFlags are appended verbatim to every nsjail invocation.
	// Useful for ops-specific quirks (e.g. --rlimit_as on hosts
	// where the cgroup mem cap is unreliable) without bloating
	// the generic Spec.
	ExtraFlags []string `json:"extraFlags,omitempty" yaml:"extraFlags,omitempty"`
}

// GetTypeMeta / GetObjectMeta satisfy [Object].
func (s Sandbox) GetTypeMeta() TypeMeta     { return s.TypeMeta }
func (s Sandbox) GetObjectMeta() ObjectMeta { return s.ObjectMeta }

// Validate is shape-only — IO (e.g. confirming Nsjail.Binary
// exists) lives in the resolver because the apispec layer must
// stay side-effect free.
func (s Sandbox) Validate() error {
	if err := s.ObjectMeta.Validate(KindSandbox); err != nil {
		return err
	}
	if s.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld Sandbox %q: apiVersion %q != %q", s.Name, s.TypeMeta.APIVersion, APIVersion)
	}
	if s.TypeMeta.Kind != KindSandbox {
		return errdefs.Validationf("vesseld Sandbox %q: kind %q != %q", s.Name, s.TypeMeta.Kind, KindSandbox)
	}
	switch s.Spec.Backend {
	case "":
		return errdefs.Validationf("vesseld Sandbox %q: spec.backend is required (local|nsjail)", s.Name)
	case "local", "nsjail":
	default:
		return errdefs.Validationf("vesseld Sandbox %q: spec.backend %q invalid (want local|nsjail)", s.Name, s.Spec.Backend)
	}
	if s.Spec.RootDir == "" {
		return errdefs.Validationf("vesseld Sandbox %q: spec.rootDir is required (the runner's filesystem confinement root)", s.Name)
	}
	if s.Spec.Nsjail != nil && s.Spec.Backend != "nsjail" {
		return errdefs.Validationf("vesseld Sandbox %q: spec.nsjail is only valid when spec.backend=nsjail", s.Name)
	}
	if n := s.Spec.Net; n != nil {
		switch n.Mode {
		case "", "default", "deny-all", "allow-list", "proxy":
		default:
			return errdefs.Validationf("vesseld Sandbox %q: spec.net.mode %q invalid (want default|deny-all|allow-list|proxy)", s.Name, n.Mode)
		}
		// We do NOT enforce "allow-list requires Allow non-empty"
		// here: an empty allow-list with mode=allow-list is a
		// legitimate "block all" posture and the runner backend
		// should be the one to reject Mode/Allow shapes it
		// cannot honour (different backends support different
		// subsets — that authority belongs at exec time).
	}
	if r := s.Spec.Resources; r != nil {
		if r.CPUMillicores < 0 {
			return errdefs.Validationf("vesseld Sandbox %q: spec.resources.cpuMillicores must be >= 0", s.Name)
		}
		if r.MemoryBytes < 0 || r.DiskBytes < 0 || r.MaxOutputBytes < 0 {
			return errdefs.Validationf("vesseld Sandbox %q: spec.resources byte caps must be >= 0", s.Name)
		}
	}
	return nil
}
