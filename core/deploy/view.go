package deploy

import (
	"github.com/GizClaw/flowcraft/core/agent"
)

// View is the read-only view of an assembled deployment handed to resource
// values after every resource and agent exists.
//
// It is deliberately narrow: a resource that needs to look something up after
// assembly declares exactly what it reads, and deploy hands it this view
// instead of the mutable [Result]. Consumers narrow it structurally — a
// package that only needs agent lookup declares its own two-method interface
// and the view satisfies it.
type View interface {
	// Agent returns the assembled agent registered under name.
	Agent(name string) (*agent.Agent, bool)
	// AgentNames returns the sorted bound agent names.
	AgentNames() []string
	// Names returns the sorted built resource names.
	Names() []string
}

// Binder is implemented by resource values that need the fully assembled
// deployment after every resource and agent has been wired: backends
// registering into hosts, directories snapshotting the agent set.
//
// Binder lives in deploy — the package that calls it — rather than in
// core/resource, because its argument is the assembly layer's view and only
// this package can name it. That keeps core/resource free of agent and
// deployment concepts.
type Binder interface {
	BindDeployment(view View) error
}

// Compile-time check: the assembled deployment is the view.
var _ View = (*Result)(nil)
