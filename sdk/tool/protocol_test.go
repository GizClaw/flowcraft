package tool

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
)

func TestProtocolTypesRoundTrip(t *testing.T) {
	def := message.Definition{
		Name:        "search",
		Description: "search documents",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	call, err := message.NewCall("call-1", "search", map[string]string{"query": "flowcraft"})
	if err != nil {
		t.Fatalf("NewCall: %v", err)
	}
	result := message.Result{CallID: call.ID, Content: "found"}

	if err := def.Validate(); err != nil {
		t.Fatalf("Definition.Validate: %v", err)
	}
	if err := call.Validate(); err != nil {
		t.Fatalf("Call.Validate: %v", err)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("Result.Validate: %v", err)
	}
	if got := string(call.Arguments); got != `{"query":"flowcraft"}` {
		t.Fatalf("Arguments = %s", got)
	}
}

func TestProtocolValidationRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"definition without name", (message.Definition{}).Validate()},
		{"call without id", (message.Call{Name: "search", Arguments: json.RawMessage(`{}`)}).Validate()},
		{"call with invalid arguments", (message.Call{ID: "1", Name: "search", Arguments: json.RawMessage(`[]`)}).Validate()},
		{"result without call id", (message.Result{Content: "x"}).Validate()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestProtocolCloneCopiesRawJSON(t *testing.T) {
	definition := message.Definition{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	call := message.Call{
		ID:        "call-1",
		Name:      "search",
		Arguments: json.RawMessage(`{"query":"x"}`),
	}
	definitionClone := definition.Clone()
	callClone := call.Clone()

	definitionClone.InputSchema[0] = '['
	callClone.Arguments[0] = '['
	if string(definition.InputSchema) != `{"type":"object"}` {
		t.Fatalf("definition clone mutated source: %s", definition.InputSchema)
	}
	if string(call.Arguments) != `{"query":"x"}` {
		t.Fatalf("call clone mutated source: %s", call.Arguments)
	}
}
