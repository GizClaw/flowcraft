package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

func TestModelCapabilitiesValidate(t *testing.T) {
	capabilities := ModelCapabilities{
		Inputs:  []message.PartKind{message.PartText, message.PartImage},
		Outputs: []message.PartKind{message.PartText},
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("valid capabilities: %v", err)
	}

	for _, outputs := range [][]message.PartKind{
		{message.PartToolCall},
		{message.PartFile, message.PartImage},
		{message.PartText, message.PartText},
		{message.PartImage, message.PartText, message.PartImage},
	} {
		if err := (ModelCapabilities{Outputs: outputs}).Validate(); err == nil {
			t.Fatalf("outputs %v unexpectedly accepted", outputs)
		}
	}

	for _, inputs := range [][]message.PartKind{
		{message.PartText, message.PartText},
		{"unknown_kind"},
	} {
		if err := (ModelCapabilities{Inputs: inputs}).Validate(); err == nil {
			t.Fatalf("inputs %v unexpectedly accepted", inputs)
		}
	}
}

func TestModelCapabilitiesCloneDoesNotShareSlices(t *testing.T) {
	original := ModelCapabilities{
		Inputs:  []message.PartKind{message.PartText, message.PartImage},
		Outputs: []message.PartKind{message.PartText},
	}
	clone := original.Clone()
	clone.Inputs[0] = message.PartAudio
	clone.Outputs[0] = message.PartImage
	if original.Inputs[0] != message.PartText || original.Outputs[0] != message.PartText {
		t.Fatalf("clone shares capability slices: original = %+v", original)
	}
}

func TestModelCapabilitiesBuilders(t *testing.T) {
	capabilities := ModelCapabilities{}.
		WithInputs(message.PartText, message.PartImage).
		WithInputs(message.PartData).
		WithOutputs(message.PartText).
		WithToolCalling().
		WithHostedWebSearch()
	wantInputs := []message.PartKind{
		message.PartText,
		message.PartImage,
		message.PartData,
	}
	if !reflect.DeepEqual(capabilities.Inputs, wantInputs) {
		t.Fatalf("inputs = %v, want %v", capabilities.Inputs, wantInputs)
	}
	if !reflect.DeepEqual(capabilities.Outputs, []message.PartKind{message.PartText}) {
		t.Fatalf("outputs = %v", capabilities.Outputs)
	}
	if !capabilities.ToolCalling || !capabilities.HostedWebSearch {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestToolCallingRoundTrip pins the capability bit that lets a driver reject
// tools against a model with no tool channel before any provider I/O.
func TestToolCallingRoundTrip(t *testing.T) {
	var capabilities ModelCapabilities
	if err := json.Unmarshal([]byte(`{"outputs":["text"],"tool_calling":true}`), &capabilities); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !capabilities.ToolCalling {
		t.Fatal("tool_calling: true must set the capability")
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(encoded); got != `{"outputs":["text"],"tool_calling":true}` {
		t.Fatalf("marshal = %s", got)
	}
}

func TestModelCapabilitiesBuildersDoNotAlias(t *testing.T) {
	base := ModelCapabilities{}.WithInputs(message.PartText)
	extended := base.WithInputs(message.PartImage)
	if len(base.Inputs) != 1 || base.Inputs[0] != message.PartText {
		t.Fatalf("base inputs mutated by builder: %v", base.Inputs)
	}
	_ = extended
}
