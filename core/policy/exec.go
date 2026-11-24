package policy

import (
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// EnvPolicy controls which host environment variables a child process can see,
// and lets the policy inject extra ones.
//
//   - Allow absent or null: inherit the host environment.
//   - Allow: [] (explicitly empty): inherit nothing; the child sees only Inject.
//   - Allow: [PATH, HOME]: forward those names only.
//
// The nil/empty distinction is the one place this package keeps it, because
// "inherit everything" and "inherit nothing" are both legitimate and neither is a
// sensible default for the other. It is why Allow carries no omitempty: writing
// null and writing [] are different statements, and a document that lost the
// difference would silently widen a child's environment.
type EnvPolicy struct {
	Allow  []string          `json:"allow" yaml:"allow"`
	Inject map[string]string `json:"inject,omitempty" yaml:"inject,omitempty"`
}

// Resources caps what a command may consume. Zero means unbounded; a backend that
// cannot enforce a non-zero value must refuse the session rather than ignore it.
//
// Two values need a mechanism rather than a number: CPUMillicores is derived from
// a per-call deadline (a backend that cannot derive one refuses it), and DiskBytes
// needs a quota no local backend has today.
type Resources struct {
	CPUMillicores  int   `json:"cpu_millicores,omitempty" yaml:"cpu_millicores,omitempty"`
	MemoryBytes    int64 `json:"memory_bytes,omitempty" yaml:"memory_bytes,omitempty"`
	DiskBytes      int64 `json:"disk_bytes,omitempty" yaml:"disk_bytes,omitempty"`
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty" yaml:"max_output_bytes,omitempty"`
}

// ExecPolicy is the command-execution half of a grant: how long a command may
// run, what it may see of the environment, and which shell a tool should use.
//
// The working directory is deliberately absent. It is a per-call argument the
// model supplies, because a session has no single directory — the platform does
// not define one, and a default here would silently become the daemon's own cwd.
type ExecPolicy struct {
	// Timeout is the ceiling for one command. A call may ask for less.
	Timeout Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// Env is the environment policy.
	Env EnvPolicy `json:"env,omitempty" yaml:"env,omitempty"`
	// Shell is the shell a command tool should use, empty for the backend's
	// default.
	Shell string `json:"shell,omitempty" yaml:"shell,omitempty"`
}

// Validate rejects an execution policy no backend could honour.
func (e ExecPolicy) Validate() error {
	if e.Timeout.Std() < 0 {
		return errdefs.Validationf("policy.exec: timeout must not be negative")
	}
	for i, name := range e.Env.Allow {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("policy.exec.env.allow[%d] is empty", i)
		}
		if strings.ContainsAny(name, "= \t") {
			return errdefs.Validationf("policy.exec.env.allow[%d] %q must be a bare variable name", i, name)
		}
	}
	for name := range e.Env.Inject {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "= \t") {
			return errdefs.Validationf("policy.exec.env.inject: %q must be a bare variable name", name)
		}
	}
	switch {
	case e.Shell == "":
		return nil
	case !filepath.IsAbs(e.Shell):
		return errdefs.Validationf("policy.exec: shell %q must be an absolute path", e.Shell)
	}
	return nil
}

// Validate rejects a resource cap that cannot mean anything.
func (r Resources) Validate() error {
	switch {
	case r.CPUMillicores < 0:
		return errdefs.Validationf("policy.resources: cpu_millicores must not be negative")
	case r.MemoryBytes < 0:
		return errdefs.Validationf("policy.resources: memory_bytes must not be negative")
	case r.DiskBytes < 0:
		return errdefs.Validationf("policy.resources: disk_bytes must not be negative")
	case r.MaxOutputBytes < 0:
		return errdefs.Validationf("policy.resources: max_output_bytes must not be negative")
	}
	return nil
}
