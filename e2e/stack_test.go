// Package e2e is the stack test: every module this workspace ships, assembled the way
// a host assembles them, running one conversation.
//
// It exists because each module verifies its own seam against a fake of the module on the
// other side of it. `core` is forbidden from importing an implementation
// (TestCoreDoesNotImportSiblingModules), so its end-to-end test uses a fake backend and
// fake tools; the sandbox and tool modules test themselves against a fake host. Nothing in
// the repository proved that the real local runner, the real file tools, the real tool
// chain, the real graph engine and the real SQLite store agree with each other — and
// agreeing is the only thing a deployment actually needs.
//
// It is not a workspace member for the release: it has no changeset and is never tagged.
// What it verifies is the working tree, which is why it is here and not in examples/forge
// (a consumer, pinned to released versions on purpose).
package e2e_test

import (
	"context"
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

// The document is read from testdata and rebased onto a temporary directory: a grant
// names the subtrees it confines, and a test that hard-coded a path would either write
// outside the test's own directory or depend on one.
const (
	workspacePlaceholder = "<workspace>"
	graphPlaceholder     = "<graph>"
)

// writer is the model this stack runs on: the first call asks for the file tool, the
// second answers. A vendor driver would make the test depend on the network and on a
// credential, and neither is what is being verified.
type writer struct {
	mu    sync.Mutex
	calls int
	// note is the absolute path the tool call names, inside the session's write set.
	note string
	// requests records what the graph asked for, so the test can assert the transcript
	// a resumed session carried.
	requests []inference.GenerateRequest
}

func (w *writer) Generate(_ context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	w.requests = append(w.requests, request)

	if w.calls == 1 {
		call, err := message.NewToolCall("call-1", files.WriteFileName, map[string]any{
			"file_path": w.note,
			"content":   "written by the stack test",
		})
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		return inference.GenerateResponse{
			Message: message.Message{
				Role:    message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{&message.ToolCallPart{Call: call}}},
			},
		}, nil
	}
	return inference.GenerateResponse{
		Message: message.NewTextMessage(message.RoleAssistant, "the note is written"),
	}, nil
}

// GenerateStream is what the graph calls: it streams a model's text to the caller, so a
// provider that only implemented Generate would make the engine fail rather than the test.
func (w *writer) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	response, err := w.Generate(ctx, request)
	if err != nil {
		return nil, err
	}
	var events []inference.StreamEvent
	// A tool call arrives as its own delta; a text answer arrives as text. Both are what a
	// real driver would stream for the same response.
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

// catalog is the model line-up this provider serves: one fake model, so the deployment
// document's grant resolves against what the process actually carries.
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

// graphSettings is the graph kind's document surface, as this binary defines it.
type graphSettings struct {
	Path  string `json:"path"`
	Model struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	} `json:"model"`
}

// impls is the table this binary compiles in: three real implementations and one fake.
func impls(fake *writer, _ string) *deploy.Impls {
	return deploy.NewImpls().
		Provider(deploy.Impl("fake", func(map[string]any) (*inference.Provider, error) {
			return &inference.Provider{
				Generator: fake,
				Catalog: catalog{models: []model.ModelDescriptor{{
					ID: model.ModelID{Provider: "fake", Name: "fake-1"},
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

// document reads one testdata document and rebases the placeholders.
func document(t *testing.T, name, workspace, graphPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	text := strings.ReplaceAll(string(data), workspacePlaceholder, filepath.ToSlash(workspace))
	text = strings.ReplaceAll(text, graphPlaceholder, filepath.ToSlash(graphPath))
	return []byte(text)
}

// TestTheStackWritesAFileAndResumes is the whole workspace in one turn: a process document
// and an agent document resolve through the real loader, the real local sandbox backs the
// session, the real file tool writes inside the grant's write set, the real graph engine
// loops model → tools → model, the real SQLite store records the turn, and a second
// process opened on that store continues the conversation.
func TestTheStackWritesAFileAndResumes(t *testing.T) {
	workspace := t.TempDir()
	note := filepath.Join(workspace, "note.txt")
	graphPath, err := filepath.Abs(filepath.Join("testdata", "graph.yaml"))
	if err != nil {
		t.Fatalf("resolve graph path: %v", err)
	}

	model := &writer{note: note}
	loader := deploy.NewLoader(impls(model, workspace))
	process := sources.Bytes("deploy.yaml", document(t, "deploy.yaml", workspace, graphPath))

	plan, err := loader.Process(t.Context(), process)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	caps := deploy.Capabilities{
		Backends: plan.BackendNames(),
		Models:   []string{"fake/fake-1"},
	}
	deployments, err := loader.Deployments(t.Context(), caps,
		sources.Bytes("agent.yaml", document(t, "agent.yaml", workspace, graphPath)))
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}

	store, err := checkpoint.Open(filepath.Join(workspace, "state.db"))
	if err != nil {
		t.Fatalf("checkpoint.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fingerprint := deploy.Fingerprint(deployments)

	first := openStack(t, plan, store, fingerprint, deployments)
	if _, err := first.Open(t.Context(), "s1", "writer"); err != nil {
		t.Fatalf("Open session: %v", err)
	}
	runTurn(t, first, "write the note")

	// The file the model asked for exists, with what it asked to be written: the tool
	// ran through the real chain and the session's real tree, not a stub.
	content, err := os.ReadFile(note)
	if err != nil {
		t.Fatalf("read the file the tool wrote: %v", err)
	}
	if string(content) != "written by the stack test" {
		t.Fatalf("file content = %q", content)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second process, same store: the turn's checkpoint is what the next session carries,
	// so the model sees the first turn's transcript rather than an empty conversation.
	second := openStack(t, plan, store, fingerprint, deployments)
	t.Cleanup(func() { _ = second.Close(t.Context()) })
	if _, err := second.Open(t.Context(), "s1", "writer"); err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	runTurn(t, second, "write another note")

	if len(model.requests) < 3 {
		t.Fatalf("the model was called %d times, want the first turn's two calls and the second's",
			len(model.requests))
	}
	resumed := model.requests[len(model.requests)-1]
	carried := resumed.Input.Content.Text() == "write the note"
	for _, m := range resumed.Context {
		if m.Content.Text() == "write the note" {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the resumed turn's request does not carry the first turn's message: %+v", resumed)
	}
}

// openStack is one boot, the way a host does it.
func openStack(t *testing.T, plan deploy.ProcessPlan, store agent.Store, fingerprint agent.Fingerprint, deployments []deploy.Deployment) *host.Agents {
	t.Helper()
	res, err := host.Open(t.Context(), plan, host.Options{
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
		t.Fatalf("host.Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	agents := host.NewAgents(res, host.WithStore(store), host.WithFingerprint(fingerprint))
	if err := agents.Add(deployments...); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return agents
}

// runTurn drains one turn and fails it on an error or on a terminal state that is not
// "completed". The intermediate states are the graph's progress, not a verdict.
func runTurn(t *testing.T, agents *host.Agents, text string) {
	t.Helper()
	state := agent.TaskState("")
	for event, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, text)) {
		if err != nil {
			t.Fatalf("run %q: %v", text, err)
		}
		if status, ok := event.(agent.Status); ok && status.State.Terminal() {
			state = status.State
		}
	}
	if state != agent.StateCompleted {
		t.Fatalf("run %q ended in %q, want completed", text, state)
	}

}
