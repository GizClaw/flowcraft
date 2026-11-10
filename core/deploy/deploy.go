package deploy

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	toolengine "github.com/GizClaw/flowcraft/core/tool/engine"
)

// CatalogSpec scopes the tool projection a local engine reads.
type CatalogSpec struct {
	// Scope is CatalogScopeRun (default) or CatalogScopeConversation.
	Scope string
}

const (
	// CatalogScopeRun gives every Run a fresh view: discoveries persist across
	// the steps of one run and reset on the next.
	CatalogScopeRun = "run"
	// CatalogScopeConversation keeps one view per conversation, so a discovery
	// survives across user turns.
	CatalogScopeConversation = "conversation"
)

// validatedScope returns the scope, rejecting anything else.
func (c CatalogSpec) validatedScope() (string, error) {
	switch c.Scope {
	case "":
		return CatalogScopeRun, nil
	case CatalogScopeRun, CatalogScopeConversation:
		return c.Scope, nil
	}
	return "", errdefs.Validationf(
		"manage: catalog.scope %q is not supported; use %q or %q",
		c.Scope, CatalogScopeRun, CatalogScopeConversation)
}

// EngineSpec selects the loop implementation and carries its settings.
type EngineSpec struct {
	// Kind is required: an agent that does not say what it is cannot be
	// deployed meaningfully.
	Kind string
	// Peer is the remote endpoint for the a2a engine.
	Peer string
	// Auth is the credential for reaching Peer, as written (JSON).
	Auth json.RawMessage
	// Graph is the graph document path for the graph engine.
	Graph string
}

// ProtocolSpec selects how the agent is exposed.
type ProtocolSpec struct {
	Kind string
	// Settings is the protocol's own configuration as written (JSON).
	Settings json.RawMessage
	// Decoded is Settings after the registration's Decode ran. The loader fills
	// it, so a deployment typo fails at load time instead of at Add.
	Decoded any
}

// Spec is everything needed to deploy one agent.
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
	// RuntimeOptions are the construction options: driver table, selectors,
	// secret resolvers. They are per deployment, not per agent, but they arrive
	// with the spec so an agent is self-contained.
	RuntimeOptions []runtime.Option
	// OnEvent observes every event a conversation produces. Optional; a host
	// uses it to stream progress without reaching into the engine.
	OnEvent func(conversationID string, event agent.Event)
	// Catalog says how the tool projection is scoped. Empty means per run.
	Catalog CatalogSpec
	// Store persists this agent's conversations. It is handed to the conversation
	// table and to every runner, because a deployment that forgets it loses the
	// ability to resume — the main reason this layer exists.
	Store agent.Store
	// IdleTimeout drops a conversation from memory after this long without use.
	// Zero keeps every conversation for the agent's lifetime: state lives in the
	// store, so eviction is a cache decision, not a data loss.
	IdleTimeout time.Duration
	// MaxSessions bounds the conversation table the agent keeps; zero is
	// unbounded. The oldest idle conversation is evicted first.
	MaxSessions int
	// ArtifactURL turns a blob reference into a URL a client can fetch. Nil keeps
	// the reference in a data part: a client then learns the pointer instead of
	// a file URL that nobody can open.
	ArtifactURL func(ref string) string
}

// Agent is one deployed agent: a runtime, the table of conversations the engine
// serves, and the protocol that exposes it.
type Agent struct {
	spec Spec
	rt   *runtime.Runtime
	// conversations owns the slot, the lifecycle and the checkpoint identity.
	// Deploy only wires it.
	conversations *agent.Agent
	protocol      protocol
	handler       http.Handler
	digest        runtime.Digest
	// views holds the tool projection of a conversation-scoped catalog.
	views sync.Map
}

// Name reports the deployed name.
func (a *Agent) Name() string { return a.spec.Name }

// Run handles one message: it starts a Task or answers the one a paused Task
// asked. The sequence carries the run; the caller consumes it.
func (a *Agent) Run(ctx context.Context, conversationID string, msg message.Message, opts ...agent.TurnOption) iter.Seq2[agent.Event, error] {
	return a.conversations.Run(ctx, conversationID, msg, opts...)
}

// State reports where the conversation's Task stands.
func (a *Agent) State(ctx context.Context, conversationID string) (agent.TaskState, error) {
	return a.conversations.State(ctx, conversationID)
}

// Cancel stops the conversation's Task.
func (a *Agent) Cancel(ctx context.Context, conversationID, reason string) error {
	return a.conversations.Cancel(ctx, conversationID, reason)
}

// Drain stops accepting work and waits for the runs in flight.
func (a *Agent) Drain(ctx context.Context) error {
	return a.conversations.Drain(ctx)
}

// Handler is the agent's external surface, nil when it is not exposed.
func (a *Agent) Handler() http.Handler { return a.handler }

// Close stops the agent: the exposure first, then the runs, then the substrate.
func (a *Agent) Close() error {
	var first error
	if a.protocol != nil {
		if err := a.protocol.Close(); err != nil && first == nil {
			first = err
		}
	}
	if a.conversations != nil {
		if err := a.conversations.Close(); err != nil && first == nil {
			first = err
		}
	}
	if a.rt != nil {
		if err := a.rt.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// runContext gives one run its execution context: the tool projection the
// catalog scope names. The conversation table calls it per run.
func (a *Agent) runContext(ctx context.Context, conversationID string) context.Context {
	if a.rt == nil {
		return ctx
	}
	if scope, _ := a.spec.Catalog.validatedScope(); scope == CatalogScopeConversation {
		view, _ := a.views.LoadOrStore(conversationID, a.rt.NewCatalogView())
		catalog, _ := view.(toolengine.CatalogView)
		return runtime.WithCatalogViewCtx(ctx, catalog)
	}
	return a.rt.NewRunContext(ctx)
}

// Manager owns the deployed agents.
type Manager struct {
	mu        sync.Mutex
	agents    map[string]*Agent
	engines   map[string]Engine
	protocols map[string]Protocol
}

// ManagerOption configures a Manager at construction.
type ManagerOption func(*Manager)

// New returns a manager with the built-in engines registered and no protocols:
// an exposure is a deployment choice, not a default.
func New(opts ...ManagerOption) *Manager {
	m := &Manager{
		agents:    make(map[string]*Agent),
		engines:   builtinEngines(),
		protocols: make(map[string]Protocol),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// Add deploys one agent. A failure leaves the manager unchanged: half-built
// agents are the kind of thing that only shows up under load.
func (m *Manager) Add(ctx context.Context, spec Spec) (*Agent, error) {
	if spec.Name == "" {
		return nil, errdefs.Validationf("manage: agent name is required")
	}
	if _, err := spec.Catalog.validatedScope(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	engineRegistration, engineFound := m.engines[spec.Engine.Kind]
	protocolRegistration, protocolFound := Protocol{}, false
	if spec.Protocol != nil {
		protocolRegistration, protocolFound = m.protocols[spec.Protocol.Kind]
	}
	engineNames := sortedKeys(m.engines)
	protocolNames := sortedKeys(m.protocols)
	m.mu.Unlock()
	if !engineFound {
		return nil, errdefs.Validationf(
			"manage: agent %q: engine %q is not registered; offered: %s",
			spec.Name, spec.Engine.Kind, strings.Join(engineNames, ", "))
	}
	// The loader enforces the same rules, so a hand-built Spec cannot slip past
	// them: a local engine needs a runtime, a remote one needs a peer, and each
	// kind speaks for its own fields.
	if err := validateEngineSpec(engineRegistration, spec.Engine, spec.Runtime.Version != 0); err != nil {
		return nil, errdefs.Validationf("manage: agent %q: %v", spec.Name, err)
	}
	if spec.Protocol != nil && !protocolFound {
		return nil, errdefs.Validationf(
			"manage: agent %q: protocol %q is not registered; offered: %s",
			spec.Name, spec.Protocol.Kind, strings.Join(protocolNames, ", "))
	}

	var rt *runtime.Runtime
	if spec.Runtime.Version != 0 {
		created, err := runtime.New(ctx, spec.Runtime, spec.RuntimeOptions...)
		if err != nil {
			return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
		}
		rt = created
	}
	deployed := &Agent{
		spec:   spec,
		rt:     rt,
		digest: deploymentDigest(runtimeDigest(rt), spec.Engine, spec.Protocol),
	}
	factory, err := engineRegistration.Build(ctx, EngineDeps{
		Spec:    spec.Engine,
		Runtime: rt,
		Store:   spec.Store,
		Digest:  deployed.digest,
	})
	if err != nil {
		_ = rt.Close()
		return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
	}
	conversations, err := agent.NewAgent(agent.AgentOptions{
		Store:       spec.Store,
		Identity:    deployed.digest,
		NewRunner:   factory,
		RunContext:  deployed.runContext,
		IdleTimeout: spec.IdleTimeout,
		MaxSessions: spec.MaxSessions,
		OnEvent:     spec.OnEvent,
		OnEvict: func(conversationID string) {
			// A conversation-scoped view lives exactly as long as its
			// conversation; the table owns that lifetime and says when it ends.
			deployed.views.Delete(conversationID)
		},
	})
	if err != nil {
		_ = rt.Close()
		return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
	}
	deployed.conversations = conversations
	if spec.Protocol != nil {
		surface, err := protocolRegistration.Build(*spec.Protocol, deployed)
		if err != nil {
			_ = conversations.Close()
			_ = rt.Close()
			return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
		}
		deployed.protocol = surface
		deployed.handler = surface.Handler()
	}
	m.mu.Lock()
	if _, exists := m.agents[spec.Name]; exists {
		m.mu.Unlock()
		_ = deployed.Close()
		return nil, errdefs.Validationf("manage: agent %q already exists", spec.Name)
	}
	m.agents[spec.Name] = deployed
	m.mu.Unlock()
	return deployed, nil
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
	return deployed.Close()
}

// Replace deploys a new runtime for one agent and stops the previous one.
//
// The old runtime's conversations cannot continue on the new one: the
// configuration digest differs, and continuing a transcript under different
// permissions is the failure the digest exists to prevent. Replace therefore
// means "new conversations use the new configuration; old ones end here".
func (m *Manager) Replace(ctx context.Context, spec Spec) (*Agent, error) {
	m.mu.Lock()
	previous := m.agents[spec.Name]
	m.mu.Unlock()
	if previous != nil {
		if err := m.Remove(ctx, spec.Name); err != nil {
			return nil, err
		}
	}
	return m.Add(ctx, spec)
}

// Agent returns a deployed agent by name.
func (m *Manager) Agent(name string) (*Agent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deployed, found := m.agents[name]
	return deployed, found
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

// Drain stops every agent from accepting work and waits for what is in flight.
func (m *Manager) Drain(ctx context.Context) error {
	m.mu.Lock()
	agents := make([]*Agent, 0, len(m.agents))
	for _, deployed := range m.agents {
		agents = append(agents, deployed)
	}
	m.mu.Unlock()
	var first error
	for _, deployed := range agents {
		if err := deployed.Drain(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Close stops every deployed agent.
func (m *Manager) Close() error {
	m.mu.Lock()
	agents := m.agents
	m.agents = make(map[string]*Agent)
	m.mu.Unlock()
	var first error
	for _, deployed := range agents {
		if err := deployed.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
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
