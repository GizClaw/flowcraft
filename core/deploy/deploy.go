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
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

// EngineSpec selects the loop implementation and carries its settings.
type EngineSpec struct {
	// Kind is required: an agent that does not say what it is cannot be
	// deployed meaningfully.
	Kind string
	// Peer is the remote card URL for the a2a engine.
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
	// proxy engine, which has no local model, tools or sandbox.
	Runtime runtime.Config
	// RuntimeOptions are the construction options: driver table, selectors,
	// secret resolvers. They are per deployment, not per agent, but they arrive
	// with the spec so an agent is self-contained.
	RuntimeOptions []runtime.Option
	// OnEvent observes every event a conversation produces. Optional; a host
	// uses it to stream progress without reaching into the engine.
	OnEvent func(conversationID string, event agent.Event)
	// Store persists this agent's conversations. It is applied to every session
	// last, because a deployment that forgets it loses the ability to resume —
	// the main reason this layer exists.
	Store agent.Store
	// IdleTimeout drops a conversation from memory after this long without use.
	// Zero keeps every conversation for the agent's lifetime: state lives in the
	// store, so eviction is a cache decision, not a data loss.
	IdleTimeout time.Duration
	// MaxSessions bounds the in-memory conversation table; zero is unbounded.
	// The oldest idle conversation is evicted first.
	MaxSessions int
}

// Agent is one deployed agent: a runtime, the engine that runs it, and the
// conversations it serves.
type Agent struct {
	spec Spec
	rt   *runtime.Runtime
	// engine opens conversations; built once, at Add.
	engine engine
	// protocol is the external surface, nil when the agent is not exposed.
	protocol protocol
	handler  http.Handler

	// base is detached from the caller's request context: a Run outlives the
	// Submit that started it, and cancelling the request must not cancel the
	// conversation.
	base   context.Context
	cancel context.CancelFunc

	// digest is the resume identity of this deployment: the runtime plus the
	// engine and protocol that make it what it is.
	digest runtime.Digest

	mu       sync.Mutex
	sessions map[string]*conversation
	closed   bool
	draining bool
	working  sync.WaitGroup
}

// session is what manage drives. *agent.Session is the local implementation; a
// proxy conversation implements the same surface over a remote A2A task.
type session interface {
	SetIdentity(taskID, conversationID string)
	State() agent.TaskState
	Run(ctx context.Context, in agent.Input) iter.Seq2[agent.Event, error]
	Resume(ctx context.Context, reply agent.Reply) iter.Seq2[agent.Event, error]
	Send(ctx context.Context, msg message.Message, mode agent.Mode) (agent.Mode, error)
	Pending() int
	Abandon() error
}

// conversation is one conversation's session and whether a Run owns it.
type conversation struct {
	session session
	// lastUsed drives idle eviction.
	lastUsed time.Time
	running  bool
	// done is closed when the current Run ends, so a caller that arrived while
	// the Task was pausing can wait for the bookkeeping instead of being told
	// the conversation is busy forever.
	done chan struct{}
}

// Name reports the deployed name.
func (a *Agent) Name() string { return a.spec.Name }

// Submit delivers one message to a conversation.
//
// The mode decides what happens to work already in flight. When the conversation
// is idle the Run starts here: a caller should not have to know whether it needs
// to pump the loop.
func (a *Agent) Submit(ctx context.Context, conversationID string, msg message.Message, mode agent.Mode) error {
	if conversationID == "" {
		return errdefs.Validationf("manage: conversation id is required")
	}
	a.mu.Lock()
	if a.closed || a.draining {
		a.mu.Unlock()
		return errdefs.NotAvailablef("manage: agent %q is not accepting work", a.spec.Name)
	}
	entry, err := a.conversationLocked(ctx, conversationID)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()

	// A paused Task is waiting on a question: a steer is the answer to it, an
	// interrupt gives up on it, a queue waits for the next Task.
	paused := entry.session.State() == agent.StateInputRequired
	switch entry.session.State() {
	case agent.StateInputRequired:
		switch mode {
		case agent.ModeSteer:
			// Resuming is a run: it has to take the same slot, or a concurrent
			// Submit could start a second Resume on the same session.
			return a.resumePaused(ctx, entry, conversationID, agent.Reply{Content: msg.Content})
		case agent.ModeInterrupt:
			if err := entry.session.Abandon(); err != nil {
				return err
			}
		}
	}

	if _, err := entry.session.Send(ctx, msg, mode); err != nil {
		return err
	}
	if paused && mode != agent.ModeInterrupt {
		// The Task is waiting on a question. Its queue is delivered by the Run
		// that answers it, not by a new Task started behind the question.
		return nil
	}
	a.mu.Lock()
	busy := entry.running
	if !busy {
		entry.running = true
		entry.done = make(chan struct{})
	}
	a.mu.Unlock()
	if busy {
		// The running Task will consume it at its own pace.
		return nil
	}
	a.goRun(entry, conversationID)
	return nil
}

// resumePaused acquires the conversation slot and drives a Resume, waiting for a
// run that is unwinding instead of failing or racing it.
func (a *Agent) resumePaused(ctx context.Context, entry *conversation, conversationID string, reply agent.Reply) error {
	a.mu.Lock()
	if a.closed || a.draining {
		a.mu.Unlock()
		return errdefs.NotAvailablef("manage: agent %q is not accepting work", a.spec.Name)
	}
	if entry.running {
		done := entry.done
		a.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		a.mu.Lock()
		if a.closed || a.draining {
			a.mu.Unlock()
			return errdefs.NotAvailablef("manage: agent %q is not accepting work", a.spec.Name)
		}
	}
	if entry.session.State() != agent.StateInputRequired {
		a.mu.Unlock()
		return errdefs.Validationf(
			"manage: conversation %q is not waiting for input", conversationID)
	}
	entry.running = true
	entry.done = make(chan struct{})
	a.mu.Unlock()
	a.goRunResume(entry, conversationID, reply)
	return nil
}

// Resume answers a Task waiting on input.
func (a *Agent) Resume(ctx context.Context, conversationID string, reply agent.Reply) error {
	a.mu.Lock()
	if a.closed || a.draining {
		a.mu.Unlock()
		return errdefs.NotAvailablef("manage: agent %q is not accepting work", a.spec.Name)
	}
	entry, err := a.conversationLocked(ctx, conversationID)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	if entry.running {
		// The Task pauses inside its Run and the event that announces it arrives
		// before the Run unwinds; a caller reacting to that event must wait for
		// the bookkeeping, not race it.
		done := entry.done
		a.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return errdefs.NotAvailablef("manage: agent %q is closed", a.spec.Name)
		}
	}
	if entry.session.State() != agent.StateInputRequired {
		a.mu.Unlock()
		return errdefs.Validationf(
			"manage: conversation %q is not waiting for input", conversationID)
	}
	entry.running = true
	entry.done = make(chan struct{})
	a.mu.Unlock()
	a.goRunResume(entry, conversationID, reply)
	return nil
}

// Handler is the agent's external surface, nil when it is not exposed.
func (a *Agent) Handler() http.Handler { return a.handler }

// Close stops the agent: running Tasks are cancelled, and whatever the mailbox
// still holds is dropped and logged.
func (a *Agent) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	dropped := 0
	for _, entry := range a.sessions {
		dropped += entry.session.Pending()
	}
	a.mu.Unlock()

	a.cancel()
	a.working.Wait()
	var first error
	if dropped > 0 {
		telemetry.Info(a.base, "manage: dropped pending input on close",
			attribute.String("agent", a.spec.Name),
			attribute.Int("messages", dropped))
	}
	if a.protocol != nil {
		if err := a.protocol.Close(); err != nil && first == nil {
			first = err
		}
	}
	if a.engine != nil {
		if err := a.engine.Close(); err != nil && first == nil {
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

// conversationLocked returns the conversation, creating or restoring it.
func (a *Agent) conversationLocked(ctx context.Context, conversationID string) (*conversation, error) {
	now := time.Now()
	a.sweepLocked(now)
	if entry, found := a.sessions[conversationID]; found {
		entry.lastUsed = now
		return entry, nil
	}
	session, err := a.openSession(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	entry := &conversation{session: session, lastUsed: now}
	a.sessions[conversationID] = entry
	// Sweep again after inserting: the bound is a property of the table, and the
	// pre-insert sweep cannot see the entry that just pushed it over.
	a.sweepLocked(now)
	return entry, nil
}

// sweepLocked evicts idle conversations, then trims to MaxSessions. A running
// conversation is never evicted: its state lives in the mailbox, not the store.
func (a *Agent) sweepLocked(now time.Time) {
	if a.spec.IdleTimeout > 0 {
		for id, entry := range a.sessions {
			if entry.running || now.Sub(entry.lastUsed) < a.spec.IdleTimeout {
				continue
			}
			delete(a.sessions, id)
		}
	}
	if a.spec.MaxSessions <= 0 || len(a.sessions) <= a.spec.MaxSessions {
		return
	}
	for len(a.sessions) > a.spec.MaxSessions {
		oldest := ""
		var oldestAt time.Time
		for id, entry := range a.sessions {
			if entry.running {
				continue
			}
			if oldest == "" || entry.lastUsed.Before(oldestAt) {
				oldest, oldestAt = id, entry.lastUsed
			}
		}
		if oldest == "" {
			return // every conversation is running
		}
		delete(a.sessions, oldest)
	}
}

// Drain stops accepting work and waits for the conversations in flight.
//
// A timeout does not cancel them — that is Close's job — so the error reports
// what is still running and lets the caller decide.
func (a *Agent) Drain(ctx context.Context) error {
	a.mu.Lock()
	if a.closed || a.draining {
		a.mu.Unlock()
		return nil
	}
	a.draining = true
	a.mu.Unlock()

	done := make(chan struct{})
	go func() {
		a.working.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		a.mu.Lock()
		names := make([]string, 0, len(a.sessions))
		for id, entry := range a.sessions {
			if entry.running {
				names = append(names, id)
			}
		}
		a.mu.Unlock()
		sort.Strings(names)
		return errdefs.NotAvailablef(
			"manage: agent %q still has %d conversations in flight: %s",
			a.spec.Name, len(names), strings.Join(names, ", "))
	}
}

// openSession asks the engine for one conversation. Nothing here branches on the
// kind: the engine table decided that at Add.
func (a *Agent) openSession(ctx context.Context, conversationID string) (session, error) {
	return a.engine.Open(ctx, conversationID)
}

func (a *Agent) goRun(entry *conversation, conversationID string) {
	a.working.Add(1)
	go func() {
		defer a.working.Done()
		a.drive(entry, conversationID, func(yield func(agent.Event, error) bool) {
			for event, err := range entry.session.Run(a.base, agent.Input{}) {
				if !yield(event, err) {
					return
				}
			}
		})
	}()
}

func (a *Agent) goRunResume(entry *conversation, conversationID string, reply agent.Reply) {
	a.working.Add(1)
	go func() {
		defer a.working.Done()
		a.drive(entry, conversationID, func(yield func(agent.Event, error) bool) {
			for event, err := range entry.session.Resume(a.base, reply) {
				if !yield(event, err) {
					return
				}
			}
		})
	}()
}

// drive runs one sequence to its end, reports events, requeues whatever was not
// consumed, and continues with it.
func (a *Agent) drive(
	entry *conversation,
	conversationID string,
	sequence func(func(agent.Event, error) bool),
) {
	var leftover []message.Message
	sequence(func(event agent.Event, err error) bool {
		if err != nil {
			telemetry.WarnErr(a.base, "manage: run failed", err,
				attribute.String("agent", a.spec.Name),
				attribute.String("conversation.id", conversationID))
			return false
		}
		if undelivered, ok := event.(agent.Undelivered); ok {
			leftover = undelivered.Messages
		}
		if a.spec.OnEvent != nil {
			a.spec.OnEvent(conversationID, event)
		}
		return true
	})

	// Undelivered input is not dropped while the conversation lives: it becomes
	// the next Task's input.
	for _, msg := range leftover {
		if _, err := entry.session.Send(a.base, msg, agent.ModeQueue); err != nil {
			break
		}
	}
	// Continue only when the Task actually ended. A paused Task keeps its
	// mailbox for Resume; pumping it here would start a new Task behind the
	// question and leave the conversation reported as busy.
	a.finish(entry)
	if entry.session.State().Terminal() {
		a.pump(entry, conversationID)
	}
}

func (a *Agent) finish(entry *conversation) {
	a.mu.Lock()
	entry.running = false
	if entry.done != nil {
		close(entry.done)
		entry.done = nil
	}
	a.mu.Unlock()
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
	// A proxy engine has no local substrate; a local engine cannot do without
	// one. The loader enforces the same rule, so a hand-built Spec cannot slip
	// past it.
	if spec.Runtime.Version == 0 && strings.TrimSpace(spec.Engine.Peer) == "" {
		return nil, errdefs.Validationf("manage: agent %q has no runtime config", spec.Name)
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
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	deployed := &Agent{
		spec:     spec,
		rt:       rt,
		base:     base,
		cancel:   cancel,
		sessions: make(map[string]*conversation),
	}

	deployed.digest = deploymentDigest(runtimeDigest(rt), spec.Engine, spec.Protocol)
	built, err := engineRegistration.Build(ctx, spec.Engine, deployed)
	if err != nil {
		_ = rt.Close()
		return nil, errdefs.Validationf("manage: agent %q: %w", spec.Name, err)
	}
	deployed.engine = built
	if spec.Protocol != nil {
		surface, err := protocolRegistration.Build(*spec.Protocol, deployed)
		if err != nil {
			_ = built.Close()
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

// runtimeDigest is the substrate's digest, empty for a proxy that has none.
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

// pump starts the next Task if the mailbox holds input and no run owns the
// conversation. Both paths call it after releasing the slot: Submit's drive loop
// and the protocol's stream wrapper.
func (a *Agent) pump(entry *conversation, conversationID string) {
	a.mu.Lock()
	if a.closed || a.draining || entry.running || entry.session.Pending() == 0 {
		a.mu.Unlock()
		return
	}
	entry.running = true
	entry.done = make(chan struct{})
	a.mu.Unlock()
	a.goRun(entry, conversationID)
}
