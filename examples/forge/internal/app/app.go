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
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/starlark"
	host "github.com/GizClaw/flowcraft/core/agent-host"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"

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
	mgr    *host.Manager
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
	return open(ctx, dir, drivers())
}

// open is Open with the driver table supplied: the production table is the one
// this binary compiles in, and a test can hand in a stub provider instead of
// reaching a network.
func open(ctx context.Context, dir string, table []runtime.Driver) (*App, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace %q: %w", dir, err)
	}
	doc, body, err := loadDocument(abs)
	if err != nil {
		return nil, err
	}
	if missing, err := requiredEnvironment(doc, abs); err != nil {
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
	sim := simtools.New()
	a := &App{dir: abs, store: store, sim: sim, infos: make(map[string]Info, len(doc.Agents)), cast: cast,
		seats: make(chan SeatEvent, 256)}
	mgr := host.New(graphKinds(abs), host.Bindings{
		Store:          store,
		RuntimeOptions: []runtime.Option{runtime.WithDrivers(table...), runtime.WithToolSource(sim)},
		LoadOptions:    workspaceOptions(abs),
		Harness:        "examples/forge",
		// The seats' events do not reach the turn's iterator: a delegated child
		// runs on its own goroutine. The host is told directly, which is how a
		// surface shows a seat speaking while the referee waits for it.
		OnEvent: a.forwardSeat,
	})
	specs, err := mgr.Loader().Load(ctx, body)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if _, err := mgr.Deploy(ctx, specs...); err != nil {
		_ = store.Close()
		return nil, err
	}
	a.mgr = mgr
	for _, spec := range specs {
		a.agents = append(a.agents, spec.Name)
		info := describe(abs, spec, sim)
		// The cast says who speaks and under what name; the deployment only says
		// which agents exist.
		info.Label = cast.Label(spec.Name)
		info.Director = cast.Director == spec.Name
		a.infos[spec.Name] = info
	}
	return a, nil
}

// graphKinds is the deployment's kind table: the built-in engines, with the graph
// engine extended by the node kinds a rule script needs.
//
// The script kind is built per runtime, because a tool call it makes goes through
// the runtime's own dispatch chain; the source is rooted at the workspace, so a
// scenario writes `rules/vote_tally.star` and means the file beside it.
func graphKinds(workspace string) deploy.Kinds {
	return deploy.BuiltinKinds().
		// The one hook the demo ships: a seat's own memory, kept in the workspace.
		WithHooks(memory.Kind()).
		WithEngines(deploy.GraphEngine(
			nodes.WithRuntimeKinds(func(rt *runtime.Runtime) ([]graph.NodeKind, error) {
				kind, err := script.New(
					script.WithBackends(starlark.New()),
					script.WithRuntime(rt),
					script.WithSource(&script.FileSource{BaseDir: workspace, TTL: time.Second}),
				)
				if err != nil {
					return nil, err
				}
				return []graph.NodeKind{kind}, nil
			}),
		))
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
		errs = append(errs, a.mgr.Close())
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
	session, ok := a.mgr.Session(name, sessionFor(name))
	if !ok {
		return nil, fmt.Errorf("agent %q is not deployed; have %s", name, strings.Join(a.agents, ", "))
	}
	return session.Run(ctx, message.NewTextMessage(message.RoleUser, text)), nil
}

// RunTurn consumes one turn, handing every event to observe, and returns the
// snapshot the run produced.
//
// A domain failure (a failed graph, an engine error) is reported as a Result
// whose State is not completed, with the stream error returned alongside; a
// caller that ignored the Result would report a failed turn as a successful one,
// so [Failure] checks both.
func (a *App) RunTurn(
	ctx context.Context,
	name, text string,
	observe func(agent.Event),
) (agent.Result, error) {
	events, err := a.Stream(ctx, name, text)
	if err != nil {
		return agent.Result{}, err
	}
	var runErr error
	for event, err := range events {
		if err != nil {
			runErr = err
			break
		}
		if observe != nil {
			observe(event)
		}
	}
	session, ok := a.mgr.Session(name, sessionFor(name))
	if !ok {
		return agent.Result{}, fmt.Errorf("agent %q is not deployed", name)
	}
	result, err := session.Result(ctx)
	if runErr != nil {
		return result, runErr
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// Failure turns a turn's outcome into an error: the stream error if there was
// one, otherwise a non-terminal or failed state.
func Failure(result agent.Result, err error) error {
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
func Inspect(dir string) ([]Info, []string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve workspace %q: %w", dir, err)
	}
	doc, _, err := loadDocument(abs)
	if err != nil {
		return nil, nil, err
	}
	cast, err := story.Load(abs)
	if err != nil {
		return nil, nil, err
	}
	infos := make([]Info, 0, len(doc.Agents))
	for _, agent := range doc.Agents {
		info := Info{
			Workspace: abs,
			Agent:     agent.Name,
			Label:     cast.Label(agent.Name),
			Director:  cast.Director == agent.Name,
			Engine:    agent.Engine.Kind,
			Graph:     expandWorkspace(enginePath(agent.Engine.Settings), abs),
			Session:   SessionID,
		}
		if agent.Runtime != nil && len(agent.Runtime.Layers) > 0 {
			info.Runtime = filepath.Base(agent.Runtime.Layers[0].Path)
		}
		info.Models, info.Tools = layerFacts(abs, agent.Runtime)
		if agent.Delegation != nil {
			info.Delegates = append(info.Delegates, agent.Delegation.Targets...)
		}
		for _, hook := range agent.Hooks {
			info.Hooks = append(info.Hooks, hook.Kind)
		}
		infos = append(infos, info)
	}
	missing, err := requiredEnvironment(doc, abs)
	if err != nil {
		return nil, nil, err
	}
	return infos, missing, nil
}

// openStore prepares the workspace's durable side. Sessions are keyed by agent
// and session id alone, which is why each workspace owns one store.
func openStore(dir string) (*checkpoint.SQLite, error) {
	path := filepath.Join(dir, ".forge")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace state dir: %w", err)
	}
	store, err := checkpoint.Open(filepath.Join(path, "sessions.db"))
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	return store, nil
}

// describe projects one spec onto what a person reads: which agent, which
// engine, which models the deployment may route to, and which tools it wires.
func describe(dir string, spec deploy.Spec, sim *simtools.Source) Info {
	info := Info{
		Workspace: dir,
		Agent:     spec.Name,
		Runtime:   spec.Runtime.Name,
		Engine:    spec.Engine.Kind,
		Session:   SessionID,
	}
	for _, model := range spec.Runtime.Inference.Models {
		info.Models = append(info.Models, model.Provider+":"+model.Model)
	}
	info.Graph = expandWorkspace(enginePath(spec.Engine.Settings), dir)
	if spec.Delegation != nil {
		info.Delegates = append(info.Delegates, spec.Delegation.Targets...)
	}
	for _, hook := range spec.Hooks {
		info.Hooks = append(info.Hooks, hook.Kind)
	}
	if len(spec.Runtime.Tools.Enable) == 0 {
		info.Tools = append(info.Tools, "files", "exec", "skills")
	} else {
		info.Tools = append(info.Tools, spec.Runtime.Tools.Enable...)
	}
	if sim != nil {
		info.Tools = append(info.Tools, sim.Names()...)
	}
	return info
}
