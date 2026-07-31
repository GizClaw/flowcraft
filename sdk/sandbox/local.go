package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Option configures a LocalRunner at construction time.
type Option func(*LocalRunner)

// WithMaxOutputBytes sets the default per-call MaxOutputBytes used when
// ExecOptions.Resources.MaxOutputBytes is zero. Pass a non-positive value
// to disable truncation (i.e. allow up to math.MaxInt64 bytes).
func WithMaxOutputBytes(n int64) Option {
	return func(r *LocalRunner) {
		if n <= 0 {
			r.defaultMaxOutput = math.MaxInt64
		} else {
			r.defaultMaxOutput = n
		}
	}
}

// LocalRunner executes commands directly on the host using os/exec. It is
// the no-isolation backend; production deployments that need real
// boundaries should swap it for a sandboxed Runner with kernel-level
// enforcement (namespace / container / microVM).
//
// Policy support matrix:
//
//   - ExecOptions.WorkDir / Stdin / Timeout: fully supported. Every
//     child runs in its own process group; timeout/cancel kills the
//     whole group, not just the leader.
//   - ExecOptions.Env: fully supported (see EnvPolicy doc).
//   - ExecOptions.Net.Mode != NetDefault: returns errdefs.NotAvailable.
//   - ExecOptions.Resources.MemoryBytes: enforced by a sampling
//     watcher on aggregate group RSS; overflow kills the whole group.
//   - ExecOptions.Resources.CPUMillicores: enforced by the same
//     watcher as group cpu-time = Timeout x millicores/1000; requires
//     Timeout > 0, otherwise errdefs.NotAvailable.
//   - ExecOptions.Resources.DiskBytes != 0: returns
//     errdefs.NotAvailable (no quota mechanism).
//   - ExecOptions.Resources.MaxOutputBytes: enforced; per-call value
//     overrides the runner's WithMaxOutputBytes default.
type LocalRunner struct {
	rootDir          string
	defaultMaxOutput int64
}

// NewLocalRunner constructs a LocalRunner rooted at rootDir. The root is
// resolved via filepath.Abs + EvalSymlinks so a later symlink swap on the
// root itself cannot be used to escape.
func NewLocalRunner(rootDir string, opts ...Option) *LocalRunner {
	real, err := filepath.Abs(rootDir)
	if err == nil {
		if resolved, evalErr := filepath.EvalSymlinks(real); evalErr == nil {
			real = resolved
		}
	}
	r := &LocalRunner{rootDir: real, defaultMaxOutput: defaultMaxOutputBytes}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Enforcement reports LocalRunner's honest surface: the env allow-list
// is honoured, memory/cpu caps are enforced by the group watcher where
// the platform supports it, and everything that is call-time
// validation only (WorkDir bounding, NetDefault pass-through) is
// deliberately not claimed.
func (r *LocalRunner) Enforcement() Enforcement {
	return Enforcement{
		EnvAllowList: true,
		MemoryCap:    groupCapsAvailable(),
		CPUCap:       groupCapsAvailable(),
	}
}

// Exec runs cmd with args under opts. See LocalRunner doc for which
// policy fields are honoured vs. rejected with errdefs.NotAvailable.
func (r *LocalRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	if opts.Net.Mode != NetDefault {
		return nil, errdefs.NotAvailablef(
			"sandbox: net policy not supported by local runner; requires a kernel-level isolation backend")
	}
	if opts.Resources.DiskBytes != 0 {
		return nil, errdefs.NotAvailablef(
			"sandbox: disk limits not supported by local runner (no quota mechanism)")
	}
	if opts.Resources.CPUMillicores != 0 && opts.Timeout <= 0 {
		return nil, errdefs.NotAvailablef(
			"sandbox: CPUMillicores requires a per-call Timeout to derive a cpu-time cap")
	}
	maxRSSKB, maxCPU := deriveGroupCaps(opts.Resources, opts.Timeout)
	if (maxRSSKB > 0 || maxCPU > 0) && !groupCapsAvailable() {
		return nil, errdefs.NotAvailablef(
			"sandbox: resource limits require process-group sampling, which is unavailable here " +
				"(non-unix platform, or ps(1) cannot be executed)")
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(ctx, cmd, args...)
	applyProcAttrs(c)
	c.Dir = r.rootDir
	if opts.WorkDir != "" {
		resolved, err := r.resolveWorkDir(opts.WorkDir)
		if err != nil {
			return nil, err
		}
		c.Dir = resolved
	}

	c.Env = buildEnv(opts.Env)

	if opts.Stdin != nil {
		c.Stdin = bytes.NewReader(opts.Stdin)
	}

	maxOut := opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = r.defaultMaxOutput
	}
	var stdout, stderr limitedBuffer
	stdout.max = maxOut
	stderr.max = maxOut
	c.Stdout = &stdout
	c.Stderr = &stderr

	if err := c.Start(); err != nil {
		return nil, classifyStartError(cmd, err)
	}
	// The child leads its own process group (applyProcAttrs), so its pid
	// is the pgid. The watcher enforces MemoryBytes / cpu caps by group
	// aggregate; Stop is nil-safe and must follow Wait.
	watcher := StartGroupCapsWatcher(c.Process.Pid, opts.Resources, opts.Timeout)

	err := c.Wait()
	watcher.Stop()
	result := &ExecResult{
		Stdout: stdout.buf.String(),
		Stderr: stderr.buf.String(),
	}
	if err != nil {
		// Order matters: a watcher that gave up on sampling killed the
		// group without proving any budget was exceeded, so reporting
		// BudgetExceeded there would be a false accusation.
		if sampleErr := watcher.Unenforceable(); sampleErr != nil {
			return result, errdefs.NotAvailablef(
				"sandbox: resource caps became unenforceable while executing %s: %v", cmd, sampleErr)
		}
		if cap := watcher.Exceeded(); cap != "" {
			return result, errdefs.BudgetExceededf(
				"sandbox: %s resource cap exceeded while executing %s", cap, cmd)
		}
		if ctx.Err() != nil {
			return result, errdefs.FromContext(fmt.Errorf("sandbox: exec %s: %w", cmd, ctx.Err()))
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, classifyStartError(cmd, err)
	}
	return result, nil
}

// deriveGroupCaps converts policy resource limits into group-watcher
// units. Memory: bytes -> KiB of aggregate group RSS. CPU: millicores
// scaled by the per-call timeout -> a cpu-time budget for the whole
// group (a cap on cumulative cpu time, not rate), so CPUMillicores is
// only actionable together with Timeout — the caller-visible guard for
// that lives in Exec.
func deriveGroupCaps(res ResourceLimits, timeout time.Duration) (maxRSSKB int64, maxCPU time.Duration) {
	if res.MemoryBytes > 0 {
		maxRSSKB = 1 + (res.MemoryBytes-1)/1024
	}
	if res.CPUMillicores > 0 && timeout > 0 {
		scaled := float64(timeout) * float64(res.CPUMillicores) / 1000
		if scaled >= float64(math.MaxInt64) {
			maxCPU = time.Duration(math.MaxInt64)
		} else {
			maxCPU = time.Duration(scaled)
		}
	}
	return maxRSSKB, maxCPU
}

// classifyStartError maps process-start failures onto errdefs
// categories: a missing binary is NotFound, a permission refusal is
// Forbidden, anything else is Internal. Callers (e.g. script bridges)
// rely on these categories instead of string-matching os/exec errors.
func classifyStartError(cmd string, err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errdefs.NotFound(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	case errors.Is(err, os.ErrPermission):
		return errdefs.Forbidden(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	default:
		return errdefs.Internal(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	}
}

// buildEnv translates an EnvPolicy into a flat []string suitable for
// exec.Cmd.Env. The empty result is returned as nil so os/exec falls
// back to its "no env at all" code path (which is what we want when the
// caller asked for an empty allow-list with no Inject).
func buildEnv(p EnvPolicy) []string {
	var env []string

	if p.Allow == nil {
		env = append(env, os.Environ()...)
	} else if len(p.Allow) > 0 {
		allow := make(map[string]bool, len(p.Allow))
		for _, name := range p.Allow {
			allow[name] = true
		}
		for _, kv := range os.Environ() {
			idx := strings.IndexByte(kv, '=')
			if idx <= 0 {
				continue
			}
			if allow[kv[:idx]] {
				env = append(env, kv)
			}
		}
	}

	if len(p.Inject) > 0 {
		injected := make(map[string]bool, len(p.Inject))
		for k := range p.Inject {
			injected[k] = true
		}
		filtered := env[:0]
		for _, kv := range env {
			idx := strings.IndexByte(kv, '=')
			if idx <= 0 {
				filtered = append(filtered, kv)
				continue
			}
			if !injected[kv[:idx]] {
				filtered = append(filtered, kv)
			}
		}
		env = filtered
		for k, v := range p.Inject {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if p.Allow != nil && len(p.Allow) == 0 && len(p.Inject) == 0 {
		// Distinguish "inherit nothing" from "inherit everything": return
		// an empty (non-nil) slice so exec.Cmd.Env is set to no entries
		// instead of falling back to os.Environ().
		return []string{}
	}
	return env
}

func (r *LocalRunner) resolveWorkDir(dir string) (string, error) {
	abs := dir
	if !filepath.IsAbs(dir) {
		abs = filepath.Join(r.rootDir, dir)
	}
	abs = filepath.Clean(abs)

	real, err := evalExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve workdir: %w", err)
	}
	if real != r.rootDir && !strings.HasPrefix(real, r.rootDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: workdir %q escapes root", ErrPathTraversal, dir)
	}
	return abs, nil
}

// evalExistingPrefix resolves symlinks for the longest existing ancestor
// of path, then appends the remaining non-existent tail. This catches
// symlink escapes even when the final target does not exist yet.
func evalExistingPrefix(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	realParent, err := evalExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}

// limitedBuffer is a bytes.Buffer that silently discards writes past max
// bytes. We pretend the write succeeded (return len(p), nil) so the
// child process is not killed by a "short write" — truncation is meant
// to be invisible to the program under test.
type limitedBuffer struct {
	buf bytes.Buffer
	max int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 || int64(b.buf.Len()) >= b.max {
		return len(p), nil
	}
	space := b.max - int64(b.buf.Len())
	if int64(len(p)) <= space {
		return b.buf.Write(p)
	}
	if _, err := b.buf.Write(p[:space]); err != nil {
		return 0, err
	}
	return len(p), nil
}
