package yaml_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	inferenceconfig "github.com/GizClaw/flowcraft/sdkx/inference/config"
	inferenceyaml "github.com/GizClaw/flowcraft/sdkx/inference/config/yaml"
	"github.com/GizClaw/flowcraft/sdkx/memory/config"
	memyaml "github.com/GizClaw/flowcraft/sdkx/memory/config/yaml"
	"github.com/GizClaw/flowcraft/sdkx/memory/hook"
)

const memoryYAML = `
version: v1
runtime:
  hard_partition: [runtime_id, user_id]
  default_scope:
    runtime_id: prod
  clock:
    impl: system
stores:
  messages:
    impl: noop
  documents:
    impl: noop
`

const embeddingMemoryYAML = memoryYAML + `
embedding:
  model:
    id:
      provider: fake
      name: model
    profile: default
  dimensions: 3
  batch_size: 2
  timeout: 1s
`

func TestDeployFactorySpec(t *testing.T) {
	factory := memyaml.NewDeployFactory(noopBuilder(t))
	spec := factory.Spec()
	if spec.Kind != memyaml.ResourceKind {
		t.Errorf("Kind = %q, want %q", spec.Kind, memyaml.ResourceKind)
	}
	if spec.Impl != "yaml" {
		t.Errorf("Impl = %q, want %q", spec.Impl, "yaml")
	}
	if spec.ItemType != "memory.Runtime" {
		t.Errorf("ItemType = %q, want memory.Runtime", spec.ItemType)
	}
	if len(spec.Deps) != 1 || spec.Deps[0].Name != "inference" ||
		spec.Deps[0].Type != "inference.Runtime" || spec.Deps[0].Required {
		t.Errorf("Deps = %+v, want optional inference.Runtime entry", spec.Deps)
	}
}

func TestDeployFactoryNew_BuildsAssembly(t *testing.T) {
	path := writeMemoryYAML(t, memoryYAML)
	t.Cleanup(func() { _ = os.Remove(path) })

	factory := memyaml.NewDeployFactory(noopBuilder(t))
	settings := settingsNode(t, "file: "+path)
	value, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: settings,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly, ok := value.(*config.Assembly)
	if !ok {
		t.Fatalf("value is %T, want *config.Assembly", value)
	}
	if assembly.Runtime == nil {
		t.Fatal("Runtime is nil")
	}
	t.Cleanup(func() { _ = assembly.Close() })
}

func TestDeployFactoryNew_RejectsMissingFile(t *testing.T) {
	factory := memyaml.NewDeployFactory(noopBuilder(t))
	settings := settingsNode(t, "file: /no/such/path/memory.yaml")
	_, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: settings,
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDeployFactoryNew_RejectsEmptyFilePath(t *testing.T) {
	factory := memyaml.NewDeployFactory(noopBuilder(t))
	_, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: settingsNode(t, ""),
	})
	if err == nil {
		t.Fatal("expected error for empty file path")
	}
}

func TestDeployFactoryNew_RejectsUnknownSettingsKey(t *testing.T) {
	path := writeMemoryYAML(t, memoryYAML)
	t.Cleanup(func() { _ = os.Remove(path) })

	factory := memyaml.NewDeployFactory(noopBuilder(t))
	settings := settingsNode(t, "file: "+path+"\nextra: oops\n")
	_, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: settings,
	})
	if err == nil {
		t.Fatal("expected error for unknown settings key")
	} else if !strings.Contains(err.Error(), "extra") {
		t.Errorf("error should mention extra, got: %v", err)
	}
}

func TestDeployBuilderBuildsMemoryResourceAndHooks(t *testing.T) {
	memoryPath := writeMemoryYAML(t, embeddingMemoryYAML)
	var recallQuery string
	registry := agent.NewRegistry()
	if err := registry.Register(integrationEngineFactory{}); err != nil {
		t.Fatal(err)
	}
	builder := deploy.NewBuilder(registry)
	builder.MustRegisterResource(inferenceyaml.NewDeployFactory(
		integrationInferenceFactories(), nil))
	builder.MustRegisterResource(memyaml.NewDeployFactory(
		integrationMemoryBuilder(t, &recallQuery)))
	builder.RegisterPreparer(hook.LoadPreparerKind, hook.NewLoadPreparerFactory())
	builder.RegisterPreparer(hook.RecallPreparerKind, hook.NewRecallPreparerFactory())
	builder.RegisterCommitter(hook.AppendCommitterKind, hook.NewAppendCommitterFactory())

	deployment := fmt.Sprintf(`
version: v1
resources:
  infer:
    kind: inference.Assembly
    impl: yaml
    settings:
      inline:
        version: v1
        providers:
          - id: fake
            driver: fake
            profiles:
              - id: default
                operations: [embed]
  memory:
    kind: memory.Assembly
    impl: yaml
    deps:
      inference: infer/runtime
    settings:
      file: %q
agents:
  remembered:
    engine: {kind: memory-test}
    prepare:
      - type: memory.load
        deps: {runtime: memory/runtime}
        settings: {into: transcript, limit: 10}
      - type: memory.recall
        deps: {runtime: memory/runtime}
        settings: {into: hits, query: {board: search_query}, top_k: 3}
    commit:
      - type: memory.append
        deps: {runtime: memory/runtime}
        settings: {channel: __main_channel}
`, memoryPath)
	doc, err := deploy.Parse([]byte(deployment))
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()
	instance, ok := result.Instance("remembered")
	if !ok {
		t.Fatal("remembered instance was not built")
	}
	response, err := instance.Execute(context.Background(), agent.Request{
		ContextID: "conversation-1",
		Message:   inference.NewTextMessage(inference.RoleUser, "hello"),
		Inputs:    map[string]any{"search_query": "from request board"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !response.Committed {
		t.Fatal("memory-backed hook chain did not commit")
	}
	if recallQuery != "from request board" {
		t.Fatalf("recall query = %q, want request input seeded into Board", recallQuery)
	}
}

// noopBuilder returns a config.Builder wired with the noop
// StoreFactory, the only factory registered in v1.
func noopBuilder(t *testing.T) *config.Builder {
	t.Helper()
	b, err := config.NewBuilder(map[string]config.StoreFactory{
		"noop": config.NoopStoreFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func integrationInferenceFactories() map[string]inferenceconfig.Factory {
	return map[string]inferenceconfig.Factory{
		"fake": inferenceconfig.FactoryFunc(func(
			_ context.Context,
			input inferenceconfig.ProviderInput,
		) (inference.ProviderDefinition, error) {
			profiles := make([]inference.ProfileDefinition, len(input.Profiles))
			for index, profile := range input.Profiles {
				profiles[index] = inference.ProfileDefinition{
					ID:         profile.ID,
					Operations: append([]inference.Operation(nil), profile.Operations...),
				}
			}
			return inference.ProviderDefinition{
				ID:       input.ID,
				Profiles: profiles,
				Models: []inference.ModelImplementation{{
					Descriptor: inference.ModelDescriptor{
						ID: inference.ModelID{Provider: input.ID, Name: "model"},
					},
					Openers: inference.Openers{
						Embed: func(
							context.Context,
							inference.ModelRef,
						) (inference.EmbedDriver, error) {
							return nil, fmt.Errorf(
								"embedding opener must not run during deploy assembly")
						},
					},
				}},
			}, nil
		}),
	}
}

func integrationMemoryBuilder(t *testing.T, recallQuery *string) *config.Builder {
	t.Helper()
	factory := config.StoreFactoryFunc(func(
		_ context.Context, input config.StoreInput,
	) (config.StoreResult, error) {
		noop, err := memory.NewNoopRuntime(memory.Spec{RuntimeID: "prod"})
		if err != nil {
			return config.StoreResult{}, err
		}
		switch input.StoreName {
		case config.StoreMessages:
			return config.StoreResult{
				Append: noop,
				Load:   noop,
				Recall: recallRecorder{Runtime: noop, Query: recallQuery},
				Closer: noop,
			}, nil
		case config.StoreDocuments:
			return config.StoreResult{
				Import:  noop,
				Compact: noop,
				Archive: noop,
				Closer:  noop,
			}, nil
		default:
			return config.StoreResult{}, nil
		}
	})
	builder, err := config.NewBuilder(map[string]config.StoreFactory{"noop": factory})
	if err != nil {
		t.Fatal(err)
	}
	return builder
}

type recallRecorder struct {
	Runtime memory.RecallOp
	Query   *string
}

func (r recallRecorder) CompileRecall(
	ctx context.Context, request memory.RecallRequest,
) memory.CompileResult {
	return r.Runtime.CompileRecall(ctx, request)
}

func (r recallRecorder) ExecuteRecall(
	ctx context.Context, request memory.RecallRequest,
) (memory.RecallResponse, error) {
	*r.Query = request.Query
	return r.Runtime.ExecuteRecall(ctx, request)
}

// writeMemoryYAML writes the given YAML to a temp file and
// returns the path.
func writeMemoryYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type integrationEngineFactory struct{}

func (integrationEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{Kind: "memory-test"}
}

func (integrationEngineFactory) New(context.Context, agent.Config) (agent.Engine, error) {
	return agent.EngineFunc(func(
		_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board,
	) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return board, nil
	}), nil
}
