//go:build windows

package windows

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// defaultMaxOutputBytes is the per-stream cap used by the one-shot
// Exec when ExecOptions.Resources.MaxOutputBytes is zero.
const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Runner executes commands directly on the host via os/exec, with the
// child confined to a Windows Job Object that owns its whole process
// tree. It is the no-filesystem-isolation backend for Windows:
// lifecycle and resource caps are enforced by the kernel (job
// objects); filesystem and network confinement are phase 2.
//
// Policy support matrix:
//
//   - WorkDir / Stdin / Timeout: fully supported. Every child runs in
//     its own job; timeout/cancel closes the job and kills the whole
//     tree, not just the leader.
//   - Env: fully supported (see sandbox.EnvPolicy doc).
//   - Net.Mode != corenet.NetDefault: errdefs.NotAvailable.
//   - Write == WriteReadOnly: errdefs.NotAvailable (no OS boundary to
//     confine writes yet).
//   - Resources.MemoryBytes: enforced as a job-wide memory limit
//     (JOB_OBJECT_LIMIT_JOB_MEMORY).
//   - Resources.CPUMillicores: enforced as a per-process user-time
//     limit = Timeout x millicores/1000; requires Timeout > 0,
//     otherwise errdefs.NotAvailable.
//   - Resources.DiskBytes != 0: errdefs.NotAvailable (no quota
//     mechanism).
//   - Resources.MaxOutputBytes: enforced; per-call value overrides the
//     runner's WithMaxOutputBytes default.
type Runner struct {
	rootDir      string
	cfg          runnerConfig
	sessions     *sandbox.SessionRegistry
	registryOnce sync.Once
}

// New constructs a Runner rooted at rootDir. The root is resolved via
// filepath.Abs + EvalSymlinks so a later symlink swap on the root
// itself cannot be used to escape.
func New(rootDir string, opts ...Option) (*Runner, error) {
	real, err := filepath.Abs(rootDir)
	if err == nil {
		if resolved, evalErr := filepath.EvalSymlinks(real); evalErr == nil {
			real = resolved
		}
	}
	r := &Runner{
		rootDir: real,
		cfg:     runnerConfig{defaultMaxOutput: defaultMaxOutputBytes},
	}
	for _, o := range opts {
		o(&r.cfg)
	}
	return r, nil
}

// Capabilities declares the honest surface of the phase-1 backend:
// env allow-lists and job-object resource caps are enforced; there is
// no filesystem or network confinement yet, and sessions are
// pipe-only (no TTY / signal / event features).
func (r *Runner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		Policy: sandbox.Enforcement{
			EnvAllowList: true,
			MemoryCap:    true,
			CPUCap:       true,
		},
		Features: sandbox.SessionFeatures{},
	}
}

// Exec runs cmd with args under opts. See Runner doc for which policy
// fields are honoured vs. rejected with errdefs.NotAvailable.
func (r *Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return sandbox.Exec(ctx, r, cmd, args, opts)
}

// Start implements Runner: policy validation mirrors Exec, then the
// command is spawned on pipes inside a fresh job object.
func (r *Runner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	return r.registry().Start(ctx, spec)
}

// List implements Runner.
func (r *Runner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	return r.registry().List(ctx)
}

// Terminate implements Runner.
func (r *Runner) Terminate(ctx context.Context, id string) error {
	return r.registry().Terminate(ctx, id)
}

// Close implements core/sandbox.Runner: it terminates every session
// started through this runner. Safe to call more than once and when
// the runner never started anything.
func (r *Runner) Close() error {
	return r.registry().Close()
}

// registry returns the session registry, initialising it lazily so a
// zero-value Runner still answers with NotAvailable instead of
// panicking on a nil interface.
func (r *Runner) registry() *sandbox.SessionRegistry {
	r.registryOnce.Do(func() {
		if r.sessions == nil {
			r.sessions = sandbox.NewSessionRegistry(r.spawnProcess)
		}
	})
	return r.sessions
}

// spawnProcess is Runner's sandbox.SessionStarter: it applies the
// same policy surface as Exec and hands the configured command to the
// session implementation, which owns job creation and assignment.
func (r *Runner) spawnProcess(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if err := r.validatePolicy(spec); err != nil {
		return nil, err
	}
	if spec.TTY {
		return nil, errdefs.NotAvailablef(
			"windows: TTY sessions are not supported (pipe sessions only)")
	}
	workDir, err := r.resolveWorkDir(spec.Opts.WorkDir)
	if err != nil {
		return nil, err
	}
	maxOut := spec.Opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = r.cfg.defaultMaxOutput
	}
	spec.Opts.Resources.MaxOutputBytes = maxOut

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = buildEnv(spec.Opts.Env)
	if err := ctx.Err(); err != nil {
		return nil, errdefs.FromContext(err)
	}
	return startSession(ctx, spec, cmd)
}

// validatePolicy mirrors sandbox.ValidateExecPolicy minus the
// process-group sampling gate: the windows backend enforces Memory /
// CPU caps through job objects, so the shared unix ps(1) sampler
// availability check does not apply. The job-object caps themselves
// are enforced by the kernel, not sampled.
func (r *Runner) validatePolicy(spec sandbox.SessionSpec) error {
	if spec.Opts.Write > sandbox.WriteReadOnly {
		return errdefs.Validationf(
			"windows: unknown write policy %d", int(spec.Opts.Write))
	}
	if spec.Opts.Write == sandbox.WriteReadOnly {
		return errdefs.NotAvailablef(
			"windows: write policy not supported (no OS-level write confinement yet)")
	}
	if spec.Opts.Net.Mode != corenet.NetDefault {
		return errdefs.NotAvailablef(
			"windows: net policy not supported; only NetDefault is available")
	}
	if spec.Opts.Resources.DiskBytes != 0 {
		return errdefs.NotAvailablef(
			"windows: disk limits not supported (no quota mechanism)")
	}
	if spec.Opts.Resources.CPUMillicores != 0 && spec.Opts.Timeout <= 0 {
		return errdefs.NotAvailablef(
			"windows: CPUMillicores requires a per-call Timeout to derive a cpu-time cap")
	}
	return nil
}

// buildEnv translates a sandbox.EnvPolicy into a flat []string
// suitable for exec.Cmd.Env. The empty result is returned as nil so
// os/exec falls back to its "no env at all" code path (which is what
// we want when the caller asked for an empty allow-list with no
// Inject).
func buildEnv(p sandbox.EnvPolicy) []string {
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
		// Distinguish "inherit nothing" from "inherit everything":
		// return an empty (non-nil) slice so exec.Cmd.Env is set to no
		// entries instead of falling back to os.Environ().
		return []string{}
	}
	return env
}

func (r *Runner) resolveWorkDir(dir string) (string, error) {
	abs := dir
	if !filepath.IsAbs(dir) {
		abs = filepath.Join(r.rootDir, dir)
	}
	abs = filepath.Clean(abs)

	real, err := sandbox.EvalExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("windows: resolve workdir: %w", err)
	}
	if real != r.rootDir && !strings.HasPrefix(real, r.rootDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: workdir %q escapes root", sandbox.ErrPathTraversal, dir)
	}
	return abs, nil
}
