package inference_test

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
)

// The contract's own per-call costs. Every pipeline call validates the request,
// names its active fields (the compiler has to account for all of them) and filters
// extensions down to its own provider, so these three run once per model call before
// any provider code does.

type benchExtension struct {
	provider string
	id       string
}

func (e benchExtension) ProviderID() string  { return e.provider }
func (e benchExtension) ExtensionID() string { return e.id }
func (e benchExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"value"}
}
func (e benchExtension) Validate() error            { return nil }
func (e benchExtension) Clone() inference.Extension { return e }

func benchRequest(b *testing.B) inference.GenerateRequest {
	b.Helper()
	tools, err := inference.NewToolset([]message.ToolDefinition{
		{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name: "exec", Description: "run", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
	})
	if err != nil {
		b.Fatalf("NewToolset: %v", err)
	}
	request := inference.GenerateRequest{
		Model:   model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-4o-mini"}},
		Context: []message.Message{message.NewTextMessage(message.RoleUser, "an earlier turn"), message.NewTextMessage(message.RoleAssistant, "an answer")},
		Input:   inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("now do the thing")},
		Output:  inference.OutputText,
		Tools:   tools,
		Extensions: inference.Extensions{
			benchExtension{provider: "openai", id: "verbosity"},
			benchExtension{provider: "other", id: "inert"},
		},
	}
	if err := request.Validate(); err != nil {
		b.Fatalf("request: %v", err)
	}
	return request
}

func BenchmarkGenerateRequestValidate(b *testing.B) {
	request := benchRequest(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := request.Validate(); err != nil {
			b.Fatalf("Validate: %v", err)
		}
	}
}

// BenchmarkActiveFields is the half of the contract check the request owns: the
// compiler reports what it honoured, and this is what it is compared against.
func BenchmarkActiveFields(b *testing.B) {
	request := benchRequest(b)
	if len(request.ActiveFields()) == 0 {
		b.Fatal("ActiveFields returned nothing")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = request.ActiveFields()
	}
}

// BenchmarkExtensionsForProvider is the filter every pipeline runs before compiling:
// a driver must not have to drop another provider's options itself.
func BenchmarkExtensionsForProvider(b *testing.B) {
	extensions := inference.Extensions{
		benchExtension{provider: "openai", id: "verbosity"},
		benchExtension{provider: "other", id: "inert"},
		benchExtension{provider: "other", id: "inert2"},
	}
	if len(extensions.ForProvider("openai")) != 1 {
		b.Fatal("the filter did not keep exactly one extension")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = extensions.ForProvider("openai")
	}
}
