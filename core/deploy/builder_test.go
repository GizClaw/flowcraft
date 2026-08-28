package deploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

type workspace struct {
	name string
}

type workspaceRegistry struct {
	root   string
	items  map[string]any
	closed bool
}

func (w *workspaceRegistry) ResolveItem(item string) (any, bool) {
	v, ok := w.items[item]
	return v, ok
}

func (w *workspaceRegistry) Close() error {
	w.closed = true
	return nil
}

type workspaceFactory struct{ fail error }

func (workspaceFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "workspace.Registry", Impl: "local"}
}

func (f workspaceFactory) New(_ context.Context, in resource.Input) (any, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var settings struct {
		Root string `json:"root"`
	}
	if err := resource.DecodeSettings(context.Background(), &settings, in.Settings); err != nil {
		return nil, err
	}
	return &workspaceRegistry{
		root: settings.Root,
		items: map[string]any{
			"ws1": &workspace{name: "ws1"},
		},
	}, nil
}

type sandbox struct {
	ws     *workspaceRegistry
	closed bool
}

func (s *sandbox) Close() error {
	s.closed = true
	return nil
}

type sandboxFactory struct{}

func (sandboxFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "sandbox.Registry",
		Impl: "local",
		Deps: []resource.DepSpec{{
			Name: "workspace", Type: "workspace.Registry", Required: true,
		}},
	}
}

func (sandboxFactory) New(_ context.Context, in resource.Input) (any, error) {
	value, ok := in.Dep("workspace")
	if !ok {
		return nil, errdefs.Validationf("sandbox: workspace dep missing")
	}
	ws, ok := value.(*workspaceRegistry)
	if !ok {
		return nil, errdefs.Validationf("sandbox: workspace dep has wrong type")
	}
	return &sandbox{ws: ws}, nil
}

type tool struct {
	ws     *workspace
	closed bool
}

func (t *tool) Close() error {
	t.closed = true
	return nil
}

type toolFactory struct{}

func (toolFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "tool.Assembly",
		Impl: "yaml",
		Deps: []resource.DepSpec{{
			Name: "workspace", Type: "workspace.Workspace", Required: true,
		}},
	}
}

func (toolFactory) New(_ context.Context, in resource.Input) (any, error) {
	value, ok := in.Dep("workspace")
	if !ok {
		return nil, errdefs.Validationf("tool: workspace dep missing")
	}
	ws, ok := value.(*workspace)
	if !ok {
		return nil, errdefs.Validationf("tool: workspace dep has wrong type")
	}
	return &tool{ws: ws}, nil
}

func buildDoc() deploy.Document {
	return deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"fs": {
				Kind:     "workspace.Registry",
				Impl:     "local",
				Settings: json.RawMessage(`{"root":"/tmp/flowcraft"}`),
			},
			"box": {
				Kind: "sandbox.Registry",
				Impl: "local",
				Deps: resource.Deps{"workspace": "fs"},
			},
			"kit": {
				Kind: "tool.Assembly",
				Impl: "yaml",
				Deps: resource.Deps{"workspace": "fs/ws1"},
			},
		},
	}
}

func newRegistry() *resource.Registry {
	reg := resource.NewRegistry()
	reg.MustRegister(workspaceFactory{})
	reg.MustRegister(sandboxFactory{})
	reg.MustRegister(toolFactory{})
	return reg
}

func TestBuildSuccessWithItems(t *testing.T) {
	result, err := deploy.NewBuilder(newRegistry()).Build(context.Background(), buildDoc())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()

	fs, ok := result.Value("fs")
	if !ok {
		t.Fatal("fs resource missing")
	}
	ws := fs.(*workspaceRegistry)
	if ws.root != "/tmp/flowcraft" {
		t.Fatalf("fs root = %q", ws.root)
	}
	box, ok := result.Value("box")
	if !ok || box.(*sandbox).ws != ws {
		t.Fatal("box did not receive the whole fs resource")
	}
	kit, ok := result.Value("kit")
	if !ok || kit.(*tool).ws.name != "ws1" {
		t.Fatal("kit did not receive the fs/ws1 item")
	}
}

func TestBuildMissingFactory(t *testing.T) {
	doc := buildDoc()
	doc.Resources["other"] = resource.Resource{Kind: "nope.Registry", Impl: "x"}
	_, err := deploy.NewBuilder(newRegistry()).Build(context.Background(), doc)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
}

func TestBuildRejectsCycle(t *testing.T) {
	doc := buildDoc()
	fs := doc.Resources["fs"]
	box := doc.Resources["box"]
	fs.Deps = resource.Deps{"dep": "box"}
	box.Deps = resource.Deps{"dep": "fs"}
	doc.Resources["fs"] = fs
	doc.Resources["box"] = box
	if _, err := deploy.NewBuilder(newRegistry()).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build cycle error = %v, want validation", err)
	}
}

type recordingValue struct {
	closed *bool
}

func (v *recordingValue) Close() error {
	*v.closed = true
	return nil
}

type recordingFactory struct{ closed *bool }

func (recordingFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "workspace.Registry", Impl: "record"}
}

func (f recordingFactory) New(context.Context, resource.Input) (any, error) {
	return &recordingValue{closed: f.closed}, nil
}

type failingFactory struct{}

func (failingFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "workspace.Registry", Impl: "fail"}
}

func (failingFactory) New(context.Context, resource.Input) (any, error) {
	return nil, errors.New("boom")
}

func TestBuildRollsBackOnFailure(t *testing.T) {
	var closed bool
	reg := resource.NewRegistry()
	reg.MustRegister(recordingFactory{closed: &closed})
	reg.MustRegister(failingFactory{})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {Kind: "workspace.Registry", Impl: "record"},
			"b": {Kind: "workspace.Registry", Impl: "fail"},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); err == nil {
		t.Fatal("Build unexpectedly succeeded")
	}
	if !closed {
		t.Fatal("built resource was not closed during rollback")
	}
}

type wireRecorder struct {
	order *[]string
	name  string
}

func (w *wireRecorder) Wire(context.Context) error {
	*w.order = append(*w.order, w.name)
	return nil
}

type observerFactory struct{ order *[]string }

func (observerFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "observer.event", Impl: "audit"}
}

func (f observerFactory) New(context.Context, resource.Input) (any, error) {
	return &wireRecorder{order: f.order, name: "observer"}, nil
}

type engineFactory struct{}

func (engineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "engine.test", Impl: "graph"}
}

func (engineFactory) New(context.Context, resource.Input) (any, error) {
	return agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	}), nil
}

// hookRecorder satisfies agent.Observer (via BaseObserver) and
// resource.Wireable (via wireRecorder) so bindAgents can attach it to
// the observe slot and record wire order.
type hookRecorder struct {
	agent.BaseObserver
	wireRecorder
}

type hookFactory struct {
	order *[]string
	fail  error
}

func (hookFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "hook.observe", Impl: "audit"}
}

func (f hookFactory) New(context.Context, resource.Input) (any, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return &hookRecorder{
		wireRecorder: wireRecorder{order: f.order, name: "hook"},
	}, nil
}

func TestDeployBuildsAgentsAndWires(t *testing.T) {
	var order []string
	reg := resource.NewRegistry()
	reg.MustRegister(observerFactory{order: &order})
	reg.MustRegister(engineFactory{})
	reg.MustRegister(hookFactory{order: &order})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"audit": {Kind: "observer.event", Impl: "audit"},
		},
		Agents: map[string]agent.Definition{
			"researcher": {
				Card:    agent.AgentCard{Name: "Researcher"},
				Engine:  agent.EngineRef{Kind: "engine.test", Impl: "graph"},
				Observe: []agent.Hook{{Type: "audit"}},
			},
		},
	}
	result, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	defer func() { _ = result.Close() }()

	bound, ok := result.Agent("researcher")
	if !ok {
		t.Fatal("agent missing from result")
	}
	if bound.Engine == nil {
		t.Fatal("engine is nil")
	}
	if len(bound.Observe) != 1 {
		t.Fatalf("hooks = %v", bound.Observe)
	}
	if len(order) != 2 || order[0] != "observer" || order[1] != "hook" {
		t.Fatalf("wire order = %v, want [observer hook]", order)
	}
}

type deploymentBinderRecorder struct {
	bound    bool
	agentIDs []string
	fail     error
}

func (b *deploymentBinderRecorder) BindDeployment(deployment any) error {
	if b.fail != nil {
		return b.fail
	}
	result, ok := deployment.(*deploy.Result)
	if !ok {
		return errors.New("deployment binder: unexpected deployment type")
	}
	b.bound = true
	b.agentIDs = append([]string(nil), result.AgentNames()...)
	return nil
}

type binderFactory struct {
	recorder *deploymentBinderRecorder
}

func (binderFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "test.DeploymentBinder", Impl: "local"}
}

func (f binderFactory) New(context.Context, resource.Input) (any, error) {
	return f.recorder, nil
}

func TestDeployBindsDeploymentAfterAgents(t *testing.T) {
	recorder := &deploymentBinderRecorder{}
	reg := resource.NewRegistry()
	reg.MustRegister(engineFactory{})
	reg.MustRegister(binderFactory{recorder: recorder})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"binder": {Kind: "test.DeploymentBinder", Impl: "local"},
		},
		Agents: map[string]agent.Definition{
			"researcher": {
				Card:   agent.AgentCard{Name: "Researcher"},
				Engine: agent.EngineRef{Kind: "engine.test", Impl: "graph"},
			},
		},
	}
	result, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	defer func() { _ = result.Close() }()

	if !recorder.bound || len(recorder.agentIDs) != 1 || recorder.agentIDs[0] != "researcher" {
		t.Fatalf("binder state = bound:%v agents:%v, want bound with researcher",
			recorder.bound, recorder.agentIDs)
	}
}

func TestDeployBindFailureRollsBack(t *testing.T) {
	recorder := &deploymentBinderRecorder{fail: errors.New("bind failed")}
	reg := resource.NewRegistry()
	reg.MustRegister(engineFactory{})
	reg.MustRegister(binderFactory{recorder: recorder})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"binder": {Kind: "test.DeploymentBinder", Impl: "local"},
		},
		Agents: map[string]agent.Definition{
			"researcher": {
				Card:   agent.AgentCard{Name: "Researcher"},
				Engine: agent.EngineRef{Kind: "engine.test", Impl: "graph"},
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc); err == nil ||
		!strings.Contains(err.Error(), "bind deployment") {
		t.Fatalf("Deploy error = %v, want bind deployment failure", err)
	}
}

func TestDeployRejectsUnknownHook(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(engineFactory{})
	doc := deploy.Document{
		Version: "v1",
		Agents: map[string]agent.Definition{
			"a": {
				Card:    agent.AgentCard{Name: "A"},
				Engine:  agent.EngineRef{Kind: "engine.test", Impl: "graph"},
				Observe: []agent.Hook{{Type: "nope"}},
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Deploy error = %v, want validation", err)
	}
}

func TestDeployRollsBackOnWireFailure(t *testing.T) {
	var closed bool
	reg := resource.NewRegistry()
	reg.MustRegister(recordingFactory{closed: &closed})
	reg.MustRegister(engineFactory{})
	reg.MustRegister(hookFactory{fail: errors.New("boom")})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {Kind: "workspace.Registry", Impl: "record"},
		},
		Agents: map[string]agent.Definition{
			"a2": {
				Card:    agent.AgentCard{Name: "A"},
				Engine:  agent.EngineRef{Kind: "engine.test", Impl: "graph"},
				Observe: []agent.Hook{{Type: "audit"}},
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc); err == nil {
		t.Fatal("Deploy unexpectedly succeeded")
	}
	if !closed {
		t.Fatal("built resource was not closed after wire failure")
	}
}

func TestResultCloseReversesOrder(t *testing.T) {
	result, err := deploy.NewBuilder(newRegistry()).Build(context.Background(), buildDoc())
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Close(); err != nil {
		t.Fatal(err)
	}
	kit, _ := result.Value("kit")
	box, _ := result.Value("box")
	fs, _ := result.Value("fs")
	if !kit.(*tool).closed || !box.(*sandbox).closed || !fs.(*workspaceRegistry).closed {
		t.Fatal("not all values were closed")
	}
}

type closableAgentEngine struct {
	closed bool
}

func (e *closableAgentEngine) Execute(
	_ context.Context,
	_ agent.Run,
	_ agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	return board, nil
}

func (e *closableAgentEngine) Close() error {
	e.closed = true
	return nil
}

type closableEngineFactory struct{ value *closableAgentEngine }

func (f closableEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "engine.test", Impl: "close"}
}

func (f closableEngineFactory) New(context.Context, resource.Input) (any, error) {
	return f.value, nil
}

type closableHook struct {
	agent.BaseObserver
	closed bool
}

func (h *closableHook) Close() error {
	h.closed = true
	return nil
}

type sequenceHookFactory struct {
	value  *closableHook
	calls  int
	failOn int
}

func (f *sequenceHookFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "hook.observe", Impl: "close"}
}

func (f *sequenceHookFactory) New(context.Context, resource.Input) (any, error) {
	f.calls++
	if f.failOn > 0 && f.calls == f.failOn {
		return nil, errors.New("hook boom")
	}
	return f.value, nil
}

func TestBindAgentDoesNotMutateResult(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(engineFactory{})

	result, err := deploy.NewBuilder(reg).Build(context.Background(), deploy.Document{Version: "v1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()

	instance, err := deploy.BindAgent(context.Background(), reg, result, nil, nil, nil, "a", agent.Definition{
		Card:   agent.AgentCard{Name: "A"},
		Engine: agent.EngineRef{Kind: "engine.test", Impl: "graph"},
	})
	if err != nil {
		t.Fatalf("BindAgent: %v", err)
	}
	if instance.ID != "a" {
		t.Fatalf("instance ID = %q, want a", instance.ID)
	}
	if _, ok := result.Agent("a"); ok {
		t.Fatal("BindAgent mutated result agents")
	}
}

func TestBindAgentRollsBackOnHookFailure(t *testing.T) {
	reg := resource.NewRegistry()
	engine := &closableAgentEngine{}
	hook := &closableHook{}
	reg.MustRegister(closableEngineFactory{value: engine})
	reg.MustRegister(&sequenceHookFactory{value: hook, failOn: 2})

	result, err := deploy.NewBuilder(reg).Build(context.Background(), deploy.Document{Version: "v1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()

	_, err = deploy.BindAgent(context.Background(), reg, result, nil, nil, nil, "a", agent.Definition{
		Card:    agent.AgentCard{Name: "A"},
		Engine:  agent.EngineRef{Kind: "engine.test", Impl: "close"},
		Observe: []agent.Hook{{Type: "close"}, {Type: "close"}},
	})
	if err == nil {
		t.Fatal("BindAgent unexpectedly succeeded")
	}
	if !engine.closed {
		t.Fatal("engine was not closed after hook failure")
	}
	if !hook.closed {
		t.Fatal("wired hook was not closed during rollback")
	}
}

func TestResultCloseClosesAgents(t *testing.T) {
	reg := resource.NewRegistry()
	engine := &closableAgentEngine{}
	hook := &closableHook{}
	reg.MustRegister(closableEngineFactory{value: engine})
	reg.MustRegister(&sequenceHookFactory{value: hook})

	doc := deploy.Document{
		Version: "v1",
		Agents: map[string]agent.Definition{
			"a": {
				Card:    agent.AgentCard{Name: "A"},
				Engine:  agent.EngineRef{Kind: "engine.test", Impl: "close"},
				Observe: []agent.Hook{{Type: "close"}},
			},
		},
	}
	result, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !engine.closed {
		t.Fatal("agent engine was not closed by Result.Close")
	}
	if !hook.closed {
		t.Fatal("agent hook was not closed by Result.Close")
	}
}
