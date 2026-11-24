package inference_test

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
)

// stubOption is one extension value whose identity and payload the tests can
// inspect after a copy.
type stubOption struct {
	value string
}

func (s stubOption) ProviderID() string  { return "openai" }
func (s stubOption) ExtensionID() string { return "option" }
func (s stubOption) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"value"}
}
func (s stubOption) Validate() error            { return nil }
func (s stubOption) Clone() inference.Extension { return s }

// TestGenerateRequestCloneSharesNothing is the property callers rely on when they
// hand a request to a retry loop or another goroutine: the clone must not be able
// to rewrite the original's messages, and the original must not be able to rewrite
// the clone's.
func TestGenerateRequestCloneSharesNothing(t *testing.T) {
	original := inference.GenerateRequest{
		Model:   model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-4o-mini"}},
		Context: []message.Message{message.NewTextMessage(message.RoleUser, "first turn")},
		Input:   inference.GenerateInput{Content: message.NewTextContent("now")},
		Extensions: inference.Extensions{
			stubOption{value: "low"},
		},
	}
	clone := original.Clone()

	textOf := func(content message.Content) string {
		part, ok := content.Parts[0].(message.TextPart)
		if !ok {
			t.Fatalf("part = %T, want message.TextPart", content.Parts[0])
		}
		return part.Text
	}

	if len(clone.Context) != 1 || textOf(clone.Context[0].Content) != "first turn" {
		t.Fatalf("clone lost the context: %+v", clone.Context)
	}

	// Replacing an element and an element's part must both stay local to the
	// clone: the message slice and every content slice inside it are separate.
	clone.Context[0] = message.NewTextMessage(message.RoleUser, "replaced")
	clone.Context[0].Content.Parts[0] = message.TextPart{Text: "rewritten"}
	clone.Input.Content.Parts[0] = message.TextPart{Text: "also rewritten"}

	if got := textOf(original.Context[0].Content); got != "first turn" {
		t.Fatalf("mutating the clone changed the original's message to %q", got)
	}
	if got := textOf(original.Input.Content); got != "now" {
		t.Fatalf("mutating the clone's input changed the original to %q", got)
	}

	// The extension is a value type here, so the copy proves the slice is separate.
	clone.Extensions[0] = stubOption{value: "high"}
	if got := original.Extensions[0].(stubOption).value; got != "low" {
		t.Fatalf("mutating the clone's extensions changed the original to %q", got)
	}
}

func TestExtensionsCloneKeepsOrderAndNilness(t *testing.T) {
	if got := inference.Extensions(nil).Clone(); got != nil {
		t.Fatalf("Clone(nil) = %v, want nil", got)
	}
	original := inference.Extensions{stubOption{value: "a"}, stubOption{value: "b"}}
	clone := original.Clone()
	if len(clone) != 2 || clone[0].ExtensionID() != "option" || clone[1].(stubOption).value != "b" {
		t.Fatalf("Clone = %v", clone)
	}
	clone[0] = stubOption{value: "changed"}
	if got := original[0].(stubOption).value; got != "a" {
		t.Fatalf("Clone shares its backing array: original[0] = %q", got)
	}
}

// TestToolsetConstructorsRejectBadDefinitions is the gate that keeps a request
// from carrying a tool list the model cannot use: a duplicate name makes a call
// ambiguous, and an invalid definition is one no provider can compile.
func TestToolsetConstructorsRejectBadDefinitions(t *testing.T) {
	valid := message.ToolDefinition{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)}
	duplicate := []message.ToolDefinition{valid, valid}
	invalid := []message.ToolDefinition{{Name: ""}}

	for name, build := range map[string]func([]message.ToolDefinition) (inference.Toolset, error){
		"NewToolset":      inference.NewToolset,
		"NewToolsetOwned": inference.NewToolsetOwned,
	} {
		t.Run(name+"/duplicate", func(t *testing.T) {
			if _, err := build(duplicate); err == nil {
				t.Fatal("a duplicate tool name was accepted")
			}
		})
		t.Run(name+"/invalid", func(t *testing.T) {
			if _, err := build(invalid); err == nil {
				t.Fatal("an invalid definition was accepted")
			}
		})
		t.Run(name+"/empty", func(t *testing.T) {
			toolset, err := build(nil)
			if err != nil {
				t.Fatalf("build(nil): %v", err)
			}
			if len(toolset.Definitions()) != 0 {
				t.Fatalf("Definitions = %v, want none", toolset.Definitions())
			}
		})
	}
}

// TestNewToolsetDoesNotAliasItsInput is the difference between the two
// constructors: NewToolset copies, NewToolsetOwned trusts the caller because the
// caller has already promised not to mutate.
func TestNewToolsetDoesNotAliasItsInput(t *testing.T) {
	definitions := []message.ToolDefinition{
		{Name: "read_file", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	toolset, err := inference.NewToolset(definitions)
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	definitions[0].Description = "rewritten"

	if got := toolset.Definitions()[0].Description; got != "read a file" {
		t.Fatalf("NewToolset aliased its input: description = %q", got)
	}
	if !toolset.Has("read_file") {
		t.Fatal("the name index lost the tool")
	}
}
