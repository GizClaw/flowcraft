package host

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Agents is the deployment table: one entry per agent this process serves.
//
// It is the active half of this package. A [Resource] says what the process carries; this says
// who is allowed to use it — and, from the next slice on, which conversations are open under each
// of them.
//
// The split between the two is lifetime, not layering: a resource is built once and closed when
// the process exits, an agent when it is deployed and closed when it is removed. That is why the
// table is here and the process is a value it holds, rather than one object doing both.
type Agents struct {
	res *Resource
	// store keeps sessions' tasks; nil keeps them in memory only.
	store agent.Store
	// fingerprint names the deployment set this process was built from.
	fingerprint agent.Fingerprint
	// approval is the host's rule for starting a command; nil installs the backend undecorated.
	approval *Approval
	// tenant and authority are the process facts a session's identity is made of.
	tenant    string
	authority string
	// runSink receives one record per finished turn; nil keeps runs in-process only.
	runSink func(agent.RunRecord)
	// harness names the build this process is, so a manifest can say which binary ran it.
	harness string

	mu       sync.Mutex
	entries  map[string]*deployment
	order    []string
	sessions map[string]*Session
	byAge    []*Session
	// edges numbers delegation edges so two children of one parent get distinct session ids.
	edges atomic.Int64
	// retiring holds the deployments a reload replaced while their sessions are still open. They
	// are out of the table but not out of the process, so Close has to know about them.
	retiring []*deployment
	closed   bool
}

// deployment is one installed agent: what the document said, resolved into the live program that
// runs it.
type deployment struct {
	name    string
	program agent.Program
	grant   policy.Grant
	// observability is the process's record level, held here because the turn path reaches it
	// through the deployment. It is not a per-agent setting: the chain and the model access it
	// controls are built once for the process.
	observability deploy.Observability
	// manifest is the version combination this deployment runs, assembled at install; the
	// turn copies it into every run record and the driver writes it into its checkpoint.
	// Its identity is [agent.Manifest.ID], hashed by the two consumers rather than stored
	// here: a cached copy would be a second answer to one question.
	manifest agent.Manifest
	// hooks are the lifecycle policies the document attached, built once at install so a bad
	// setting fails here rather than on the first turn that reaches a boundary.
	hooks agent.Hooks
	// projection is this deployment's tool visibility: the grant's exposure and budget, over the
	// process's catalog. One per deployment, because the policy is the deployment's.
	// newView builds one session's projection from this deployment's authority. It is a closure so the
	// host names no projection implementation.
	newView func() tool.View
	// live counts the open sessions, which is what the grant's limit is measured against.
	live int
	// retired marks a deployment that a reload replaced: its program is closed when the last
	// session opened under it goes away, because a session is pinned to what it was opened under.
	retired bool
}

// NewAgents returns an empty deployment table over the process's resources.
func NewAgents(res *Resource, opts ...AgentsOption) *Agents {
	if res == nil {
		panic("host.NewAgents: a resource is required")
	}
	a := &Agents{
		res:      res,
		entries:  map[string]*deployment{},
		sessions: map[string]*Session{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

// Add installs one or more deployments.
//
// A program is built outside the lock — construction may dial or compile, and holding the table
// across that would stall every other deployment — and the whole call is all-or-nothing: when the
// second deployment fails, the program the first one built is closed and the table is left as it
// was. Half a deployment set is a process whose documented behaviour does not match its name.
func (a *Agents) Add(deps ...deploy.Deployment) error {
	if len(deps) == 0 {
		return nil
	}
	built := make([]*deployment, 0, len(deps))
	for _, dep := range deps {
		entry, err := a.build(dep)
		if err != nil {
			return errors.Join(err, closeAll(built))
		}
		built = append(built, entry)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errors.Join(errdefs.NotAvailablef("host: the agent table is closed"), closeAll(built))
	}
	for _, entry := range built {
		if _, exists := a.entries[entry.name]; exists {
			return errors.Join(errdefs.Conflictf("host: agent %q is already deployed", entry.name),
				closeAll(built))
		}
	}
	for _, entry := range built {
		a.entries[entry.name] = entry
		a.order = append(a.order, entry.name)
	}
	return nil
}

// ErrSessionsActive reports that an agent cannot be removed while conversations are open under it.
//
// Removing it anyway would either abandon those sessions or close their boundary under a running
// turn; both are decisions the caller has to make explicitly, by closing them first.
var ErrSessionsActive = errors.New("host: the agent still has open sessions")

// Remove uninstalls one agent and closes its program.
func (a *Agents) Remove(name string) error {
	a.mu.Lock()
	entry, ok := a.entries[name]
	if !ok {
		a.mu.Unlock()
		return errdefs.NotFoundf("host: agent %q is not deployed", name)
	}
	if entry.live > 0 {
		live := entry.live
		a.mu.Unlock()
		return fmt.Errorf("%w (agent %q holds %d)", ErrSessionsActive, name, live)
	}
	a.drop(name)
	a.mu.Unlock()
	return entry.close()
}

// Reload replaces a deployment's program.
//
// A conversation is pinned to the deployment it was opened under, so a reload never changes what
// a running session sees: the old program is closed when nothing is left using it. With no
// sessions yet that is immediately; the session table is what makes it later.
func (a *Agents) Reload(dep deploy.Deployment) error {
	entry, err := a.build(dep)
	if err != nil {
		return err
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return errors.Join(errdefs.NotAvailablef("host: the agent table is closed"),
			closeAll([]*deployment{entry}))
	}
	previous, ok := a.entries[entry.name]
	if !ok {
		a.mu.Unlock()
		return errors.Join(errdefs.NotFoundf("host: agent %q is not deployed", entry.name),
			closeAll([]*deployment{entry}))
	}
	a.entries[entry.name] = entry
	// A session opened under the previous deployment keeps running against it, so its program is
	// released only when the last of them is gone. With none open that is now.
	previous.retired = true
	drain := previous.live == 0
	if !drain {
		a.retiring = append(a.retiring, previous)
	}
	a.mu.Unlock()

	if drain {
		return previous.close()
	}
	return nil
}

// Names lists the deployed agents in installation order.
func (a *Agents) Names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.order...)
}

// Deployed reports whether name is installed.
func (a *Agents) Deployed(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.entries[name]
	return ok
}

// Close uninstalls every agent, newest first, and forgets them.
//
// It does not close the [Resource]: the process outlives its deployments, and the layer that built
// it is the layer that closes it.
func (a *Agents) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	sessions := a.byAge
	deployments := make([]*deployment, 0, len(a.order))
	for i := len(a.order) - 1; i >= 0; i-- {
		if entry, ok := a.entries[a.order[i]]; ok {
			deployments = append(deployments, entry)
		}
	}
	retiring := a.retiring
	a.entries = map[string]*deployment{}
	a.order = nil
	a.sessions = map[string]*Session{}
	a.byAge = nil
	a.retiring = nil
	a.mu.Unlock()

	// Decided under the lock, released outside it: closing a driver may run a turn's last cleanup
	// and closing a cell may stop processes, neither of which should hold the table.
	errs := make([]error, 0, len(sessions)+len(deployments)+len(retiring))
	perAgent := make(map[string]int, len(deployments))
	for _, session := range sessions {
		perAgent[session.agent]++
		if err := session.close(); err != nil {
			errs = append(errs, err)
		}
	}
	for agentName, count := range perAgent {
		recordSessionsClosed(ctx, agentName, count)
	}
	for _, entry := range deployments {
		if err := entry.close(); err != nil {
			errs = append(errs, err)
		}
	}
	// A deployment a reload replaced is closed here too: its sessions are gone with the ones above,
	// and leaving its program to the garbage collector is how a process keeps a compiled graph it
	// will never run again.
	for _, entry := range retiring {
		if err := entry.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// build resolves one deployment into a live program. It holds no lock: construction is the slow
// part and the table does not depend on it.
func (a *Agents) build(dep deploy.Deployment) (*deployment, error) {
	if dep.Name == "" {
		return nil, errdefs.Validationf("host: a deployment needs a name")
	}
	program, err := dep.Program.New()
	if err != nil {
		return nil, fmt.Errorf("host: agent %q: program: %w", dep.Name, err)
	}
	if program == nil {
		return nil, fmt.Errorf("host: agent %q: program built nothing", dep.Name)
	}
	hooks, err := buildHooks(dep.Hooks)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("host: agent %q: hooks: %w", dep.Name, err), program.Close())
	}
	if a.res.newView == nil {
		return nil, errdefs.NotAvailablef("host: agent %q: this process has no tool projection", dep.Name)
	}
	proj := a.res.newView(a.res.tools, dep.Grant.Tools)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("host: agent %q: tool exposure: %w", dep.Name, err), program.Close())
	}
	manifest := a.manifestOf(dep.Program.Impl, dep.Grant.Models, proj)
	return &deployment{
		name:          dep.Name,
		program:       program,
		grant:         dep.Grant,
		observability: a.res.observability,
		manifest:      manifest,
		hooks:         hooks,
		newView:       func() tool.View { return proj },
	}, nil
}

// drop removes one entry from the table. The caller holds the lock.
func (a *Agents) drop(name string) {
	delete(a.entries, name)
	for i, existing := range a.order {
		if existing == name {
			a.order = append(a.order[:i], a.order[i+1:]...)
			return
		}
	}
}

// close releases the program this entry owns.
func (d *deployment) close() error {
	if d.program == nil {
		return nil
	}
	err := d.program.Close()
	d.program = nil
	return err
}

// buildHooks resolves the document's hook list into one chain.
//
// The kinds arrive as constructors because the implementation is the application's: the document
// names one, and the factory it registered produces the policy. Chaining follows the contract the
// agent package documents — the boundary hooks stop at the first refusal, the after-turn hooks all
// run — so one deployment's list behaves like one policy, not like a stack of unrelated ones.
func buildHooks(planned []deploy.PlannedHook) (agent.Hooks, error) {
	if len(planned) == 0 {
		return agent.Hooks{}, nil
	}
	hooks := make([]agent.Hooks, 0, len(planned))
	for _, p := range planned {
		built, err := p.New()
		if err != nil {
			return agent.Hooks{}, fmt.Errorf("%s: %w", p.Impl, err)
		}
		hooks = append(hooks, built)
	}
	return agent.Chain(hooks...), nil
}

// projectionPolicy is the grant's tool visibility as the projection understands it.
//
// The exposure levels are the deployment's, not the process's: two agents on one process can show
// different tools, which is why the projection is per deployment while the catalog it reads is
// shared.

// closeAll releases the deployments a refused Add or Reload built. The close errors are returned
// joined rather than dropped: a program that cannot be released is the second problem, and the caller
// is the only one who can report it.
func closeAll(entries []*deployment) error {
	var errs []error
	for _, entry := range entries {
		if err := entry.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
