// Package local is the no-isolation backend: commands run directly on the host through os/exec.
//
// It exists for the deployment that has nothing to confine — a developer's machine, a CI job, a
// trusted tool. Every other backend is a sibling of this one, not a wrapper around it, which is why
// the caller selects a backend by name and the confine it is built with is what makes the difference.
//
// It owns no directory and no policy of its own: what one instance may do is its
// [sandbox.Confine], fixed when the instance is built because a backend serves exactly one session.
package local

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/sandbox/sessionkit"
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

// Runner executes commands directly on the host using os/exec.
//
// What it can honour is declared in [Runner.Capabilities] and checked by [sandbox.ValidateConfine]
// when the instance is built: the environment allow-list, the memory and cpu ceilings the shared
// session layer can sample, and nothing about the filesystem — there is no OS boundary here to
// confine a write set with, so a confine that narrows writes is refused at construction rather than
// silently ignored on the first command.
//
// Everything per-call is supported: the working directory (an absolute path the caller names), stdin,
// the deadline, and output truncation. A cpu budget is derived from the deadline, so asking for one
// without a deadline is refused by [sandbox.Confine.Narrow] before it reaches this backend.
type Runner struct {
	confine          sandbox.Confine
	caps             sandbox.Capabilities
	defaultMaxOutput int64
	sessions         *sessionkit.SessionRegistry
	registryOnce     sync.Once
}

// New builds one backend instance for one session.
//
// The confine is the instance's whole policy, so construction is where an unenforceable one is
// refused: a network posture this backend cannot apply, a write set it cannot confine, or a unix
// socket policy it has no way to implement is an error here, where an operator sees it, instead of a
// surprise on the first command that needs it.
func New(confine sandbox.Confine, opts ...Option) (*Runner, error) {
	r := &Runner{confine: confine, defaultMaxOutput: defaultMaxOutputBytes}
	for _, o := range opts {
		o(r)
	}
	// The declaration comes before the check: "can this backend cap memory" is answered by the
	// sampler that would do the enforcing, and an allow-list is honoured by the env filtering below.
	r.caps = sandbox.Capabilities{
		Policy:   sessionkit.Enforcement(sandbox.Enforcement{EnvAllowList: true}),
		Features: sessionkit.SessionFeatures(),
	}
	if err := sandbox.ValidateConfine(confine, r.caps.Policy); err != nil {
		return nil, err
	}
	r.sessions = sessionkit.NewSessionRegistry(r.spawnProcess)
	return r, nil
}

// Capabilities declares Runner's honest surface: the env
// allow-list is honoured, memory/cpu caps are enforced by the group
// watcher where the platform supports it, sessions are whatever the shared
// session layer supports here, and everything that is call-time validation only (WorkDir
// bounding, NetDefault pass-through) is deliberately not claimed.
func (r *Runner) Capabilities() sandbox.Capabilities { return r.caps }

// Start implements [sandbox.Runner]: it hands the spec to the shared session layer after narrowing
// its budgets against the confine's ceiling.
func (r *Runner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	return r.registry().Start(ctx, spec)
}

// Attach implements sandbox.SessionAttacher: the registry still owns the process,
// so a caller holding an id can read its output log again. It is what makes
// sandbox.Attach usable against a local backend — and therefore against a remote
// server whose runner is local.
func (r *Runner) Attach(ctx context.Context, id string) (sandbox.Session, error) {
	return r.registry().Attach(ctx, id)
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
func (r *Runner) Close() error { return r.registry().Close() }

// registry returns the session registry, initialising it lazily so a
// zero-value Runner still answers with NotAvailable instead of
// panicking on a nil interface.
func (r *Runner) registry() *sessionkit.SessionRegistry {
	r.registryOnce.Do(func() {
		if r.sessions == nil {
			r.sessions = sessionkit.NewSessionRegistry(r.spawnProcess)
		}
	})
	return r.sessions
}

// buildEnv translates a sandbox.EnvPolicy into a flat []string suitable
// for exec.Cmd.Env. The empty result is returned as nil so os/exec falls
// back to its "no env at all" code path (which is what we want when the
// caller asked for an empty allow-list with no Inject).
func buildEnv(p policy.EnvPolicy) []string {
	var env []string

	switch p.Allow {
	case nil:
		env = append(env, os.Environ()...)
	default:
		// An allow-list, including one that selected nothing, must produce a non-nil slice:
		// exec.Cmd reads nil Env as "inherit the caller's environment", so a list whose names
		// happen to be unset on this host would hand the child every secret the daemon holds.
		// The previous code returned the nil it started from, which made that a silent widening
		// rather than the empty environment the document asked for.
		env = make([]string, 0, len(p.Allow))
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

	if p.Allow != nil && len(env) == 0 {
		// The same distinction at the end: an allow-list that matched nothing is "inherit
		// nothing", and it has to be spelled as a non-nil empty slice for exec.Cmd.
		return []string{}
	}
	return env
}

// resolveWorkDir checks the directory the command runs in.
//
// It must be absolute: there is no working directory and no root to resolve a relative path against,
// and inventing one would make the same document mean different things on different hosts.
func resolveWorkDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errdefs.Validationf("sandbox: workdir is required")
	}
	if !filepath.IsAbs(dir) {
		return "", errdefs.Validationf("sandbox: workdir %q must be an absolute path", dir)
	}
	return filepath.Clean(dir), nil
}
