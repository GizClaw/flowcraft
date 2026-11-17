package runtime

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// SandboxFactory builds the runner a deployment named in sandbox.backend.
//
// The abstraction is [sandbox.Runner]; this is only the name-to-constructor edge
// the composition root needs, because each backend ships its own options type and
// the constructors are therefore not interchangeable. A host registers the table
// (see [WithSandboxBackends]); core/runtime/backends is the opt-in set for a host
// that ships several of the backends core provides.
type SandboxFactory func(ctx context.Context, in SandboxBuild) (sandbox.Runner, error)

// SandboxBuild is what a factory receives: the shared build context every backend
// needs, plus the whole sandbox block so a factory reads only its own section.
type SandboxBuild struct {
	// Dir is the runtime's working directory, the root a backend confines.
	Dir string
	// Writes are the paths the deployment declared writable.
	Writes []string
	// Limits are the resolved resource limits.
	Limits sandbox.ResourceLimits
	// Journal is the file-journal configuration, nil when the block is absent.
	Journal *journal.Config
	// Config is the sandbox block as written, for the backend's own settings.
	Config SandboxConfig
}

// WithSandboxBackends registers the backends this process may build, keyed by the
// value a deployment writes in sandbox.backend. A later call replaces the table
// rather than merging it, which is what lets a host build the set once and a test
// override it.
//
// There is no default table: a backend the host does not register is a validation
// error naming what is registered. That is the rule drivers already follow — the
// platform owns the seam, the host owns the set it ships — and it is what keeps a
// host that ships only the local runner from linking every other backend.
func WithSandboxBackends(backends map[string]SandboxFactory) Option {
	return func(o *options) { o.sandboxBackends = backends }
}

// sandboxBackendTable validates a registered table. A map makes duplicate and
// missing names impossible, so only an empty key or a nil entry can be wrong.
func sandboxBackendTable(backends map[string]SandboxFactory) (map[string]SandboxFactory, error) {
	if len(backends) == 0 {
		return nil, nil
	}
	table := make(map[string]SandboxFactory, len(backends))
	for name, build := range backends {
		if strings.TrimSpace(name) == "" {
			return nil, errdefs.Validationf("runtime: a sandbox backend has no name")
		}
		if build == nil {
			return nil, errdefs.Validationf("runtime: sandbox backend %q has no constructor", name)
		}
		table[name] = build
	}
	return table, nil
}

// offeredBackends renders the registered names for an error message, so an
// unknown backend says what this process could have built.
func offeredBackends(table map[string]SandboxFactory) string {
	if len(table) == 0 {
		return "none"
	}
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
