package model

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

func TestModelCapabilitiesValidate(t *testing.T) {
	capabilities := ModelCapabilities{
		Inputs: []message.PartKind{message.PartText, message.PartImage},
		Modes:  []Mode{ModeText},
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("valid capabilities: %v", err)
	}

	for _, modes := range [][]Mode{
		{"file"},
		{""},
		{ModeText, ModeText},
		{ModeImage, ModeText, ModeImage},
	} {
		if err := (ModelCapabilities{Modes: modes}).Validate(); err == nil {
			t.Fatalf("modes %v unexpectedly accepted", modes)
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
		Inputs: []message.PartKind{message.PartText, message.PartImage},
		Modes:  []Mode{ModeText},
	}
	clone := original.Clone()
	clone.Inputs[0] = message.PartAudio
	clone.Modes[0] = ModeImage
	if original.Inputs[0] != message.PartText || original.Modes[0] != ModeText {
		t.Fatalf("clone shares capability slices: original = %+v", original)
	}
}

func TestModelCapabilitiesBuilders(t *testing.T) {
	capabilities := ModelCapabilities{}.
		WithInputs(message.PartText, message.PartImage).
		WithInputs(message.PartData).
		WithModes(ModeText, ModeToolCall).
		WithHostedWebSearch()
	wantInputs := []message.PartKind{
		message.PartText,
		message.PartImage,
		message.PartData,
	}
	if !reflect.DeepEqual(capabilities.Inputs, wantInputs) {
		t.Fatalf("inputs = %v, want %v", capabilities.Inputs, wantInputs)
	}
	if !reflect.DeepEqual(capabilities.Modes, []Mode{ModeText, ModeToolCall}) {
		t.Fatalf("modes = %v", capabilities.Modes)
	}
	if !capabilities.HostedWebSearch {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestToolCallModeRoundTrip pins the capability bit that lets a driver reject
// tools against a model with no tool channel before any provider I/O.
func TestToolCallModeRoundTrip(t *testing.T) {
	var capabilities ModelCapabilities
	if err := json.Unmarshal([]byte(`{"modes":["text","tool_call"],"hosted_web_search":true}`), &capabilities); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !capabilities.Can(ModeToolCall) {
		t.Fatal("tool_call: true must set the mode")
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(encoded); got != `{"modes":["text","tool_call"],"hosted_web_search":true}` {
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
