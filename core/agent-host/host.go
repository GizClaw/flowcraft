// Package host owns the process-level side of deployed agents: the bindings a
// host supplies, the deployment table, and the session handle a protocol or a
// caller drives. Documents and kinds live in core/deploy.

package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"
	toolengine "github.com/GizClaw/flowcraft/core/tool/engine"
)

// Bindings is what the host supplies besides configuration: the durable store, the
// process-wide construction options, and the hooks a document cannot describe.
type Bindings struct {
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
	//
	// It runs synchronously on the session's run goroutine, so it must be fast
	// and non-blocking: a slow observer stalls the run and its protocol stream.
	OnEvent func(agentName, sessionID string, event agent.Event)
	// ArtifactURL turns a blob reference into a URL a client can fetch. Nil keeps
	// the reference in a data part.
	ArtifactURL func(ref string) string
	// Trace controls the spans every run of every deployment emits. Zero emits
	// none; a durable host usually asks for TraceNode.
	Trace telemetry.TraceLevel
	// Harness is the host's build identity (version or git SHA), recorded in
	// each deployment's manifest.
	Harness string
	// TenantID is the tenant every agent in this Manager serves. It is stamped
	// on every run, span and RunRecord. Empty means single-tenant. A process
	// serving several tenants constructs one Manager per tenant: the session
	// table is not tenant-keyed.
	TenantID string
	// OnRun observes every finished run, named by agent. Like OnEvent it runs
	// synchronously on the run's goroutine, so it must be fast.
	OnRun func(agentName string, record agent.RunRecord)
}

// Manager owns the deployed agents: one table, one kind registry, one set of
// host bindings.
type Manager struct {
	mu       sync.Mutex
	agents   map[string]*deployment
	kinds    deploy.Kinds
	bindings Bindings
}

// New returns a manager over the given kinds and host bindings. Pass
// BuiltinKinds() to start from the defaults, or a zero deploy.Kinds for none.
func New(kinds deploy.Kinds, bindings Bindings) *Manager {
	if kinds.Engines == nil && kinds.Protocols == nil {
		kinds = deploy.BuiltinKinds()
	}
	return &Manager{
		agents:   make(map[string]*deployment),
		kinds:    kinds,
		bindings: bindings,
	}
}

// Loader returns a document loader over the same kinds this manager deploys: a
// kind that loads and a kind that deploys cannot diverge.
func (m *Manager) Loader() deploy.Loader {
	return deploy.Loader{Kinds: m.kinds, LoadOptions: m.bindings.LoadOptions}
}

// Add deploys one agent. A failure leaves the manager unchanged: half-built
// agents are the kind of thing that only shows up under load.
func (m *Manager) Add(ctx context.Context, spec deploy.Spec) error {
	if spec.Name == "" {
		return errdefs.Validationf("manage: agent name is required")
	}
	m.mu.Lock()
	_, exists := m.agents[spec.Name]
	m.mu.Unlock()
	if exists {
		return errdefs.Validationf("manage: agent %q already exists", spec.Name)
	}
	deployed, err := m.build(ctx, spec)
	if err != nil {
		return err
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

// build validates a spec and constructs its deployment without installing it, so
// Add and Replace share one path and a failure leaves the manager untouched.
func (m *Manager) build(ctx context.Context, spec deploy.Spec) (*deployment, error) {
	if _, err := spec.Catalog.ValidatedScope(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	engineKind, engineFound := m.kinds.Engines[spec.Engine.Kind]
	protocolKind, protocolFound := deploy.ProtocolKind{}, false
	if spec.Protocol != nil {
		protocolKind, protocolFound = m.kinds.Protocols[spec.Protocol.Kind]
	}
	engineNames := sortedKeys(m.kinds.Engines)
	protocolNames := sortedKeys(m.kinds.Protocols)
	m.mu.Unlock()
	if !engineFound {
		return nil, errdefs.Validationf(
			"manage: agent %q: engine %q is not registered; offered: %s",
			spec.Name, spec.Engine.Kind, strings.Join(engineNames, ", "))
	}
	if spec.Protocol != nil && !protocolFound {
		return nil, errdefs.Validationf(
			"manage: agent %q: protocol %q is not registered; offered: %s",
			spec.Name, spec.Protocol.Kind, strings.Join(protocolNames, ", "))
	}
	if err := deploy.ValidateEngineSpec(engineKind, spec.Engine, spec.Runtime.Version != 0); err != nil {
		return nil, errdefs.Validationf("manage: agent %q: %v", spec.Name, err)
	}

	runtimeOptions := append([]runtime.Option(nil), m.bindings.RuntimeOptions...)
	var delegationService *delegation.Service
	if spec.Delegation != nil {
		if spec.Runtime.Version == 0 {
			return nil, errdefs.Validationf(
				"manage: agent %q: a remote engine cannot delegate", spec.Name)
		}
		service, err := delegation.New(
			delegationSessions{m: m}, delegationPolicy(spec.Delegation))
		if err != nil {
			return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
		}
		delegationService = service
		runtimeOptions = append(runtimeOptions,
			runtime.WithToolSource(delegation.Tools(delegationService)))
	}

	var rt *runtime.Runtime
	if spec.Runtime.Version != 0 {
		created, err := runtime.New(ctx, spec.Runtime, runtimeOptions...)
		if err != nil {
			return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
		}
		rt = created
	}
	deployed := &deployment{
		name:       spec.Name,
		tenant:     m.bindings.TenantID,
		spec:       spec,
		rt:         rt,
		delegation: delegationService,
	}
	engineFingerprint := deploy.CanonicalSettings(spec.Engine.Settings)
	if engineKind.Fingerprint != nil {
		engineFingerprint = engineKind.Fingerprint(spec.Engine.Settings)
	}
	protocolName := ""
	if spec.Protocol != nil {
		protocolName = spec.Protocol.Kind
	}
	deployed.fingerprint = deploymentFingerprint(runtimeDigest(rt), spec.Engine.Kind, engineFingerprint, protocolName)
	deployed.manifest = agent.Manifest{
		Deployment: deployed.fingerprint,
		Engine:     spec.Engine.Kind,
		Protocol:   protocolName,
		Models:     manifestModels(rt),
		Tools:      manifestTools(rt),
		Harness:    m.bindings.Harness,
	}

	factory, err := engineKind.Build(ctx, deploy.EngineDeps{
		Spec:        spec.Engine,
		Runtime:     rt,
		Store:       m.bindings.Store,
		Fingerprint: deployed.fingerprint,
		Manifest:    deployed.manifest,
		Trace:       m.bindings.Trace,
	})
	if err != nil {
		if rt != nil {
			_ = rt.Close()
		}
		return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
	}
	table, err := agent.NewAgent(agent.AgentOptions{
		Store:      m.bindings.Store,
		Identity:   deployed.fingerprint,
		NewRunner:  factory,
		RunContext: deployed.runContext,
		Trace:      m.bindings.Trace,
		OnRun: func(record agent.RunRecord) {
			if m.bindings.OnRun != nil {
				m.bindings.OnRun(spec.Name, record)
			}
		},
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
		return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
	}
	deployed.agent = table
	if spec.Protocol != nil {
		surface, err := protocolKind.Build(ctx, deploy.ProtocolDeps{
			Spec:        *spec.Protocol,
			Name:        spec.Name,
			Store:       m.bindings.Store,
			Fingerprint: deployed.fingerprint,
			ArtifactURL: m.bindings.ArtifactURL,
			Agent:       deployed.agent,
		})
		if err != nil {
			_ = table.Close()
			if rt != nil {
				_ = rt.Close()
			}
			return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
		}
		deployed.protocol = surface
	}
	return deployed, nil
}

// Replace deploys a new runtime for one agent and stops the previous one.
//
// The new deployment is built before the old one is touched, so a spec that
// fails to build leaves the running agent exactly as it was.
//
// The old runtime's sessions cannot continue on the new one: the
// configuration fingerprint differs, and continuing a transcript under different
// permissions is the failure the fingerprint exists to prevent. Replace therefore
// means "new sessions use the new configuration; old ones end here".
func (m *Manager) Replace(ctx context.Context, spec deploy.Spec) error {
	if spec.Name == "" {
		return errdefs.Validationf("manage: agent name is required")
	}
	m.mu.Lock()
	_, exists := m.agents[spec.Name]
	m.mu.Unlock()
	if !exists {
		return errdefs.Validationf("manage: agent %q is not deployed", spec.Name)
	}
	deployed, err := m.build(ctx, spec)
	if err != nil {
		return err
	}
	m.mu.Lock()
	previous, exists := m.agents[spec.Name]
	if !exists {
		m.mu.Unlock()
		_ = deployed.close()
		return errdefs.Validationf("manage: agent %q is not deployed", spec.Name)
	}
	m.agents[spec.Name] = deployed
	m.mu.Unlock()
	return previous.close()
}

// Deploy adds every spec or none: on the first failure the agents this call
// already added are closed and removed, so a half-applied document never serves
// traffic.
func (m *Manager) Deploy(ctx context.Context, specs ...deploy.Spec) ([]string, error) {
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
	name        string
	tenant      string
	spec        deploy.Spec
	rt          *runtime.Runtime
	agent       *agent.Agent
	protocol    deploy.Protocol
	delegation  *delegation.Service
	fingerprint agent.Fingerprint
	manifest    agent.Manifest
	// views holds the tool projection of a session-scoped catalog.
	views     sync.Map
	closeOnce sync.Once
	closeErr  error
}

// runContext gives one run its execution context: the tool projection the
// catalog scope names. The session table calls it per run.
func (d *deployment) runContext(ctx context.Context, sessionID string) context.Context {
	ctx = runctx.WithRun(ctx, runctx.Run{
		SessionID: sessionID,
		AgentName: d.name,
		TenantID:  d.tenant,
		Manifest:  d.manifest.ID(),
	})
	if d.rt == nil {
		return ctx
	}
	ctx = d.rt.Context(ctx)
	if scope, _ := d.spec.Catalog.ValidatedScope(); scope == deploy.CatalogScopeSession {
		view, _ := d.views.LoadOrStore(sessionID, d.rt.NewCatalogView())
		catalog, _ := view.(toolengine.CatalogView)
		return runtime.WithCatalogViewCtx(ctx, catalog)
	}
	return d.rt.NewRunContext(ctx)
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
		if d.delegation != nil {
			if err := d.delegation.Close(); err != nil && first == nil {
				first = err
			}
		}
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

// delegationSessions adapts the manager to delegation.Sessions. It is a value
// so the dependency points host → delegation, never the reverse.
type delegationSessions struct{ m *Manager }

func (s delegationSessions) Session(name, sessionID string) (delegation.Session, bool) {
	session, ok := s.m.Session(name, sessionID)
	if !ok {
		return nil, false
	}
	return session, true
}

func (s delegationSessions) Names() []string { return s.m.Names() }

func delegationPolicy(spec *deploy.DelegationSpec) delegation.Policy {
	modes := make([]delegation.Mode, len(spec.Modes))
	for i, mode := range spec.Modes {
		modes[i] = delegation.Mode(mode)
	}
	return delegation.Policy{
		Targets:       spec.Targets,
		Modes:         modes,
		MaxDepth:      spec.MaxDepth,
		MaxConcurrent: spec.MaxConcurrent,
		Timeout:       spec.Timeout,
		MaxInputBytes: spec.MaxInputBytes,
	}
}

// observer names the agent in every event: one host usually serves many.
func (m *Manager) observer(name string) func(string, agent.Event) {
	if m.bindings.OnEvent == nil {
		return nil
	}
	return func(sessionID string, event agent.Event) {
		m.bindings.OnEvent(name, sessionID, event)
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
