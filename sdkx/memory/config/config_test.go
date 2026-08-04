package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/memory/config"
)

const validYAML = `
version: v1
runtime:
  hard_partition: [runtime_id, user_id]
  default_scope:
    runtime_id: prod
    user_id: tenant-1
  clock:
    impl: system
stores:
  messages:
    impl: noop
  documents:
    impl: noop
embedding:
  model:
    id:
      provider: openai
      name: text-embedding-3-small
    profile: default
  dimensions: 1536
  batch_size: 32
  timeout: 30s
lifecycle:
  compact:
    cron: "@hourly"
    older_than: 720h
    keep: 50
  archive:
    cron: "0 3 * * *"
    older_than: 4320h
    destination: ./archive/
`

const embeddingYAML = `embedding:
  model:
    id:
      provider: openai
      name: text-embedding-3-small
    profile: default
  dimensions: 1536
  batch_size: 32
  timeout: 30s`

// decode parses a YAML literal through the same path the
// production loader uses. The factory and the test suite
// share this entry point so a typo caught here is the same
// one a host would catch.
func decode(t *testing.T, src string) config.Document {
	t.Helper()
	doc, err := config.DecodeYAML(strings.NewReader(src))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc
}

func TestDocumentValidate_AcceptsCanonical(t *testing.T) {
	doc := decode(t, validYAML)
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDocumentValidate_RejectsUnknownVersion(t *testing.T) {
	doc := decode(t, strings.Replace(validYAML, "version: v1", "version: v2", 1))
	if err := doc.Validate(); err == nil {
		t.Fatal("expected error for unknown version")
	} else if !strings.Contains(err.Error(), "v2") {
		t.Errorf("error should mention v2, got: %v", err)
	}
}

func TestDocumentValidate_RejectsWrongHardPartition(t *testing.T) {
	bad := strings.Replace(validYAML,
		"hard_partition: [runtime_id, user_id]",
		"hard_partition: [user_id, runtime_id]", 1)
	doc := decode(t, bad)
	if err := doc.Validate(); err == nil {
		t.Fatal("expected error for hard_partition reordering")
	} else if !strings.Contains(err.Error(), "hard_partition") {
		t.Errorf("error should mention hard_partition, got: %v", err)
	}
}

func TestDocumentValidate_RejectsEmptyRuntimeID(t *testing.T) {
	bad := strings.Replace(validYAML,
		"runtime_id: prod",
		`runtime_id: ""`, 1)
	doc := decode(t, bad)
	if err := doc.Validate(); err == nil {
		t.Fatal("expected error for empty runtime_id")
	}
}

func TestDocumentValidate_EmbeddingZeroValueDisablesEmbedding(t *testing.T) {
	doc := decode(t, strings.Replace(validYAML, embeddingYAML, "embedding: {}", 1))
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate disabled embedding: %v", err)
	}
}

func TestDocumentValidate_RejectsLegacyEmbeddingsStore(t *testing.T) {
	doc := decode(t, strings.Replace(validYAML,
		"  documents:\n    impl: noop",
		"  documents:\n    impl: noop\n  embeddings:\n    impl: noop", 1))
	if err := doc.Validate(); err == nil || !strings.Contains(err.Error(), "supported slot") {
		t.Fatalf("Validate error = %v, want rejected embeddings slot", err)
	}
}

func TestDocumentValidate_RejectsNegativeAndIncompleteOperationalValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Document)
	}{
		{
			name: "compact cron required",
			mutate: func(doc *config.Document) {
				doc.Lifecycle.Compact.Cron = ""
			},
		},
		{
			name: "compact older_than positive",
			mutate: func(doc *config.Document) {
				doc.Lifecycle.Compact.OlderThan = 0
			},
		},
		{
			name: "negative compact keep",
			mutate: func(doc *config.Document) {
				doc.Lifecycle.Compact.Keep = -1
			},
		},
		{
			name: "archive cron required",
			mutate: func(doc *config.Document) {
				doc.Lifecycle.Archive.Cron = ""
			},
		},
		{
			name: "archive older_than positive",
			mutate: func(doc *config.Document) {
				doc.Lifecycle.Archive.OlderThan = -time.Second
			},
		},
		{
			name: "archive destination required",
			mutate: func(doc *config.Document) {
				doc.Lifecycle.Archive.Destination = ""
			},
		},
		{
			name: "embedding model valid",
			mutate: func(doc *config.Document) {
				doc.Embedding.Model.ID.Provider = ""
			},
		},
		{
			name: "non-positive embedding dimensions",
			mutate: func(doc *config.Document) {
				doc.Embedding.Dimensions = 0
			},
		},
		{
			name: "non-positive embedding batch size",
			mutate: func(doc *config.Document) {
				doc.Embedding.BatchSize = 0
			},
		},
		{
			name: "non-positive embedding timeout",
			mutate: func(doc *config.Document) {
				doc.Embedding.Timeout = 0
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := decode(t, validYAML)
			test.mutate(&doc)
			if err := doc.Validate(); err == nil {
				t.Fatal("Validate accepted invalid config")
			}
			builder, err := config.NewBuilder(map[string]config.StoreFactory{
				"noop": config.NoopStoreFactory{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := builder.NewAssembly(context.Background(), doc, nil); err == nil ||
				!errdefs.IsValidation(err) {
				t.Fatalf("NewAssembly error = %v, want validation", err)
			}
		})
	}
}

func TestDocumentValidate_RejectsUnknownTopLevelKey(t *testing.T) {
	bad := validYAML + "\nextra_key: oops\n"
	_, err := config.DecodeYAML(strings.NewReader(bad))
	if err == nil {
		t.Fatal("expected error for unknown top-level key")
	} else if !strings.Contains(err.Error(), "extra_key") {
		t.Errorf("error should mention extra_key, got: %v", err)
	}
}

func TestDecodeYAML_RejectsUnknownLifecycleKey(t *testing.T) {
	bad := strings.Replace(validYAML, `lifecycle:
  compact:
    cron: "@hourly"
    older_than: 720h
    keep: 50
  archive:
    cron: "0 3 * * *"
    older_than: 4320h
    destination: ./archive/`, `lifecycle:
  compact_after: 1h`, 1)
	_, err := config.DecodeYAML(strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "compact_after") {
		t.Fatalf("DecodeYAML error = %v, want rejected legacy field", err)
	}
}

func TestDecodeYAML_ConvertsNestedStoreSettingsToJSON(t *testing.T) {
	source := strings.Replace(validYAML, "  messages:\n    impl: noop", `  messages:
    impl: noop
    settings:
      endpoint: memory://messages
      retry:
        attempts: 3
        enabled: true
      labels: [primary, 7, null]`, 1)
	doc := decode(t, source)
	var captured json.RawMessage
	factory := config.StoreFactoryFunc(func(_ context.Context, in config.StoreInput) (config.StoreResult, error) {
		if in.StoreName == config.StoreMessages {
			captured = append(json.RawMessage(nil), in.Settings...)
		}
		return config.StoreResult{}, nil
	})
	builder, err := config.NewBuilder(map[string]config.StoreFactory{"noop": factory})
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := builder.NewAssembly(t.Context(), doc, inferenceDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	var got struct {
		Endpoint string `json:"endpoint"`
		Retry    struct {
			Attempts int  `json:"attempts"`
			Enabled  bool `json:"enabled"`
		} `json:"retry"`
		Labels []any `json:"labels"`
	}
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("settings are not JSON: %v (%s)", err, captured)
	}
	if got.Endpoint != "memory://messages" || got.Retry.Attempts != 3 ||
		!got.Retry.Enabled || len(got.Labels) != 3 ||
		got.Labels[0] != "primary" || got.Labels[1] != float64(7) ||
		got.Labels[2] != nil {
		t.Fatalf("factory settings = %+v", got)
	}
}

func TestDecodeYAML_StoreEntryKnownFieldsRemainStrict(t *testing.T) {
	bad := strings.Replace(validYAML,
		"  messages:\n    impl: noop",
		"  messages:\n    impl: noop\n    settingz: {}", 1)
	_, err := config.DecodeYAML(strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "settingz") {
		t.Fatalf("DecodeYAML error = %v, want unknown store field", err)
	}
}

func TestDecodeYAML_RejectsTrailingDocument(t *testing.T) {
	_, err := config.DecodeYAML(strings.NewReader(validYAML + "\n---\nversion: v1\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("DecodeYAML error = %v, want multiple documents", err)
	}
}

func TestDocumentValidate_RejectsUnknownStoreSlot(t *testing.T) {
	doc := decode(t, strings.Replace(validYAML,
		"  messages:\n    impl: noop",
		"  cache:\n    impl: noop", 1))
	err := doc.Validate()
	if err == nil || !strings.Contains(err.Error(), "supported slot") {
		t.Fatalf("Validate error = %v, want supported-slot error", err)
	}
}

func TestBuilderNewAssembly_NoopStores(t *testing.T) {
	doc := decode(t, validYAML)
	builder, err := config.NewBuilder(map[string]config.StoreFactory{
		"noop": config.NoopStoreFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := builder.NewAssembly(context.Background(), doc, inferenceDeps(t))
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	if assembly.Runtime == nil {
		t.Fatal("Runtime is nil")
	}
	if got, ok := assembly.ResolveItem("runtime"); !ok || got != assembly.Runtime {
		t.Fatalf("ResolveItem(runtime) = (%T, %v), want assembly runtime", got, ok)
	}
	if assembly.Spec.RuntimeID != "prod" {
		t.Errorf("Spec.RuntimeID = %q, want %q", assembly.Spec.RuntimeID, "prod")
	}
	if assembly.Embedding.Model.ID.Name != "text-embedding-3-small" {
		t.Errorf("Embedding.Model = %+v", assembly.Embedding.Model)
	}
	if assembly.Lifecycle.Compact.OlderThan != 720*time.Hour {
		t.Errorf("Lifecycle.Compact.OlderThan = %s, want 720h",
			assembly.Lifecycle.Compact.OlderThan)
	}
}

func TestBuilderNewAssembly_RejectsOverlappingOperationsDeterministically(t *testing.T) {
	doc := decode(t, validYAML)
	factory := config.StoreFactoryFunc(func(context.Context, config.StoreInput) (config.StoreResult, error) {
		return config.StoreResult{Append: appendNoop{}}, nil
	})
	builder, err := config.NewBuilder(map[string]config.StoreFactory{"noop": factory})
	if err != nil {
		t.Fatal(err)
	}
	_, err = builder.NewAssembly(context.Background(), doc, inferenceDeps(t))
	if err == nil {
		t.Fatal("expected overlapping append error")
	}
	want := `stores["documents"] and stores["messages"] both provide append`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestBuilderStoreOwnershipSuccessAndRollback(t *testing.T) {
	t.Run("successful assembly closes stores in reverse slot order", func(t *testing.T) {
		doc := decode(t, validYAML)
		var order []string
		factory := config.StoreFactoryFunc(func(_ context.Context, in config.StoreInput) (config.StoreResult, error) {
			result := config.StoreResult{CloseFunc: func() error {
				order = append(order, in.StoreName)
				return nil
			}}
			if in.StoreName == config.StoreMessages {
				result.Append = appendNoop{}
			}
			return result, nil
		})
		builder, _ := config.NewBuilder(map[string]config.StoreFactory{"noop": factory})
		assembly, err := builder.NewAssembly(context.Background(), doc, inferenceDeps(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(order) != 0 {
			t.Fatal("store closed before assembly shutdown")
		}
		if err := assembly.Close(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(order, ","); got != "messages,documents" {
			t.Fatalf("close order = %q, want messages,documents", got)
		}
	})

	t.Run("failed build rolls back completed stores", func(t *testing.T) {
		doc := decode(t, validYAML)
		var order []string
		factory := config.StoreFactoryFunc(func(_ context.Context, in config.StoreInput) (config.StoreResult, error) {
			if in.StoreName == config.StoreMessages {
				return config.StoreResult{}, errors.New("build failed")
			}
			return config.StoreResult{CloseFunc: func() error {
				order = append(order, in.StoreName)
				return nil
			}}, nil
		})
		builder, _ := config.NewBuilder(map[string]config.StoreFactory{"noop": factory})
		if _, err := builder.NewAssembly(context.Background(), doc, inferenceDeps(t)); err == nil {
			t.Fatal("expected build failure")
		}
		if got := strings.Join(order, ","); got != "documents" {
			t.Fatalf("rollback order = %q, want documents", got)
		}
	})
}

func TestBuilderStoreOwnershipConflictCleansBothClosersOnce(t *testing.T) {
	doc := decode(t, validYAML)
	delete(doc.Stores, config.StoreDocuments)
	closeFuncCalls := 0
	closerCalls := 0
	factory := config.StoreFactoryFunc(func(context.Context, config.StoreInput) (config.StoreResult, error) {
		return config.StoreResult{
			CloseFunc: func() error {
				closeFuncCalls++
				return nil
			},
			Closer: closerFunc(func() error {
				closerCalls++
				return nil
			}),
		}, nil
	})
	builder, err := config.NewBuilder(map[string]config.StoreFactory{"noop": factory})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.NewAssembly(t.Context(), doc, inferenceDeps(t)); err == nil ||
		!strings.Contains(err.Error(), "both CloseFunc and Closer") {
		t.Fatalf("NewAssembly error = %v, want ownership conflict", err)
	}
	if closeFuncCalls != 1 || closerCalls != 1 {
		t.Fatalf("cleanup calls = CloseFunc %d, Closer %d; want 1 each",
			closeFuncCalls, closerCalls)
	}
}

func TestBuilderNewAssembly_UnknownImpl(t *testing.T) {
	bad := strings.Replace(validYAML,
		"  messages:\n    impl: noop",
		"  messages:\n    impl: postgres", 1)
	doc := decode(t, bad)
	builder, err := config.NewBuilder(map[string]config.StoreFactory{
		"noop": config.NoopStoreFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = builder.NewAssembly(context.Background(), doc, inferenceDeps(t))
	if err == nil {
		t.Fatal("expected error for unregistered store impl")
	} else if !strings.Contains(err.Error(), "postgres") {
		t.Errorf("error should mention postgres, got: %v", err)
	}
}

func TestBuilderNewAssembly_RuntimeUsable(t *testing.T) {
	doc := decode(t, validYAML)
	builder, _ := config.NewBuilder(map[string]config.StoreFactory{
		"noop": config.NoopStoreFactory{},
	})
	assembly, err := builder.NewAssembly(context.Background(), doc, inferenceDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	scope := memory.Scope{
		RuntimeID: "prod",
		UserID:    "tenant-1",
	}
	// All six ops must work against the noop-backed runtime.
	_, err = assembly.Runtime.ExecuteAppend(context.Background(), memory.AppendRequest{
		Scope:          scope,
		ConversationID: "c1",
		Records: []memory.Record{
			{ID: "r-1", Message: msg("hi")},
		},
	})
	if err != nil {
		t.Errorf("ExecuteAppend: %v", err)
	}
	_, err = assembly.Runtime.ExecuteLoad(context.Background(), memory.LoadRequest{
		Scope:          scope,
		ConversationID: "c1",
		Limit:          10,
	})
	if err != nil {
		t.Errorf("ExecuteLoad: %v", err)
	}
	_, err = assembly.Runtime.ExecuteRecall(context.Background(), memory.RecallRequest{
		Scope: scope,
		Query: "x",
		TopK:  3,
	})
	if err != nil {
		t.Errorf("ExecuteRecall: %v", err)
	}
	_, err = assembly.Runtime.ExecuteImport(context.Background(), memory.ImportRequest{
		Scope:  scope,
		Source: "memory://x",
	})
	if err != nil {
		t.Errorf("ExecuteImport: %v", err)
	}
	_, err = assembly.Runtime.ExecuteCompact(context.Background(), memory.CompactRequest{
		Scope:     scope,
		OlderThan: time.Now().Add(-720 * time.Hour),
	})
	if err != nil {
		t.Errorf("ExecuteCompact: %v", err)
	}
	_, err = assembly.Runtime.ExecuteArchive(context.Background(), memory.ArchiveRequest{
		Scope:       scope,
		OlderThan:   time.Now().Add(-4320 * time.Hour),
		Destination: "memory://archive",
	})
	if err != nil {
		t.Errorf("ExecuteArchive: %v", err)
	}
}

func TestBuilderNewBuilder_RejectsBadName(t *testing.T) {
	_, err := config.NewBuilder(map[string]config.StoreFactory{
		"Bad Name!": config.NoopStoreFactory{},
	})
	if err == nil {
		t.Fatal("expected error for invalid factory name")
	}
}

func TestBuilderNewBuilder_RejectsNilFactory(t *testing.T) {
	_, err := config.NewBuilder(map[string]config.StoreFactory{
		"noop": nil,
	})
	if err == nil {
		t.Fatal("expected error for nil factory")
	}
}

func TestBuilderNewAssembly_InferenceDepForwarded(t *testing.T) {
	type capture struct {
		runtime *inference.Runtime
		model   inference.ModelRef
	}
	var captured []capture
	spy := config.StoreFactoryFunc(func(_ context.Context, in config.StoreInput) (config.StoreResult, error) {
		captured = append(captured, capture{
			runtime: in.Inference,
			model:   in.Embedding.Model,
		})
		return config.StoreResult{}, nil
	})

	doc := decode(t, validYAML)
	builder, _ := config.NewBuilder(map[string]config.StoreFactory{
		"noop": spy,
	})
	runtime := newInferenceRuntime(t)
	assembly, err := builder.NewAssembly(context.Background(), doc, map[string]any{
		"inference": runtime,
	})
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })
	if len(captured) != len(doc.Stores) {
		t.Fatalf("factory calls = %d, want %d", len(captured), len(doc.Stores))
	}
	for i, got := range captured {
		if got.runtime != runtime {
			t.Errorf("call %d Inference = %p, want %p", i, got.runtime, runtime)
		}
		if got.model != doc.Embedding.Model {
			t.Errorf("call %d Embedding.Model = %+v, want %+v",
				i, got.model, doc.Embedding.Model)
		}
	}
}

func TestBuilderNewAssembly_ValidatesInferenceDependency(t *testing.T) {
	builder, err := config.NewBuilder(map[string]config.StoreFactory{
		"noop": config.NoopStoreFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	enabled := decode(t, validYAML)
	var typedNil *inference.Runtime
	for _, test := range []struct {
		name string
		deps map[string]any
	}{
		{name: "missing"},
		{name: "wrong type", deps: map[string]any{"inference": "not a runtime"}},
		{name: "typed nil", deps: map[string]any{"inference": typedNil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := builder.NewAssembly(t.Context(), enabled, test.deps)
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("NewAssembly error = %v, want validation", err)
			}
		})
	}

	disabled := decode(t, strings.Replace(validYAML, embeddingYAML, "embedding: {}", 1))
	assembly, err := builder.NewAssembly(t.Context(), disabled, nil)
	if err != nil {
		t.Fatalf("disabled embedding without inference: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })
}

func TestBuilderNewAssembly_ValidatesEmbeddingModelResolution(t *testing.T) {
	builder, err := config.NewBuilder(map[string]config.StoreFactory{
		"noop": config.NoopStoreFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		mutate  func(*config.Document)
		runtime func(*testing.T) *inference.Runtime
	}{
		{
			name: "unknown model",
			mutate: func(doc *config.Document) {
				doc.Embedding.Model.ID.Name = "missing-model"
			},
			runtime: newInferenceRuntime,
		},
		{
			name:   "model does not support embed",
			mutate: func(*config.Document) {},
			runtime: func(t *testing.T) *inference.Runtime {
				return newTestInferenceRuntime(t, false)
			},
		},
		{
			name: "unknown profile",
			mutate: func(doc *config.Document) {
				doc.Embedding.Model.Profile = "missing-profile"
			},
			runtime: newInferenceRuntime,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := decode(t, validYAML)
			test.mutate(&doc)
			_, err := builder.NewAssembly(t.Context(), doc, map[string]any{
				"inference": test.runtime(t),
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("NewAssembly error = %v, want validation", err)
			}
			if !strings.Contains(err.Error(), "embedding.model") {
				t.Fatalf("NewAssembly error = %v, want embedding.model context", err)
			}
		})
	}
}

func newInferenceRuntime(t *testing.T) *inference.Runtime {
	t.Helper()
	return newTestInferenceRuntime(t, true)
}

func newTestInferenceRuntime(t *testing.T, supportsEmbed bool) *inference.Runtime {
	t.Helper()
	openers := inference.Openers{}
	profileOperations := []inference.Operation{inference.OperationGenerate}
	if supportsEmbed {
		openers.Embed = func(
			context.Context,
			inference.ModelRef,
		) (inference.EmbedDriver, error) {
			return nil, errors.New("embedding opener must not run during config validation")
		}
		profileOperations = []inference.Operation{inference.OperationEmbed}
	} else {
		openers.Generate = func(
			context.Context,
			inference.ModelRef,
		) (inference.GenerateOperations, error) {
			return inference.GenerateOperations{}, nil
		}
	}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "openai",
		Profiles: []inference.ProfileDefinition{{
			ID:         "default",
			Operations: profileOperations,
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{
					Provider: "openai",
					Name:     "text-embedding-3-small",
				},
			},
			Openers: openers,
		}},
	}})
	if err != nil {
		t.Fatalf("inference.NewRuntime: %v", err)
	}
	return runtime
}

func inferenceDeps(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{"inference": newInferenceRuntime(t)}
}

// msg builds a minimal message.Message for tests.
func msg(text string) message.Message {
	return message.Message{
		Role: message.RoleUser,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: text},
		}},
	}
}

type appendNoop struct{}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

var _ io.Closer = closerFunc(nil)

func (appendNoop) CompileAppend(context.Context, memory.AppendRequest) memory.CompileResult {
	return memory.CompileResult{Op: memory.OpAppend, Decisions: []memory.Decision{
		{Field: memory.FieldAppendScope, Disposition: memory.DispositionNative},
		{Field: memory.FieldAppendConversationID, Disposition: memory.DispositionNative},
		{Field: memory.FieldAppendIdempotencyKey, Disposition: memory.DispositionNative},
		{Field: memory.FieldAppendRecords, Disposition: memory.DispositionNative},
		{Field: memory.FieldAppendMetadata, Disposition: memory.DispositionNative},
	}}
}

func (appendNoop) ExecuteAppend(context.Context, memory.AppendRequest) (memory.AppendResponse, error) {
	return memory.AppendResponse{}, nil
}
