package sandbox

import (
	"errors"

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

// ResourceLimits caps how much the child process may consume.
//
// MemoryBytes caps aggregate resident memory used by the child process
// group. The local runner enforces it with its unix group watcher;
// sandboxed backends may use cgroups or VM caps instead.
//
// CPUMillicores expresses a cpu-time budget in thousandths of a core:
// backends derive a hard cap from it (sandbox-local: aggregate group
// cpu-time = Timeout x millicores/1000 via its sampling watcher).
// Because the budget is derived from the wall-clock timeout,
// The local runner requires Timeout > 0 when CPUMillicores is set and
// returns errdefs.NotAvailable otherwise.
//
// DiskBytes needs a quota mechanism no local backend has today; any
// non-zero value is rejected with errdefs.NotAvailable everywhere.
//
// MaxOutputBytes caps the bytes captured into ExecResult.Stdout and
// ExecResult.Stderr independently; excess output is dropped silently
// (the child process is not killed). The local runner enforces this directly.
// When zero, the runner's default applies (see local.WithMaxOutputBytes
// WithMaxOutputBytes option).
type ResourceLimits struct {
	CPUMillicores  int
	MemoryBytes    int64
	DiskBytes      int64
	MaxOutputBytes int64
}

// ErrPathTraversal is returned when a WorkDir resolves outside the
// runner's root, including via symlinks.
//
// It is deliberately independent of the filesystem layer's own refusals
// (fsx.ErrDenied): that one answers "the write boundary refused this", while
// this one answers "the working directory is not inside the sandbox". A caller
// that renders an error to a model should not have to guess which of the two it
// got.
var ErrPathTraversal = errdefs.Forbidden(errors.New("sandbox: path traversal denied"))
