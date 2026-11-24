// Package app assembles one runnable forge application from a workspace
// directory: it loads the native deployment document, binds the driver table and
// the application-owned tool source, deploys the agents into a host manager, and
// drives one session through the run iterator. It owns no CLI, UI or scenario
// concern.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/agent/checkpoint"
	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/host"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/GizClaw/flowcraft/core/tool/projection"
	"github.com/GizClaw/flowcraft/tool/builtin/delegation"
	tooldispatch "github.com/GizClaw/flowcraft/tool/dispatch"

	"github.com/GizClaw/flowcraft/examples/forge/internal/hooks/memory"
	"github.com/GizClaw/flowcraft/examples/forge/internal/simtools"
	"github.com/GizClaw/flowcraft/examples/forge/internal/story"
)

// SessionID is the session forge drives inside one workspace. State lives in the
// workspace's store, so reopening the same workspace resumes the conversation.
const SessionID = "main"

// sessionFor names the session one agent's conversation lives in.
//
// A checkpoint is looked up by session, and the store is shared by the whole
// workspace, so two agents driven directly with the same session would resume each
// other's tasks: the referee would find its state seeded from a seat's turn. The
// agent name is the qualifier because the agent is what a session belongs to.
func sessionFor(name string) string {
	if name == "" {
		return SessionID
	}
	return name + "/" + SessionID
}

// App owns one deployed workspace.
//
// It holds a manager, not a runtime: the runtime of each agent belongs to the
// deployment the manager built, and the manager is what closes it.
// SeatEvent is one event of an agent the workspace does not drive directly: a seat
// reached through delegation, or any agent beside the primary one.
//
// A seat runs on its own goroutine inside the delegating tool call, so its events
// never appear on the turn's iterator. This is the second stream a surface reads.
type SeatEvent struct {
	// Agent is the deployment name, Label its user-facing name.
	Agent string
	Label string
	// Phase is the game phase in force when the seat spoke: the last state the
	// delegated graph reported. A seat speaks inside the step after the rule that
	// chose the phase, so the latest report is the phase that seat acted in.
	Phase string
	Event agent.Event
}

type App struct {
	dir    string
	cast   story.Cast
	seats  chan SeatEvent
	mu     sync.Mutex
	phase  string
	priv   map[string]bool
	res    *host.Resource
	mgr    *host.Agents
	store  *checkpoint.SQLite
	sim    *simtools.Source
	agents []string
	infos  map[string]Info
}

// Info is the small metadata read out of the deployment for inspection and TUI
// display.
type Info struct {
	Workspace string
	Agent     string
	// Label is the user-facing name the workspace's cast gives this agent.
	Label string
	// Director reports that the agent plans or referees: it does not speak, and a
	// surface has no label to show for it.
	Director bool
	// Graph is the graph document the engine runs, from the engine's settings.
	Graph   string
	Runtime string
	Engine  string
	Session string
	Models  []string
	Tools   []string
	// Delegates are the agents this one may reach through the delegate tool.
	Delegates []string
	// Hooks are the hook kinds attached to the agent, in declaration order.
	Hooks []string
}

// Open builds every agent a workspace's deploy.yaml declares.
//
// The document is read twice on purpose: once undecoded of placeholders, so a
// missing environment variable is reported as a list instead of as the first
// failure expansion hits, and once through the loader, which resolves kinds and
// merges the runtime layers.
func Open(ctx context.Context, dir string) (*App, error) {
	sim := simtools.New()
	return open(ctx, dir, implsWith(sim), sim)
}

// open is Open with the implementation table supplied: the production table is the one this binary
// compiles in, and a test can hand in a stub provider instead of reaching a network.
//
// The simulated source is passed in rather than built here because the app reports what it executed: a
// table that built its own instance would leave the counter watching a different one.
func open(ctx context.Context, dir string, impls *deploy.Impls, sim *simtools.Source) (*App, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace %q: %w", dir, err)
	}
	if missing, err := requiredEnvironment(ctx, abs); err != nil {
		return nil, err
	} else if len(missing) > 0 {
		return nil, fmt.Errorf(
			"workspace %s needs environment variables: %s (add them to .env)",
			abs, strings.Join(missing, ", "))
	}
	store, err := openStore(abs)
	if err != nil {
		return nil, err
	}
	cast, err := story.Load(abs)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	// The process first: what this machine offers, then the agents that select from it. The
	// capabilities come from the built resource because a provider's settings are opaque to the
	// document layer.
	options := workspaceOptions(abs)
	plan, err := process(ctx, impls, abs, options)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	res, err := host.Open(ctx, plan, host.Options{
		Capabilities: capabilities(sim),
		Dispatcher:   dispatcher,
		View:         view,
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	specs, err := deployments(ctx, impls, abs, options, res.Capabilities())
	if err != nil {
		_ = res.Close()
		_ = store.Close()
		return nil, err
	}
	a := &App{dir: abs, store: store, sim: sim, res: res, cast: cast,
		seats: make(chan SeatEvent, 256), infos: make(map[string]Info, len(specs))}
	// The store is the host's, not a decoration around it: the turns it writes are what the next turn
	// continues from, and the fingerprint is what stops a checkpoint written by a different deployment
	// set from being resumed as if it were this one.
	agents := host.NewAgents(res,
		host.WithStore(store),
		host.WithFingerprint(deploy.Fingerprint(specs)),
		host.WithHarness(forgeHarness))
	if err := agents.Add(specs...); err != nil {
		_ = res.Close()
		_ = store.Close()
		return nil, err
	}
	a.mgr = agents
	for _, spec := range specs {
		a.agents = append(a.agents, spec.Name)
		info := describe(abs, spec, sim, a.res.Capabilities().ModelIDs())
		info.Label = cast.Label(spec.Name)
		info.Director = cast.Director == spec.Name
		a.infos[spec.Name] = info
	}
	return a, nil
}

// capabilities is what this process can hand a tool: the simulated set is application-owned, and the
// file boundary and process surface are filled per session by the host.
func capabilities(sim *simtools.Source) *tool.CapSet {
	set := tool.NewCapSet()
	// Declared, not filled: the host fills each of these per session, from the authority the document's
	// grant selected. Declaring one here is what lets a tool that needs it be registered at all, and a
	// process that declares nothing refuses every such tool at assembly rather than at the first call.
	platform.TreeSlot.Provide(set, nil)
	platform.ProcessSlot.Provide(set, nil)
	platform.SkillsSlot.Provide(set, nil)
	platform.DelegationSlot.Provide(set, nil)
	return set
}

// dispatcher and view are the engine halves the host is opened with: the application picks them, which
// is what keeps core free of both.
func dispatcher(catalog tool.Catalog, chain []tool.Middleware) tool.Dispatcher {
	return tooldispatch.New(catalog, chain...)
}

func view(catalog tool.Catalog, allowed policy.ToolSet) tool.View {
	projected, err := projection.New(catalog, projection.For(allowed))
	if err != nil {
		// A policy the projection refuses is a document error the loader reports; reaching it here
		// means an authority this process cannot project, which is not a recoverable state.
		return nil
	}
	return projected.NewView()
}

// impls is the table this binary compiles in: the providers it carries, the process tool sources it
// wires, the middleware chain it offers, the hook kinds it ships, and the graph engine.
func impls() *deploy.Impls { return implsWith(simtools.New()) }

// implsWith is the same table over one simulated source, so the caller that counts executions and the
// tool that runs are the same object.
func implsWith(sim *simtools.Source) *deploy.Impls {
	registered := deploy.NewImpls()
	registered = providers()(registered)
	registered = sandboxBackends(registered)
	registered = registered.
		ToolSource(deploy.Impl("sim", func(struct{}) (tool.Source, error) { return sim, nil })).
		ToolSource(deploy.Impl("files", builtinFiles)).
		ToolSource(deploy.Impl("exec", builtinExec)).
		// The delegate tool is a source like any other; a session whose grant names no delegation policy
		// is not handed the capability, so it is the grant that decides, not this table.
		ToolSource(deploy.Impl("delegation", func(struct{}) (tool.Source, error) { return delegation.New(), nil })).
		Hook(memory.Impl()).
		Program(deploy.Impl("graph", graphProgram))
	registered = chainLinks(registered)
	return registered
}

// Close shuts the deployments down, then the store: a session that outlives its
// engine writes into a corpse, and a store that outlives its sessions drops the
// last events.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	if a.mgr != nil {
		errs = append(errs, a.mgr.Close(context.WithoutCancel(context.Background())))
	}
	if a.res != nil {
		errs = append(errs, a.res.Close())
	}
	if a.store != nil {
		errs = append(errs, a.store.Close())
	}
	return errors.Join(errs...)
}

// Events is the feed of the agents beside the primary one. It is buffered and
// non-blocking on the producing side: an observer that stops reading costs dropped
// seat lines, never a stalled run.
func (a *App) Events() <-chan SeatEvent {
	if a == nil {
		return nil
	}
	return a.seats
}

// forwardSeat copies one agent's events onto the feed, skipping the primary agent:
// its stream is the turn's iterator, and showing it twice would double every line.
// It runs on the producing agent's run goroutine, so it never blocks.
func (a *App) forwardSeat(agentName, sessionID string, event agent.Event) {
	if a == nil || len(a.agents) == 0 {
		return
	}
	// The primary agent's stream reaches the turn's iterator, so it is not
	// forwarded; its state reports still tell this app which phase the seats are
	// speaking in.
	a.observeState(event)
	if agentName == a.agents[0] {
		return
	}
	label := a.cast.Label(agentName)
	select {
	case a.seats <- SeatEvent{Agent: agentName, Label: label, Phase: a.Phase(), Event: event}:
	default:
	}
}

// observeState folds a graph's state report into the phase a surface labels a seat's
// line with. The rules declare which phases are private, so the app holds no game
// knowledge of its own.
func (a *App) observeState(event agent.Event) {
	extension, ok := event.(agent.EventExtension[graph.GraphState])
	if !ok {
		return
	}
	raw, ok := extension.Value.Values["game"]
	if !ok {
		return
	}
	var game struct {
		Phase   string   `json:"phase"`
		Private []string `json:"private_phases"`
	}
	if err := json.Unmarshal(raw, &game); err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if game.Phase != "" {
		a.phase = game.Phase
	}
	if game.Private != nil {
		a.priv = make(map[string]bool, len(game.Private))
		for _, phase := range game.Private {
			a.priv[phase] = true
		}
	}
}

// Phase is the last game phase the graph reported.
func (a *App) Phase() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.phase
}

// Private reports whether a phase is one only the role acting in it may hear.
func (a *App) Private(phase string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.priv[phase]
}

// Dir is the absolute workspace directory.
func (a *App) Dir() string {
	if a == nil {
		return ""
	}
	return a.dir
}

// Agents lists the deployed agent names in document order.
func (a *App) Agents() []string {
	if a == nil {
		return nil
	}
	return append([]string(nil), a.agents...)
}

// enginePath is the document an engine's settings name, when it names one: the
// graph a graph engine runs is the fact a reader asks for first.
func enginePath(settings json.RawMessage) string {
	var value struct {
		Path string `json:"path"`
	}
	if len(settings) == 0 || json.Unmarshal(settings, &value) != nil {
		return ""
	}
	return value.Path
}

// Silent reports whether one of an agent's graph nodes speaks for the engine rather
// than for the reader: a scheduler's control line, a judge's verdict.
func (a *App) Silent(node string) bool {
	if a == nil {
		return false
	}
	return a.cast.Silent(node)
}

// Info returns the metadata of the first agent: the one a single-agent workspace
// and the TUI address.
func (a *App) Info() Info {
	if a == nil || len(a.agents) == 0 {
		return Info{}
	}
	return a.infos[a.agents[0]]
}

// InfoFor returns one agent's metadata.
func (a *App) InfoFor(name string) Info {
	if a == nil {
		return Info{}
	}
	return a.infos[name]
}

// Stream starts one turn and returns its event sequence. Nothing runs until the
// sequence is consumed, the first item of a busy session is
// [agent.ErrSessionBusy], and a consumer that stops early cancels the run.
func (a *App) Stream(ctx context.Context, name, text string) (iter.Seq2[agent.Event, error], error) {
	if a == nil || a.mgr == nil {
		return nil, errors.New("forge app is not open")
	}
	id := sessionFor(name)
	// A session is opened on first use and kept: the store makes the conversation durable, and the host
	// refuses a second turn while one is in flight, which is what a busy session should report.
	if _, ok := a.mgr.Session(id); !ok {
		if _, err := a.mgr.Open(ctx, id, name); err != nil {
			return nil, fmt.Errorf("agent %q: %w; have %s", name, err, strings.Join(a.agents, ", "))
		}
	}
	return a.mgr.Run(ctx, id, message.NewTextMessage(message.RoleUser, text)), nil
}

// Snapshot is what one turn reached, folded from its events: the host owns the durable projection, and
// this is the part a surface shows.
type Snapshot struct {
	State   agent.TaskState
	Content message.Content
	// Question is what a paused run asked.
	Question *message.Content
}

// RunTurn consumes one turn, handing every event to observe, and returns what it reached.
//
// A domain failure (a failed graph, an engine error) ends the stream with an error and a non-completed
// state; a caller that ignored one of the two would report a failed turn as a successful one, so
// [Failure] checks both.
func (a *App) RunTurn(
	ctx context.Context,
	name, text string,
	observe func(agent.Event),
) (Snapshot, error) {
	events, err := a.Stream(ctx, name, text)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	var runErr error
	for event, err := range events {
		if err != nil {
			runErr = err
			break
		}
		switch typed := event.(type) {
		case agent.Text:
			snapshot.Content = appendText(snapshot.Content, typed.Text)
		case agent.Status:
			if typed.State.Terminal() || typed.State == agent.StateInputRequired {
				snapshot.State = typed.State
			}
			if typed.State == agent.StateInputRequired && typed.Reason != "" {
				question := message.NewTextContent(typed.Reason)
				snapshot.Question = &question
			}
		}
		if observe != nil {
			observe(event)
		}
	}
	return snapshot, runErr
}

// appendText adds one delta to a turn's answer.
func appendText(content message.Content, text string) message.Content {
	if text == "" {
		return content
	}
	if said := content.Text(); said != "" {
		return message.NewTextContent(said + text)
	}
	return message.NewTextContent(text)
}

// Failure turns a turn's outcome into an error: the stream error if there was
// one, otherwise a non-terminal or failed state.
func Failure(result Snapshot, err error) error {
	if err != nil {
		return withCause(err)
	}
	if result.State != agent.StateCompleted {
		return fmt.Errorf("turn ended in state %q", result.State)
	}
	return nil
}

// withCause appends the cause an inference error keeps out of its own message.
//
// The contract renders a redacted form ("provider_failure") on purpose: the
// wrapped cause may carry a provider payload, a prompt or a credential, and an
// error that travels to a remote client must not. A forge transcript is a local
// file written by the person who supplied the credential, which is exactly where
// the cause belongs — without it a failed turn is unactionable.
func withCause(err error) error {
	var failure *inference.Error
	if !errors.As(err, &failure) {
		return err
	}
	cause := failure.Unwrap()
	if cause == nil {
		return err
	}
	return fmt.Errorf("%w: %v", err, cause)
}

// Inspect reads workspace metadata without building anything and without
// requiring a credential: it decodes the document, names the agents, and reports
// the environment the runtime layers still need.
// Inspect describes a workspace without building it: the agents its document names, and what the
// process beside it declares. A workspace whose key is unset is exactly the one an operator inspects, so
// this reads structure and never expands a placeholder it cannot resolve.
func Inspect(dir string) ([]Info, []string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	names := deployAgents(abs)
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("%s: no agents declared", filepath.Join(abs, agentsFile))
	}
	models, tools := processFacts(abs)
	missing, err := requiredEnvironment(context.WithoutCancel(context.Background()), abs)
	if err != nil {
		return nil, nil, err
	}
	cast, _ := story.Load(abs)
	facts := agentsFacts(abs)
	infos := make([]Info, 0, len(names))
	for i, name := range names {
		info := Info{
			Workspace: abs,
			Agent:     name,
			Session:   SessionID,
			Models:    models,
			Tools:     tools,
		}
		if i < len(facts) {
			info.Graph = facts[i].Graph
			info.Delegates = facts[i].Delegates
			info.Hooks = facts[i].Hooks
		}
		info.Label = cast.Label(name)
		info.Director = cast.Director == name
		infos = append(infos, info)
	}
	return infos, missing, nil
}

func openStore(dir string) (*checkpoint.SQLite, error) {
	path := filepath.Join(dir, ".forge")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace state dir: %w", err)
	}
	store, err := checkpoint.Open(filepath.Join(path, "sessions.db"),
		// A Forge workspace is a local development store, not a durable host: a
		// power loss can lose the last commits, which the event log replays.
		checkpoint.WithSynchronous(checkpoint.SynchronousNormal))
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	return store, nil
}

// describe projects one spec onto what a person reads: which agent, which
// engine, which models the deployment may route to, and which tools it wires.
func describe(dir string, spec deploy.Deployment, sim *simtools.Source, models []model.ModelID) Info {
	info := Info{
		Workspace: dir,
		Agent:     spec.Name,
		Engine:    spec.Program.Impl,
		Session:   SessionID,
	}
	for _, id := range models {
		info.Models = append(info.Models, id.Provider+":"+id.Name)
	}
	if delegation := spec.Grant.Delegation; delegation != nil {
		info.Delegates = append(info.Delegates, delegation.Targets...)
	}
	for _, hook := range spec.Hooks {
		info.Hooks = append(info.Hooks, hook.Impl)
	}
	if allowed := spec.Grant.Tools.Allow; len(allowed) > 0 {
		info.Tools = append(info.Tools, allowed...)
	} else {
		// Nothing is written down, so everything the process mounted is callable: a source is named as
		// itself, and the simulated tools by their own names, which is what a `sim` source actually is.
		for _, source := range processSources(dir) {
			if source == "sim" {
				continue
			}
			info.Tools = append(info.Tools, source)
		}
	}
	if sim != nil {
		info.Tools = append(info.Tools, sim.Names()...)
	}
	return info
}
