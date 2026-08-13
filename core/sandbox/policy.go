package sandbox

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// EnvPolicy controls which host environment variables a child process
// can observe, and lets the caller inject extra variables on top.
//
//   - Allow == nil: inherit the full host environment (back-compat with
//     the pre-sandbox behaviour of LocalCommandRunner).
//   - Allow == []string{} (non-nil empty slice): inherit nothing; the
//     child only sees the names listed in Inject.
//   - Allow == []string{"PATH", "HOME", ...}: only those names are
//     forwarded from the host; everything else is dropped.
//
// Inject is applied on top of the allow-list. Names in Inject win over
// host values of the same name.
type EnvPolicy struct {
	Allow  []string
	Inject map[string]string
}

// NetMode names the network access posture the sandbox should enforce.
type NetMode int

const (
	// NetDefault leaves networking to the host. LocalRunner accepts this
	// mode; sandboxed backends interpret it as "no policy applied".
	NetDefault NetMode = iota
	// NetDenyAll forbids any outbound connection. Requires a sandboxing
	// backend (bubblewrap / container / microvm) to enforce.
	NetDenyAll
	// NetAllowList permits only destinations listed in AllowHosts.
	// Requires a sandboxing backend.
	NetAllowList
	// NetProxy routes all traffic through Proxy. Requires a sandboxing
	// backend.
	NetProxy
)

// NetPolicy controls outbound networking for the child process.
// LocalRunner only honours NetDefault; any other mode is rejected with
// errdefs.NotAvailable until a sandboxed backend with kernel-level
// enforcement is wired up.
type NetPolicy struct {
	Mode        NetMode
	AllowHosts  []string    // deprecated: compiled as trailing allow rules
	Rules       []NetRule   // explicit rules; deny wins over allow
	Proxy       string      // http://host:port or socks5://[user:pass@]host:port
	UnixSockets []string    // allowed host unix socket paths (backend-gated)
	MITM        *MITMPolicy // non-nil + Enabled enables HTTPS content hooks
}

// NetAction is the verdict of one network rule. Rules express an
// already-decided policy; they never trigger approval.
type NetAction int

const (
	// NetDeny blocks the destination.
	NetDeny NetAction = iota
	// NetAllow permits the destination.
	NetAllow
)

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

// NetRule is one host/port-level allow or deny rule.
//
// Host forms:
//
//   - "example.com": the bare domain AND every subdomain (legacy
//     AllowHosts semantics; this is domain-and-descendants, not
//     exact-only).
//   - "*.example.com": subdomains only (any depth), never the bare
//     domain.
//   - "1.2.3.4": exact IP literal.
//   - "10.0.0.0/8": CIDR prefix.
//
// Unicode hostnames are normalized to punycode when the policy is
// compiled (see core/utils). Port 0 matches any port;
// otherwise the request port (URL explicit port or protocol default,
// CONNECT target port) must match exactly.
type NetRule struct {
	Action NetAction
	Host   string
	Port   int
}

// MITMPolicy enables TLS termination and content hooks for CONNECT
// traffic. It is opt-in: a nil policy or Enabled=false leaves CONNECT
// tunnels untouched.
//
// Host selection: empty Hosts means "all CONNECT traffic is MITM'd"
// (the default; pinned clients then fail closed at TLS). Non-empty
// Hosts restricts MITM to the listed hosts, and ExcludeHosts always
// bypasses MITM with a raw tunnel (allow/deny rules still apply).
// Exclude wins over Hosts. Host forms follow NetRule: "example.com",
// "*.example.com", IP literals, and CIDR prefixes.
type MITMPolicy struct {
	Enabled       bool
	InspectBodies bool
	MaxBodyBytes  int64
	Hosts         []string // non-empty: only these hosts get MITM
	ExcludeHosts  []string // never MITM these hosts (raw tunnel)
}

// Validate checks NetPolicy's cross-backend invariants. Backend
// capability gating (which modes / features a runner enforces) is
// separate and happens via Enforcement.
func (p NetPolicy) Validate() error {
	if p.Mode < NetDefault || p.Mode > NetProxy {
		return errdefs.Validationf("sandbox: unknown net mode %d", int(p.Mode))
	}
	if p.Proxy != "" {
		u, err := url.Parse(p.Proxy)
		if err != nil {
			return errdefs.Validationf("sandbox: invalid proxy URL %q: %v", p.Proxy, err)
		}
		if u.Scheme != "http" && u.Scheme != "socks5" {
			return errdefs.Validationf(
				"sandbox: proxy scheme %q must be http or socks5", u.Scheme)
		}
		if u.Host == "" {
			return errdefs.Validationf("sandbox: proxy URL %q has no host", p.Proxy)
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword && u.User.Username() == "" {
				return errdefs.Validationf(
					"sandbox: proxy URL %q has a password but no username", p.Proxy)
			}
		}
	}
	for i, rule := range p.Rules {
		if rule.Action != NetDeny && rule.Action != NetAllow {
			return errdefs.Validationf("sandbox: net rule %d has invalid action %d", i, int(rule.Action))
		}
		if strings.TrimSpace(rule.Host) == "" {
			return errdefs.Validationf("sandbox: net rule %d has an empty host", i)
		}
		if rule.Port < 0 || rule.Port > 65535 {
			return errdefs.Validationf("sandbox: net rule %d port %d out of range", i, rule.Port)
		}
	}
	for _, path := range p.UnixSockets {
		if !filepath.IsAbs(path) {
			return errdefs.Validationf("sandbox: unix socket path %q must be absolute", path)
		}
	}
	if p.MITM != nil {
		if p.MITM.MaxBodyBytes < 0 {
			return errdefs.Validationf("sandbox: mitm.max_body_bytes must be non-negative")
		}
		for i, host := range p.MITM.Hosts {
			if strings.TrimSpace(host) == "" {
				return errdefs.Validationf("sandbox: mitm.hosts[%d] is empty", i)
			}
		}
		for i, host := range p.MITM.ExcludeHosts {
			if strings.TrimSpace(host) == "" {
				return errdefs.Validationf("sandbox: mitm.exclude_hosts[%d] is empty", i)
			}
		}
	}
	return nil
}

// ResourceLimits caps how much the child process may consume.
//
// MemoryBytes caps aggregate resident memory used by the child process
// group. LocalRunner enforces it with its unix group watcher;
// sandboxed backends may use cgroups or VM caps instead.
//
// CPUMillicores expresses a cpu-time budget in thousandths of a core:
// backends derive a hard cap from it (LocalRunner: aggregate group
// cpu-time = Timeout x millicores/1000 via its sampling watcher).
// Because the budget is derived from the wall-clock timeout,
// LocalRunner requires Timeout > 0 when CPUMillicores is set and
// returns errdefs.NotAvailable otherwise.
//
// DiskBytes needs a quota mechanism no local backend has today; any
// non-zero value is rejected with errdefs.NotAvailable everywhere.
//
// MaxOutputBytes caps the bytes captured into ExecResult.Stdout and
// ExecResult.Stderr independently; excess output is dropped silently
// (the child process is not killed). LocalRunner enforces this directly.
// When zero, the runner's default applies (see LocalRunner's
// WithMaxOutputBytes option).
type ResourceLimits struct {
	CPUMillicores  int
	MemoryBytes    int64
	DiskBytes      int64
	MaxOutputBytes int64
}

// ValidateExecPolicy runs the policy checks every built-in backend
// applies before spawning anything, whether through Exec or
// Runner.Start:
//
//   - DiskBytes is rejected everywhere (no backend has a quota
//     mechanism yet).
//   - CPUMillicores derives its budget from Timeout, so it is rejected
//     when Timeout is absent.
//   - MemoryBytes / CPUMillicores ride the shared process-group
//     sampler; where that sampler cannot run, honouring the request
//     would silently run without caps, so it is rejected instead.
//
// Backend-specific posture checks (which Net modes a runner enforces,
// WorkDir confinement) stay in each backend.
func ValidateExecPolicy(opts ExecOptions) error {
	if opts.Resources.DiskBytes != 0 {
		return errdefs.NotAvailablef(
			"sandbox: disk limits not supported (no quota mechanism)")
	}
	if opts.Resources.CPUMillicores != 0 && opts.Timeout <= 0 {
		return errdefs.NotAvailablef(
			"sandbox: CPUMillicores requires a per-call Timeout to derive a cpu-time cap")
	}
	if (opts.Resources.MemoryBytes > 0 || opts.Resources.CPUMillicores > 0) && !groupCapsAvailable() {
		return errdefs.NotAvailablef(
			"sandbox: resource limits require process-group sampling, which is unavailable here")
	}
	return nil
}

// ErrPathTraversal is returned when a WorkDir resolves outside the
// runner's root, including via symlinks. sandbox owns its own
// ErrPathTraversal so this package does not depend on sdk/workspace
// (which would create an import cycle through the deprecation aliases).
// sdk/workspace keeps a separate ErrPathTraversal for its filesystem
// API.
var ErrPathTraversal = errdefs.Forbidden(errors.New("sandbox: path traversal denied"))
