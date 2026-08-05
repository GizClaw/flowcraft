package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/lifecycle"
	"github.com/GizClaw/flowcraft/memory/lines/chat"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestAssemblyRunOnceDerivesAndRetrieves(t *testing.T) {
	runtime, generate, embed := testRuntime(t)
	builder, err := NewBuilder(workspace.NewMemWorkspace(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "memory", UserID: "tenant"}
	assembly, err := builder.NewAssembly(context.Background(), Settings{
		Generate: modelSettings(generate), Embed: modelSettings(embed),
		Scopes:   []ScopeSettings{{RuntimeID: scope.RuntimeID, UserID: scope.UserID}},
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Close()

	var _ sdkmemory.ContextProvider = assembly.System
	var _ sdkmemory.TurnSink = assembly.System
	var _ sdkmemory.DocumentSink = assembly.System
	if err := assembly.System.CommitTurn(context.Background(), sdkmemory.Turn{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "run-1",
		Messages: []message.Message{message.NewTextMessage(message.RoleUser, "alpha preference")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := assembly.System.PutDocument(context.Background(), sdkmemory.Document{
		Scope: scope, DatasetID: "docs", DocumentID: "one", IdempotencyKey: "doc-1",
		Content:    message.Content{Parts: []message.Part{message.TextPart{Text: "# Alpha\nalpha knowledge"}}},
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceDocument, ID: "source/one"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := assembly.Runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := assembly.System.Context(context.Background(), sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", DatasetIDs: []string{"docs"},
		Query: "alpha", Budget: sdkmemory.Budget{MaxItems: 10, MaxTokens: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) < 2 {
		t.Fatalf("retrieved %d items, want derived chat and knowledge items", len(result.Items))
	}
	var recent, hierarchy, summary bool
	for _, item := range result.Items {
		if item.SourceClass == sdkmemory.ContextSourceRecent {
			recent = item.MessageRole == message.RoleUser && item.Sequence == 1 &&
				len(item.Sources) == 1 && item.Sources[0].Kind == sdkmemory.SourceMessage
		}
		if item.Kind == sdkmemory.ContextDocumentChunk && item.ParentID != "" && item.Level > 0 &&
			len(item.Sources) > 0 {
			hierarchy = true
		}
		if item.Kind == sdkmemory.ContextSummary && item.Hint != nil &&
			len(item.Hint.SourceRefs) > 0 && item.Content.Text() != "" {
			summary = true
		}
	}
	if !recent || !hierarchy || !summary {
		t.Fatalf("source→worker→provider metadata missing: %+v", result.Items)
	}
	if _, ok := assembly.ResolveItem("system"); !ok {
		t.Fatal("system item was not resolved")
	}
	if _, ok := assembly.ResolveItem("runtime"); ok {
		t.Fatal("unknown legacy item resolved")
	}
}

func TestAssemblyLinksFactsAcrossTwoWorkerCommitsThroughSharedVectorIndex(t *testing.T) {
	runtime, generate, embed := testRuntimeWithResponses(t, []string{
		`{"facts":[{"text":"alpha fact","event_time":"2020-01-01T00:00:00Z"}]}`,
		`{"facts":[{"text":"related fact","event_time":"2021-01-01T00:00:00Z"}]}`,
	})
	ws := workspace.NewMemWorkspace()
	builder, err := NewBuilder(ws, runtime)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "memory", UserID: "tenant"}
	assembly, err := builder.NewAssembly(context.Background(), Settings{
		Generate: modelSettings(generate), Embed: modelSettings(embed),
		Scopes: []ScopeSettings{{RuntimeID: scope.RuntimeID, UserID: scope.UserID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Close()
	store, err := factview.NewWorkspaceStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	for index, text := range []string{"first turn", "second turn"} {
		if err := assembly.System.CommitTurn(context.Background(), sdkmemory.Turn{
			Scope: scope, ConversationID: "conversation", IdempotencyKey: text,
			Messages: []message.Message{message.NewTextMessage(message.RoleUser, text)},
		}); err != nil {
			t.Fatal(err)
		}
		if err := assembly.Runner.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		facts, err := store.List(context.Background(), scope, "conversation", factview.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && (len(facts) != 1 || len(facts[0].LinkedMemoryIDs) != 0) {
			t.Fatalf("first commit facts = %#v", facts)
		}
		if index == 1 {
			if len(facts) != 2 {
				t.Fatalf("second commit facts = %#v", facts)
			}
			byText := map[string]factview.Fact{}
			for _, fact := range facts {
				byText[fact.Text] = fact
			}
			if got := byText["related fact"].LinkedMemoryIDs; len(got) != 1 ||
				got[0] != byText["alpha fact"].ID {
				t.Fatalf("cross-commit links = %#v facts=%#v", got, facts)
			}
		}
	}
}

func TestAssemblyPreservesExactMergeLinksBeforeProjectionExists(t *testing.T) {
	runtime, generate, embed := testRuntimeWithResponses(t, []string{
		`{"facts":[{"text":"shared fact","event_time":"2020-01-01T00:00:00Z"}]}`,
		`{"facts":[{"text":"shared fact","event_time":"2021-01-01T00:00:00Z"},{"text":"batch peer","event_time":"2022-01-01T00:00:00Z"}]}`,
	})
	ws := workspace.NewMemWorkspace()
	builder, err := NewBuilder(ws, runtime)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "memory", UserID: "tenant"}
	assembly, err := builder.NewAssembly(context.Background(), Settings{
		Generate: modelSettings(generate), Embed: modelSettings(embed),
		Scopes: []ScopeSettings{{RuntimeID: scope.RuntimeID, UserID: scope.UserID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Close()
	for _, key := range []string{"first", "second"} {
		if err := assembly.System.CommitTurn(context.Background(), sdkmemory.Turn{
			Scope: scope, ConversationID: "conversation", IdempotencyKey: key,
			Messages: []message.Message{message.NewTextMessage(message.RoleUser, key)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := assembly.Runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	store, err := factview.NewWorkspaceStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := store.List(context.Background(), scope, "conversation", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("exact merge facts = %#v", facts)
	}
	byText := map[string]factview.Fact{}
	for _, fact := range facts {
		byText[fact.Text] = fact
	}
	if got := byText["shared fact"].LinkedMemoryIDs; len(got) != 1 ||
		got[0] != byText["batch peer"].ID {
		t.Fatalf("exact merge links = %#v", got)
	}
}

func TestSummaryTypedDisable(t *testing.T) {
	runtime, generate, embed := testRuntime(t)
	builder, _ := NewBuilder(workspace.NewMemWorkspace(), runtime)
	scope := sdkmemory.Scope{RuntimeID: "memory", UserID: "tenant"}
	assembly, err := builder.NewAssembly(context.Background(), Settings{
		Generate: modelSettings(generate), Embed: modelSettings(embed),
		Scopes:  []ScopeSettings{{RuntimeID: scope.RuntimeID, UserID: scope.UserID}},
		Summary: SummarySettings{Disabled: true}, Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Close()
	if err := assembly.System.CommitTurn(context.Background(), sdkmemory.Turn{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []message.Message{message.NewTextMessage(message.RoleUser, "disable summary")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := assembly.Runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := assembly.System.Context(context.Background(), sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "summary",
		Budget: sdkmemory.Budget{MaxItems: 10, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Items {
		if item.Kind == sdkmemory.ContextSummary {
			t.Fatal("disabled summary branch returned a summary")
		}
	}
}

func TestSettingsRejectInvalidLaneCalibration(t *testing.T) {
	runtime, generate, embed := testRuntime(t)
	builder, _ := NewBuilder(workspace.NewMemWorkspace(), runtime)
	_, err := builder.NewAssembly(context.Background(), Settings{
		Generate: modelSettings(generate), Embed: modelSettings(embed),
		Scopes: []ScopeSettings{{RuntimeID: "memory"}},
		Lanes: LanesSettings{Vector: LaneSettings{
			Weight: 1, Calibration: CalibrationSettings{Kind: "logistic"},
		}},
	})
	if err == nil {
		t.Fatal("accepted logistic calibration without slope")
	}
}

func TestFactSettingsDefaultSimpleNoneAndCaps(t *testing.T) {
	defaults := (Settings{}).withDefaults()
	if defaults.Fact.Strategy != chat.StrategySimple || defaults.Fact.TailMaxChars != 15000 {
		t.Fatalf("fact defaults = %#v", defaults.Fact)
	}
	runtime, _, embed := testRuntime(t)
	builder, _ := NewBuilder(workspace.NewMemWorkspace(), runtime)
	assembly, err := builder.NewAssembly(context.Background(), Settings{
		Fact:  FactSettings{Strategy: chat.StrategyNone},
		Embed: modelSettings(embed), Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("none requires generation unexpectedly: %v", err)
	}
	_ = assembly.Close()

	_, generate, _ := testRuntime(t)
	_, err = builder.NewAssembly(context.Background(), Settings{
		Generate: modelSettings(generate), Embed: modelSettings(embed),
		Fact: FactSettings{MaxFacts: -1}, Interval: time.Hour,
	})
	if err == nil {
		t.Fatal("negative fact cap accepted")
	}
}

func TestLifecycleDefaultsAreEnabledAuditOnlyAndAffectPolicyDigest(t *testing.T) {
	defaults := (Settings{}).withDefaults()
	if defaults.Lifecycle.Disabled || defaults.Lifecycle.Periodic ||
		defaults.Lifecycle.Forget.Mode != lifecycle.ModeAuditOnly ||
		defaults.Lifecycle.Decay.Version != lifecycle.DecayAlgorithmVersion {
		t.Fatalf("unsafe lifecycle defaults = %#v", defaults.Lifecycle)
	}
	base, err := ComputePolicyDigest(Settings{})
	if err != nil {
		t.Fatal(err)
	}
	changed := Settings{Lifecycle: LifecycleSettings{Decay: lifecycle.DecayConfig{HalfLife: 48 * time.Hour}}}
	other, err := ComputePolicyDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if base == other {
		t.Fatal("lifecycle algorithm config omitted from policy digest")
	}
}

func TestProjectionRepairEvidenceKeepsStoredAndComputedDigestTypesAligned(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	matching := projectionRepairEvidence("vector", "source", "source", "build", "build")
	plan := lifecycle.InspectRepair(scope, lifecycle.RepairInput{Projections: []lifecycle.ProjectionEvidence{matching}})
	if len(plan.Actions) != 0 {
		t.Fatalf("matching projection evidence produced repair: %#v", plan.Actions)
	}
	tampered := projectionRepairEvidence("vector", "stored-source", "computed-source", "stored-build", "computed-build")
	plan = lifecycle.InspectRepair(scope, lifecycle.RepairInput{Projections: []lifecycle.ProjectionEvidence{tampered}})
	if len(plan.Actions) != 1 || plan.Actions[0].Target != "projection:vector" {
		t.Fatalf("tampered projection evidence = %#v", plan.Actions)
	}
}

func modelSettings(ref inference.ModelRef) ModelSettings {
	return ModelSettings{Provider: ref.ID.Provider, Name: ref.ID.Name, Profile: ref.Profile}
}

type embedWire struct{ Texts []string }
type embedRaw struct{ Vectors [][]float32 }

func testRuntime(t *testing.T) (*inference.Runtime, inference.ModelRef, inference.ModelRef) {
	return testRuntimeWithResponses(t, []string{`{"facts":[{"text":"alpha fact"}]}`})
}

func testRuntimeWithResponses(t *testing.T, responses []string) (*inference.Runtime, inference.ModelRef, inference.ModelRef) {
	t.Helper()
	generate := inference.ModelRef{ID: inference.ModelID{Provider: "fake", Name: "generate"}}
	embed := inference.ModelRef{ID: inference.ModelID{Provider: "fake", Name: "embed"}}
	generateDriver, err := inference.BindGenerate(
		func(_ context.Context, _ inference.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[string], error) {
			decisions := make([]inference.Decision, 0)
			for _, field := range request.ActiveFieldsFor(shape) {
				decisions = append(decisions, inference.Decision{Field: field, Disposition: inference.Native})
			}
			return inference.Compiled[string]{Wire: "wire", Report: inference.CompileReport{
				Operation: inference.OperationGenerate, Decisions: decisions,
			}}, nil
		},
		func(context.Context, string) (string, error) {
			if len(responses) == 0 {
				return "", errors.New("no generate response configured")
			}
			response := responses[0]
			if len(responses) > 1 {
				responses = responses[1:]
			}
			return response, nil
		},
		func(_ context.Context, raw string) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{
				Message:      message.NewTextMessage(message.RoleAssistant, raw),
				FinishReason: inference.FinishCompleted,
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	embedDriver, err := inference.BindEmbed(
		func(_ context.Context, _ inference.ModelRef, request inference.EmbedRequest) (inference.Compiled[embedWire], error) {
			texts := make([]string, len(request.Items))
			decisions := make([]inference.Decision, 0)
			for _, field := range request.ActiveFields() {
				decisions = append(decisions, inference.Decision{Field: field, Disposition: inference.Native})
			}
			for i, item := range request.Items {
				texts[i] = item.Content.Text()
			}
			return inference.Compiled[embedWire]{Wire: embedWire{Texts: texts}, Report: inference.CompileReport{
				Operation: inference.OperationEmbed, Decisions: decisions,
			}}, nil
		},
		func(_ context.Context, wire embedWire) (embedRaw, error) {
			vectors := make([][]float32, len(wire.Texts))
			for i := range vectors {
				vectors[i] = []float32{1, 1}
			}
			return embedRaw{Vectors: vectors}, nil
		},
		func(_ context.Context, raw embedRaw) (inference.EmbedResponse, error) {
			embeddings := make([]inference.Embedding, len(raw.Vectors))
			for i, value := range raw.Vectors {
				embeddings[i].Vector = value
			}
			return inference.EmbedResponse{Embeddings: embeddings}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake",
		Models: []inference.ModelImplementation{
			{Descriptor: inference.ModelDescriptor{ID: generate.ID}, Openers: inference.Openers{
				Generate: func(context.Context, inference.ModelRef) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: generateDriver}, nil
				},
			}},
			{Descriptor: inference.ModelDescriptor{ID: embed.ID}, Openers: inference.Openers{
				Embed: func(context.Context, inference.ModelRef) (inference.EmbedDriver, error) {
					return embedDriver, nil
				},
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, generate, embed
}
