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
	"github.com/GizClaw/flowcraft/sandbox/journal"
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

// WithFileJournal attaches a file journal to the runner: every write
// under the runner root becomes a readable event (see the
// core/sandbox/journal package). The journal is an observation stream,
// never a boundary — sandbox-local has no filesystem confinement, so
// this reports what happened rather than preventing anything.
//
// The root is taken from the runner, so the option only carries what to
// watch and how much to keep. A journal that cannot start leaves the
// runner usable but reports
// sandbox.JournalCapabilities.Enabled = false and fails
// sandbox.OpenJournal with the reason; deployments configure the
// journal through the resource settings, which fail the build instead.
func WithFileJournal(cfg journal.Config) Option {
	return func(r *Runner) {
		requested := cfg
		r.journalReq = &requested
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
	// journalReq is the requested journal, resolved in New once the
	// root is known. Exactly one of journal / journalErr is set when a
	// journal was asked for.
	journalReq *journal.Config
	journal    *journal.Journal
	journalErr error
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
	if r.journalReq != nil {
		cfg := *r.journalReq
		r.journalReq = nil
		// A journal that cannot start is recorded rather than fatal: the backend still runs
		// commands, and Capabilities/OpenJournal report the honest reason instead of claiming a
		// journal that is not there.
		if j, err := newJournalEngine(cfg); err != nil {
			r.journalErr = err
		} else {
			r.journal = j
		}
		r.caps.Journal = r.journal.Capabilities()
	}
	r.sessions = sessionkit.NewSessionRegistry(r.spawnProcess)
	return r, nil
}

// newJournalEngine builds the platform watch engine. It is a package
// variable so the factory tests can exercise the "settings accepted,
// engine refused" path without exhausting the host's own watch
// resources.
var newJournalEngine = journal.New

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
func (r *Runner) Close() error {
	err := r.registry().Close()
	if r.journal != nil {
		// The journal stops with the runner it belongs to, and the
		// events it already holds stay readable for the readers that
		// were opened before.
		if jerr := r.journal.Close(); jerr != nil && err == nil {
			err = jerr
		}
	}
	return err
}

// OpenJournal implements core/sandbox.JournalProvider. It fails with
// errdefs.NotAvailable when no journal was attached; a journal that was
// requested but could not start reports its construction error instead,
// so the reason is visible rather than flattened into "none".
func (r *Runner) OpenJournal(ctx context.Context) (sandbox.FileJournal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.journal == nil {
		if r.journalErr != nil {
			return nil, r.journalErr
		}
		return nil, errdefs.NotAvailablef(
			"sandbox-local: no file journal is attached to this runner")
	}
	return r.journal.Open()
}

var (
	_ sandbox.JournalProvider = (*Runner)(nil)
	_ sandbox.SessionAttacher = (*Runner)(nil)
)

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
