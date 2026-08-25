// Package local provides the no-isolation Runner backed by os/exec.
// It lives in a subpackage so core/sandbox keeps only contracts, shared
// session machinery, and composition helpers, while local remains an
// explicitly importable backend alongside bwrap and seatbelt.
package local

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Option configures a Runner at construction time.
type Option func(*Runner)

// WithMaxOutputBytes sets the default per-call MaxOutputBytes used when
// sandbox.ExecOptions.Resources.MaxOutputBytes is zero. Pass a
// non-positive value to disable truncation (i.e. allow up to
// math.MaxInt64 bytes).
func WithMaxOutputBytes(n int64) Option {
	return func(r *Runner) {
		if n <= 0 {
			r.defaultMaxOutput = math.MaxInt64
		} else {
			r.defaultMaxOutput = n
		}
	}
}

// Runner executes commands directly on the host using os/exec. It is
// the no-isolation backend; production deployments that need real
// boundaries should swap it for a sandboxed Runner with kernel-level
// enforcement (namespace / container / microVM).
//
// Policy support matrix:
//
//   - WorkDir / Stdin / Timeout: fully supported. Every child runs in
//     its own process group; timeout/cancel kills the whole group, not
//     just the leader.
//   - Env: fully supported (see sandbox.EnvPolicy doc).
//   - Net.Mode != corenet.NetDefault: returns errdefs.NotAvailable.
//   - Write == WriteReadOnly: returns errdefs.NotAvailable (no OS
//     boundary to confine writes).
//   - Resources.MemoryBytes: enforced by a sampling watcher on aggregate
//     group RSS; overflow kills the whole group.
//   - Resources.CPUMillicores: enforced by the same watcher as group
//     cpu-time = Timeout x millicores/1000; requires Timeout > 0,
//     otherwise errdefs.NotAvailable.
//   - Resources.DiskBytes != 0: returns errdefs.NotAvailable (no quota
//     mechanism).
//   - Resources.MaxOutputBytes: enforced; per-call value overrides the
//     runner's WithMaxOutputBytes default.
type Runner struct {
	rootDir          string
	defaultMaxOutput int64
	sessions         *sandbox.SessionRegistry
	registryOnce     sync.Once
}

// New constructs a Runner rooted at rootDir. The root is resolved via
// filepath.Abs + EvalSymlinks so a later symlink swap on the root itself
// cannot be used to escape.
func New(rootDir string, opts ...Option) *Runner {
	real, err := filepath.Abs(rootDir)
	if err == nil {
		if resolved, evalErr := filepath.EvalSymlinks(real); evalErr == nil {
			real = resolved
		}
	}
	r := &Runner{rootDir: real, defaultMaxOutput: defaultMaxOutputBytes}
	for _, o := range opts {
		o(r)
	}
	r.sessions = sandbox.NewSessionRegistry(r.spawnProcess)
	return r
}

// Capabilities declares Runner's honest surface: the env
// allow-list is honoured, memory/cpu caps are enforced by the group
// watcher where the platform supports it, sessions are available on
// unix, and everything that is call-time validation only (WorkDir
// bounding, NetDefault pass-through) is deliberately not claimed.
func (r *Runner) Capabilities() sandbox.Capabilities {
	features := sandbox.SessionFeatures{}
	if sessionsAvailable() {
		features = sandbox.SessionFeatures{TTY: true, Signal: true, Events: true}
	}
	return sandbox.Capabilities{
		Policy: sandbox.Enforcement{
			EnvAllowList: true,
			MemoryCap:    sandbox.GroupCapsSupported(),
			CPUCap:       sandbox.GroupCapsSupported(),
		},
		Features: features,
	}
}

// Exec runs cmd with args under opts. See Runner doc for which
// policy fields are honoured vs. rejected with errdefs.NotAvailable.
func (r *Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return sandbox.Exec(ctx, r, cmd, args, opts)
}

// Start implements the Runner session capability of Runner.
// Policy validation mirrors Exec (sandbox.ValidateExecPolicy plus the
// NetDefault-only posture), then the command is spawned either on pipes
// or a pty through the shared sandbox.StartSession implementation.
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
// started through this runner. Safe to call more than once and when the
// runner never started anything.
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

// buildEnv translates a sandbox.EnvPolicy into a flat []string suitable
// for exec.Cmd.Env. The empty result is returned as nil so os/exec falls
// back to its "no env at all" code path (which is what we want when the
// caller asked for an empty allow-list with no Inject).
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
		// Distinguish "inherit nothing" from "inherit everything": return
		// an empty (non-nil) slice so exec.Cmd.Env is set to no entries
		// instead of falling back to os.Environ().
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
		return "", fmt.Errorf("sandbox/local: resolve workdir: %w", err)
	}
	if real != r.rootDir && !strings.HasPrefix(real, r.rootDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: workdir %q escapes root", sandbox.ErrPathTraversal, dir)
	}
	return abs, nil
}
