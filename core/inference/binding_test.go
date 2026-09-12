package inference

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// multiOperationAssembly declares one model that serves generate and embed
// from a profile-restricted provider, which is the shape a profile-scoped
// credential has: the model is usable, but not for every operation it
// declares.
func multiOperationAssembly(t *testing.T) (*Assembly, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var opens, embeds atomic.Int64
	compile := GenerateCompiler[string](
		func(
			_ context.Context,
			_ ModelRef,
			request GenerateRequest,
			shape GenerateExecutionShape,
		) (Compiled[string], error) {
			fields := request.ActiveFieldsFor(shape)
			decisions := make([]Decision, len(fields))
			for index, field := range fields {
				decisions[index] = Decision{Field: field, Disposition: Native}
			}
			return Compiled[string]{
				Wire:   "wire",
				Report: CompileReport{Operation: OperationGenerate, Decisions: decisions},
			}, nil
		},
	)
	driver, err := BindGenerate(
		compile,
		Transport[string, string](func(context.Context, string) (string, error) {
			return "raw", nil
		}),
		Decoder[string, GenerateResponse](
			func(context.Context, string) (GenerateResponse, error) {
				return GenerateResponse{
					Message: message.Message{
						Role:    message.RoleAssistant,
						Content: message.NewTextContent("ok"),
					},
					FinishReason: FinishCompleted,
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	embed, err := BindEmbed(
		func(
			_ context.Context,
			_ ModelRef,
			request EmbedRequest,
		) (Compiled[string], error) {
			fields := request.ActiveFields()
			decisions := make([]Decision, len(fields))
			for index, field := range fields {
				decisions[index] = Decision{Field: field, Disposition: Native}
			}
			return Compiled[string]{
				Wire:   "wire",
				Report: CompileReport{Operation: OperationEmbed, Decisions: decisions},
			}, nil
		},
		Transport[string, string](func(context.Context, string) (string, error) {
			return "raw", nil
		}),
		Decoder[string, EmbedResponse](
			func(context.Context, string) (EmbedResponse, error) {
				return EmbedResponse{
					Embeddings: []Embedding{{Vector: []float32{1}}},
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindEmbed: %v", err)
	}
	return &Assembly{providers: map[string]ProviderDefinition{
		"multi": {
			ID: "multi",
			Profiles: []ProfileDefinition{
				{ID: "default", Operations: []Operation{OperationGenerate}},
			},
			Models: []ModelImplementation{{
				Descriptor: ModelDescriptor{
					ID: ModelID{Provider: "multi", Name: "model-1"},
				},
				Openers: Openers{
					Generate: func(
						context.Context, ModelRef,
					) (GenerateOperations, error) {
						opens.Add(1)
						return GenerateOperations{Unary: driver}, nil
					},
					Embed: func(context.Context, ModelRef) (EmbedDriver, error) {
						embeds.Add(1)
						return embed, nil
					},
				},
			}},
		},
	}}, &opens, &embeds
}

// TestBindingSkipsOperationTheProfileRefuses pins the per-operation rule: a
// profile allow-list that covers one of a model's operations must not fail the
// whole bind — the allowed operation stays usable, and the refused one reports
// the same rejection the per-call path produces.
func TestBindingSkipsOperationTheProfileRefuses(t *testing.T) {
	assembly, opens, embeds := multiOperationAssembly(t)
	ref := ModelRef{ID: ModelID{Provider: "multi", Name: "model-1"}, Profile: "default"}

	binding, err := assembly.Bind(context.Background(), ref)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if opens.Load() != 1 {
		t.Fatalf("generate openers ran %d times, want 1", opens.Load())
	}
	if embeds.Load() != 0 {
		t.Fatalf("embed opener ran %d times for a refused operation", embeds.Load())
	}
	if _, err := binding.PrepareGenerate(context.Background(), bindingRequest()); err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	embedRequest := EmbedRequest{Items: []EmbedItem{{Content: message.NewTextContent("hi")}}}
	_, err = binding.PrepareEmbed(context.Background(), embedRequest)
	if err == nil {
		t.Fatal("PrepareEmbed on a profile-refused operation succeeded")
	}
	if kind := errorKind(t, err); kind != UnsupportedOperation {
		t.Fatalf("PrepareEmbed kind = %q, want %q", kind, UnsupportedOperation)
	}
	// The caller has to be able to tell the profile refusal from "this model
	// has no embed driver", so the cause names the profile.
	if got := errors.Unwrap(err).Error(); !strings.Contains(got, `"default" does not allow embed`) {
		t.Fatalf("PrepareEmbed cause = %q, want the profile refusal", got)
	}

	// The per-call path must agree with the binding: both refuse and both
	// allow the same operation.
	if _, err := assembly.Embed(context.Background(), ref, embedRequest); err == nil ||
		errorKind(t, err) != UnsupportedOperation {
		t.Fatalf("Assembly.Embed = %v, want the same profile refusal", err)
	}
}

// errorKind extracts the inference error kind from err.
func errorKind(t *testing.T, err error) ErrorKind {
	t.Helper()
	var inferenceErr *Error
	if !errors.As(err, &inferenceErr) {
		t.Fatalf("error %v is not an *inference.Error", err)
	}
	return inferenceErr.Kind
}

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
