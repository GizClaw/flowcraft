package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/storage"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type recordingStepConfig struct {
	Label string `json:"label"`
}

type recordingStep struct {
	mu    *sync.Mutex
	calls *[]string
	fail  *bool
}

func (step recordingStep) Run(ctx context.Context, state *RunState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	step.mu.Lock()
	*step.calls = append(*step.calls, state.NodeID)
	shouldFail := step.fail != nil && *step.fail
	step.mu.Unlock()
	if shouldFail {
		return errors.New("injected node failure")
	}
	return nil
}

type taskCloneMutatingStep struct {
	fail *bool
}

func (step taskCloneMutatingStep) Run(_ context.Context, state *RunState) error {
	task := state.Task()
	task.Branch = "mutated"
	task.Scope.RuntimeID = "mutated"
	if step.fail != nil && *step.fail {
		return errors.New("injected node failure")
	}
	return nil
}

func TestDefaultDAGOrder(t *testing.T) {
	dag, err := Build(nil, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"integrate", "compact", "decay", "forget", "repair"}
	got := dag.TopologicalOrder()
	if len(got) != len(want) {
		t.Fatalf("order=%v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("order=%v", got)
		}
	}
}

func TestTypedCustomStepRunsAndCompletedNodesReplaySelectively(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	fail := true
	catalog := NewCatalog()
	if err := RegisterTypedStep(catalog, "test.record", "v1", StepContract{},
		func(recordingStepConfig) (Step, error) {
			return recordingStep{mu: &mu, calls: &calls}, nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterTypedStep(catalog, "test.fail", "v1", StepContract{},
		func(recordingStepConfig) (Step, error) {
			return recordingStep{mu: &mu, calls: &calls, fail: &fail}, nil
		}); err != nil {
		t.Fatal(err)
	}
	dag, err := Build(catalog, Spec{Nodes: []NodeSpec{
		{ID: "custom", Factory: NewStepSpec("test.record", recordingStepConfig{Label: "one"})},
		{ID: "fails", Factory: NewStepSpec("test.fail", recordingStepConfig{Label: "two"}), DependsOn: []string{"custom"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewMemWorkspace()
	checkpoints := newCheckpointStore(t, ws)
	if err != nil {
		t.Fatal(err)
	}
	state := &RunState{task: Task{
		Scope: sdkmemory.Scope{RuntimeID: "runtime"}, PublicationID: "publication",
		PolicyDigest: "policy", Branch: "branch",
	}, fact: factview.Fact{ID: "fact", Scope: sdkmemory.Scope{RuntimeID: "runtime"}}}
	if err := dag.Run(context.Background(), state, checkpoints); err == nil {
		t.Fatal("failing custom node succeeded")
	}
	task := state.Task()
	failedKey := CheckpointKey{
		Scope: task.Scope, PublicationID: task.PublicationID,
		Node: "fails", Branch: task.Branch, PolicyDigest: task.PolicyDigest, DAGDigest: dag.Digest(),
	}
	failed, found, err := checkpoints.Load(context.Background(), failedKey)
	if err != nil || !found || failed.Status != CheckpointError {
		t.Fatalf("failed checkpoint=%#v found=%v err=%v", failed, found, err)
	}
	checkpoints = newCheckpointStore(t, ws)
	if err != nil {
		t.Fatal(err)
	}
	fail = false
	if err := dag.Run(context.Background(), state, checkpoints); err != nil {
		t.Fatal(err)
	}
	completed, found, err := checkpoints.Load(context.Background(), failedKey)
	if err != nil || !found || completed.Status != CheckpointCompleted {
		t.Fatalf("completed checkpoint=%#v found=%v err=%v", completed, found, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := calls; len(got) != 3 || got[0] != "custom" || got[1] != "fails" || got[2] != "fails" {
		t.Fatalf("calls=%v", got)
	}
}

func TestBuildRejectsBadLifecycleTopologyAndFactory(t *testing.T) {
	tests := []Spec{
		{Nodes: []NodeSpec{{ID: "x", Phase: PhaseIntegrate}, {ID: "x", Phase: PhaseRepair}}},
		{Nodes: []NodeSpec{{ID: "x", Phase: PhaseRepair, DependsOn: []string{"missing"}}}},
		{Nodes: []NodeSpec{{ID: "x", Phase: PhaseRepair, DependsOn: []string{"y"}}, {ID: "y", Phase: PhaseRepair, DependsOn: []string{"x"}}}},
		{Nodes: []NodeSpec{{ID: "x", Factory: NewStepSpec("missing", recordingStepConfig{})}}},
	}
	for _, spec := range tests {
		if _, err := Build(NewCatalog(), spec); err == nil {
			t.Fatalf("accepted bad spec %#v", spec)
		}
	}
	catalog := NewCatalog()
	if err := RegisterTypedStep(catalog, "test.record", "v1", StepContract{},
		func(recordingStepConfig) (Step, error) {
			return recordingStep{mu: &sync.Mutex{}, calls: &[]string{}}, nil
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(catalog, Spec{Nodes: []NodeSpec{{
		ID: "wrong-type", Factory: NewStepSpec("test.record", struct{ Wrong bool }{Wrong: true}),
	}}}); err == nil {
		t.Fatal("accepted lifecycle factory config type mismatch")
	}
	if err := RegisterTypedStep(catalog, "test.produce-task", "v1", StepContract{
		Produces: []StateKind{StateTask},
	}, func(recordingStepConfig) (Step, error) {
		return recordingStep{mu: &sync.Mutex{}, calls: &[]string{}}, nil
	}); err == nil {
		t.Fatal("accepted custom lifecycle node that produces immutable task state")
	}
}

func TestTaskIsImmutableAcrossFailedNodeRestart(t *testing.T) {
	fail := true
	catalog := NewCatalog()
	if err := RegisterTypedStep(catalog, "test.mutate-task", "v1", StepContract{
		Requires: []StateKind{StateTask},
	}, func(recordingStepConfig) (Step, error) {
		return taskCloneMutatingStep{fail: &fail}, nil
	}); err != nil {
		t.Fatal(err)
	}
	dag, err := Build(catalog, Spec{Nodes: []NodeSpec{{
		ID: "mutate", Factory: NewStepSpec("test.mutate-task", recordingStepConfig{}),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	original := Task{
		Scope: sdkmemory.Scope{RuntimeID: "runtime"}, PublicationID: "publication",
		PolicyDigest: "policy", Branch: "branch",
	}
	state := &RunState{task: original}
	checkpoints := newCheckpointStore(t, workspace.NewMemWorkspace())
	if err := dag.Run(context.Background(), state, checkpoints); err == nil {
		t.Fatal("failing custom node succeeded")
	}
	if got := state.Task(); got != original {
		t.Fatalf("failed node mutated task: %#v", got)
	}
	fail = false
	if err := dag.Run(context.Background(), state, checkpoints); err != nil {
		t.Fatal(err)
	}
	if got := state.Task(); got != original {
		t.Fatalf("restart changed task: %#v", got)
	}
}

func TestDAGPolicyAndScopeIsolationAndCancellation(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	catalog := NewCatalog()
	if err := RegisterTypedStep(catalog, "test.record", "v1", StepContract{},
		func(recordingStepConfig) (Step, error) {
			return recordingStep{mu: &mu, calls: &calls}, nil
		}); err != nil {
		t.Fatal(err)
	}
	dag, err := Build(catalog, Spec{Nodes: []NodeSpec{{
		ID: "custom", Factory: NewStepSpec("test.record", recordingStepConfig{}),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := newCheckpointStore(t, workspace.NewMemWorkspace())
	run := func(scope, policy string) error {
		return dag.Run(context.Background(), &RunState{task: Task{
			Scope: sdkmemory.Scope{RuntimeID: scope}, PublicationID: "publication",
			PolicyDigest: policy, Branch: "branch",
		}}, checkpoints)
	}
	if err := run("a", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := run("a", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := run("a", "p2"); err != nil {
		t.Fatal(err)
	}
	if err := run("b", "p1"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls=%v", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dag.Run(ctx, &RunState{task: Task{
		Scope: sdkmemory.Scope{RuntimeID: "c"}, PublicationID: "publication",
		PolicyDigest: "p1", Branch: "branch",
	}}, checkpoints); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func newCheckpointStore(t *testing.T, ws workspace.Workspace) *LifecycleCheckpointStore {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewLifecycleCheckpointStore(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
