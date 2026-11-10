package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	toolengine "github.com/GizClaw/flowcraft/core/tool/engine"
)

// CatalogSpec scopes the tool projection a local engine reads.
type CatalogSpec struct {
	// Scope is CatalogScopeRun (default) or CatalogScopeSession.
	Scope string
}

const (
	// CatalogScopeRun gives every Run a fresh view: discoveries persist across
	// the steps of one run and reset on the next.
	CatalogScopeRun = "run"
	// CatalogScopeSession keeps one view per session, so a discovery
	// survives across user turns.
	CatalogScopeSession = "session"
)

// validatedScope returns the scope, rejecting anything else.
func (c CatalogSpec) validatedScope() (string, error) {
	switch c.Scope {
	case "":
		return CatalogScopeRun, nil
	case CatalogScopeRun, CatalogScopeSession:
		return c.Scope, nil
	}
	return "", errdefs.Validationf(
		"manage: catalog.scope %q is not supported; use %q or %q",
		c.Scope, CatalogScopeRun, CatalogScopeSession)
}

// Host is what the host supplies besides configuration: the durable store, the
// process-wide construction options, and the hooks a document cannot describe.
type Host struct {
	// Store persists every deployment's sessions. Nil keeps them in memory
	// only, which loses the ability to resume — the main reason this layer exists.
	Store agent.Store
	// RuntimeOptions are the construction options: driver table, selectors,
	// secret resolvers. They are per process, not per agent.
	RuntimeOptions []runtime.Option
	// LoadOptions are handed to runtime.LoadLayers: resolvers for placeholder
	// schemes.
	LoadOptions []runtime.LoadOption
	// OnEvent observes every event of every session, named by agent. The
	// agent name is not decoration: one host usually serves many agents.
	OnEvent func(agentName, sessionID string, event agent.Event)
	// ArtifactURL turns a blob reference into a URL a client can fetch. Nil keeps
	// the reference in a data part.
	ArtifactURL func(ref string) string
}

// Manager owns the deployed agents: one table, one kind registry, one set of
// host bindings.
type Manager struct {
	mu     sync.Mutex
	agents map[string]*deployment
	kinds  Kinds
	host   Host
}

// New returns a manager over the given kinds and host bindings. Pass
// BuiltinKinds() to start from the defaults, or a zero Kinds for none.
func New(kinds Kinds, host Host) *Manager {
	if kinds.Engines == nil && kinds.Protocols == nil {
		kinds = BuiltinKinds()
	}
	return &Manager{
		agents: make(map[string]*deployment),
		kinds:  kinds,
		host:   host,
	}
}

// Loader returns a document loader over the same kinds this manager deploys: a
// kind that loads and a kind that deploys cannot diverge.
func (m *Manager) Loader() Loader {
	return Loader{Kinds: m.kinds, LoadOptions: m.host.LoadOptions}
}

// Add deploys one agent. A failure leaves the manager unchanged: half-built
// agents are the kind of thing that only shows up under load.
func (m *Manager) Add(ctx context.Context, spec Spec) error {
	if spec.Name == "" {
		return errdefs.Validationf("manage: agent name is required")
	}
	if _, err := spec.Catalog.validatedScope(); err != nil {
		return err
	}
	m.mu.Lock()
	engineKind, engineFound := m.kinds.Engines[spec.Engine.Kind]
	protocolKind, protocolFound := ProtocolKind{}, false
	if spec.Protocol != nil {
		protocolKind, protocolFound = m.kinds.Protocols[spec.Protocol.Kind]
	}
	engineNames := sortedKeys(m.kinds.Engines)
	protocolNames := sortedKeys(m.kinds.Protocols)
	_, exists := m.agents[spec.Name]
	m.mu.Unlock()
	if exists {
		return errdefs.Validationf("manage: agent %q already exists", spec.Name)
	}
	if !engineFound {
		return errdefs.Validationf(
			"manage: agent %q: engine %q is not registered; offered: %s",
			spec.Name, spec.Engine.Kind, strings.Join(engineNames, ", "))
	}
	if spec.Protocol != nil && !protocolFound {
		return errdefs.Validationf(
			"manage: agent %q: protocol %q is not registered; offered: %s",
			spec.Name, spec.Protocol.Kind, strings.Join(protocolNames, ", "))
	}
	if err := validateEngineSpec(engineKind, spec.Engine, spec.Runtime.Version != 0); err != nil {
		return errdefs.Validationf("manage: agent %q: %v", spec.Name, err)
	}

	var rt *runtime.Runtime
	if spec.Runtime.Version != 0 {
		created, err := runtime.New(ctx, spec.Runtime, m.host.RuntimeOptions...)
		if err != nil {
			return errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
		}
		rt = created
	}
	deployed := &deployment{name: spec.Name, spec: spec, rt: rt}
	engineDigest := canonicalSettings(spec.Engine.Settings)
	if engineKind.Digest != nil {
		engineDigest = engineKind.Digest(spec.Engine.Settings)
	}
	protocolName := ""
	if spec.Protocol != nil {
		protocolName = spec.Protocol.Kind
	}
	deployed.digest = deploymentDigest(runtimeDigest(rt), spec.Engine.Kind, engineDigest, protocolName)

	factory, err := engineKind.Build(ctx, EngineDeps{
		Spec:    spec.Engine,
		Runtime: rt,
		Store:   m.host.Store,
		Digest:  deployed.digest,
	})
	if err != nil {
		if rt != nil {
			_ = rt.Close()
		}
		return errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
	}
	table, err := agent.NewAgent(agent.AgentOptions{
		Store:       m.host.Store,
		Identity:    deployed.digest,
		NewRunner:   factory,
		RunContext:  deployed.runContext,
		IdleTimeout: spec.IdleTimeout,
		MaxSessions: spec.MaxSessions,
		OnEvent:     m.observer(spec.Name),
		OnEvict: func(sessionID string) {
			// A session-scoped view lives exactly as long as its
			// session; the table owns that lifetime and says when it ends.
			deployed.views.Delete(sessionID)
		},
	})
	if err != nil {
		if rt != nil {
			_ = rt.Close()
		}
		return errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
	}
	deployed.agent = table
	if spec.Protocol != nil {
		surface, err := protocolKind.Build(ctx, ProtocolDeps{
			Spec:        *spec.Protocol,
			Name:        spec.Name,
			Store:       m.host.Store,
			Digest:      deployed.digest,
			ArtifactURL: m.host.ArtifactURL,
			Runner:      deployed.protocolRunner,
		})
		if err != nil {
			_ = table.Close()
			if rt != nil {
				_ = rt.Close()
			}
			return errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
		}
		deployed.protocol = surface
	}
	m.mu.Lock()
	if _, exists := m.agents[spec.Name]; exists {
		m.mu.Unlock()
		_ = deployed.close()
		return errdefs.Validationf("manage: agent %q already exists", spec.Name)
	}
	m.agents[spec.Name] = deployed
	m.mu.Unlock()
	return nil
}

// Deploy adds every spec or none: on the first failure the agents this call
// already added are closed and removed, so a half-applied document never serves
// traffic.
func (m *Manager) Deploy(ctx context.Context, specs ...Spec) ([]string, error) {
	added := make([]string, 0, len(specs))
	for _, spec := range specs {
		if err := m.Add(ctx, spec); err != nil {
			for _, name := range added {
				_ = m.Remove(ctx, name)
			}
			return nil, err
		}
		added = append(added, spec.Name)
	}
	return added, nil
}

// Remove stops one agent and drops it from the manager.
func (m *Manager) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	deployed, found := m.agents[name]
	delete(m.agents, name)
	m.mu.Unlock()
	if !found {
		return errdefs.Validationf("manage: agent %q is not deployed", name)
	}
	return deployed.close()
}

// Replace deploys a new runtime for one agent and stops the previous one.
//
// The old runtime's sessions cannot continue on the new one: the
// configuration digest differs, and continuing a transcript under different
// permissions is the failure the digest exists to prevent. Replace therefore
// means "new sessions use the new configuration; old ones end here".
func (m *Manager) Replace(ctx context.Context, spec Spec) error {
	if err := m.Remove(ctx, spec.Name); err != nil {
		return err
	}
	return m.Add(ctx, spec)
}

// Names returns the deployed agent names, sorted.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.agents))
	for name := range m.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Handler is the agent's external surface, nil when it is not exposed.
func (m *Manager) Handler(name string) (http.Handler, bool) {
	m.mu.Lock()
	deployed, found := m.agents[name]
	m.mu.Unlock()
	if !found {
		return nil, false
	}
	return deployed.handler(), true
}

// Session returns one session's handle.
func (m *Manager) Session(name, sessionID string) (*Session, bool) {
	m.mu.Lock()
	deployed, found := m.agents[name]
	m.mu.Unlock()
	if !found {
		return nil, false
	}
	return deployed.session(sessionID), true
}

// Drain stops every agent from accepting work and waits for what is in flight.
// Every failure is reported, not just the first: a caller deciding whether a
// restart is safe needs to know all of them.
func (m *Manager) Drain(ctx context.Context) error {
	m.mu.Lock()
	agents := make([]*deployment, 0, len(m.agents))
	for _, deployed := range m.agents {
		agents = append(agents, deployed)
	}
	m.mu.Unlock()
	var errs []error
	for _, deployed := range agents {
		if err := deployed.agent.Drain(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", deployed.name, err))
		}
	}
	return errors.Join(errs...)
}

// Close stops every deployed agent, reporting every failure.
func (m *Manager) Close() error {
	m.mu.Lock()
	agents := m.agents
	m.agents = make(map[string]*deployment)
	m.mu.Unlock()
	var errs []error
	for _, deployed := range agents {
		if err := deployed.close(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", deployed.name, err))
		}
	}
	return errors.Join(errs...)
}

// EngineNames lists the registered engine kinds, sorted.
func (m *Manager) EngineNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.kinds.Engines)
}

// ProtocolNames lists the registered protocol kinds, sorted.
func (m *Manager) ProtocolNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.kinds.Protocols)
}

// deployment is one built agent: the resources a spec produced, and the few
// fields of the spec it still needs.
type deployment struct {
	name     string
	spec     Spec
	rt       *runtime.Runtime
	agent    *agent.Agent
	protocol Protocol
	digest   runtime.Digest
	// views holds the tool projection of a session-scoped catalog.
	views     sync.Map
	closeOnce sync.Once
	closeErr  error
}

// runContext gives one run its execution context: the tool projection the
// catalog scope names. The session table calls it per run.
func (d *deployment) runContext(ctx context.Context, sessionID string) context.Context {
	if d.rt == nil {
		return ctx
	}
	if scope, _ := d.spec.Catalog.validatedScope(); scope == CatalogScopeSession {
		view, _ := d.views.LoadOrStore(sessionID, d.rt.NewCatalogView())
		catalog, _ := view.(toolengine.CatalogView)
		return runtime.WithCatalogViewCtx(ctx, catalog)
	}
	return d.rt.NewRunContext(ctx)
}

// protocolRunner binds one session for the protocol.
func (d *deployment) protocolRunner(ctx context.Context, sessionID, taskID string) (agent.Runner, error) {
	return &protocolSession{
		Session: d.session(sessionID),
		taskID:  taskID,
		ctx:     ctx,
	}, nil
}

func (d *deployment) session(sessionID string) *Session {
	return &Session{agent: d.agent, sessionID: sessionID}
}

func (d *deployment) handler() http.Handler {
	if d.protocol == nil {
		return nil
	}
	return d.protocol.Handler()
}

// close stops the exposure first, then the runs, then the substrate. It is
// idempotent: the first result is kept and returned again.
func (d *deployment) close() error {
	d.closeOnce.Do(func() {
		var first error
		if d.protocol != nil {
			if err := d.protocol.Close(); err != nil && first == nil {
				first = err
			}
		}
		if d.agent != nil {
			if err := d.agent.Close(); err != nil && first == nil {
				first = err
			}
		}
		if d.rt != nil {
			if err := d.rt.Close(); err != nil && first == nil {
				first = err
			}
		}
		d.closeErr = first
	})
	return d.closeErr
}

// observer names the agent in every event: one host usually serves many.
func (m *Manager) observer(name string) func(string, agent.Event) {
	if m.host.OnEvent == nil {
		return nil
	}
	return func(sessionID string, event agent.Event) {
		m.host.OnEvent(name, sessionID, event)
	}
}

// runtimeDigest is the substrate's digest, empty for a remote engine that has
// none.
func runtimeDigest(rt *runtime.Runtime) runtime.Digest {
	if rt == nil {
		return ""
	}
	return rt.Config().Digest()
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Spec is everything needed to deploy one agent: configuration only. Host
// dependencies live in Host, so a Spec can be compared, logged and rebuilt.
type Spec struct {
	// Name identifies the agent. It is unique within a Manager and is what a
	// caller addresses.
	Name string
	// Engine says what runs the agent.
	Engine EngineSpec
	// Protocol says how it is reached. Nil means not exposed.
	Protocol *ProtocolSpec
	// Runtime is the substrate configuration for this agent. Absent for a
	// remote engine, which has no local model, tools or sandbox.
	Runtime runtime.Config
	// Catalog says how the tool projection is scoped. Empty means per run.
	Catalog CatalogSpec
	// IdleTimeout drops a session from memory after this long without use.
	// Zero keeps every session for the agent's lifetime: state lives in the
	// store, so eviction is a cache decision, not a data loss.
	IdleTimeout time.Duration
	// MaxSessions bounds the session table the agent keeps; zero is
	// unbounded. The oldest idle session is evicted first.
	MaxSessions int
}
