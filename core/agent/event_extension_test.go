package agent_test

import (
	"testing"

	agent "github.com/GizClaw/flowcraft/core/agent"
)

type extensionPayload struct {
	Phase string
	Step  int
}

// TestEventExtensionKeepsTheVocabularyClosed pins both halves of the contract: the
// kind is the framework's one value, and the payload is the engine's own type.
func TestEventExtensionKeepsTheVocabularyClosed(t *testing.T) {
	event := agent.EventExtension[extensionPayload]{
		Name:  "graph.state",
		Value: extensionPayload{Phase: "night_wolf", Step: 3},
	}
	if got := event.Kind(); got != agent.KindExtension {
		t.Fatalf("kind = %q, want %q", got, agent.KindExtension)
	}

	// A consumer that knows the engine gets the typed payload; one that does not
	// gets the name and the value erased.
	var typed agent.Event = event
	switched, ok := typed.(agent.EventExtension[extensionPayload])
	if !ok || switched.Value.Phase != "night_wolf" || switched.Value.Step != 3 {
		t.Fatalf("typed view = %+v", switched)
	}
	erased, ok := typed.(agent.Extension)
	if !ok {
		t.Fatal("an extension must offer the type-erased view")
	}
	if erased.ExtensionName() != "graph.state" {
		t.Fatalf("name = %q", erased.ExtensionName())
	}
	if payload, ok := erased.ExtensionValue().(extensionPayload); !ok || payload.Phase != "night_wolf" {
		t.Fatalf("payload = %#v", erased.ExtensionValue())
	}
}
