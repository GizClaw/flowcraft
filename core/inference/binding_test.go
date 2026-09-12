package inference

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// bindingAssembly builds a one-model generate provider with counting openers
// and compilers. openerErr, when non-nil, makes opening fail — the shape a
// missing credential takes.
func bindingAssembly(
	t *testing.T,
	opens, compiles *atomic.Int64,
	openerErr error,
) *Assembly {
	t.Helper()
	compile := GenerateCompiler[string](
		func(
			_ context.Context,
			_ ModelRef,
			request GenerateRequest,
			shape GenerateExecutionShape,
		) (Compiled[string], error) {
			compiles.Add(1)
			fields := request.ActiveFieldsFor(shape)
			decisions := make([]Decision, len(fields))
			for index, field := range fields {
				decisions[index] = Decision{Field: field, Disposition: Native}
			}
			return Compiled[string]{
				Wire: "wire",
				Report: CompileReport{
					Operation: OperationGenerate,
					Decisions: decisions,
				},
			}, nil
		},
	)
	transport := Transport[string, string](
		func(context.Context, string) (string, error) { return "raw", nil },
	)
	decode := Decoder[string, GenerateResponse](
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{
						Parts: []message.Part{message.TextPart{Text: "ok"}},
					},
				},
				FinishReason: FinishCompleted,
				Usage: Usage{
					InputTokens:  3,
					OutputTokens: 4,
					TotalTokens:  7,
				},
			}, nil
		},
	)
	driver, err := BindGenerate(compile, transport, decode)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	// Enough credential profiles to address more distinct references than the
	// binding cache retains, which is what exercises eviction.
	profiles := []ProfileDefinition{{}}
	for index := 1; index <= maxCachedBindings+4; index++ {
		profiles = append(profiles, ProfileDefinition{ID: fmt.Sprintf("p%d", index)})
	}
	return &Assembly{providers: map[string]ProviderDefinition{
		"binding": {
			ID:       "binding",
			Profiles: profiles,
			Models: []ModelImplementation{{
				Descriptor: ModelDescriptor{
					ID: ModelID{Provider: "binding", Name: "model-1"},
				},
				Openers: Openers{
					Generate: func(
						context.Context, ModelRef,
					) (GenerateOperations, error) {
						opens.Add(1)
						if openerErr != nil {
							return GenerateOperations{}, openerErr
						}
						return GenerateOperations{Unary: driver}, nil
					},
				},
			}},
		},
	}}
}

var bindingRef = ModelRef{ID: ModelID{Provider: "binding", Name: "model-1"}}

func bindingRequest() GenerateRequest {
	return GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			Intent:  Intent{Text: &TextIntent{}},
		},
	}}
}

// TestBindingOpensOnceAndPreparesPerRequest pins the split between the two
// lifetimes: opening is deployment-scoped and happens once per binding, while
// preparing is request-scoped and compiles per request. Executing a handle
// reuses both — it neither reopens nor recompiles.
func TestBindingOpensOnceAndPreparesPerRequest(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles, nil)
	ctx := context.Background()

	binding, err := assembly.Bind(ctx, bindingRef)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens after Bind = %d, want 1", got)
	}
	if binding.Ref() != bindingRef {
		t.Fatalf("Ref() = %+v, want %+v", binding.Ref(), bindingRef)
	}

	first, err := binding.PrepareGenerate(ctx, bindingRequest())
	if err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	second, err := binding.PrepareGenerate(ctx, bindingRequest())
	if err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	if got := compiles.Load(); got != 2 {
		t.Fatalf("compiles after two prepares = %d, want 2", got)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens after two prepares = %d, want 1 (binding is reused)", got)
	}
	for name, prepared := range map[string]*Prepared[GenerateResponse]{
		"first": first, "second": second,
	} {
		response, err := prepared.Execute(ctx)
		if err != nil {
			t.Fatalf("%s Execute: %v", name, err)
		}
		if response.FinishReason != FinishCompleted {
			t.Fatalf("%s FinishReason = %q", name, response.FinishReason)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens after execution = %d, want 1", got)
	}
	if got := compiles.Load(); got != 2 {
		t.Fatalf("compiles after execution = %d, want 2", got)
	}
}

// TestBindingDefersCredentialFailureToFirstUse pins the deployment rule the
// module adopted: credentials are resolved when a binding is opened, never when
// the deployment is assembled, so a declared-but-unconfigured provider does not
// fail the build.
func TestBindingDefersCredentialFailureToFirstUse(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles,
		errors.New("profile needs an api_key"))

	if err := assembly.Validate(); err != nil {
		t.Fatalf("assembly validation must not need credentials: %v", err)
	}
	if _, err := assembly.InspectModel(bindingRef); err != nil {
		t.Fatalf("inspection must not need credentials: %v", err)
	}
	if _, err := assembly.Bind(context.Background(), bindingRef); err == nil {
		t.Fatal("Bind must surface the missing credential")
	}
}

// TestPreparedExplanationAndModel pins what a prepared attempt reports about
// itself without executing anything.
func TestPreparedExplanationAndModel(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles, nil)
	binding, err := assembly.Bind(context.Background(), bindingRef)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	prepared, err := binding.PrepareGenerate(context.Background(), bindingRequest())
	if err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	if prepared.Model() != bindingRef {
		t.Fatalf("Model() = %+v, want %+v", prepared.Model(), bindingRef)
	}
	explanation := prepared.Explanation()
	if explanation.Operation != OperationGenerate {
		t.Fatalf("Operation = %q", explanation.Operation)
	}
	if explanation.Model != bindingRef {
		t.Fatalf("Model = %+v, want %+v", explanation.Model, bindingRef)
	}
	if len(explanation.Decisions) == 0 {
		t.Fatal("Explanation carries no decisions")
	}
	for _, decision := range explanation.Decisions {
		if decision.Disposition != Native {
			t.Fatalf("unexpected disposition %q for %q", decision.Disposition, decision.Field)
		}
	}
}

// TestBindingPrepareRejectsAboveDeclaredLimit pins that the declaration check
// is part of preparing, not executing: the rejection happens before any
// provider work, and the handle is never produced.
func TestBindingPrepareRejectsAboveDeclaredLimit(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles, nil)
	for index := range assembly.providers["binding"].Models {
		model := assembly.providers["binding"].Models[index]
		model.Descriptor.Limits = ModelLimits{}.WithMaxOutputTokens(100)
		assembly.providers["binding"].Models[index] = model
	}
	binding, err := assembly.Bind(context.Background(), bindingRef)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	requested := 101
	request := bindingRequest()
	request.Input.Content.Intent.Text.MaxOutputTokens = &requested

	if _, err := binding.PrepareGenerate(context.Background(), request); !IsKind(err, UnsupportedFeature) {
		t.Fatalf("PrepareGenerate err = %v, want %v", err, UnsupportedFeature)
	}
	if got := compiles.Load(); got != 0 {
		t.Fatalf("compiles after a rejected request = %d, want 0", got)
	}
}

// TestAssemblyPrepareReportsUnsupportedOperation pins the shared helper's
// availability check on the Assembly path.
func TestAssemblyPrepareReportsUnsupportedOperation(t *testing.T) {
	assembly := &Assembly{providers: map[string]ProviderDefinition{
		"embed-only": {
			ID: "embed-only",
			Models: []ModelImplementation{{
				Descriptor: ModelDescriptor{
					ID: ModelID{Provider: "embed-only", Name: "model-1"},
				},
				Openers: Openers{
					Generate: func(
						context.Context, ModelRef,
					) (GenerateOperations, error) {
						return GenerateOperations{}, nil
					},
				},
			}},
		},
	}}
	ref := ModelRef{ID: ModelID{Provider: "embed-only", Name: "model-1"}}
	if _, err := assembly.PrepareGenerate(
		context.Background(), ref, bindingRequest(),
	); !IsKind(err, UnsupportedOperation) {
		t.Fatalf("PrepareGenerate err = %v, want %v", err, UnsupportedOperation)
	}
}
