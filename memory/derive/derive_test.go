package derive

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

type fakeDeriver struct {
	mu    sync.Mutex
	calls []string
	fn    func(component.Artifact) ([]component.Artifact, error)
}

func (deriver *fakeDeriver) Derive(_ context.Context, input component.Artifact) ([]component.Artifact, error) {
	deriver.mu.Lock()
	deriver.calls = append(deriver.calls, input.ID)
	deriver.mu.Unlock()
	return deriver.fn(input)
}

func (deriver *fakeDeriver) callIDs() []string {
	deriver.mu.Lock()
	defer deriver.mu.Unlock()
	return append([]string(nil), deriver.calls...)
}

func TestBuildValidationAndDeterministicOrder(t *testing.T) {
	registry := registryWith(t, map[string]*fakeDeriver{
		"ok": {fn: passthrough("out")},
	})
	tests := []struct {
		name string
		spec Spec
	}{
		{"no nodes", Spec{}},
		{"empty id", Spec{Nodes: []NodeSpec{{Deriver: component.Spec{Name: "ok"}}}}},
		{"missing factory", Spec{Nodes: []NodeSpec{{ID: "node", Deriver: component.Spec{Name: "missing"}}}}},
		{"duplicate id", Spec{Nodes: []NodeSpec{
			{ID: "same", Deriver: component.Spec{Name: "ok"}},
			{ID: "same", Deriver: component.Spec{Name: "ok"}},
		}}},
		{"missing dependency", Spec{Nodes: []NodeSpec{
			{ID: "node", Deriver: component.Spec{Name: "ok"}, DependsOn: []string{"missing"}},
		}}},
		{"self dependency", Spec{Nodes: []NodeSpec{
			{ID: "node", Deriver: component.Spec{Name: "ok"}, DependsOn: []string{"node"}},
		}}},
		{"duplicate dependency", Spec{Nodes: []NodeSpec{
			{ID: "root", Deriver: component.Spec{Name: "ok"}},
			{ID: "node", Deriver: component.Spec{Name: "ok"}, DependsOn: []string{"root", "root"}},
		}}},
		{"cycle", Spec{Nodes: []NodeSpec{
			{ID: "a", Deriver: component.Spec{Name: "ok"}, DependsOn: []string{"b"}},
			{ID: "b", Deriver: component.Spec{Name: "ok"}, DependsOn: []string{"a"}},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Build(registry, test.spec); err == nil {
				t.Fatal("Build error = nil")
			}
		})
	}

	dag, err := Build(registry, Spec{Nodes: []NodeSpec{
		{ID: "z", Deriver: component.Spec{Name: "ok"}, DependsOn: []string{"b", "a"}},
		{ID: "b", Deriver: component.Spec{Name: "ok"}},
		{ID: "a", Deriver: component.Spec{Name: "ok"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := dag.TopologicalOrder(); !reflect.DeepEqual(got, []string{"a", "b", "z"}) {
		t.Fatalf("topological order = %v", got)
	}
}

func TestRunHappyPathAndMultiInputOrder(t *testing.T) {
	a := &fakeDeriver{fn: func(input component.Artifact) ([]component.Artifact, error) {
		return []component.Artifact{
			derived("view", "a1", input),
			derived("view", "a2", input),
		}, nil
	}}
	b := &fakeDeriver{fn: func(input component.Artifact) ([]component.Artifact, error) {
		return []component.Artifact{derived("view", "b1", input)}, nil
	}}
	join := &fakeDeriver{fn: func(input component.Artifact) ([]component.Artifact, error) {
		return []component.Artifact{derived("projection", "joined-"+input.ID, input)}, nil
	}}
	dag := buildDAG(t, map[string]*fakeDeriver{"a": a, "b": b, "join": join}, Spec{Nodes: []NodeSpec{
		{ID: "join", Deriver: component.Spec{Name: "join"}, DependsOn: []string{"b", "a"}},
		{ID: "b", Deriver: component.Spec{Name: "b"}},
		{ID: "a", Deriver: component.Spec{Name: "a"}},
	}})

	result, err := dag.Run(context.Background(), artifact("source"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "join"} {
		if got := mustNode(t, result, id).Status; got != StatusSuccess {
			t.Fatalf("%s status = %s", id, got)
		}
	}
	if got := join.callIDs(); !reflect.DeepEqual(got, []string{"b1", "a1", "a2"}) {
		t.Fatalf("join input order = %v", got)
	}
	if got := len(mustNode(t, result, "join").Artifacts); got != 3 {
		t.Fatalf("join artifacts = %d", got)
	}
}

func TestRunIsolatesFailedBranch(t *testing.T) {
	failure := errors.New("branch failed")
	fail := &fakeDeriver{fn: func(component.Artifact) ([]component.Artifact, error) {
		return nil, failure
	}}
	child := &fakeDeriver{fn: passthrough("child")}
	ok := &fakeDeriver{fn: passthrough("ok")}
	dag := buildDAG(t, map[string]*fakeDeriver{"fail": fail, "child": child, "ok": ok}, Spec{Nodes: []NodeSpec{
		{ID: "fail", Deriver: component.Spec{Name: "fail"}},
		{ID: "child", Deriver: component.Spec{Name: "child"}, DependsOn: []string{"fail"}},
		{ID: "ok", Deriver: component.Spec{Name: "ok"}},
	}})
	result, err := dag.Run(context.Background(), artifact("source"))
	if err != nil {
		t.Fatal(err)
	}
	failed := mustNode(t, result, "fail")
	if failed.Status != StatusFailed || !errors.Is(failed.Err, failure) || failed.Error == "" {
		t.Fatalf("failed result = %#v", failed)
	}
	blocked := mustNode(t, result, "child")
	if blocked.Status != StatusBlocked || !reflect.DeepEqual(blocked.BlockedBy, []string{"fail"}) || len(child.callIDs()) != 0 {
		t.Fatalf("child result = %#v calls=%v", blocked, child.callIDs())
	}
	if success := mustNode(t, result, "ok"); success.Status != StatusSuccess || len(ok.callIDs()) != 1 {
		t.Fatalf("independent result = %#v calls=%v", success, ok.callIDs())
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || !containsString(string(data), `"error"`) {
		t.Fatalf("serialized result = %s", data)
	}
}

func TestRunRejectsBadArtifactAndDiscardsPartialOutputs(t *testing.T) {
	bad := &fakeDeriver{fn: func(input component.Artifact) ([]component.Artifact, error) {
		valid := derived("view", "valid", input)
		invalid := derived("view", "invalid", input)
		invalid.Sources = nil
		return []component.Artifact{valid, invalid}, nil
	}}
	dag := buildDAG(t, map[string]*fakeDeriver{"bad": bad}, Spec{Nodes: []NodeSpec{
		{ID: "bad", Deriver: component.Spec{Name: "bad"}},
	}})
	result, err := dag.Run(context.Background(), artifact("source"))
	if err != nil {
		t.Fatal(err)
	}
	node := mustNode(t, result, "bad")
	if node.Status != StatusFailed || len(node.Artifacts) != 0 || node.Error == "" {
		t.Fatalf("bad artifact result = %#v", node)
	}
}

func TestRunMarksUnexecutedNodesCancelled(t *testing.T) {
	first := &fakeDeriver{fn: passthrough("first")}
	second := &fakeDeriver{fn: passthrough("second")}
	dag := buildDAG(t, map[string]*fakeDeriver{"first": first, "second": second}, Spec{Nodes: []NodeSpec{
		{ID: "first", Deriver: component.Spec{Name: "first"}},
		{ID: "second", Deriver: component.Spec{Name: "second"}, DependsOn: []string{"first"}},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := dag.Run(ctx, artifact("source"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		node := mustNode(t, result, id)
		if node.Status != StatusBlocked || !node.Cancelled {
			t.Fatalf("%s result = %#v", id, node)
		}
	}
	if len(first.callIDs()) != 0 || len(second.callIDs()) != 0 {
		t.Fatalf("cancelled run invoked derivers: first=%v second=%v", first.callIDs(), second.callIDs())
	}
}

func TestRetryExecutesCancelledNodes(t *testing.T) {
	first := &fakeDeriver{fn: passthrough("first")}
	second := &fakeDeriver{fn: passthrough("second")}
	dag := buildDAG(t, map[string]*fakeDeriver{"first": first, "second": second}, Spec{Nodes: []NodeSpec{
		{ID: "first", Deriver: component.Spec{Name: "first"}},
		{ID: "second", Deriver: component.Spec{Name: "second"}, DependsOn: []string{"first"}},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, err := dag.Run(ctx, artifact("source"))
	if err != nil {
		t.Fatal(err)
	}

	retried, err := dag.Retry(context.Background(), cancelled)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if got := mustNode(t, retried, id).Status; got != StatusSuccess {
			t.Fatalf("%s retry status = %s", id, got)
		}
	}
	if len(first.callIDs()) != 1 || len(second.callIDs()) != 1 {
		t.Fatalf("retry calls: first=%v second=%v", first.callIDs(), second.callIDs())
	}
}

func TestRetryReusesSuccessAndRecoversDescendants(t *testing.T) {
	stable := &fakeDeriver{fn: passthrough("stable")}
	attempt := 0
	flaky := &fakeDeriver{fn: func(input component.Artifact) ([]component.Artifact, error) {
		attempt++
		if attempt == 1 {
			return nil, errors.New("transient")
		}
		return []component.Artifact{derived("projection", "recovered", input)}, nil
	}}
	child := &fakeDeriver{fn: passthrough("context")}
	dag := buildDAG(t, map[string]*fakeDeriver{"stable": stable, "flaky": flaky, "child": child}, Spec{Nodes: []NodeSpec{
		{ID: "stable", Deriver: component.Spec{Name: "stable"}},
		{ID: "flaky", Deriver: component.Spec{Name: "flaky"}, DependsOn: []string{"stable"}},
		{ID: "child", Deriver: component.Spec{Name: "child"}, DependsOn: []string{"flaky"}},
	}})
	first, err := dag.Run(context.Background(), artifact("source"))
	if err != nil {
		t.Fatal(err)
	}
	if mustNode(t, first, "flaky").Status != StatusFailed || mustNode(t, first, "child").Status != StatusBlocked {
		t.Fatalf("first result = %#v", first)
	}
	second, err := dag.Retry(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"stable", "flaky", "child"} {
		if got := mustNode(t, second, id).Status; got != StatusSuccess {
			t.Fatalf("%s retry status = %s", id, got)
		}
	}
	if got := stable.callIDs(); !reflect.DeepEqual(got, []string{"source"}) {
		t.Fatalf("successful node repeated: %v", got)
	}
	if got := flaky.callIDs(); !reflect.DeepEqual(got, []string{"stable", "stable"}) {
		t.Fatalf("flaky calls = %v", got)
	}
	if got := child.callIDs(); !reflect.DeepEqual(got, []string{"recovered"}) {
		t.Fatalf("child calls = %v", got)
	}
	if mustNode(t, second, "stable").Attempt != 1 || mustNode(t, second, "flaky").Attempt != 2 || mustNode(t, second, "child").Attempt != 1 {
		t.Fatalf("retry attempts = %#v", second.Nodes)
	}
}

func TestRunOwnsSourceInputsAndOutputs(t *testing.T) {
	var returned []component.Artifact
	mutating := &fakeDeriver{fn: func(input component.Artifact) ([]component.Artifact, error) {
		input.Content.Parts[0] = sdkmessage.TextPart{Text: "input mutation"}
		input.Metadata["key"] = "input mutation"
		returned = []component.Artifact{derived("view", "output", input)}
		return returned, nil
	}}
	dag := buildDAG(t, map[string]*fakeDeriver{"mutating": mutating}, Spec{Nodes: []NodeSpec{
		{ID: "mutating", Deriver: component.Spec{Name: "mutating"}},
	}})
	source := artifact("source")
	source.Metadata = sdkmemory.Metadata{"key": "original"}
	result, err := dag.Run(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.Content.Text() != "source" || result.Source.Metadata["key"] != "original" {
		t.Fatalf("deriver mutated owned source: %#v", result.Source)
	}
	returned[0].Content.Parts[0] = sdkmessage.TextPart{Text: "late mutation"}
	returned[0].Metadata["key"] = "late mutation"
	node := mustNode(t, result, "mutating")
	if node.Artifacts[0].Content.Text() != "output" || node.Artifacts[0].Metadata["key"] != "input mutation" {
		t.Fatalf("result aliases deriver output: %#v", node.Artifacts[0])
	}
	cloned := result.Clone()
	cloned.Source.Content.Parts[0] = sdkmessage.TextPart{Text: "clone mutation"}
	cloned.Nodes[0].Artifacts[0].Content.Parts[0] = sdkmessage.TextPart{Text: "clone mutation"}
	if result.Source.Content.Text() != "source" || mustNode(t, result, "mutating").Artifacts[0].Content.Text() != "output" {
		t.Fatal("RunResult.Clone aliases original")
	}
}

func registryWith(t *testing.T, derivers map[string]*fakeDeriver) *component.Registry {
	t.Helper()
	registry := component.NewRegistry()
	kinds := []component.ArtifactKind{"source", "derived", "view", "projection", "context", "out"}
	for name, deriver := range derivers {
		value := deriver
		if err := component.RegisterTypedDeriver(
			registry,
			name,
			"test",
			component.Ports{Inputs: kinds, Outputs: kinds},
			func(struct{}) (component.Deriver, error) { return value, nil },
		); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func buildDAG(t *testing.T, derivers map[string]*fakeDeriver, spec Spec) *DAG {
	t.Helper()
	dag, err := Build(registryWith(t, derivers), spec)
	if err != nil {
		t.Fatal(err)
	}
	return dag
}

func artifact(id string) component.Artifact {
	return component.Artifact{
		Kind:    "source",
		ID:      id,
		Content: sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: id}}},
		Sources: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceDocument, ID: "document"}},
		Metadata: sdkmemory.Metadata{
			"key": "original",
		},
	}
}

func derived(kind component.ArtifactKind, id string, input component.Artifact) component.Artifact {
	return component.Artifact{
		Kind:     kind,
		ID:       id,
		Content:  sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: id}}},
		Sources:  append([]sdkmemory.SourceRef(nil), input.Sources...),
		Metadata: sdkmemory.Metadata{"key": input.Metadata["key"]},
	}
}

func passthrough(id string) func(component.Artifact) ([]component.Artifact, error) {
	return func(input component.Artifact) ([]component.Artifact, error) {
		return []component.Artifact{derived("derived", id, input)}, nil
	}
}

func mustNode(t *testing.T, result RunResult, id string) NodeResult {
	t.Helper()
	node, ok := result.Node(id)
	if !ok {
		t.Fatalf("node %q missing", id)
	}
	return node
}

func containsString(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
