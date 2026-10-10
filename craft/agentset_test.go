package craft

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/tool"
)

// agentsetProbe is the shared state of the agentset fixture: what the
// engine factory and the runtime binder were asked to do, plus the
// channels the serialization test parks a reload's binder on.
type agentsetProbe struct {
	mu      sync.Mutex
	engines int
	binds   int
	order   []string

	block   chan struct{} // non-nil: a bind waits for it to close
	entered chan struct{} // non-nil: a bind sends once it has started

	// turnRelease, non-nil, is what the fixture engine waits on before
	// finishing a run: a turn that is live until the test closes the
	// channel, which is what makes a removal's drain observable.
	turnRelease chan struct{}
}

func (p *agentsetProbe) countEngines() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.engines
}

func (p *agentsetProbe) countBinds() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.binds
}

func (p *agentsetProbe) events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.order)
}

func (p *agentsetProbe) parkNextBind(block, entered chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.block, p.entered = block, entered
}

func (p *agentsetProbe) parkTurns(release chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turnRelease = release
}

func (p *agentsetProbe) turnGate() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.turnRelease
}

type agentsetCapability struct{ probe *agentsetProbe }

func (agentsetCapability) Name() string { return "agentset" }

func (c agentsetCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	if err := registry.Register(agentsetAssemblyFactory{}); err != nil {
		return err
	}
	return registry.Register(agentsetEngineFactory(c))
}

// agentsetAssemblyFactory provides the tool assembly a dynamic
// catalog's default points at; a real application mounts tool sources
// into the standard assembly instead.
type agentsetAssemblyFactory struct{}

func (agentsetAssemblyFactory) Spec() resource.Spec {
	return resource.Spec{Kind: tool.AssemblyKind, Impl: "test"}
}

func (agentsetAssemblyFactory) New(context.Context, resource.Input) (any, error) {
	return tool.NewAssembly(nil)
}

func (c agentsetCapability) BindRuntime(
	_ context.Context,
	key RuntimeKey,
	_ *runtime.Runtime,
) error {
	probe := c.probe
	probe.mu.Lock()
	probe.binds++
	probe.order = append(probe.order, "bind:"+string(key))
	block, entered := probe.block, probe.entered
	probe.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return nil
}

// agentsetEngineFactory builds a no-op engine per agent instance and
// counts the builds: registration, replacement and reload re-binds all
// materialize one.
type agentsetEngineFactory struct{ probe *agentsetProbe }

func (agentsetEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "agent.Engine", Impl: "test"}
}

func (f agentsetEngineFactory) New(context.Context, resource.Input) (any, error) {
	f.probe.mu.Lock()
	f.probe.engines++
	f.probe.order = append(f.probe.order, "engine")
	f.probe.mu.Unlock()
	return agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if release := f.probe.turnGate(); release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return board, ctx.Err()
			}
		}
		return board, nil
	}), nil
}

// agentsetBaseDeploy declares no agents and no dynamic catalog: every
// agent in these tests is registered through SyncAgents.
const agentsetBaseDeploy = `
version: v1
resources:
  bus: {kind: event.Bus, impl: memory}
runtime:
  event_bus: bus
`

// agentsetRemoveTimeoutDeploy is agentsetBaseDeploy with the runtime's
// configured removal bound: the fallback a pass uses when no
// WithAgentRemoveTimeout is given.
func agentsetRemoveTimeoutDeploy(timeout string) string {
	return `
version: v1
resources:
  bus: {kind: event.Bus, impl: memory}
runtime:
  event_bus: bus
  agents:
    remove_timeout: ` + timeout + `
`
}

// agentsetDeployedDeploy declares a deployed agent, so a declaration
// with the same name must fail instead of shadowing it.
const agentsetDeployedDeploy = `
version: v1
resources:
  bus: {kind: event.Bus, impl: memory}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: bus
`

// agentsetCatalogDeploy is the migration shape for applications whose
// dynamic agents need a tool assembly: the dynamic catalog declares a
// default instead of a per-agent entry.
const agentsetCatalogDeploy = `
version: v1
resources:
  bus: {kind: event.Bus, impl: memory}
  tools: {kind: tool.Assembly, impl: test}
runtime:
  event_bus: bus
  dynamic_catalog:
    tools:
      default: tools
`

// agentsetCatalogNoDefaultDeploy maps only a deployed agent, so a
// dynamically declared name has no assembly to run with.
const agentsetCatalogNoDefaultDeploy = `
version: v1
resources:
  bus: {kind: event.Bus, impl: memory}
  tools: {kind: tool.Assembly, impl: test}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: bus
  dynamic_catalog:
    tools:
      bot: tools
`

// newAgentsetCraft builds a started Craft with one open runtime and the
// probe capability registered.
func newAgentsetCraft(t *testing.T, deployYAML string) (*Craft, *agentsetProbe) {
	t.Helper()
	probe := &agentsetProbe{}
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  version: 0.1.0
deploy:
` + indent(deployYAML, "  ")))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{agentsetCapability{probe: probe}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	return c, probe
}

// agentsetDecl builds a minimal declaration: a card and the fixture
// engine, which is all deploy.BindAgent needs.
func agentsetDecl(name, rev string) AgentDecl {
	return AgentDecl{
		Name: name,
		Def: agent.Definition{
			Card:   agent.AgentCard{Name: name},
			Engine: agent.EngineRef{Kind: "agent.Engine", Impl: "test"},
		},
		Rev: rev,
	}
}

// agentsetRuntime returns the fixture's open runtime.
func agentsetRuntime(t *testing.T, c *Craft) *runtime.Runtime {
	t.Helper()
	rt, ok := c.Runtime(DefaultKey)
	if !ok {
		t.Fatal("runtime is not open")
	}
	return rt
}

// checkAgentsetReport asserts the three lists and that the pass
// reported no failure.
func checkAgentsetReport(
	t *testing.T,
	report AgentSyncReport,
	registered, updated, removed []string,
) {
	t.Helper()
	if !slices.Equal(report.Registered, registered) {
		t.Fatalf("Registered = %v, want %v", report.Registered, registered)
	}
	if !slices.Equal(report.Updated, updated) {
		t.Fatalf("Updated = %v, want %v", report.Updated, updated)
	}
	if !slices.Equal(report.Removed, removed) {
		t.Fatalf("Removed = %v, want %v", report.Removed, removed)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", report.Errors)
	}
}

func TestSyncAgentsRegistersDeclaredAgents(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	// Unsorted input: the pass works and reports in name order.
	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("beta", "r1"),
		agentsetDecl("alpha", "r1"),
	})
	if err != nil {
		t.Fatalf("SyncAgents: %v", err)
	}
	checkAgentsetReport(t, report, []string{"alpha", "beta"}, nil, nil)
	for _, name := range []string{"alpha", "beta"} {
		if _, ok := rt.Agent(name); !ok {
			t.Fatalf("agent %q is not live after the pass", name)
		}
	}
	if got := probe.countEngines(); got != 2 {
		t.Fatalf("engine builds = %d, want 2", got)
	}
	if got := probe.countBinds(); got != 1 {
		t.Fatalf("runtime binds = %d, want 1 (OpenRuntime only)", got)
	}

	// The same set with the same revs is a no-op.
	report, err = c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("beta", "r1"),
	})
	if err != nil {
		t.Fatalf("SyncAgents (in sync): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, nil)
	if got := probe.countEngines(); got != 2 {
		t.Fatalf("engine builds after a no-op pass = %d, want 2", got)
	}
}

func TestSyncAgentsUpdateReplacesLiveAgent(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	if _, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")}); err != nil {
		t.Fatalf("SyncAgents (register): %v", err)
	}
	first, ok := rt.Agent("alpha")
	if !ok {
		t.Fatal("alpha is not live after the first pass")
	}

	// The runtime plane reports the replacement as removal + registration.
	subjects := make(chan event.Subject, 4)
	detach, err := rt.Attach(ctx, runtime.PatternAgentLifecycle(), event.SinkFunc(
		func(_ context.Context, envelope event.Envelope) error {
			subjects <- envelope.Subject
			return nil
		}))
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer detach()

	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r2")})
	if err != nil {
		t.Fatalf("SyncAgents (update): %v", err)
	}
	checkAgentsetReport(t, report, nil, []string{"alpha"}, nil)
	second, ok := rt.Agent("alpha")
	if !ok {
		t.Fatal("alpha is not live after the update")
	}
	if first == second {
		t.Fatal("the update kept the old agent instance")
	}
	if got := probe.countEngines(); got != 2 {
		t.Fatalf("engine builds = %d, want 2 (register + replacement)", got)
	}

	var got []event.Subject
	for len(got) < 2 {
		select {
		case subject := <-subjects:
			got = append(got, subject)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for lifecycle events, got %v", got)
		}
	}
	if got[0] != runtime.SubjectAgentRemoved("alpha") ||
		got[1] != runtime.SubjectAgentRegistered("alpha") {
		t.Fatalf("subjects = %v, want [removed registered]", got)
	}

	// The new rev is what the pass recorded: the same set is a no-op.
	report, err = c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r2")})
	if err != nil {
		t.Fatalf("SyncAgents (after update): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, nil)
}

func TestSyncAgentsRemovesUndeclaredAgents(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	if _, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("beta", "r1"),
	}); err != nil {
		t.Fatalf("SyncAgents (register): %v", err)
	}
	kept, ok := rt.Agent("alpha")
	if !ok {
		t.Fatal("alpha is not live")
	}

	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")})
	if err != nil {
		t.Fatalf("SyncAgents (remove): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, []string{"beta"})
	if _, ok := rt.Agent("beta"); ok {
		t.Fatal("beta is still live after being undeclared")
	}
	if live, _ := rt.Agent("alpha"); live != kept {
		t.Fatal("the removal pass re-registered the agent that stayed declared")
	}
	if got := probe.countEngines(); got != 2 {
		t.Fatalf("engine builds = %d, want 2 (no rebuild for the kept agent)", got)
	}
}

func TestSyncAgentsEmptySetRemovesEverything(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	if _, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("beta", "r1"),
	}); err != nil {
		t.Fatalf("SyncAgents (register): %v", err)
	}
	report, err := c.SyncAgents(ctx, DefaultKey, nil)
	if err != nil {
		t.Fatalf("SyncAgents (empty set): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, []string{"alpha", "beta"})
	if names := rt.AgentNames(); len(names) != 0 {
		t.Fatalf("AgentNames = %v, want none", names)
	}

	// The records were applied to the removals: an empty set again has
	// nothing left to remove and reports nothing.
	report, err = c.SyncAgents(ctx, DefaultKey, nil)
	if err != nil {
		t.Fatalf("SyncAgents (empty set, second): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, nil)
	if got := probe.countEngines(); got != 2 {
		t.Fatalf("engine builds = %d, want 2", got)
	}
}

// TestSyncAgentsRemoveTimeoutIsConfigurable covers the bound a pass puts
// on one removal's drain. The default ([DefaultAgentRemoveTimeout]) is
// too long to observe here, so the pass shortens it with
// [WithAgentRemoveTimeout]: the removal runs out of time against a live
// turn, the pass reports it, and the agent stays live with its record
// until a later pass retries.
func TestSyncAgentsRemoveTimeoutIsConfigurable(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)
	release := make(chan struct{})
	probe.parkTurns(release)

	if _, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")}); err != nil {
		t.Fatalf("SyncAgents (register): %v", err)
	}
	lease, err := rt.Sessions().GetOrCreate(
		ctx, session.Key{AgentID: "alpha", ContextID: "conv"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(ctx, agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The set no longer declares alpha and its turn outlives the
	// shortened bound: the declaration fails, so the agent is left in
	// place and the pass reports it instead of removing it half-way.
	start := time.Now()
	report, err := c.SyncAgents(
		ctx, DefaultKey, nil, WithAgentRemoveTimeout(50*time.Millisecond))
	// A pass that ignored the option would still time out, only after
	// DefaultAgentRemoveTimeout: the elapsed time is what proves the
	// bound came from the option.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the pass took %v, want the shortened bound", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SyncAgents (short bound) error = %v, want DeadlineExceeded", err)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("Errors = %v, want one", report.Errors)
	}
	if report.Registered != nil || report.Updated != nil || report.Removed != nil {
		t.Fatalf("report lists = %+v, want none", report)
	}
	if _, ok := rt.Agent("alpha"); !ok {
		t.Fatal("alpha disappeared although its removal timed out")
	}

	// Released, then retried with the default bound: the record survived
	// the failed removal, so the next pass removes the name.
	close(release)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("turn Wait: %v", err)
	}
	report, err = c.SyncAgents(ctx, DefaultKey, nil)
	if err != nil {
		t.Fatalf("SyncAgents (retry): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, []string{"alpha"})
	if _, ok := rt.Agent("alpha"); ok {
		t.Fatal("alpha is still live after the retry")
	}
}

// TestSyncAgentsRejectsInvalidRemoveTimeout pins that the option is
// checked before anything mutates: a bound that is not positive and a
// nil option both reject the call with the zero report.
func TestSyncAgentsRejectsInvalidRemoveTimeout(t *testing.T) {
	t.Parallel()
	c, _ := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)
	decls := []AgentDecl{agentsetDecl("alpha", "r1")}

	for _, tc := range []struct {
		name string
		opts []AgentSyncOption
	}{
		{name: "zero", opts: []AgentSyncOption{WithAgentRemoveTimeout(0)}},
		{name: "negative", opts: []AgentSyncOption{WithAgentRemoveTimeout(-time.Second)}},
		{name: "nil", opts: []AgentSyncOption{nil}},
	} {
		report, err := c.SyncAgents(ctx, DefaultKey, decls, tc.opts...)
		if !errdefs.IsValidation(err) {
			t.Fatalf("%s bound error = %v, want Validation", tc.name, err)
		}
		if len(report.Registered)+len(report.Updated)+len(report.Removed)+len(report.Errors) != 0 {
			t.Fatalf("%s bound report = %+v, want the zero report", tc.name, report)
		}
		if _, ok := rt.Agent("alpha"); ok {
			t.Fatalf("%s bound registered the declaration", tc.name)
		}
	}
}

// TestSyncAgentsUsesDocumentRemoveTimeout covers the runtime document's
// removal bound as the pass's fallback: with no WithAgentRemoveTimeout,
// the pass drains under runtime.agents.remove_timeout instead of the
// craft default.
func TestSyncAgentsUsesDocumentRemoveTimeout(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetRemoveTimeoutDeploy("50ms"))
	ctx := context.Background()
	rt := agentsetRuntime(t, c)
	release := make(chan struct{})
	probe.parkTurns(release)

	if _, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")}); err != nil {
		t.Fatalf("SyncAgents (register): %v", err)
	}
	lease, err := rt.Sessions().GetOrCreate(
		ctx, session.Key{AgentID: "alpha", ContextID: "conv"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(ctx, agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The set no longer declares alpha and its turn outlives the
	// document's bound: the declaration fails, so the agent stays in
	// place and the pass reports it instead of removing it half-way.
	start := time.Now()
	report, err := c.SyncAgents(ctx, DefaultKey, nil)
	// A pass that ignored the document would still time out, only after
	// DefaultAgentRemoveTimeout: the elapsed time is what proves the
	// bound came from the document.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the pass took %v, want the document's shortened bound", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SyncAgents (document bound) error = %v, want DeadlineExceeded", err)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("Errors = %v, want one", report.Errors)
	}
	if _, ok := rt.Agent("alpha"); !ok {
		t.Fatal("alpha disappeared although its removal timed out")
	}

	// Released, then retried: the record survived the failed removal, so
	// the next pass removes the name.
	close(release)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("turn Wait: %v", err)
	}
	report, err = c.SyncAgents(ctx, DefaultKey, nil)
	if err != nil {
		t.Fatalf("SyncAgents (retry): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, []string{"alpha"})
	if _, ok := rt.Agent("alpha"); ok {
		t.Fatal("alpha is still live after the retry")
	}
}

// TestSyncAgentsOptionOverridesDocumentRemoveTimeout pins the order: an
// explicit WithAgentRemoveTimeout replaces the document's bound.
func TestSyncAgentsOptionOverridesDocumentRemoveTimeout(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetRemoveTimeoutDeploy("1h"))
	ctx := context.Background()
	rt := agentsetRuntime(t, c)
	release := make(chan struct{})
	probe.parkTurns(release)

	if _, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")}); err != nil {
		t.Fatalf("SyncAgents (register): %v", err)
	}
	lease, err := rt.Sessions().GetOrCreate(
		ctx, session.Key{AgentID: "alpha", ContextID: "conv"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(ctx, agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The document's hour-long bound cannot be what stopped this drain.
	start := time.Now()
	report, err := c.SyncAgents(
		ctx, DefaultKey, nil, WithAgentRemoveTimeout(50*time.Millisecond))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the pass took %v, want the option's shortened bound", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SyncAgents (option bound) error = %v, want DeadlineExceeded", err)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("Errors = %v, want one", report.Errors)
	}

	close(release)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("turn Wait: %v", err)
	}
	report, err = c.SyncAgents(ctx, DefaultKey, nil)
	if err != nil {
		t.Fatalf("SyncAgents (retry): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, []string{"alpha"})
	if _, ok := rt.Agent("alpha"); ok {
		t.Fatal("alpha is still live after the retry")
	}
}

// TestSyncAgentsRevalidatesRecordsAgainstLiveView pins the rule that a
// record is trusted only while the runtime's live view still has the
// agent: a reopened runtime is a new live view, so a recorded name
// registers again, and a recorded name that is gone is dropped without
// pretending it was removed.
func TestSyncAgentsRevalidatesRecordsAgainstLiveView(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()

	if _, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")}); err != nil {
		t.Fatalf("SyncAgents (register): %v", err)
	}

	reopen := func() {
		t.Helper()
		if err := c.CloseRuntime(ctx, DefaultKey); err != nil {
			t.Fatalf("CloseRuntime: %v", err)
		}
		if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); err != nil {
			t.Fatalf("OpenRuntime: %v", err)
		}
	}

	reopen()
	rt := agentsetRuntime(t, c)
	if _, ok := rt.Agent("alpha"); ok {
		t.Fatal("the reopened runtime still has alpha")
	}
	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")})
	if err != nil {
		t.Fatalf("SyncAgents (after reopen): %v", err)
	}
	checkAgentsetReport(t, report, []string{"alpha"}, nil, nil)
	if got := probe.countEngines(); got != 2 {
		t.Fatalf("engine builds = %d, want 2 (one per runtime)", got)
	}

	reopen()
	report, err = c.SyncAgents(ctx, DefaultKey, nil)
	if err != nil {
		t.Fatalf("SyncAgents (empty set after reopen): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, nil)
}

// TestSyncAgentsReplacesUnrecordedLiveAgent pins the other half of the
// rule: the declaration set is authoritative over a live agent nothing
// recorded, so a direct registration is replaced rather than trusted.
func TestSyncAgentsReplacesUnrecordedLiveAgent(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	direct, err := c.RegisterAgent(ctx, DefaultKey, "alpha", agentsetDecl("alpha", "r1").Def)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")})
	if err != nil {
		t.Fatalf("SyncAgents: %v", err)
	}
	checkAgentsetReport(t, report, nil, []string{"alpha"}, nil)
	live, ok := rt.Agent("alpha")
	if !ok {
		t.Fatal("alpha is not live after the pass")
	}
	if live == direct {
		t.Fatal("the direct registration survived the pass")
	}
	if got := probe.countEngines(); got != 2 {
		t.Fatalf("engine builds = %d, want 2 (direct + replacement)", got)
	}

	report, err = c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")})
	if err != nil {
		t.Fatalf("SyncAgents (in sync): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, nil)
}

func TestSyncAgentsAggregatesFailures(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetDeployedDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)
	built := probe.countEngines() // the deployed agent's engine

	broken := agentsetDecl("broken", "r1")
	broken.Def.Card.Name = "" // fails Definition.Validate inside RegisterAgent

	// Sorted by name the pass sees alpha, bot, broken, zeta: a failure
	// in the middle (bot) or at the end (broken) must not rob the
	// declarations behind it.
	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("bot", "r1"), // deployed: cannot be registered or removed
		broken,
		agentsetDecl("zeta", "r1"),
	})
	if err == nil {
		t.Fatal("SyncAgents error = nil, want the joined per-declaration failures")
	}
	if !slices.Equal(report.Registered, []string{"alpha", "zeta"}) {
		t.Fatalf("Registered = %v, want [alpha zeta]", report.Registered)
	}
	if len(report.Updated) != 0 || len(report.Removed) != 0 {
		t.Fatalf("Updated/Removed = %v/%v, want none", report.Updated, report.Removed)
	}
	if len(report.Errors) != 2 {
		t.Fatalf("Errors = %v, want [bot broken]", report.Errors)
	}
	if !errdefs.IsConflict(report.Errors[0]) {
		t.Fatalf("Errors[0] = %v, want a conflict (the deployed name)", report.Errors[0])
	}
	for _, want := range []string{
		`sync agents "default"`, `agent "bot"`, `agent "broken"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("joined error %q does not mention %q", err, want)
		}
	}
	if _, ok := rt.Agent("bot"); !ok {
		t.Fatal("the deployed agent disappeared")
	}
	if _, ok := rt.Agent("alpha"); !ok {
		t.Fatal("the healthy declaration was not applied")
	}
	if _, ok := rt.Agent("zeta"); !ok {
		t.Fatal("the declaration behind the failures was not applied")
	}
	if got := probe.countEngines(); got != built+2 {
		t.Fatalf("engine builds = %d, want %d", got, built+2)
	}

	// Failures are not recorded: the next pass retries them while the
	// healthy declaration stays in sync.
	report, err = c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("bot", "r1"),
		broken,
		agentsetDecl("zeta", "r1"),
	})
	if err == nil {
		t.Fatal("SyncAgents (retry) error = nil, want the failures again")
	}
	if len(report.Registered) != 0 || len(report.Errors) != 2 {
		t.Fatalf("retry reported Registered = %v, Errors = %v; want none / 2",
			report.Registered, report.Errors)
	}
	if got := probe.countEngines(); got != built+2 {
		t.Fatalf("engine builds after the retry = %d, want %d", got, built+2)
	}
}

// TestSyncAgentsRepairsFailureWithSameRev pins the repair path the
// reference implementation needed for hand-authored declarations: a
// declaration that failed is not recorded, so fixing its definition
// applies on the next pass even when its rev did not change.
func TestSyncAgentsRepairsFailureWithSameRev(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	broken := agentsetDecl("alpha", "r1")
	broken.Def.Card.Name = ""
	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{broken})
	if err == nil {
		t.Fatal("SyncAgents error = nil, want the definition failure")
	}
	if len(report.Errors) != 1 || len(report.Registered) != 0 {
		t.Fatalf("report = %+v, want one error and no registration", report)
	}
	if _, ok := rt.Agent("alpha"); ok {
		t.Fatal("the broken declaration registered anyway")
	}

	report, err = c.SyncAgents(ctx, DefaultKey, []AgentDecl{agentsetDecl("alpha", "r1")})
	if err != nil {
		t.Fatalf("SyncAgents (repaired, same rev): %v", err)
	}
	checkAgentsetReport(t, report, []string{"alpha"}, nil, nil)
	if got := probe.countEngines(); got != 1 {
		t.Fatalf("engine builds = %d, want 1", got)
	}
}

func TestSyncAgentsRejectsMalformedSetsBeforeMutating(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	cases := []struct {
		name  string
		ctx   context.Context
		key   RuntimeKey
		decls []AgentDecl
		want  string
	}{{
		name: "nil context",
		ctx:  nil,
		key:  DefaultKey,
		want: "context is required",
	}, {
		name:  "unknown key",
		ctx:   ctx,
		key:   RuntimeKey("nope"),
		decls: []AgentDecl{agentsetDecl("alpha", "r1")},
		want:  ErrRuntimeNotFound.Error(),
	}, {
		name:  "empty name",
		ctx:   ctx,
		key:   DefaultKey,
		decls: []AgentDecl{{Name: " ", Rev: "r1"}},
		want:  "empty name",
	}, {
		name:  "empty rev",
		ctx:   ctx,
		key:   DefaultKey,
		decls: []AgentDecl{agentsetDecl("alpha", " ")},
		want:  "empty rev",
	}, {
		name: "duplicate name",
		ctx:  ctx,
		key:  DefaultKey,
		decls: []AgentDecl{
			agentsetDecl("alpha", "r1"),
			agentsetDecl("alpha", "r2"),
		},
		want: "duplicate declaration",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := c
			report, err := c.SyncAgents(tc.ctx, tc.key, tc.decls)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SyncAgents error = %v, want %q", err, tc.want)
			}
			if len(report.Registered) != 0 || len(report.Updated) != 0 ||
				len(report.Removed) != 0 || len(report.Errors) != 0 {
				t.Fatalf("report = %+v, want the zero report", report)
			}
		})
	}

	// Nothing was mutated by any rejected call.
	if names := rt.AgentNames(); len(names) != 0 {
		t.Fatalf("AgentNames = %v, want none", names)
	}
	if got := probe.countEngines(); got != 0 {
		t.Fatalf("engine builds = %d, want 0", got)
	}
}

func TestSyncAgentsNeverReloads(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()

	// A reload of this Craft would publish craft.reload.* on the plane
	// every consumer attaches to.
	var mu sync.Mutex
	var subjects []event.Subject
	detach, err := c.Attach(ctx, PatternCraft(), event.SinkFunc(
		func(_ context.Context, envelope event.Envelope) error {
			mu.Lock()
			subjects = append(subjects, envelope.Subject)
			mu.Unlock()
			return nil
		}))
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer detach()

	passes := [][]AgentDecl{
		{agentsetDecl("alpha", "r1")},
		{agentsetDecl("alpha", "r2"), agentsetDecl("beta", "r1")},
		{agentsetDecl("beta", "r1")},
	}
	for i, decls := range passes {
		if _, err := c.SyncAgents(ctx, DefaultKey, decls); err != nil {
			t.Fatalf("SyncAgents pass %d: %v", i, err)
		}
	}

	if got := probe.countBinds(); got != 1 {
		t.Fatalf("runtime binds = %d, want 1 (OpenRuntime only)", got)
	}
	time.Sleep(50 * time.Millisecond) // let any published event be delivered
	mu.Lock()
	got := slices.Clone(subjects)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("craft-plane events during SyncAgents = %v, want none", got)
	}
}

func TestSyncAgentsUsesDynamicCatalogDefault(t *testing.T) {
	t.Parallel()
	c, _ := newAgentsetCraft(t, agentsetCatalogDeploy)
	rt := agentsetRuntime(t, c)

	report, err := c.SyncAgents(context.Background(), DefaultKey,
		[]AgentDecl{agentsetDecl("sub", "r1")})
	if err != nil {
		t.Fatalf("SyncAgents: %v", err)
	}
	checkAgentsetReport(t, report, []string{"sub"}, nil, nil)
	if _, ok := rt.Agent("sub"); !ok {
		t.Fatal("sub is not live")
	}
}

func TestSyncAgentsWithoutCatalogDefaultFailsPerName(t *testing.T) {
	t.Parallel()
	c, _ := newAgentsetCraft(t, agentsetCatalogNoDefaultDeploy)
	rt := agentsetRuntime(t, c)

	report, err := c.SyncAgents(context.Background(), DefaultKey,
		[]AgentDecl{agentsetDecl("sub", "r1")})
	if err == nil {
		t.Fatal("SyncAgents error = nil, want the missing-assembly failure")
	}
	if len(report.Errors) != 1 ||
		!strings.Contains(report.Errors[0].Error(), "WithToolAssembly") {
		t.Fatalf("Errors = %v, want one missing-assembly error", report.Errors)
	}
	if _, ok := rt.Agent("sub"); ok {
		t.Fatal("sub registered without an assembly")
	}
}

// TestSyncAgentsAbsorbsReloadRebind is the migration regression for the
// reference implementation's LoadAll / LoadMissing / AdoptKnown trio:
// cold start registers everything, an in-place reload re-binds the
// dynamic agents by itself, and the pass after the reload has nothing
// to replay because the records live on the Craft, not on a generation.
func TestSyncAgentsAbsorbsReloadRebind(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()
	rt := agentsetRuntime(t, c)

	report, err := c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("beta", "r1"),
	})
	if err != nil {
		t.Fatalf("SyncAgents (cold start): %v", err)
	}
	checkAgentsetReport(t, report, []string{"alpha", "beta"}, nil, nil)
	cold := probe.countEngines()
	before, ok := rt.Agent("alpha")
	if !ok {
		t.Fatal("alpha is not live")
	}

	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonPlugin); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	rebound := probe.countEngines()
	if rebound <= cold {
		t.Fatalf("engine builds across the reload = %d, want more than %d "+
			"(the reload re-binds dynamic agents)", rebound, cold)
	}
	after, ok := rt.Agent("alpha")
	if !ok {
		t.Fatal("alpha did not survive the reload")
	}
	if after == before {
		t.Fatal("the reload did not re-bind alpha")
	}

	// The LoadMissing pass: same set, same revs, nothing to do.
	report, err = c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("beta", "r1"),
	})
	if err != nil {
		t.Fatalf("SyncAgents (after reload): %v", err)
	}
	checkAgentsetReport(t, report, nil, nil, nil)
	if got := probe.countEngines(); got != rebound {
		t.Fatalf("engine builds after the post-reload pass = %d, want %d", got, rebound)
	}

	// A declaration that appears after the reload is the only work left.
	report, err = c.SyncAgents(ctx, DefaultKey, []AgentDecl{
		agentsetDecl("alpha", "r1"),
		agentsetDecl("beta", "r1"),
		agentsetDecl("gamma", "r1"),
	})
	if err != nil {
		t.Fatalf("SyncAgents (new declaration): %v", err)
	}
	checkAgentsetReport(t, report, []string{"gamma"}, nil, nil)
	if got := probe.countEngines(); got != rebound+1 {
		t.Fatalf("engine builds after the new declaration = %d, want %d", got, rebound+1)
	}
}

func TestSyncAgentsSerializesWithReload(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()

	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	probe.parkNextBind(block, entered)

	reloadErr := make(chan error, 1)
	go func() { reloadErr <- c.ReloadRuntime(ctx, DefaultKey, ReasonManual) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reload never reached its parked binder")
	}

	type result struct {
		report AgentSyncReport
		err    error
	}
	done := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		report, err := c.SyncAgents(ctx, DefaultKey,
			[]AgentDecl{agentsetDecl("alpha", "r1")})
		done <- result{report, err}
	}()
	<-started

	// The reload is parked inside the lifecycle lock: a pass started
	// meanwhile must not slip a registration in while it waits.
	time.Sleep(100 * time.Millisecond)
	if got := probe.countEngines(); got != 0 {
		t.Fatalf("engine builds while the reload held the lifecycle lock = %d, want 0", got)
	}

	close(block)
	if err := <-reloadErr; err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("SyncAgents: %v", got.err)
	}
	checkAgentsetReport(t, got.report, []string{"alpha"}, nil, nil)

	// And it ran after the reload: the parked binder precedes the
	// registration in the fixture's observation order.
	order := probe.events()
	lastBind := lastIndex(order, "bind:"+string(DefaultKey))
	register := lastIndex(order, "engine")
	if lastBind < 0 || register < 0 || lastBind > register {
		t.Fatalf("observation order = %v, want the reload's bind before the register", order)
	}
}

// lastIndex returns the last position of value in list, or -1.
func lastIndex(list []string, value string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == value {
			return i
		}
	}
	return -1
}

func TestSyncAgentsConcurrentPassesAgree(t *testing.T) {
	t.Parallel()
	c, probe := newAgentsetCraft(t, agentsetBaseDeploy)
	ctx := context.Background()

	const passes = 4
	reports := make([]AgentSyncReport, passes)
	errs := make([]error, passes)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range passes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			reports[i], errs[i] = c.SyncAgents(ctx, DefaultKey,
				[]AgentDecl{agentsetDecl("alpha", "r1")})
		}(i)
	}
	close(start)
	wg.Wait()

	registered := 0
	for i := range reports {
		if errs[i] != nil {
			t.Fatalf("pass %d: %v", i, errs[i])
		}
		if len(reports[i].Errors) != 0 || len(reports[i].Updated) != 0 ||
			len(reports[i].Removed) != 0 {
			t.Fatalf("pass %d report = %+v, want a clean pass", i, reports[i])
		}
		registered += len(reports[i].Registered)
	}
	if registered != 1 {
		t.Fatalf("registrations across %d concurrent passes = %d, want exactly 1",
			passes, registered)
	}
	if got := probe.countEngines(); got != 1 {
		t.Fatalf("engine builds = %d, want 1", got)
	}
}
