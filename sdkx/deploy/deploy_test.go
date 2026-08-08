package deploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// ---------- fakes ----------

type fakeEngineFactory struct {
	spec     sdkconfig.Spec
	gotCfg   sdkconfig.Input
	newCalls int
}

func (f *fakeEngineFactory) Spec() sdkconfig.Spec { return f.spec }

func (f *fakeEngineFactory) New(_ context.Context, cfg sdkconfig.Input) (any, error) {
	f.newCalls++
	f.gotCfg = cfg
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "ok"))
		return b, nil
	}), nil
}

// fakeRegistry is a container resource: ResolveItem resolves named items,
// matching how a workspace / sandbox registry serves refs.
// It records construction order and closure through a shared journal.
type fakeRegistry struct {
	name    string
	items   map[string]string
	journal *journal
}

func (r *fakeRegistry) ResolveItem(ref string) (any, bool) {
	v, ok := r.items[ref]
	return v, ok
}

func (r *fakeRegistry) Close() error {
	r.journal.closed = append(r.journal.closed, r.name)
	return nil
}

// journal records the order resources were built and closed in, which
// is what the topological-order and reverse-close tests assert on.
type journal struct {
	built  []string
	closed []string
}

// fakeJSRuntime is a whole-bound, non-container, closable resource.
type fakeJSRuntime struct {
	pool    int
	journal *journal
}

func (f *fakeJSRuntime) Close() error {
	f.journal.closed = append(f.journal.closed, "js_main")
	return nil
}

// fakeStore is bound only by hooks, never by an agent dep.
type fakeStore struct{ workspace string }

type fakeResourceFactory struct {
	spec sdkconfig.Spec
	new  func(context.Context, sdkconfig.Input) (any, error)
}

func (f *fakeResourceFactory) Spec() sdkconfig.Spec { return f.spec }

func (f *fakeResourceFactory) New(ctx context.Context, in sdkconfig.Input) (any, error) {
	return f.new(ctx, in)
}

func resourceFactory(
	kind, impl, itemType string,
	deps []sdkconfig.DepSpec,
	newFn func(context.Context, sdkconfig.Input) (any, error),
) sdkconfig.Factory {
	return &fakeResourceFactory{
		spec: sdkconfig.Spec{
			Kind: kind, Impl: impl, Deps: deps, ItemType: itemType,
		},
		new: newFn,
	}
}

type recordingHook struct {
	agent.BaseObserver
	store *fakeStore
}

type recordingCommitter struct {
	store *fakeStore
	calls int
}

func (c *recordingCommitter) Commit(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
	c.calls++
	return nil
}

type recordingBefore struct {
	window int
	store  *fakeStore
}

func (r recordingBefore) Before(_ context.Context, _ agent.Identity, req *agent.Request, prev *agent.Board) (*agent.Board, error) {
	b := prev.Clone()
	b.AppendChannelMessage(agent.MainChannel, req.Message)
	b.SetVar("window", r.window)
	return b, nil
}

func graphSpec() sdkconfig.Spec {
	return sdkconfig.Spec{
		Kind: "graph",
		Deps: []sdkconfig.DepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "runner", Type: "sandbox.Runner"},
			{Name: "script_runtime", Type: "script.runtime"},
			{Name: "tools", Type: "tool.Catalog"},
		},
	}
}

type testBuilder struct {
	*deploy.Builder
	graph   *fakeEngineFactory
	inline  *fakeEngineFactory
	journal *journal
	hooks   *hookCapture
}

// hookCapture records what the hook factories actually received, so
// tests can assert that resource deps reach the hook layer.
type hookCapture struct {
	before    *recordingBefore
	hook      *recordingHook
	committer *recordingCommitter
}

func newTestBuilder(t *testing.T) *testBuilder {
	t.Helper()
	b := deploy.NewBuilder()
	graph := &fakeEngineFactory{spec: graphSpec()}
	inline := &fakeEngineFactory{spec: sdkconfig.Spec{Kind: "inline"}}
	for _, f := range []*fakeEngineFactory{graph, inline} {
		if err := b.RegisterEngine(f); err != nil {
			t.Fatalf("register %s: %v", f.spec.Kind, err)
		}
	}

	jr := &journal{}
	captured := &hookCapture{}

	b.RegisterSource("host.tools", func(_ context.Context, ref string) (any, error) {
		return "catalog:" + ref, nil
	})

	registryFactory := func(kind string) func(context.Context, sdkconfig.Input) (any, error) {
		return func(_ context.Context, in sdkconfig.Input) (any, error) {
			type s struct {
				Names []string `json:"names"`
			}
			dec, err := sdkconfig.DecodeSettings[s](in.Settings)
			if err != nil {
				return nil, err
			}
			items := make(map[string]string, len(dec.Names))
			for _, n := range dec.Names {
				items[n] = kind + ":" + n
			}
			jr.built = append(jr.built, kind)
			return &fakeRegistry{name: kind, items: items, journal: jr}, nil
		}
	}
	b.MustRegisterResource(resourceFactory(
		"workspace.Registry", "fake", "workspace.Workspace", nil, registryFactory("fs")))
	b.MustRegisterResource(resourceFactory(
		"sandbox.Registry", "fake", "sandbox.Runner",
		[]sdkconfig.DepSpec{{Name: "workspaces", Type: "workspace.Registry", Required: true}},
		func(_ context.Context, in sdkconfig.Input) (any, error) {
			// A resource binding another resource: the workspace registry
			// must already exist by the time this runs.
			dep, ok := in.Dep("workspaces")
			if !ok {
				return nil, errString("sandbox.Registry: workspaces dep is required")
			}
			if _, ok := dep.(*fakeRegistry); !ok {
				return nil, errString("sandbox.Registry: workspaces dep is not a registry")
			}
			return registryFactory("box")(context.Background(), in)
		}))
	b.MustRegisterResource(resourceFactory(
		"script.runtime", "fakejs", "", nil,
		func(_ context.Context, in sdkconfig.Input) (any, error) {
			type s struct {
				PoolSize int `json:"pool_size"`
			}
			dec, err := sdkconfig.DecodeSettings[s](in.Settings)
			if err != nil {
				return nil, err
			}
			jr.built = append(jr.built, "js_main")
			return &fakeJSRuntime{pool: dec.PoolSize, journal: jr}, nil
		}))
	b.MustRegisterResource(resourceFactory(
		"fake.Store", "fake", "", []sdkconfig.DepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
		}, func(_ context.Context, in sdkconfig.Input) (any, error) {
			ws, ok := in.Dep("workspace")
			if !ok {
				return nil, errString("fake.Store: workspace dep is required")
			}
			jr.built = append(jr.built, "store")
			return &fakeStore{workspace: ws.(string)}, nil
		}))

	b.RegisterObserver("fake_hook", observerFactory{captured: captured})
	b.RegisterPreparer("fake_before", preparerFactory{captured: captured})
	b.RegisterCommitter("fake_commit", committerFactory{captured: captured})

	return &testBuilder{Builder: b, graph: graph, inline: inline, journal: jr, hooks: captured}
}

// observerFactory implements config.Factory for the fake_hook observer.
type observerFactory struct{ captured *hookCapture }

func (observerFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: deploy.HookKindObserver, Impl: "fake_hook"}
}

func (f observerFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	type s struct {
		Store string `json:"store"`
	}
	if _, err := sdkconfig.DecodeSettings[s](in.Settings); err != nil {
		return nil, err
	}
	h := &recordingHook{}
	if dep, ok := in.Dep("store"); ok {
		h.store, _ = dep.(*fakeStore)
	}
	f.captured.hook = h
	return h, nil
}

// preparerFactory implements config.Factory for the fake_before preparer.
type preparerFactory struct{ captured *hookCapture }

func (preparerFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: deploy.HookKindPreparer, Impl: "fake_before"}
}

func (f preparerFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	type s struct {
		Window int `json:"window"`
	}
	dec, err := sdkconfig.DecodeSettings[s](in.Settings)
	if err != nil {
		return nil, err
	}
	out := recordingBefore{window: dec.Window}
	if dep, ok := in.Dep("store"); ok {
		out.store, _ = dep.(*fakeStore)
	}
	f.captured.before = &out
	return out, nil
}

// committerFactory implements config.Factory for the fake_commit committer.
type committerFactory struct{ captured *hookCapture }

func (committerFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: deploy.HookKindCommitter, Impl: "fake_commit"}
}

func (f committerFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	committer := &recordingCommitter{}
	if dep, ok := in.Dep("store"); ok {
		committer.store, _ = dep.(*fakeStore)
	}
	f.captured.committer = committer
	return committer, nil
}

// nilCommitterFactory implements config.Factory for a committer that
// returns a typed nil, which Build must reject.
type nilCommitterFactory struct{}

func (nilCommitterFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: deploy.HookKindCommitter, Impl: "nil_commit"}
}

func (nilCommitterFactory) New(context.Context, sdkconfig.Input) (any, error) {
	var committer *recordingCommitter
	return committer, nil
}

type errString string

func (e errString) Error() string { return string(e) }

type closeRecorder struct {
	name    string
	calls   int
	journal *[]string
	err     error
	started chan<- struct{}
	release <-chan struct{}
}

func (c *closeRecorder) Close() error {
	c.calls++
	if c.started != nil {
		close(c.started)
	}
	if c.release != nil {
		<-c.release
	}
	if c.journal != nil {
		*c.journal = append(*c.journal, c.name)
	}
	return c.err
}

func buildOwnedResult(t *testing.T, values map[string]any) *deploy.Result {
	t.Helper()
	return buildDependencyResult(t, values, nil)
}

func buildDependencyResult(t *testing.T, values map[string]any, deps map[string][]string) *deploy.Result {
	t.Helper()
	b := deploy.NewBuilder()
	resources := make(map[string]deploy.ResourceEntry, len(values))
	for name, value := range values {
		kind := name + ".Kind"
		specDeps := make([]sdkconfig.DepSpec, 0, len(deps[name]))
		entryDeps := make(map[string]deploy.DepRef, len(deps[name]))
		for i, dependency := range deps[name] {
			depName := fmt.Sprintf("dep%d", i)
			specDeps = append(specDeps, sdkconfig.DepSpec{
				Name: depName, Type: dependency + ".Kind", Required: true,
			})
			entryDeps[depName] = deploy.DepRef{Resource: dependency}
		}
		b.MustRegisterResource(resourceFactory(
			kind, "fake", "", specDeps,
			func(context.Context, sdkconfig.Input) (any, error) { return value, nil },
		))
		resources[name] = deploy.ResourceEntry{
			Kind: kind, Impl: "fake", Export: true, Deps: entryDeps,
		}
	}
	result, err := b.Build(context.Background(), deploy.Document{
		Version:   deploy.VersionV1,
		Resources: resources,
		Agents:    map[string]deploy.AgentEntry{},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return result
}

type countingResolver struct {
	calls int
	item  any
}

func (r *countingResolver) ResolveItem(string) (any, bool) {
	r.calls++
	return r.item, true
}

type closingResolver struct {
	closeRecorder
	item any
}

func (r *closingResolver) ResolveItem(string) (any, bool) {
	return r.item, true
}

func loadDoc(t *testing.T) deploy.Document {
	t.Helper()
	data, err := os.ReadFile("testdata/deploy.yaml")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	doc, err := deploy.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

func parse(t *testing.T, body string) deploy.Document {
	t.Helper()
	doc, err := deploy.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

func writeAgentFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write agent file %s: %v", name, err)
	}
}

func agentFileBuilder(t *testing.T, opts ...deploy.BuilderOption) *deploy.Builder {
	t.Helper()
	b := deploy.NewBuilder(opts...)
	if err := b.RegisterEngine(&fakeEngineFactory{
		spec: sdkconfig.Spec{Kind: "inline"},
	}); err != nil {
		t.Fatalf("register inline engine: %v", err)
	}
	return b
}

// ---------- Resource registration ----------

func TestRegisterResource_ValidatesAndSortsSpecs(t *testing.T) {
	b := deploy.NewBuilder()
	for _, spec := range []sdkconfig.Spec{
		{Kind: "z.Kind", Impl: "a"},
		{Kind: "a.Kind", Impl: "z"},
		{Kind: "a.Kind", Impl: "a", Deps: []sdkconfig.DepSpec{
			{Name: "dep", Type: "dep.Kind"},
		}},
	} {
		if err := b.RegisterResource(resourceFactory(
			spec.Kind, spec.Impl, spec.ItemType, spec.Deps,
			func(context.Context, sdkconfig.Input) (any, error) { return struct{}{}, nil },
		)); err != nil {
			t.Fatalf("RegisterResource(%+v): %v", spec, err)
		}
	}

	got := b.Specs()
	want := []sdkconfig.Spec{
		{Kind: "a.Kind", Impl: "a", Deps: []sdkconfig.DepSpec{
			{Name: "dep", Type: "dep.Kind"},
		}},
		{Kind: "a.Kind", Impl: "z"},
		{Kind: deploy.HookKindReferee, Impl: "discard_on_interrupt"},
		{Kind: "z.Kind", Impl: "a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Specs = %+v, want %+v", got, want)
	}
	got[0].Kind = "mutated.Kind"
	got[0].Deps[0].Name = "mutated"
	if again := b.Specs(); !reflect.DeepEqual(again, want) {
		t.Fatalf("Specs after caller mutation = %+v, want defensive copy %+v", again, want)
	}

	err := b.RegisterResource(resourceFactory(
		"a.Kind", "a", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return struct{}{}, nil },
	))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("duplicate registration error = %v, want validation", err)
	}
}

func TestRegisterResource_RejectsInvalidSpecsAndTypedNilFactory(t *testing.T) {
	b := deploy.NewBuilder()
	var nilFactory *fakeResourceFactory
	if err := b.RegisterResource(nilFactory); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("typed nil factory error = %v, want validation", err)
	}

	tests := []sdkconfig.Spec{
		{Impl: "x"},
		{Kind: "x"},
		{Kind: "x", Impl: "y", Deps: []sdkconfig.DepSpec{{Name: "", Type: "T"}}},
		{Kind: "x", Impl: "y", Deps: []sdkconfig.DepSpec{{Name: "d"}}},
		{Kind: "x", Impl: "y", Deps: []sdkconfig.DepSpec{
			{Name: "d", Type: "T"}, {Name: "d", Type: "T"},
		}},
	}
	for _, spec := range tests {
		calls := 0
		err := b.RegisterResource(&fakeResourceFactory{
			spec: spec,
			new: func(context.Context, sdkconfig.Input) (any, error) {
				calls++
				return struct{}{}, nil
			},
		})
		if err == nil || !errdefs.IsValidation(err) {
			t.Errorf("RegisterResource(%+v) error = %v, want validation", spec, err)
		}
		if calls != 0 {
			t.Errorf("RegisterResource(%+v) called New", spec)
		}
	}
}

func TestSpecsSnapshotsFactorySpec(t *testing.T) {
	b := deploy.NewBuilder()
	factory := &fakeResourceFactory{
		spec: sdkconfig.Spec{
			Kind: "snapshot.Kind",
			Impl: "fake",
			Deps: []sdkconfig.DepSpec{{Name: "dep", Type: "dep.Kind"}},
		},
		new: func(context.Context, sdkconfig.Input) (any, error) {
			return struct{}{}, nil
		},
	}
	if err := b.RegisterResource(factory); err != nil {
		t.Fatal(err)
	}
	factory.spec.Kind = "mutated.Kind"
	factory.spec.Deps[0].Name = "mutated"

	want := []sdkconfig.Spec{
		{Kind: deploy.HookKindReferee, Impl: "discard_on_interrupt"},
		{
			Kind: "snapshot.Kind",
			Impl: "fake",
			Deps: []sdkconfig.DepSpec{{Name: "dep", Type: "dep.Kind"}},
		},
	}
	if got := b.Specs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Specs = %+v, want registration snapshot %+v", got, want)
	}
}

func TestMustRegisterResource_PanicsOnInvalidFactory(t *testing.T) {
	b := deploy.NewBuilder()
	defer func() {
		if recover() == nil {
			t.Fatal("MustRegisterResource did not panic")
		}
	}()
	b.MustRegisterResource((*fakeResourceFactory)(nil))
}

// ---------- Parse ----------

func TestParse_HappyPath(t *testing.T) {
	doc := loadDoc(t)
	if doc.Version != deploy.VersionV1 {
		t.Fatalf("Version = %q", doc.Version)
	}
	if len(doc.Agents) != 2 {
		t.Fatalf("Agents = %v, want 2 entries", doc.Agents)
	}
	if len(doc.Resources) != 4 {
		t.Fatalf("Resources = %v, want 4 entries", doc.Resources)
	}
	if got := doc.Resources["box"].Deps["workspaces"]; got.Resource != "fs" || got.Ref != "" {
		t.Errorf("Resources[box].deps[workspaces] = %+v", got)
	}
	if got := doc.Resources["store"].Deps["workspace"]; got.Resource != "fs" || got.Ref != "project" {
		t.Errorf("Resources[store].deps[workspace] = %+v", got)
	}

	r := doc.Agents["researcher"]
	if r.Engine.Kind != "graph" {
		t.Errorf("Engine = %+v", r.Engine)
	}
	var engineSettings struct {
		MaxSteps int `json:"max_steps"`
	}
	if err := json.Unmarshal(r.Engine.Settings, &engineSettings); err != nil || engineSettings.MaxSteps != 8 {
		t.Errorf("Engine settings = %s", r.Engine.Settings)
	}
	if len(r.Prepare) == 0 || r.Prepare[0].Type != "fake_before" ||
		r.Prepare[0].Deps["store"].Resource != "store" {
		t.Errorf("Prepare = %+v", r.Prepare)
	}
	if len(r.Observe) != 1 || r.Observe[0].Deps["store"].Resource != "store" {
		t.Errorf("Observe = %+v", r.Observe)
	}
	if len(r.Commit) != 1 || r.Commit[0].Type != "fake_commit" ||
		r.Commit[0].Deps["store"].Resource != "store" {
		t.Errorf("Commit = %+v", r.Commit)
	}
}

// TestParse_DepRefScalarAndMapping pins the two dep spellings. A
// scalar always means a resource; a mapping is how a host-owned
// source is named, which is the visible half of the ownership rule.
func TestParse_DepRefScalarAndMapping(t *testing.T) {
	doc := parse(t, `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps:
      whole: infer
      item: fs/project
      borrowed: {source: host.tools, ref: default}
`)
	deps := doc.Agents["a"].Deps
	if got := deps["whole"]; got.Resource != "infer" || got.Ref != "" || got.Source != "" {
		t.Errorf("whole = %+v", got)
	}
	if got := deps["item"]; got.Resource != "fs" || got.Ref != "project" {
		t.Errorf("item = %+v", got)
	}
	if got := deps["borrowed"]; got.Source != "host.tools" || got.Ref != "default" || got.Resource != "" {
		t.Errorf("borrowed = %+v", got)
	}
}

func TestParse_RejectsMalformedDepRef(t *testing.T) {
	for name, body := range map[string]string{
		"empty scalar":      "deps: {x: \"\"}",
		"leading slash":     "deps: {x: /project}",
		"trailing slash":    "deps: {x: fs/}",
		"two separators":    "deps: {x: a/b/c}",
		"both forms":        "deps: {x: {source: s, resource: r}}",
		"neither form":      "deps: {x: {ref: only}}",
		"unknown dep key":   "deps: {x: {resource: r, bogus: 1}}",
		"unknown agent key": "deps: {x: ok}\n    bogus: 1",
	} {
		body := "version: v1\nagents:\n  a:\n    engine: {kind: graph}\n    " + body + "\n"
		if _, err := deploy.Parse([]byte(body)); err == nil {
			t.Errorf("%s: Parse succeeded, want error", name)
		}
	}
}

func TestParse_RejectsSeparatorInResourceName(t *testing.T) {
	_, err := deploy.Parse([]byte(
		"version: v1\nresources:\n  a/b: {kind: k, impl: i}\nagents: {}\n"))
	if err == nil {
		t.Fatal("resource name containing the separator must fail Parse")
	}
}

func TestParse_StrictAndVersioned(t *testing.T) {
	if _, err := deploy.Parse([]byte("version: v2\nagents: {}\n")); err == nil {
		t.Error("unsupported version must fail")
	}
	if _, err := deploy.Parse([]byte("version: v1\nbogus: 1\nagents: {}\n")); err == nil {
		t.Error("unknown top-level field must fail")
	}
	if _, err := deploy.Parse([]byte("version: v1\nagents: {}\n---\nversion: v1\nagents: {}\n")); err == nil {
		t.Error("trailing document must fail")
	}
	if _, err := deploy.Parse([]byte(
		"version: v1\nresources:\n  a: {impl: i}\nagents: {}\n")); err == nil {
		t.Error("resource without kind must fail")
	}
}

func TestParse_RuntimeIsOpaqueButOtherTopLevelFieldsRemainStrict(t *testing.T) {
	doc := parse(t, `
version: v1
runtime:
  arbitrary:
    nested: [one, {two: true}]
agents: {}
`)
	if doc.Runtime == nil {
		t.Fatal("Runtime = nil, want opaque subtree")
	}

	if _, err := deploy.Parse([]byte(`
version: v1
runtime: {arbitrary: true}
bogus: true
agents: {}
`)); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("unknown top-level field error = %v, want validation", err)
	}
}

func TestParse_AgentFileAndInlineFieldsAreMutuallyExclusive(t *testing.T) {
	_, err := deploy.Parse([]byte(`
version: v1
agents:
  researcher:
    source: {file: ./agents/researcher.yaml}
    engine: {kind: inline}
`))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Parse error = %v, want validation", err)
	}
	if !strings.Contains(err.Error(), `agents["researcher"]`) ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Parse error = %v, want agent context and mutual-exclusion detail", err)
	}
}

// ---------- Build ----------

func TestBuild_LoadsSingleAgentFile(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "researcher.yaml", `
version: v1
card: {name: Researcher, description: Deep research}
tools: [search, fetch]
engine: {kind: inline}
policy: {max_revise: 2, artifact_channels: [report]}
`)
	doc := parse(t, `
version: v1
agents:
  researcher:
    source: {file: researcher.yaml}
`)
	result, err := agentFileBuilder(t, deploy.WithBaseDir(dir)).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })

	inst, ok := result.Instance("researcher")
	if !ok {
		t.Fatal("Instance(researcher) missing")
	}
	if inst.Agent.ID != "researcher" || inst.Agent.Card.Name != "Researcher" ||
		!reflect.DeepEqual(inst.Agent.Tools, []string{"search", "fetch"}) {
		t.Fatalf("Agent = %+v", inst.Agent)
	}
}

func TestBuild_LoadsMultipleAgentFilesAlongsideInlineAgents(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "researcher.yaml", `
version: v1
card: {name: Researcher}
engine: {kind: inline}
`)
	writeAgentFile(t, dir, "writer.yaml", `
version: v1
card: {name: Writer}
engine: {kind: inline}
`)
	doc := parse(t, `
version: v1
agents:
  researcher: {source: {file: researcher.yaml}}
  reviewer:
    card: {name: Reviewer}
    engine: {kind: inline}
  writer: {source: {file: writer.yaml}}
`)
	result, err := agentFileBuilder(t, deploy.WithBaseDir(dir)).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })
	if got := result.InstanceNames(); !reflect.DeepEqual(got, []string{"researcher", "reviewer", "writer"}) {
		t.Fatalf("InstanceNames = %v", got)
	}
}

func TestBuild_ResolvesAgentFileRelativeToBaseDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/agents", 0o700); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	writeAgentFile(t, dir+"/agents", "researcher.yaml", `
version: v1
engine: {kind: inline}
`)
	doc := parse(t, `
version: v1
agents:
  researcher: {source: {file: agents/researcher.yaml}}
`)
	result, err := agentFileBuilder(t, deploy.WithBaseDir(dir)).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })
	if _, ok := result.Instance("researcher"); !ok {
		t.Fatal("Instance(researcher) missing")
	}
}

func TestBuild_MissingAgentFileIsNotFound(t *testing.T) {
	dir := t.TempDir()
	doc := parse(t, `
version: v1
agents:
  researcher: {source: {file: agents/missing.yaml}}
`)
	_, err := agentFileBuilder(t, deploy.WithBaseDir(dir)).Build(context.Background(), doc)
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("Build error = %v, want not found", err)
	}
	if !strings.Contains(err.Error(), `agents["researcher"]`) ||
		!strings.Contains(err.Error(), "agents/missing.yaml") {
		t.Fatalf("Build error = %v, want agent and path context", err)
	}
}

func TestBuild_RejectsProgrammaticWhitespaceAgentFile(t *testing.T) {
	doc := deploy.Document{
		Version: deploy.VersionV1,
		Agents: map[string]deploy.AgentEntry{
			"researcher": {Source: &sdkconfig.Ref{}},
		},
	}
	_, err := agentFileBuilder(t).Build(context.Background(), doc)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
	if !strings.Contains(err.Error(), `agents["researcher"]`) ||
		!strings.Contains(err.Error(), "source is required") {
		t.Fatalf("Build error = %v, want agent and file validation context", err)
	}
}

func TestBuild_RejectsInvalidAgentFiles(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": `
version: v1
engine: {kind: inline}
bogus: true
`,
		"declared id": `
version: v1
id: forbidden
engine: {kind: inline}
`,
		"wrong version": `
version: v2
engine: {kind: inline}
`,
		"trailing document": `
version: v1
engine: {kind: inline}
---
version: v1
engine: {kind: inline}
`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeAgentFile(t, dir, "agent.yaml", body)
			doc := parse(t, `
version: v1
agents:
  a: {source: {file: agent.yaml}}
`)
			_, err := agentFileBuilder(t, deploy.WithBaseDir(dir)).Build(context.Background(), doc)
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want validation", err)
			}
			if !strings.Contains(err.Error(), `agents["a"]`) ||
				!strings.Contains(err.Error(), "agent.yaml") {
				t.Fatalf("Build error = %v, want agent and path context", err)
			}
		})
	}
}

func TestBuild_AssemblesRunnableInstance(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := res.InstanceNames(); !reflect.DeepEqual(got, []string{"minimal", "researcher"}) {
		t.Fatalf("InstanceNames = %v", got)
	}

	r, ok := res.Instance("researcher")
	if !ok {
		t.Fatal("Instance(researcher) missing")
	}
	if r.Agent.ID != "researcher" || r.Agent.Card.Name != "研究员" {
		t.Errorf("Agent = %+v", r.Agent)
	}

	// A container resource addressed by item yields the item, not the
	// container.
	if v, _ := tb.graph.gotCfg.Dep("workspace"); v != "fs:project" {
		t.Errorf("cfg.Dep(workspace) = %v, want fs:project", v)
	}
	if v, _ := tb.graph.gotCfg.Dep("runner"); v != "box:coding" {
		t.Errorf("cfg.Dep(runner) = %v, want box:coding", v)
	}
	// A whole-bound resource yields the instance itself.
	js, ok := tb.graph.gotCfg.Dep("script_runtime")
	if !ok {
		t.Fatal("cfg.Dep(script_runtime) missing")
	}
	if rt, ok := js.(*fakeJSRuntime); !ok || rt.pool != 8 {
		t.Errorf("script_runtime = %#v, want fakeJSRuntime{pool:8}", js)
	}
	if got, err := deploy.ResourceAs[*fakeJSRuntime](res, "js_main"); err != nil || got != js {
		t.Error("ResourceAs(js_main) should return the same instance the engine got")
	}
	// A source is borrowed, resolved through the host closure.
	if v, _ := tb.graph.gotCfg.Dep("tools"); v != "catalog:default" {
		t.Errorf("cfg.Dep(tools) = %v", v)
	}
	var gotGraph struct {
		Graph string `json:"graph"`
	}
	if err := json.Unmarshal(tb.graph.gotCfg.Settings, &gotGraph); err != nil {
		t.Fatalf("decode engine settings: %v", err)
	}
	if gotGraph.Graph != "graphs/research.yaml" {
		t.Errorf("engine settings graph = %q", gotGraph.Graph)
	}

	execRes, err := r.Execute(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("instance Execute: %v", err)
	}
	if execRes.Status != agent.StatusCompleted {
		t.Errorf("Status = %q", execRes.Status)
	}
	if tb.hooks.committer == nil || tb.hooks.committer.calls != 1 {
		t.Fatalf("Committer calls = %v, want 1", tb.hooks.committer)
	}

	minimal, ok := res.Instance("minimal")
	if !ok {
		t.Fatal("Instance(minimal) missing")
	}
	if _, err := minimal.Execute(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	}); err != nil {
		t.Fatalf("minimal Execute: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBuild_RejectsTypedNilCommitter(t *testing.T) {
	b := agentFileBuilder(t)
	b.RegisterCommitter("nil_commit", nilCommitterFactory{})
	doc := parse(t, `
version: v1
agents:
  a:
    engine: {kind: inline}
    commit:
      - {type: nil_commit}
`)

	_, err := b.Build(context.Background(), doc)
	if err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("Build error = %v, want internal typed-nil error", err)
	}
	if !strings.Contains(err.Error(), `agents["a"].commit[0]`) ||
		!strings.Contains(err.Error(), "non-nil") {
		t.Fatalf("Build error = %v, want commit location and nil detail", err)
	}
}

// TestBuild_ResourceDepsBuildInTopologicalOrder is the regression test
// for ordering. "box" depends on "fs" but sorts before it, so a
// lexical build order would hand the sandbox factory a missing dep.
func TestBuild_ResourceDepsBuildInTopologicalOrder(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	fsAt, boxAt, storeAt := -1, -1, -1
	for i, name := range tb.journal.built {
		switch name {
		case "fs":
			fsAt = i
		case "box":
			boxAt = i
		case "store":
			storeAt = i
		}
	}
	if fsAt < 0 || boxAt < 0 || storeAt < 0 {
		t.Fatalf("built = %v, want fs, box and store", tb.journal.built)
	}
	if fsAt > boxAt {
		t.Errorf("built = %v: fs must precede box", tb.journal.built)
	}
	if fsAt > storeAt {
		t.Errorf("built = %v: fs must precede store", tb.journal.built)
	}
}

// TestBuild_CloseReversesConstructionOrder: a resource must never
// outlive something it depends on, so close runs backwards.
func TestBuild_CloseReversesConstructionOrder(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fsAt, boxAt := -1, -1
	for i, name := range tb.journal.closed {
		switch name {
		case "fs":
			fsAt = i
		case "box":
			boxAt = i
		}
	}
	if fsAt < 0 || boxAt < 0 {
		t.Fatalf("closed = %v, want fs and box", tb.journal.closed)
	}
	if boxAt > fsAt {
		t.Errorf("closed = %v: box must close before fs", tb.journal.closed)
	}
}

func TestResultAccessorsAndCloseAreTypedStableAndIdempotent(t *testing.T) {
	firstErr := errors.New("first close")
	secondErr := errors.New("second close")
	var closed []string
	first := &closeRecorder{name: "first", journal: &closed, err: firstErr}
	second := &closeRecorder{name: "second", journal: &closed, err: secondErr}

	b := deploy.NewBuilder()
	engine := &fakeEngineFactory{spec: sdkconfig.Spec{
		Kind: "closer-test",
		Deps: []sdkconfig.DepSpec{
			{Name: "first", Type: "first.Kind", Required: true},
			{Name: "second", Type: "second.Kind", Required: true},
		},
	}}
	if err := b.RegisterEngine(engine); err != nil {
		t.Fatal(err)
	}
	b.MustRegisterResource(resourceFactory(
		"first.Kind", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return first, nil },
	))
	b.MustRegisterResource(resourceFactory(
		"second.Kind", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return second, nil },
	))
	doc := parse(t, `
version: v1
resources:
  second: {kind: second.Kind, impl: fake}
  first: {kind: first.Kind, impl: fake}
agents:
  a:
    engine: {kind: closer-test}
    deps: {first: first, second: second}
`)
	res, err := b.Build(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.ResourceNames(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("ResourceNames = %v", got)
	}
	if got, err := deploy.ResourceAs[*closeRecorder](res, "first"); err != nil || got != first {
		t.Fatalf("ResourceAs(first) = (%v, %v)", got, err)
	}
	if _, err := deploy.ResourceAs[string](res, "first"); err == nil ||
		!errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), `"first"`) ||
		!strings.Contains(err.Error(), "*deploy_test.closeRecorder") ||
		!strings.Contains(err.Error(), "string") {
		t.Fatalf("ResourceAs wrong-type error = %v", err)
	}
	if _, err := deploy.ResourceAs[*closeRecorder](res, "missing"); err == nil ||
		!errdefs.IsNotFound(err) ||
		!strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("ResourceAs missing error = %v", err)
	}

	err = res.Close()
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("Close error = %v, want both close errors", err)
	}
	if !reflect.DeepEqual(closed, []string{"second", "first"}) {
		t.Fatalf("closed = %v, want reverse construction order", closed)
	}
	err2 := res.Close()
	if !errors.Is(err2, firstErr) || !errors.Is(err2, secondErr) {
		t.Fatalf("second Close error = %v, want cached joined error", err2)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("close calls = (%d, %d), want (1, 1)", first.calls, second.calls)
	}
}

func TestResultCloseResourceThenClose(t *testing.T) {
	var closed []string
	secondErr := errors.New("second close failed")
	first := &closeRecorder{name: "first", journal: &closed}
	second := &closeRecorder{name: "second", journal: &closed, err: secondErr}
	res := buildOwnedResult(t, map[string]any{"first": first, "second": second})

	resourceErr := res.CloseResource("second")
	if !errors.Is(resourceErr, secondErr) {
		t.Fatalf("CloseResource(second) error = %v, want %v", resourceErr, secondErr)
	}
	if err := res.Close(); !errors.Is(err, resourceErr) {
		t.Fatalf("Close error = %v, want cached resource error %v", err, resourceErr)
	}
	if !reflect.DeepEqual(closed, []string{"second", "first"}) {
		t.Fatalf("closed = %v, want individually closed resource skipped by Close", closed)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("close calls = (%d, %d), want (1, 1)", first.calls, second.calls)
	}
}

func TestResultCloseThenCloseResourceReturnsStableError(t *testing.T) {
	closeErr := errors.New("close failed")
	resource := &closeRecorder{err: closeErr}
	res := buildOwnedResult(t, map[string]any{"runtime": resource})

	allErr := res.Close()
	firstErr := res.CloseResource("runtime")
	secondErr := res.CloseResource("runtime")
	if !errors.Is(allErr, closeErr) || !errors.Is(firstErr, closeErr) {
		t.Fatalf("close errors = (%v, %v), want stable underlying error", allErr, firstErr)
	}
	if firstErr != secondErr {
		t.Fatalf("CloseResource errors are not stable: %p != %p", firstErr, secondErr)
	}
	if !errors.Is(allErr, firstErr) {
		t.Fatalf("Close error = %v, want cached resource error %v", allErr, firstErr)
	}
	if resource.calls != 1 {
		t.Fatalf("close calls = %d, want 1", resource.calls)
	}
}

func TestResultCloseAndCloseResourceConcurrent(t *testing.T) {
	closeErr := errors.New("close failed")
	resource := &closeRecorder{err: closeErr}
	res := buildOwnedResult(t, map[string]any{"runtime": resource})

	const goroutines = 64
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				errs <- res.Close()
				return
			}
			errs <- res.CloseResource("runtime")
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if !errors.Is(err, closeErr) {
			t.Errorf("concurrent close error = %v, want %v", err, closeErr)
		}
	}
	if resource.calls != 1 {
		t.Fatalf("close calls = %d, want 1", resource.calls)
	}
}

func TestResultCloseResourceNonCloserAndMissing(t *testing.T) {
	res := buildOwnedResult(t, map[string]any{"config": struct{}{}})
	if err := res.CloseResource("config"); err != nil {
		t.Fatalf("CloseResource(non-closer): %v", err)
	}
	if err := res.CloseResource("missing"); err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("CloseResource(missing) error = %v, want not found", err)
	}
	var nilResult *deploy.Result
	if err := nilResult.CloseResource("missing"); err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("nil Result.CloseResource error = %v, want not found", err)
	}
}

func TestResultCloseResourceRejectsActiveTransitiveDependents(t *testing.T) {
	base := &closeRecorder{name: "base"}
	middle := &closeRecorder{name: "middle"}
	leaf := &closeRecorder{name: "leaf"}
	res := buildDependencyResult(t,
		map[string]any{"base": base, "middle": middle, "leaf": leaf},
		map[string][]string{"middle": {"base"}, "leaf": {"middle"}},
	)

	if err := res.CloseResource("base"); err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("CloseResource(base) error = %v, want conflict", err)
	}
	if base.calls != 0 {
		t.Fatalf("base close calls = %d, want 0", base.calls)
	}
	if err := res.CloseResource("leaf"); err != nil {
		t.Fatalf("CloseResource(leaf): %v", err)
	}
	if err := res.CloseResource("middle"); err != nil {
		t.Fatalf("CloseResource(middle): %v", err)
	}
	if err := res.CloseResource("base"); err != nil {
		t.Fatalf("CloseResource(base) after dependents: %v", err)
	}
	if base.calls != 1 || middle.calls != 1 || leaf.calls != 1 {
		t.Fatalf("close calls = (%d, %d, %d), want (1, 1, 1)",
			base.calls, middle.calls, leaf.calls)
	}
}

func TestResultCloseResourceCountsNonCloserDependentAsActive(t *testing.T) {
	base := &closeRecorder{}
	res := buildDependencyResult(t,
		map[string]any{"base": base, "consumer": struct{}{}},
		map[string][]string{"consumer": {"base"}},
	)

	if err := res.CloseResource("base"); err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("CloseResource(base) error = %v, want conflict", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base close calls = %d, want Result.Close to bypass conflict", base.calls)
	}
}

func TestResultCloseResourceConcurrentWithClosePreservesDependencyOrder(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var closed []string
	base := &closeRecorder{name: "base", journal: &closed}
	dependent := &closeRecorder{
		name: "dependent", journal: &closed, started: started, release: release,
	}
	res := buildDependencyResult(t,
		map[string]any{"base": base, "dependent": dependent},
		map[string][]string{"dependent": {"base"}},
	)

	dependentErr := make(chan error, 1)
	go func() {
		dependentErr <- res.CloseResource("dependent")
	}()
	<-started

	if err := res.CloseResource("base"); err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("CloseResource(base) while dependent closes = %v, want conflict", err)
	}
	closeErr := make(chan error, 1)
	go func() {
		closeErr <- res.Close()
	}()
	close(release)

	if err := <-dependentErr; err != nil {
		t.Fatalf("CloseResource(dependent): %v", err)
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !reflect.DeepEqual(closed, []string{"dependent", "base"}) {
		t.Fatalf("closed = %v, want dependency-safe order", closed)
	}
	if base.calls != 1 || dependent.calls != 1 {
		t.Fatalf("close calls = (%d, %d), want (1, 1)", base.calls, dependent.calls)
	}
}

func TestResultCloseResourceWaitsForResultCloseOrder(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var closed []string
	base := &closeRecorder{name: "base", journal: &closed}
	dependent := &closeRecorder{
		name: "dependent", journal: &closed, started: started, release: release,
	}
	res := buildDependencyResult(t,
		map[string]any{"base": base, "dependent": dependent},
		map[string][]string{"dependent": {"base"}},
	)

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- res.Close()
	}()
	<-started

	baseErr := make(chan error, 1)
	go func() {
		baseErr <- res.CloseResource("base")
	}()
	close(release)

	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-baseErr; err != nil {
		t.Fatalf("CloseResource(base): %v", err)
	}
	if !reflect.DeepEqual(closed, []string{"dependent", "base"}) {
		t.Fatalf("closed = %v, want Result.Close order", closed)
	}
	if base.calls != 1 || dependent.calls != 1 {
		t.Fatalf("close calls = (%d, %d), want (1, 1)", base.calls, dependent.calls)
	}
}

func TestResultCloseBorrowsSourcesAndClosesOnlyContainer(t *testing.T) {
	item := &closeRecorder{}
	source := &closeRecorder{}
	container := &closingResolver{item: item}

	b := deploy.NewBuilder()
	engine := &fakeEngineFactory{spec: sdkconfig.Spec{
		Kind: "ownership-test",
		Deps: []sdkconfig.DepSpec{
			{Name: "item", Type: "item.Kind", Required: true},
			{Name: "source", Type: "source.Kind", Required: true},
		},
	}}
	if err := b.RegisterEngine(engine); err != nil {
		t.Fatal(err)
	}
	b.MustRegisterResource(resourceFactory(
		"container.Kind", "fake", "item.Kind", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return container, nil },
	))
	b.RegisterSource("host.source", func(context.Context, string) (any, error) {
		return source, nil
	})
	doc := parse(t, `
version: v1
resources:
  container: {kind: container.Kind, impl: fake}
agents:
  a:
    engine: {kind: ownership-test}
    deps:
      item: container/item
      source: {source: host.source}
`)
	res, err := b.Build(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.CloseResource("container"); err != nil {
		t.Fatal(err)
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	if container.calls != 1 {
		t.Fatalf("container close calls = %d, want 1", container.calls)
	}
	if item.calls != 0 {
		t.Fatalf("item close calls = %d, want 0", item.calls)
	}
	if source.calls != 0 {
		t.Fatalf("source close calls = %d, want 0", source.calls)
	}
}

func TestBuild_FailureJoinsCleanupErrors(t *testing.T) {
	buildErr := errors.New("factory failed")
	firstCloseErr := errors.New("first close failed")
	secondCloseErr := errors.New("second close failed")
	first := &closeRecorder{err: firstCloseErr}
	second := &closeRecorder{err: secondCloseErr}

	b := deploy.NewBuilder()
	b.MustRegisterResource(resourceFactory(
		"first.Kind", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return first, nil },
	))
	b.MustRegisterResource(resourceFactory(
		"second.Kind", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return second, nil },
	))
	b.MustRegisterResource(resourceFactory(
		"failure.Kind", "fake", "", []sdkconfig.DepSpec{
			{Name: "first", Type: "first.Kind", Required: true},
			{Name: "second", Type: "second.Kind", Required: true},
		}, func(context.Context, sdkconfig.Input) (any, error) {
			return nil, buildErr
		},
	))
	doc := parse(t, `
version: v1
resources:
  first: {kind: first.Kind, impl: fake}
  second: {kind: second.Kind, impl: fake}
  failure:
    kind: failure.Kind
    impl: fake
    deps: {first: first, second: second}
agents: {}
`)
	_, err := b.Build(context.Background(), doc)
	if !errors.Is(err, buildErr) ||
		!errors.Is(err, firstCloseErr) ||
		!errors.Is(err, secondCloseErr) {
		t.Fatalf("Build error = %v, want build and both cleanup errors", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("close calls = (%d, %d), want (1, 1)", first.calls, second.calls)
	}
}

func TestBuild_ResourceCycleFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  a: {kind: workspace.Registry, impl: fake, deps: {peer: b}}
  b: {kind: workspace.Registry, impl: fake, deps: {peer: a}}
agents: {}
`)
	_, err := tb.Build(context.Background(), doc)
	if err == nil {
		t.Fatal("dependency cycle must fail Build")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to mention a cycle", err)
	}
}

func TestBuild_ResourceDepOnUndefinedResourceFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  a: {kind: workspace.Registry, impl: fake, deps: {peer: ghost}}
agents: {}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("resource dep on an undefined resource must fail Build")
	}
}

// TestBuild_HookDepsReachResources is what makes the resource area
// useful beyond engines: persistence / observer wiring lives in
// the hook layer, so hooks must be able to bind resources too.
func TestBuild_HookDepsReachResources(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	if tb.hooks.before == nil || tb.hooks.before.store == nil {
		t.Fatal("before factory did not receive the store resource")
	}
	if tb.hooks.before.window != 20 {
		t.Errorf("before.window = %d, want 20", tb.hooks.before.window)
	}
	if tb.hooks.hook == nil || tb.hooks.hook.store == nil {
		t.Fatal("hook factory did not receive the store resource")
	}
	if tb.hooks.committer == nil || tb.hooks.committer.store == nil {
		t.Fatal("committer factory did not receive the store resource")
	}
	// The hook's store is the same instance the resource area built,
	// and it resolved its own workspace item dep.
	store, err := deploy.ResourceAs[*fakeStore](res, "store")
	if err != nil {
		t.Fatalf("ResourceAs(store): %v", err)
	}
	if tb.hooks.hook.store != store {
		t.Error("hook store should be the resource-area instance")
	}
	if tb.hooks.committer.store != store {
		t.Error("committer store should be the resource-area instance")
	}
	if got := store.workspace; got != "fs:project" {
		t.Errorf("store.workspace = %q, want fs:project", got)
	}
}

// TestBuild_ResourceBoundOnlyByHookCountsAsUsed: the dead-config rule
// must count every consumer, not only agent deps, or a store bound
// exclusively by a hook would be rejected.
func TestBuild_ResourceBoundOnlyByHookCountsAsUsed(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
  store: {kind: fake.Store, impl: fake, deps: {workspace: fs/project}}
agents:
  a:
    engine: {kind: inline}
    observe:
      - {type: fake_hook, deps: {store: store}}
`)
	res, err := tb.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
}

func TestBuild_UnusedResourceFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  orphan: {kind: script.runtime, impl: fakejs, settings: {pool_size: 1}}
agents:
  a:
    engine: {kind: inline}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("a resource nothing binds must fail Build")
	}
}

func TestBuild_ExportedResourceIsApplicationRoot(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  runtime:
    kind: script.runtime
    impl: fakejs
    export: true
    settings: {pool_size: 1}
agents: {}
`)
	result, err := tb.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })
	if _, err := deploy.ResourceAs[*fakeJSRuntime](result, "runtime"); err != nil {
		t.Fatalf("ResourceAs: %v", err)
	}
}

func TestBuild_ExternalResourceConsumerRetainsUnexportedResource(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  runtime:
    kind: script.runtime
    impl: fakejs
    settings: {pool_size: 1}
agents: {}
`)
	result, err := tb.Build(
		context.Background(),
		doc,
		deploy.WithExternalResourceConsumers("runtime"),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })

	got, err := result.Resource("runtime")
	if err != nil {
		t.Fatalf("Resource(runtime): %v", err)
	}
	if _, ok := got.(*fakeJSRuntime); !ok {
		t.Fatalf("Resource(runtime) = %T, want *fakeJSRuntime", got)
	}
}

func TestBuild_UnknownExternalResourceConsumerFailsBeforeConstruction(t *testing.T) {
	calls := 0
	b := deploy.NewBuilder()
	b.MustRegisterResource(resourceFactory(
		"runtime.Kind", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) {
			calls++
			return struct{}{}, nil
		},
	))
	doc := parse(t, `
version: v1
resources:
  runtime: {kind: runtime.Kind, impl: fake, export: true}
agents: {}
`)

	_, err := b.Build(
		context.Background(),
		doc,
		deploy.WithExternalResourceConsumers("missing"),
	)
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("Build error = %v, want not found", err)
	}
	if calls != 0 {
		t.Fatalf("resource factory calls = %d, want 0", calls)
	}
}

func TestBuild_OriginalCallAndBorrowedResourceErrors(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  runtime: {kind: script.runtime, impl: fakejs, export: true}
agents: {}
`)

	result, err := tb.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("original Build call: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })

	if _, err := result.Resource("missing"); err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("Resource(missing) error = %v, want not found", err)
	}
	var nilResult *deploy.Result
	if _, err := nilResult.Resource("runtime"); err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("nil Result.Resource error = %v, want not found", err)
	}
}

// TestBuild_ItemRefOnNonContainerFails: an inference runtime or tool
// assembly is a single object; addressing an item inside one is a
// wiring mistake and must be caught at build time.
func TestBuild_ItemRefOnNonContainerFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  js_main: {kind: script.runtime, impl: fakejs, settings: {pool_size: 1}}
agents:
  a:
    engine: {kind: graph}
    deps:
      workspace: js_main/nope
`)
	_, err := tb.Build(context.Background(), doc)
	if err == nil {
		t.Fatal("item ref on a non-container resource must fail Build")
	}
	if !strings.Contains(err.Error(), "not a container") {
		t.Errorf("error = %v, want it to say the resource is not a container", err)
	}
}

func TestBuild_UnknownItemInContainerFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
agents:
  a:
    engine: {kind: graph}
    deps:
      workspace: fs/ghost
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unknown item inside a container must fail Build")
	}
}

// TestBuild_WholeBindingChecksKindAgainstDepSpec: whole binding is the
// only form where the declared category is verifiable, so it must be
// verified.
func TestBuild_WholeBindingChecksKindAgainstDepSpec(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  js_main: {kind: script.runtime, impl: fakejs, settings: {pool_size: 1}}
agents:
  a:
    engine: {kind: graph}
    deps:
      workspace: js_main
`)
	_, err := tb.Build(context.Background(), doc)
	if err == nil {
		t.Fatal("kind/DepSpec.Type mismatch must fail Build")
	}
	if !strings.Contains(err.Error(), "dep expects") {
		t.Errorf("error = %v, want a kind mismatch message", err)
	}
}

func TestBuild_FailureClosesAlreadyBuiltResources(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
agents:
  a:
    engine: {kind: ghost}
    deps:
      workspace: fs/project
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered engine kind must fail Build")
	}
	found := false
	for _, name := range tb.journal.closed {
		if name == "fs" {
			found = true
		}
	}
	if !found {
		t.Errorf("closed = %v, want fs closed after the failed build", tb.journal.closed)
	}
}

func TestBuild_UnknownResourceKindImplFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, `
version: v1
resources:
  x: {kind: workspace.Registry, impl: nope}
agents: {}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered (kind, impl) must fail Build")
	}
}

func TestBuild_DepValidation(t *testing.T) {
	tb := newTestBuilder(t)

	for name, body := range map[string]string{
		"undeclared dep": `
version: v1
resources:
  fs: {kind: workspace.Registry, impl: fake, settings: {names: [project]}}
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: fs/project, ghost: fs/project}
`,
		"missing required dep": `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps: {tools: {source: host.tools}}
`,
		"unknown source": `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: {source: nowhere}}
`,
		"undefined resource": `
version: v1
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: ghost/x}
`,
	} {
		if _, err := tb.Build(context.Background(), parse(t, body)); err == nil {
			t.Errorf("%s: Build succeeded, want error", name)
		}
	}
}

func TestBuild_ResourceDepSpecRejectsUnknownAndMissingDepsBeforeNew(t *testing.T) {
	for name, tc := range map[string]struct {
		deps       string
		classified func(error) bool
	}{
		"unknown": {
			deps:       "{required: whole, ghost: whole}",
			classified: errdefs.IsValidation,
		},
		"missing required": {
			deps:       "{}",
			classified: errdefs.IsNotFound,
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := deploy.NewBuilder()
			b.MustRegisterResource(resourceFactory(
				"whole.Kind", "fake", "", nil,
				func(context.Context, sdkconfig.Input) (any, error) { return struct{}{}, nil },
			))
			calls := 0
			b.MustRegisterResource(resourceFactory(
				"consumer.Kind", "fake", "", []sdkconfig.DepSpec{
					{Name: "required", Type: "whole.Kind", Required: true},
				}, func(context.Context, sdkconfig.Input) (any, error) {
					calls++
					return struct{}{}, nil
				},
			))
			doc := parse(t, `
version: v1
resources:
  whole: {kind: whole.Kind, impl: fake}
  consumer: {kind: consumer.Kind, impl: fake, deps: `+tc.deps+`}
agents: {}
`)
			_, err := b.Build(context.Background(), doc)
			if err == nil || !tc.classified(err) {
				t.Fatalf("Build error = %v, want expected classification", err)
			}
			if calls != 0 {
				t.Fatalf("consumer factory New calls = %d, want 0", calls)
			}
		})
	}
}

func TestBuild_ResourceDepChecksWholeKind(t *testing.T) {
	b := deploy.NewBuilder()
	b.MustRegisterResource(resourceFactory(
		"actual.Kind", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return struct{}{}, nil },
	))
	calls := 0
	b.MustRegisterResource(resourceFactory(
		"consumer.Kind", "fake", "", []sdkconfig.DepSpec{
			{Name: "dep", Type: "expected.Kind", Required: true},
		}, func(context.Context, sdkconfig.Input) (any, error) {
			calls++
			return struct{}{}, nil
		},
	))
	doc := parse(t, `
version: v1
resources:
  actual: {kind: actual.Kind, impl: fake}
  consumer: {kind: consumer.Kind, impl: fake, deps: {dep: actual}}
agents: {}
`)
	if _, err := b.Build(context.Background(), doc); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
	if calls != 0 {
		t.Fatalf("consumer factory New calls = %d, want 0", calls)
	}
}

func TestBuild_ItemTypeMismatchDoesNotCallResolver(t *testing.T) {
	resolver := &countingResolver{item: "item"}
	b := deploy.NewBuilder()
	b.MustRegisterResource(resourceFactory(
		"container.Kind", "fake", "actual.Item", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return resolver, nil },
	))
	b.MustRegisterResource(resourceFactory(
		"consumer.Kind", "fake", "", []sdkconfig.DepSpec{
			{Name: "dep", Type: "expected.Item", Required: true},
		}, func(context.Context, sdkconfig.Input) (any, error) {
			return struct{}{}, nil
		},
	))
	doc := parse(t, `
version: v1
resources:
  container: {kind: container.Kind, impl: fake}
  consumer: {kind: consumer.Kind, impl: fake, deps: {dep: container/item}}
agents: {}
`)
	if _, err := b.Build(context.Background(), doc); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("ResolveItem calls = %d, want 0", resolver.calls)
	}
}

func TestBuild_HookItemRefRequiresDeclaredItemType(t *testing.T) {
	tb := newTestBuilder(t)
	resolver := &countingResolver{item: &fakeStore{}}
	tb.MustRegisterResource(resourceFactory(
		"undeclared.Container", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return resolver, nil },
	))
	doc := parse(t, `
version: v1
resources:
  container: {kind: undeclared.Container, impl: fake}
agents:
  a:
    engine: {kind: inline}
    observe:
      - {type: fake_hook, deps: {store: container/item}}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("ResolveItem calls = %d, want 0", resolver.calls)
	}
}

func TestBuild_DeclaredItemTypeRequiresItemResolver(t *testing.T) {
	tb := newTestBuilder(t)
	tb.MustRegisterResource(resourceFactory(
		"broken.Container", "fake", "workspace.Workspace", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return struct{}{}, nil },
	))
	doc := parse(t, `
version: v1
resources:
  container: {kind: broken.Container, impl: fake}
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: container/item}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("Build error = %v, want internal", err)
	}
}

func TestBuild_RejectsTypedNilItem(t *testing.T) {
	tb := newTestBuilder(t)
	var item *fakeStore
	resolver := &countingResolver{item: item}
	tb.MustRegisterResource(resourceFactory(
		"nilitem.Container", "fake", "workspace.Workspace", nil,
		func(context.Context, sdkconfig.Input) (any, error) { return resolver, nil },
	))
	doc := parse(t, `
version: v1
resources:
  container: {kind: nilitem.Container, impl: fake}
agents:
  a:
    engine: {kind: graph}
    deps: {workspace: container/item}
`)
	if _, err := tb.Build(context.Background(), doc); err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("Build error = %v, want internal", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("ResolveItem calls = %d, want 1", resolver.calls)
	}
}

func TestBuild_RejectsTypedNilResource(t *testing.T) {
	b := deploy.NewBuilder()
	b.MustRegisterResource(resourceFactory(
		"nil.Kind", "fake", "", nil,
		func(context.Context, sdkconfig.Input) (any, error) {
			var resource *closeRecorder
			return resource, nil
		},
	))
	doc := parse(t, `
version: v1
resources:
  nil_resource: {kind: nil.Kind, impl: fake}
agents: {}
`)
	if _, err := b.Build(context.Background(), doc); err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("Build error = %v, want internal", err)
	}
}

func TestBuild_UnknownHookTypeFails(t *testing.T) {
	tb := newTestBuilder(t)
	doc := parse(t, "version: v1\nagents:\n  a:\n    engine: {kind: inline}\n    observe: [{type: ghost}]\n")
	if _, err := tb.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered hook type must fail Build")
	}
}

func TestBuild_BuiltinDiscardOnInterrupt(t *testing.T) {
	tb := newTestBuilder(t)
	res, err := tb.Build(context.Background(), loadDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	intr := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "partial"))
		return b, agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput})
	})
	inst, ok := res.Instance("researcher")
	if !ok {
		t.Fatal("Instance(researcher) missing")
	}
	inst.Engine = intr

	execRes, err := inst.Execute(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if execRes.Status != agent.StatusInterrupted {
		t.Fatalf("Status = %q", execRes.Status)
	}
	if execRes.Committed {
		t.Error("builtin discard_on_interrupt must set Committed=false on user_input")
	}
}

func TestDecodeSettings_StrictAndNilSafe(t *testing.T) {
	type s struct {
		Window int `json:"window"`
	}
	if v, err := sdkconfig.DecodeSettings[s](nil); err != nil || v.Window != 0 {
		t.Fatalf("nil node: (%v, %v)", v, err)
	}

	var settings json.RawMessage
	if err := json.Unmarshal([]byte(`{"window":3}`), &settings); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, err := sdkconfig.DecodeSettings[s](settings)
	if err != nil || v.Window != 3 {
		t.Fatalf("known field: (%v, %v)", v, err)
	}

	if err := json.Unmarshal([]byte(`{"windo":3}`), &settings); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := sdkconfig.DecodeSettings[s](settings); err == nil {
		t.Fatal("typo key must fail strict decode")
	}
}
