package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestDeclaredLineUpPublishesItsFacts locks the declaration contract: the
// provider publishes exactly the models a deployment declares, each carrying
// the capabilities, limits, and lifecycle it stated, under the deployment's
// own provider identity and in a stable order.
func TestDeclaredLineUpPublishesItsFacts(t *testing.T) {
	settings := ResourceSettings{
		ID: "gateway",
		Spec: json.RawMessage(`{"models":[
			{"name":"old-claude","kind":"generate",
			 "capabilities":{"inputs":["text","image"],"outputs":["text"]},
			 "limits":{"max_input_tokens":200000,"max_output_tokens":64000},
			 "lifecycle":{"status":"deprecated","replacement":
			   {"provider":"gateway","name":"new-claude"}}},
			{"name":"new-claude","kind":"generate",
			 "capabilities":{"inputs":["text"],"outputs":["text"]}}
		]}`),
	}
	provider, err := buildProvider(context.Background(), settings, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	var names []string
	byName := make(map[string]model.ModelDescriptor, len(provider.Models))
	for _, impl := range provider.Models {
		names = append(names, impl.Descriptor.ID.Name)
		byName[impl.Descriptor.ID.Name] = impl.Descriptor
	}
	if len(names) != 2 || names[0] != "new-claude" || names[1] != "old-claude" {
		t.Fatalf("published models = %v, want them sorted", names)
	}
	old := byName["old-claude"]
	if !slicesContainKind(old.Capabilities.Inputs, message.PartImage) ||
		len(old.Capabilities.Outputs) != 1 ||
		old.Capabilities.Outputs[0] != message.PartText {
		t.Fatalf("old-claude capabilities = %+v", old.Capabilities)
	}
	if in, out := old.Limits.Values(); in != 200_000 || out != 64_000 {
		t.Fatalf("old-claude limits = %d/%d", in, out)
	}
	if old.Lifecycle.Status != model.ModelStatusDeprecated ||
		old.Lifecycle.Replacement == nil ||
		old.Lifecycle.Replacement.Provider != "gateway" {
		t.Fatalf("old-claude lifecycle = %+v", old.Lifecycle)
	}
	if active := byName["new-claude"].Lifecycle; active != (model.ModelLifecycle{}) {
		t.Fatalf("undeclared lifecycle = %+v, want empty", active)
	}
}

// TestLifecycleRejections keeps the discovery facts honest: a model cannot
// replace itself and an unknown status is not a lifecycle.
func TestLifecycleRejections(t *testing.T) {
	for _, raw := range []string{
		`{"name":"m","kind":"generate","capabilities":{"outputs":["text"]},` +
			`"lifecycle":{"status":"deprecated","replacement":
			  {"provider":"gateway","name":"m"}}}`,
		`{"name":"m","kind":"generate","capabilities":{"outputs":["text"]},` +
			`"lifecycle":{"status":"sunset"}}`,
	} {
		settings := ResourceSettings{
			ID:   "gateway",
			Spec: json.RawMessage(`{"models":[` + raw + `]}`),
		}
		if _, err := buildProvider(context.Background(), settings, nil); err == nil {
			t.Fatalf("buildProvider accepted %s", raw)
		}
	}
}

// slicesContainKind keeps the assertion readable without importing slices for
// one call.
func slicesContainKind(kinds []message.PartKind, want message.PartKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}
