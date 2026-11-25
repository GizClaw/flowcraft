// Package stack assembles the whole workspace the way a host assembles it: documents
// through the real loader, the real local sandbox, the real file tools, the real tool
// chain, the real graph engine and a real SQLite checkpoint.
//
// It exists because the packages under test/ would otherwise each carry their own copy of
// four hundred lines of wiring, and the wiring is the thing under test: a document field
// the host stopped reading, a group the implementation table lost, a setting renamed in
// the fixture and nowhere else. One assembler means one place to fix and one place to
// look when a stack test fails.
//
// The one fake is the model provider. A vendor driver would make every test here depend on
// the network and a credential, and neither is what these tests verify.
package stack

import (
	"context"
	"embed"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/agent/checkpoint"
	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/host"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/GizClaw/flowcraft/core/tool/projection"
	"github.com/GizClaw/flowcraft/sandbox/local"
	"github.com/GizClaw/flowcraft/tool/builtin/files"
	tooldispatch "github.com/GizClaw/flowcraft/tool/dispatch"
)

// documents is the fixture set, embedded rather than read from disk: a stack opened from
// another package's test has that package's working directory, and a relative path that
// resolves from here but not from there is a fixture that fails in the second caller.
//
//go:embed testdata/*.yaml
var documents embed.FS

// The documents are rebased onto the stack's temporary directory: a grant names the
// subtrees it confines, and a fixture that hard-coded a path would either write outside the
// test's own directory or depend on one.
const (
	workspacePlaceholder = "<workspace>"
	graphPlaceholder     = "<graph>"
)

// The agent and session every stack serves. They are fixed rather than parameters because
// a test that renamed them would be testing the fixture.
const (
	AgentName   = "writer"
	SessionID   = "s1"
	ModelRef    = "fake/fake-1"
	providerKey = "fake"
	modelName   = "fake-1"
)

// Authority is what the stack lets a session do.
//
// It stands where a real application's own policy would: a backend this process configures, the
// model the fake provider declares, and the one tool the workspace writes through. The write set
// is deliberately unconstrained, because the local backend has no OS boundary to enforce one.
func Authority() policy.Grant {
	return policy.Grant{
		Sandbox: "local",
		Models:  []policy.ModelGrant{{ModelRef: model.ModelRef{ID: model.ModelID{Provider: providerKey, Name: modelName}}}},
		Tools:   policy.ToolSet{Allow: []string{"write_file"}},
	}
}

// Reply is one scripted model call.
type Reply struct {
	// ToolCall makes this call a request for a tool instead of an answer.
	ToolCall *message.ToolCall
	// Text is the assistant's answer.
	Text string
	// Err fails the call. A test that injects a model failure sets this and expects the
	// turn to end in a classified error rather than a completed state.
	Err error
}

// Stack is one assembled process, with everything a test needs to drive it and to look at
// what it did.
type Stack struct {
	// Workspace is the temporary directory the documents were rebased onto, and the only
	// place the file tools may write.
	Workspace string
	// Model is the scripted provider, and the record of what it was asked.
	Model *Model
	// Store is the SQLite checkpoint file both the first process and every restart use.
	Store *checkpoint.SQLite
	// Agents is the process currently serving the stack.
	Agents *host.Agents

	plan        deploy.ProcessPlan
	deployments []deploy.Deployment
	fingerprint agent.Fingerprint
}

// Open assembles one process from the shipped documents.
//
// The replies are consumed in order and the last one repeats, so "always answer text" is
// one Reply and "ask for a tool, then answer" is two. A stack with no replies answers
// every call with "ok".
func Open(t *testing.T, replies ...Reply) *Stack {
	t.Helper()
	if len(replies) == 0 {
		replies = []Reply{{Text: "ok"}}
	}
	workspace := t.TempDir()
	// The graph document is compiled from a path, so it is written into the workspace: the
	// loader, the engine and the process document all resolve it the way a deployment does.
	graphPath := filepath.Join(workspace, "graph.yaml")
	if err := os.WriteFile(graphPath, rebase(t, "graph.yaml", workspace, graphPath), 0o644); err != nil {
		t.Fatalf("stack: write the graph document: %v", err)
	}

	s := &Stack{Workspace: workspace, Model: &Model{replies: replies}}
	s.plan, s.deployments = s.load(t, graphPath)

	store, err := checkpoint.Open(filepath.Join(workspace, "state.db"))
	if err != nil {
		t.Fatalf("stack: open the checkpoint store: %v", err)
	}
	s.Store = store
	t.Cleanup(func() { _ = s.Store.Close() })
	s.fingerprint = deploy.Fingerprint(s.deployments)
	s.start(t)
	return s
}

// load resolves both documents through the real loader and the real factory table.
func (s *Stack) load(t *testing.T, graphPath string) (deploy.ProcessPlan, []deploy.Deployment) {
	t.Helper()
	loader := deploy.NewLoader(impls(s.Model))
	process := sources.Bytes("deploy.yaml", rebase(t, "deploy.yaml", s.Workspace, graphPath))

	plan, err := loader.Process(t.Context(), process)
	if err != nil {
		t.Fatalf("stack: resolve the process document: %v", err)
	}
	deployments, err := loader.Deployments(t.Context(),
		sources.Bytes("agent.yaml", rebase(t, "agent.yaml", s.Workspace, graphPath)))
	if err != nil {
		t.Fatalf("stack: resolve the agent document: %v", err)
	}
	return plan, deployments
}

// start opens one process over the store and installs the deployment into it.
func (s *Stack) start(t *testing.T) {
	t.Helper()
	res, err := host.Open(t.Context(), s.plan, host.Options{
		// The slots are declared and left empty: the host fills the session's own tree and
		// process into them when it opens one, which is the seam the real file tools read.
		Capabilities: capabilities(),
		Dispatcher: func(catalog tool.Catalog, chain []tool.Middleware) tool.Dispatcher {
			return tooldispatch.New(catalog, chain...)
		},
		View: func(catalog tool.Catalog, allowed policy.ToolSet) (tool.ViewFactory, error) {
			projected, err := projection.New(catalog, projection.For(allowed))
			if err != nil {
				return nil, err
			}
			return func() tool.View { return projected.NewView() }, nil
		},
	})
	if err != nil {
		t.Fatalf("stack: open the process: %v", err)
	}
	s.Agents = host.NewAgents(res, host.WithStore(s.Store), host.WithFingerprint(s.fingerprint))
	if err := s.Agents.Add(s.deployments...); err != nil {
		_ = res.Close()
		t.Fatalf("stack: install the deployment: %v", err)
	}
	t.Cleanup(func() { _ = s.Agents.Close(context.Background()) })
}

// Restart closes this process and opens another over the same store, which is what an
// upgrade or a crash recovery looks like from the host's side: the checkpoint file is the
// only thing that crosses.
func (s *Stack) Restart(t *testing.T) {
	t.Helper()
	if err := s.Agents.Close(t.Context()); err != nil {
		t.Fatalf("stack: close the process: %v", err)
	}
	s.start(t)
}

// OpenSession opens one conversation on the current process.
func (s *Stack) OpenSession(t *testing.T, id string) {
	t.Helper()
	if _, err := s.Agents.Open(t.Context(), id, AgentName, Authority()); err != nil {
		t.Fatalf("stack: open session %q: %v", id, err)
	}
}

// Run drains one turn and fails the test if it errors or ends in anything but a completed
// state. A test that expects a failure calls RunErr.
func (s *Stack) Run(t *testing.T, id, text string) []agent.Event {
	t.Helper()
	events, err := s.RunErr(id, text)
	if err != nil {
		t.Fatalf("stack: turn %q: %v", text, err)
	}
	if state := terminalState(events); state != agent.StateCompleted {
		t.Fatalf("stack: turn %q ended in %q, want completed; events: %v", text, state, events)
	}
	return events
}

// RunErr drains one turn and returns its error, for the tests that inject one.
func (s *Stack) RunErr(id, text string) ([]agent.Event, error) {
	var events []agent.Event
	for event, err := range s.Agents.Run(context.Background(), id, message.NewTextMessage(message.RoleUser, text)) {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

// terminalState reports the last terminal status in a turn, or "" when it never reached one.
func terminalState(events []agent.Event) agent.TaskState {
	state := agent.TaskState("")
	for _, event := range events {
		if status, ok := event.(agent.Status); ok && status.State.Terminal() {
			state = status.State
		}
	}
	return state
}

// Model is the scripted provider: it answers the replies the stack was opened with and
// records every request, which is how a test asserts what a resumed session carried.
type Model struct {
	mu      sync.Mutex
	replies []Reply
	calls   int
	asked   []inference.GenerateRequest
}

// Requests returns a copy of every request the model was asked.
func (m *Model) Requests() []inference.GenerateRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]inference.GenerateRequest(nil), m.asked...)
}

// Calls is how many times the model was called.
func (m *Model) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Generate performs one scripted call.
func (m *Model) Generate(_ context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	m.mu.Lock()
	reply := m.replies[min(m.calls, len(m.replies)-1)]
	m.calls++
	m.asked = append(m.asked, request)
	m.mu.Unlock()

	if reply.Err != nil {
		return inference.GenerateResponse{}, reply.Err
	}
	if reply.ToolCall != nil {
		return inference.GenerateResponse{
			Message: message.Message{
				Role: message.RoleAssistant,
				// The pointer form on purpose: it is what a driver that assembles a
				// response by hand produces, and both forms are legal (message.Part).
				Content: message.Content{Parts: []message.Part{&message.ToolCallPart{Call: *reply.ToolCall}}},
			},
		}, nil
	}
	return inference.GenerateResponse{
		Message: message.NewTextMessage(message.RoleAssistant, reply.Text),
	}, nil
}

// GenerateStream is what the graph calls: it streams a model's text to the caller, so a
// provider that only implemented Generate would make the engine fail rather than the test.
func (m *Model) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	response, err := m.Generate(ctx, request)
	if err != nil {
		return nil, err
	}
	var events []inference.StreamEvent
	for _, part := range response.Message.Content.Parts {
		switch value := part.(type) {
		case message.TextPart:
			events = append(events, inference.StreamEvent{Kind: inference.StreamText, Text: value.Text})
		case *message.ToolCallPart:
			events = append(events, inference.StreamEvent{
				Kind: inference.StreamToolCall,
				Tool: &inference.ToolCallDelta{
					ID:        value.Call.ID,
					Name:      value.Call.Name,
					Arguments: string(value.Call.Arguments.Raw()),
				},
			})
		}
	}
	return &stream{events: events, response: response}, nil
}

// stream replays a fixed event list and then reports the folded response.
type stream struct {
	events   []inference.StreamEvent
	response inference.GenerateResponse
}

func (s *stream) Next(context.Context) (inference.StreamEvent, error) {
	if len(s.events) == 0 {
		return inference.StreamEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *stream) Result() (inference.GenerateResponse, error) { return s.response, nil }
func (*stream) Close() error                                  { return nil }

// catalog is the model line-up this provider serves, so a grant resolves against what the
// process actually carries.
type catalog struct{ models []model.ModelDescriptor }

func (c catalog) Models() []model.ModelDescriptor { return c.models }

func (c catalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.models {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}

// graphSettings is the graph kind's document surface, as this binary defines it.
type graphSettings struct {
	Path  string `json:"path"`
	Model struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	} `json:"model"`
}

// impls is the table this binary compiles in: three real implementations and one fake.
func impls(scripted *Model) *deploy.Impls {
	return deploy.NewImpls().
		Provider(deploy.Impl(providerKey, func(map[string]any) (*inference.Provider, error) {
			return &inference.Provider{
				Generator: scripted,
				Catalog: catalog{models: []model.ModelDescriptor{{
					ID: model.ModelID{Provider: providerKey, Name: modelName},
				}}},
			}, nil
		})).
		// The real runner: no OS boundary, which is what a local demo uses and what the
		// file tools' kernel handles come from.
		Backend(deploy.BackendImpl("local", func(_ struct{}, confine sandbox.Confine) (sandbox.Runner, error) {
			return local.New(confine)
		})).
		ToolSource(deploy.Impl("files", func(struct {
			Images bool `json:"images"`
		}) (tool.Source, error) {
			return files.New(), nil
		})).
		Program(deploy.Impl("graph", func(settings graphSettings) (agent.Program, error) {
			compiled, err := nodes.Open(context.Background(), settings.Path, config.File(settings.Path))
			if err != nil {
				return nil, err
			}
			return graph.NewProgram(compiled, graph.WithProgramDefaultModel(model.ModelRef{
				ID: model.ModelID{Provider: settings.Model.Provider, Name: settings.Model.Name},
			})), nil
		}))
}

// capabilities is what this process can hand a tool. The slots are declared and left
// empty: the host fills the session's own tree and process into them when it opens one,
// which is the seam the real file tools read through.
func capabilities() *tool.CapSet {
	set := tool.NewCapSet()
	platform.TreeSlot.Provide(set, nil)
	platform.ProcessSlot.Provide(set, nil)
	platform.SkillsSlot.Provide(set, nil)
	platform.DelegationSlot.Provide(set, nil)
	return set
}

// rebase reads one testdata document and substitutes the two paths it cannot know.
func rebase(t *testing.T, name, workspace, graphPath string) []byte {
	t.Helper()
	data, err := documents.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("stack: read %s: %v", name, err)
	}
	text := strings.ReplaceAll(string(data), workspacePlaceholder, filepath.ToSlash(workspace))
	text = strings.ReplaceAll(text, graphPlaceholder, filepath.ToSlash(graphPath))
	return []byte(text)
}
