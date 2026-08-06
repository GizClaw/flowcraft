package inference

import (
	"context"
	"reflect"
	"testing"
)

type testExtension struct {
	provider string
	id       string
	fields   []ExtensionField
}

func (e testExtension) ProviderID() string  { return e.provider }
func (e testExtension) ExtensionID() string { return e.id }
func (e testExtension) ActiveFields() []ExtensionField {
	return append([]ExtensionField(nil), e.fields...)
}
func (testExtension) Validate() error    { return nil }
func (e testExtension) Clone() Extension { return e }

type lossyCloneExtension struct {
	fields []ExtensionField
}

func (lossyCloneExtension) ProviderID() string  { return "fake" }
func (lossyCloneExtension) ExtensionID() string { return "chat_options" }
func (e lossyCloneExtension) ActiveFields() []ExtensionField {
	return append([]ExtensionField(nil), e.fields...)
}
func (lossyCloneExtension) Validate() error { return nil }
func (e lossyCloneExtension) Clone() Extension {
	return lossyCloneExtension{fields: append([]ExtensionField(nil), e.fields[:1]...)}
}

type pointerExtension struct{}

func (*pointerExtension) ProviderID() string  { return "fake" }
func (*pointerExtension) ExtensionID() string { return "option" }
func (*pointerExtension) ActiveFields() []ExtensionField {
	return []ExtensionField{"value"}
}
func (*pointerExtension) Validate() error { return nil }
func (e *pointerExtension) Clone() Extension {
	if e == nil {
		return (*pointerExtension)(nil)
	}
	clone := *e
	return &clone
}

func TestExtensionsForProviderFiltersForeignAttempts(t *testing.T) {
	extensions := Extensions{
		testExtension{
			provider: "anthropic",
			id:       "thinking",
			fields:   []ExtensionField{"budget_tokens"},
		},
		nil,
		testExtension{
			provider: "openai",
			id:       "chat_options",
			fields:   []ExtensionField{"store"},
		},
	}
	filtered := extensions.ForProvider("openai")
	if len(filtered) != 1 || filtered[0].ExtensionID() != "chat_options" {
		t.Fatalf("filtered = %#v", filtered)
	}
	if rest := extensions.ForProvider("gemini"); rest != nil {
		t.Fatalf("unmatched provider subset = %#v, want nil", rest)
	}
	// The source list is untouched: filtering is per-attempt, not destructive.
	if len(extensions) != 3 {
		t.Fatalf("source mutated: %d entries", len(extensions))
	}
}

func TestExtensionFieldsAreQualifiedAndValidated(t *testing.T) {
	extension := testExtension{
		provider: "openai",
		id:       "chat_options",
		fields:   []ExtensionField{"service_tier", "store"},
	}
	extensions := Extensions{extension}
	if err := extensions.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := extensions.ActiveFields()
	want := []FieldID{
		"extension.openai.chat_options.service_tier",
		"extension.openai.chat_options.store",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ActiveFields = %v, want %v", got, want)
	}
	if got := (ExtensionField("store")).Qualify(extension); got != want[1] {
		t.Fatalf("Qualify = %q, want %q", got, want[1])
	}

	for _, fields := range [][]ExtensionField{
		nil,
		{"store", "store"},
		{"invalid.field"},
	} {
		if err := (Extensions{testExtension{
			provider: "openai",
			id:       "chat_options",
			fields:   fields,
		}}).Validate(); err == nil {
			t.Fatalf("fields %v unexpectedly validated", fields)
		}
	}
}

func TestExtensionCloneMustPreserveActiveFields(t *testing.T) {
	type wire struct{}
	compileCalls := 0
	driver, err := BindGenerate(
		func(
			_ context.Context,
			_ ModelRef,
			request GenerateRequest,
			shape GenerateExecutionShape,
		) (Compiled[wire], error) {
			compileCalls++
			active := request.ActiveFieldsFor(shape)
			decisions := make([]Decision, 0, len(active))
			for _, field := range active {
				decisions = append(decisions, Decision{Field: field, Disposition: Native})
			}
			return Compiled[wire]{Report: CompileReport{
				Operation: OperationGenerate,
				Decisions: decisions,
			}}, nil
		},
		func(context.Context, wire) (struct{}, error) { return struct{}{}, nil },
		func(context.Context, struct{}) (GenerateResponse, error) {
			return GenerateResponse{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	request := validGenerateTextRequest()
	request.Extensions = Extensions{lossyCloneExtension{
		fields: []ExtensionField{"service_tier", "store"},
	}}
	_, err = driver.Explain(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
		request,
	)
	if !IsKind(err, CompilerContractViolation) || compileCalls != 0 {
		t.Fatalf("Explain error=%v compile calls=%d", err, compileCalls)
	}
}

func TestTypedNilExtensionIsRejected(t *testing.T) {
	var extension *pointerExtension
	request := validGenerateTextRequest()
	request.Extensions = []Extension{extension}
	if err := request.Validate(); err == nil {
		t.Fatal("expected typed-nil extension validation error")
	}
}

// Foreign extensions must not fail or leak into an attempt for another
// provider: the pipeline strips them before compilation, so a request may
// carry several providers' settings across route fallback.
func TestPipelineAppliesOnlyOwnProviderExtensions(t *testing.T) {
	type wire struct{ Prompt string }
	operations, err := BindGenerateOperations(
		nativeGenerateCompile(wire{Prompt: "native"}),
		func(context.Context, wire) (string, error) { return "ok", nil },
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, nil
		},
		func(context.Context, wire) (ProviderStream[string], error) {
			return &generateStringStream{}, nil
		},
		func(context.Context, string) (GenerateStreamEvent, error) {
			return GenerateStreamEvent{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	model := ModelRef{ID: ModelID{Provider: "openai", Name: "generate"}}
	request := validGenerateTextRequest()
	own := testExtension{
		provider: "openai",
		id:       "chat_options",
		fields:   []ExtensionField{"store"},
	}
	foreign := testExtension{
		provider: "anthropic",
		id:       "thinking",
		fields:   []ExtensionField{"budget_tokens"},
	}
	request.Extensions = Extensions{foreign, own}
	before := request.Clone()

	explanation, err := operations.Unary.Explain(context.Background(), model, request)
	if err != nil {
		t.Fatalf("Explain with mixed extensions: %v", err)
	}
	ownField := FieldID("extension.openai.chat_options.store")
	foreignField := FieldID("extension.anthropic.thinking.budget_tokens")
	var sawOwn bool
	for _, decision := range explanation.Decisions {
		if decision.Field == foreignField {
			t.Fatalf("foreign extension leaked into decisions: %+v", decision)
		}
		if decision.Field == ownField {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Fatalf("own extension missing from decisions: %+v", explanation.Decisions)
	}
	if !reflect.DeepEqual(before, request.Clone()) {
		t.Fatal("pipeline mutated the caller request")
	}
}
