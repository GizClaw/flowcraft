package deploy

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
)

// ProcessPlan is what a process document resolves to: the settings of every resource, and
// the factory that will build it.
//
// Nothing here is built. The plan is safe to print, compare and assert on — the closures are
// the only opaque part — which is what lets a deployment's shape be tested without dialling
// a provider or starting a sandbox.
type ProcessPlan struct {
	// Providers are the model targets this process serves.
	Providers []PlannedProvider
	// Backends are the sandbox backends it can run a session in.
	Backends []PlannedBackend
	// Tools are the tool sources it aggregates into one table.
	Tools []PlannedToolSource
	// Middleware is the tool chain every call passes through, outermost first.
	Middleware []PlannedMiddleware
	// SkillRoots are the directories a session's skill discovery walks, as absolute paths
	// on this host. The roots are a process fact; the index they produce is a session fact,
	// because discovery reads through the session's tree.
	SkillRoots []string
	// Route is the retry and fan-out policy for model calls. Selectors are the host's, so
	// only the value-configured half is here.
	Route route.RetryPolicy
	// Observability is how much of a turn this process records. It is a process fact because
	// everything it controls — the tool chain, the model access, the backends — is built once
	// for the process, so a level two agents could disagree about would be a knob with no effect.
	Observability Observability
}

// Observability is how much of a turn the process records: the trace level and whether a
// model response's bytes are kept.
//
// Both fields are required by the document. A process that left one out would inherit
// something, and there is nothing to inherit: "we record nothing" and "nobody said" are
// different facts, and the second one is how a deployment ends up with no trace at all.
type Observability struct {
	// Trace is how much of every turn is recorded. See [agent.TraceLevel].
	Trace agent.TraceLevel
	// Bodies keeps the bytes of each model response, which is what a replay answers from. It
	// is a retention decision rather than a level: the answer is the user's content.
	Bodies bool
}

// PlannedMiddleware is one link of the tool chain, resolved but not built.
type PlannedMiddleware struct {
	// Name addresses the link in an error message; the chain itself is ordered.
	Name string
	// Impl is the registered implementation the document's kind selected.
	Impl string
	// New returns the constructor for this link, which receives the tool catalog: per-tool policy
	// reads a tool's metadata from there. A nil middleware is a disabled link, which the chain
	// skips.
	New func() (func(tool.Catalog) (tool.Middleware, error), error)
	// Trace is the least level this link is worth installing at, declared by its registration.
	// The loader leaves a link out of the plan when the process records less than this, so the
	// assembly never builds one that would only turn itself off per call.
	Trace agent.TraceLevel
}

// PlannedHook is one lifecycle policy, resolved but not built.
type PlannedHook struct {
	// Impl is the registered implementation the document's kind selected.
	Impl string
	// New builds the hook. One kind may produce several hooks, or none.
	New func() (agent.Hooks, error)
}

// PlannedProvider is one model target, resolved but not built.
type PlannedProvider struct {
	// Name addresses the target, both from an agent's grant and from a route policy.
	Name string
	// Impl is the registered implementation the document's kind selected.
	Impl string
	// New builds the provider. It is called once, by the host, and never by this package.
	New func() (*inference.Provider, error)
}

// PlannedBackend is one sandbox backend, resolved but not built.
type PlannedBackend struct {
	// Name is what a grant's sandbox field selects.
	Name string
	// Impl is the registered implementation the document's kind selected.
	Impl string
	// New returns the constructor for one session's backend instance. The factory is process-wide;
	// the instance is not, because the confine it enforces comes from the session's grant.
	New func() (func(sandbox.Confine) (sandbox.Runner, error), error)
}

// PlannedToolSource is one contributor of tools, resolved but not built.
type PlannedToolSource struct {
	// Name is how a deployment addresses the source, for error messages and provenance.
	Name string
	// Impl is the registered implementation the document's kind selected.
	Impl string
	// New builds the source. It is called once, by the host.
	New func() (tool.Source, error)
}

// Deployment is one agent, resolved against a process — still a value.
//
// It is what host.Add installs: a name, a prompt, the grant its sessions run under, and the
// program that runs them. The program arrives as a closure for the same reason everything
// else does: building it is the host's job, because the host is what closes it.
type Deployment struct {
	// Name is the agent's identity within the host. It must be unique.
	Name string
	// Program is what runs the conversations.
	Program PlannedProgram
	// Grant is the authority its sessions run under, already validated.
	Grant policy.Grant
	// Hooks are the lifecycle policies attached to it, in declaration order. Each one carries
	// its own settings because a hook without them cannot do anything: forge's seat memory
	// needs a directory and a retention count.
	Hooks []PlannedHook
}

// PlannedProgram is the program an agent runs, resolved but not built.
type PlannedProgram struct {
	// Impl is the registered implementation the document's kind selected.
	Impl string
	// New builds the program. It is called once per deployment, by the host.
	New func() (agent.Program, error)
}

// Capabilities is what a running process offers, as a value: the questions an agent document
// can ask before it is installed.
//
// It holds only what the *process* decides. Registered implementations — program kinds, hook
// kinds, middleware, provider kinds — are checked against the kind table, which the loader
// already has and which is a build-time fact rather than a running one.
type Capabilities struct {
	// Backends are the backend instances this process configured; a grant's sandbox field
	// selects one of them by name.
	Backends []string
	// Models are the models it serves, as "provider/name": the ids its providers' catalogs
	// report. They are read after the process is built, because a provider's settings are opaque
	// here — `deploy` decodes none of them.
	Models []string
}

// BackendNames lists the backend instances the plan configured, which is the set a grant's
// sandbox field may select.
func (p ProcessPlan) BackendNames() []string {
	names := make([]string, 0, len(p.Backends))
	for _, backend := range p.Backends {
		names = append(names, backend.Name)
	}
	return names
}

// ModelIDs is Models as the typed value an agent document is checked against.
//
// The strings are what a capability list reports to an operator; a caller comparing them with a
// document needs the type, and parsing "provider/name" at each call site is how the two spellings drift
// apart. A name without a provider is skipped rather than guessed at.
func (c Capabilities) ModelIDs() []model.ModelID {
	out := make([]model.ModelID, 0, len(c.Models))
	for _, ref := range c.Models {
		provider, name, ok := strings.Cut(ref, "/")
		if !ok || provider == "" || name == "" {
			continue
		}
		out = append(out, model.ModelID{Provider: provider, Name: name})
	}
	return out
}
