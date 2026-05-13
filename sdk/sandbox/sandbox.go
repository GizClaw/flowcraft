package sandbox

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Runner executes a command under the sandbox's policy. Implementations
// MUST honour ExecOptions.Timeout, surface non-zero exits as ExitCode on
// ExecResult (returning err == nil for that case), and reject any policy
// they cannot enforce with an errdefs.NotAvailable error rather than
// silently downgrading the request.
type Runner interface {
	Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error)
}

// ExecOptions configures one Runner.Exec call.
//
// Field semantics:
//
//   - WorkDir: directory the command runs in. Relative paths are resolved
//     against the runner's root (e.g. LocalRunner.rootDir); absolute paths
//     must stay inside the root or the call is rejected with
//     ErrPathTraversal. Empty means "use the runner's root".
//   - Stdin: bytes piped to the command's stdin. nil means no stdin.
//   - Timeout: per-call deadline. Zero means "no sandbox-imposed timeout"
//     (the caller's ctx still applies).
//   - Env: see EnvPolicy. Replaces the historical "inherit everything"
//     behaviour while staying back-compat when EnvPolicy.Allow is nil.
//   - Net: see NetPolicy. LocalRunner only accepts NetDefault.
//   - Resources: see ResourceLimits. LocalRunner only enforces
//     MaxOutputBytes.
type ExecOptions struct {
	WorkDir   string
	Stdin     []byte
	Timeout   time.Duration
	Env       EnvPolicy
	Net       NetPolicy
	Resources ResourceLimits
}

// ExecResult captures the outcome of a Runner.Exec call. ExitCode is the
// command's exit status (0 on success, non-zero on failure that the OS
// surfaced via *exec.ExitError or equivalent). Stdout / Stderr are the
// captured output, possibly truncated to Resources.MaxOutputBytes.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

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
	// backend (nsjail / container / microvm) to enforce.
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
	Mode       NetMode
	AllowHosts []string
	Proxy      string
}

// ResourceLimits caps how much the child process may consume.
//
// CPUMillicores / MemoryBytes / DiskBytes are *hard* limits that need
// kernel-level enforcement (cgroups, rlimits, VM caps). LocalRunner
// returns errdefs.NotAvailable when any of them is non-zero.
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

// ErrPathTraversal is returned when a WorkDir resolves outside the
// runner's root, including via symlinks. sandbox owns its own
// ErrPathTraversal so this package does not depend on sdk/workspace
// (which would create an import cycle through the deprecation aliases).
// sdk/workspace keeps a separate ErrPathTraversal for its filesystem
// API.
var ErrPathTraversal = errdefs.Forbidden(errors.New("sandbox: path traversal denied"))
