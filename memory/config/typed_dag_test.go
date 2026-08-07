package config

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/lifecycle"
	"github.com/GizClaw/flowcraft/memory/lines/chat"
	"github.com/GizClaw/flowcraft/memory/lines/knowledge"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestDefaultTypedDAGsAndPolicyDigest(t *testing.T) {
	runtime, generate, embed := testRuntime(t)
	builder := newBuilder(t, runtime, nil)
	settings := Settings{
		Generate: FromModelRef(generate),
		Embed:    FromModelRef(embed),
		Interval: Duration(time.Second),
	}
	first, err := builder.NewAssembly(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if first.PolicyDigest == "" {
		t.Fatal("empty automatic policy digest")
	}
	if got := first.ChatDAG.TopologicalOrder(); len(got) != 1 || got[0] != "extract-facts" {
		t.Fatalf("default chat DAG order = %v", got)
	}
	if got := first.KnowledgeDAG.TopologicalOrder(); len(got) != 1 || got[0] != "chunk-document" {
		t.Fatalf("default knowledge DAG order = %v", got)
	}

	second, err := builder.NewAssembly(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.PolicyDigest != second.PolicyDigest {
		t.Fatalf("same normalized settings changed digest: %q != %q", first.PolicyDigest, second.PolicyDigest)
	}
	settings.Interval = Duration(10 * time.Second)
	third, err := builder.NewAssembly(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if first.PolicyDigest != third.PolicyDigest {
		t.Fatal("runtime poll interval changed semantic policy digest")
	}
	settings.Chunk.MaxRunes = 42
	fourth, err := builder.NewAssembly(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer fourth.Close()
	if first.PolicyDigest == fourth.PolicyDigest {
		t.Fatal("semantic chunk configuration did not change digest")
	}
}

func TestPolicyDigestIncludesCalibrationAndRetrievalParameters(t *testing.T) {
	base := Settings{}.withDefaults()
	first, err := ComputePolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Lanes.Vector.Calibration.DisableFloor = true
	second, err := ComputePolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("semantic floor policy did not change digest")
	}
	base = Settings{}.withDefaults()
	base.BM25.K1 = 1.3
	third, err := ComputePolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("BM25 algorithm parameters did not change digest")
	}
	base = Settings{}.withDefaults()
	base.ProjectionStorage.MaxSegments++
	fourth, err := ComputePolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	if first == fourth {
		t.Fatal("projection storage thresholds did not change digest")
	}
}

type customChatConfig struct {
	Prefix string `json:"prefix"`
}

type customChatDeriver struct {
	prefix string
}

func (d customChatDeriver) Derive(_ context.Context, input component.Artifact) ([]component.Artifact, error) {
	output := input.Clone()
	output.Kind = chat.KindFact
	output.ID = d.prefix + input.ID
	return []component.Artifact{output}, nil
}

func TestCustomTypedDAGAndBuildDiagnostics(t *testing.T) {
	runtime, generate, embed := testRuntime(t)
	builder := newBuilder(t, runtime, nil)
	factories := NewFactoryCatalog()
	if err := component.RegisterTypedDeriver(
		factories,
		"custom.prefix",
		"1.0.0",
		component.Ports{Inputs: []component.ArtifactKind{chat.KindRawMessage}, Outputs: []component.ArtifactKind{chat.KindFact}},
		func(config customChatConfig) (component.Deriver, error) {
			return customChatDeriver{prefix: config.Prefix}, nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := component.RegisterTypedDeriver(
		factories,
		"custom.chunk",
		"1.0.0",
		component.Ports{Inputs: []component.ArtifactKind{knowledge.KindDocument}, Outputs: []component.ArtifactKind{knowledge.KindDocumentChunk}},
		func(config ChunkSettings) (component.Deriver, error) {
			return knowledge.NewChunker(knowledge.ChunkerConfig{
				MaxRunes: config.MaxRunes, OverlapRunes: config.OverlapRunes,
			})
		},
	); err != nil {
		t.Fatal(err)
	}
	settings := Settings{
		Generate:       FromModelRef(generate),
		Embed:          FromModelRef(embed),
		FactoryCatalog: factories,
		ChatDAG: ChatDAGSettings{Nodes: []ChatNodeSettings{
			CustomChatNode("custom", component.NewDeriverSpec("custom.prefix", customChatConfig{Prefix: "x-"})),
		}},
	}
	assembly, err := builder.NewAssembly(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Close()
	if got := assembly.ChatDAG.TopologicalOrder(); len(got) != 1 || got[0] != "custom" {
		t.Fatalf("custom order = %v", got)
	}

	settings.ChatDAG.Nodes[0] = CustomChatNode(
		"bad-config",
		component.NewDeriverSpec("custom.prefix", struct{ Wrong bool }{Wrong: true}),
	)
	if _, err := builder.NewAssembly(context.Background(), settings); err == nil {
		t.Fatal("factory config type mismatch was accepted")
	}

	settings.ChatDAG.Nodes = []ChatNodeSettings{
		CustomChatNode("chunk", component.NewDeriverSpec("custom.chunk", ChunkSettings{MaxRunes: 100}), "fact"),
		{ID: "fact", Fact: &FactSettings{}},
	}
	if _, err := builder.NewAssembly(context.Background(), settings); err == nil {
		t.Fatal("chat artifact type mismatch was accepted")
	}
}

type customLifecycleConfig struct {
	Label string `json:"label"`
}

type customLifecycleStep struct {
	calls *atomic.Int32
}

func (step customLifecycleStep) Run(_ context.Context, state *lifecycle.RunState) error {
	fact := state.Fact()
	if state.Task().Scope.RuntimeID == "memory" && fact.Text != "" {
		step.calls.Add(1)
	}
	fact.Text = "attempted mutation"
	return nil
}

func TestAssemblyExecutesCustomTypedLifecycleDAGForRealFactTask(t *testing.T) {
	runtime, generate, embed := testRuntime(t)
	ws := workspace.NewMemWorkspace()
	builder := newBuilder(t, runtime, ws)
	factories := lifecycle.NewCatalog()
	var calls atomic.Int32
	if err := lifecycle.RegisterTypedStep(
		factories, "custom.observe", "v1", lifecycle.StepContract{},
		func(customLifecycleConfig) (lifecycle.Step, error) {
			return customLifecycleStep{calls: &calls}, nil
		},
	); err != nil {
		t.Fatal(err)
	}
	scope := memory.Scope{RuntimeID: "memory", UserID: "tenant"}
	settings := Settings{
		Generate: FromModelRef(generate), Embed: FromModelRef(embed), LifecycleFactoryCatalog: factories,
		Scopes: []ScopeSettings{{RuntimeID: scope.RuntimeID, UserID: scope.UserID}},
		LifecycleDAG: LifecycleDAGSettings{Nodes: []LifecycleNodeSettings{
			CustomLifecycleNode("custom", lifecycle.NewStepSpec("custom.observe", customLifecycleConfig{Label: "called"})),
		}},
	}
	assembly, err := builder.NewAssembly(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Close()
	if assembly.LifecycleDAG == nil || assembly.LifecycleRunner == nil {
		t.Fatal("lifecycle DAG was not wired into the runner")
	}
	if err := assembly.System.CommitTurn(context.Background(), memory.Turn{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []message.Message{message.NewTextMessage(message.RoleUser, "remember lifecycle")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := assembly.Runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := assembly.LifecycleRunner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("custom lifecycle calls=%d", calls.Load())
	}
	facts := newFactStore(t, ws)
	if err != nil {
		t.Fatal(err)
	}
	values, err := facts.List(context.Background(), scope, "conversation", factview.ListOptions{})
	if err != nil || len(values) != 1 || values[0].Text == "attempted mutation" {
		t.Fatalf("custom step mutated canonical fact: values=%#v err=%v", values, err)
	}
}

func TestPolicyDigestIncludesTopologyAndAlgorithmVersions(t *testing.T) {
	base := Settings{
		ChatDAG: ChatDAGSettings{Nodes: []ChatNodeSettings{
			{ID: "facts-a", Fact: &FactSettings{}},
			{ID: "facts-b", Fact: &FactSettings{}},
		}},
		KnowledgeDAG: KnowledgeDAGSettings{Nodes: []KnowledgeNodeSettings{
			{ID: "chunks", Chunk: &ChunkSettings{}},
		}},
	}.withDefaults()
	first, err := ComputePolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	reorderedCatalog := AlgorithmCatalog{
		{Name: "knowledge.chunk", Version: knowledge.AlgorithmVersion},
		{Name: "chat.fact", Version: chat.AlgorithmVersion},
	}
	base.AlgorithmCatalog = reorderedCatalog
	base.ChatDAG.Nodes[0], base.ChatDAG.Nodes[1] = base.ChatDAG.Nodes[1], base.ChatDAG.Nodes[0]
	second, err := ComputePolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("algorithm registration order changed digest")
	}
	base.ChatDAG.Nodes[0].ID = "renamed"
	third, err := ComputePolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("DAG topology did not change digest")
	}
}

func TestPolicyDigestIncludesLifecycleFactoryVersionAndTopology(t *testing.T) {
	catalog := func(version string) *lifecycle.Catalog {
		result := lifecycle.NewCatalog()
		if err := lifecycle.RegisterTypedStep(
			result, "custom.observe", version, lifecycle.StepContract{},
			func(customLifecycleConfig) (lifecycle.Step, error) {
				return customLifecycleStep{calls: &atomic.Int32{}}, nil
			},
		); err != nil {
			t.Fatal(err)
		}
		return result
	}
	settings := Settings{
		LifecycleFactoryCatalog: catalog("v1"),
		LifecycleDAG: LifecycleDAGSettings{Nodes: []LifecycleNodeSettings{
			CustomLifecycleNode("custom", lifecycle.NewStepSpec("custom.observe", customLifecycleConfig{Label: "one"})),
		}},
	}
	first, err := ComputePolicyDigest(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.LifecycleFactoryCatalog = catalog("v2")
	second, err := ComputePolicyDigest(settings)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("lifecycle factory version omitted from policy digest")
	}
	settings.LifecycleFactoryCatalog = catalog("v1")
	settings.LifecycleDAG.Nodes[0].ID = "renamed"
	third, err := ComputePolicyDigest(settings)
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("lifecycle topology omitted from policy digest")
	}
}

func TestPolicyDigestIgnoresFactoryRegistrationOrder(t *testing.T) {
	firstCatalog := NewFactoryCatalog()
	registerTestPrefix(t, firstCatalog)
	registerTestChunk(t, firstCatalog)
	secondCatalog := NewFactoryCatalog()
	registerTestChunk(t, secondCatalog)
	registerTestPrefix(t, secondCatalog)
	settings := Settings{
		FactoryCatalog: firstCatalog,
		ChatDAG: ChatDAGSettings{Nodes: []ChatNodeSettings{
			CustomChatNode("custom", component.NewDeriverSpec("custom.prefix", customChatConfig{Prefix: "x-"})),
		}},
	}
	first, err := ComputePolicyDigest(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.FactoryCatalog = secondCatalog
	second, err := ComputePolicyDigest(settings)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("factory registration order changed digest")
	}
}

func registerTestPrefix(t *testing.T, factories *FactoryCatalog) {
	t.Helper()
	if err := component.RegisterTypedDeriver(
		factories,
		"custom.prefix",
		"1.0.0",
		component.Ports{Inputs: []component.ArtifactKind{chat.KindRawMessage}, Outputs: []component.ArtifactKind{chat.KindFact}},
		func(config customChatConfig) (component.Deriver, error) {
			return customChatDeriver{prefix: config.Prefix}, nil
		},
	); err != nil {
		t.Fatal(err)
	}
}

func registerTestChunk(t *testing.T, factories *FactoryCatalog) {
	t.Helper()
	if err := component.RegisterTypedDeriver(
		factories,
		"custom.chunk",
		"1.0.0",
		component.Ports{Inputs: []component.ArtifactKind{knowledge.KindDocument}, Outputs: []component.ArtifactKind{knowledge.KindDocumentChunk}},
		func(config ChunkSettings) (component.Deriver, error) {
			return knowledge.NewChunker(knowledge.ChunkerConfig{
				MaxRunes: config.MaxRunes, OverlapRunes: config.OverlapRunes,
			})
		},
	); err != nil {
		t.Fatal(err)
	}
}
