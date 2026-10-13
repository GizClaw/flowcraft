package typesafe

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestDeclarationRejectsOutputKind pins the family contract: a decision model
// produces no content kind, because its answer is an answer to a question
// rather than content. A declared output is a capability the runtime would
// route on and the wire could never deliver, so the build fails instead.
func TestDeclarationRejectsOutputKind(t *testing.T) {
	declared := decisionSpec(DefaultModel)
	declared.Capabilities.Outputs = []message.PartKind{message.PartText}

	_, err := newProvider(t, providerSettingsJSON(t, modelsSpec(t, declared)))
	if err == nil {
		t.Fatal("New accepted a decision model that declares an output kind")
	}
	if !strings.Contains(err.Error(), "declares no generate output") {
		t.Errorf("error = %v, want the family contract named", err)
	}
}

// TestDeclarationRejectsImageInput pins the other half of the contract: the
// wire carries one text state value, so an image input kind is a capability the
// driver cannot honour and must refuse rather than advertise.
func TestDeclarationRejectsImageInput(t *testing.T) {
	declared := decisionSpec(DefaultModel)
	declared.Capabilities.Inputs = []message.PartKind{
		message.PartText,
		message.PartImage,
	}

	_, err := newProvider(t, providerSettingsJSON(t, modelsSpec(t, declared)))
	if err == nil {
		t.Fatal("New accepted a decision model that declares image input")
	}
	if !strings.Contains(err.Error(), "carries one text state value") {
		t.Errorf("error = %v, want the carrier semantics named", err)
	}
}

// TestDeclarationRejectsUnknownKind pins that the kind selects the family this
// driver serves and nothing else: the System One route answers questions, so a
// declaration for another workload's kind has no opener here.
func TestDeclarationRejectsUnknownKind(t *testing.T) {
	declared := decisionSpec(DefaultModel)
	declared.Kind = "generate"

	_, err := newProvider(t, providerSettingsJSON(t, modelsSpec(t, declared)))
	if err == nil {
		t.Fatal("New accepted a generate declaration from the decision driver")
	}
	if !strings.Contains(err.Error(), `unknown kind "generate"`) {
		t.Errorf("error = %v, want the unknown kind named", err)
	}
}

// TestUndeclaredModelIsNotServed pins fail-closed discovery: the driver ships no
// model line-up, so a model exists exactly because a declaration states it and
// there is no fallback catalog to reach for one that is missing.
func TestUndeclaredModelIsNotServed(t *testing.T) {
	provider := mustBuildProvider(t, providerSettingsJSON(t, modelsSpec(t, decisionSpec(DefaultModel))))

	served := make(map[string]bool, len(provider.Models))
	for _, declared := range provider.Models {
		served[declared.Descriptor.ID.Name] = true
	}
	if !served[DefaultModel] {
		t.Errorf("provider serves %v, want the declared model", served)
	}
	if served["jev-1.13.0"] {
		t.Errorf("provider serves a model the deployment never declared")
	}
}

// TestProviderWithoutDeclarationsServesNothing pins the same rule from the other
// side: a spec that declares no models exposes none, and building it still
// succeeds, because a provider may register before its line-up does.
func TestProviderWithoutDeclarationsServesNothing(t *testing.T) {
	provider := mustBuildProvider(t, providerSettingsJSON(t, `{}`))
	if len(provider.Models) != 0 {
		t.Fatalf("provider = %+v, want no models", provider.Models)
	}
}

// TestDeclarationCarriesCapabilitiesAndProfile pins the descriptor a declaration
// produces — the declared text capability and no output kind — and that the
// opener resolves through the profile the deployment declared.
func TestDeclarationCarriesCapabilitiesAndProfile(t *testing.T) {
	provider := mustBuildProvider(t, providerSettingsJSON(t, modelsSpec(t, decisionSpec(DefaultModel))))
	if len(provider.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(provider.Models))
	}
	declared := provider.Models[0]
	if len(declared.Descriptor.Capabilities.Outputs) != 0 {
		t.Errorf("outputs = %v, want none", declared.Descriptor.Capabilities.Outputs)
	}
	if len(declared.Descriptor.Capabilities.Inputs) != 1 ||
		declared.Descriptor.Capabilities.Inputs[0] != message.PartText {
		t.Errorf("inputs = %v, want text", declared.Descriptor.Capabilities.Inputs)
	}
	if declared.Openers.Decision == nil {
		t.Fatal("declaration has no decision opener")
	}
	if _, err := declared.Openers.Decision(
		context.Background(), typesafeModel(DefaultModel)); err != nil {
		t.Fatalf("open decision driver: %v", err)
	}
	if _, err := declared.Openers.Decision(
		context.Background(),
		typesafeModelInProfile(DefaultModel, "absent"),
	); err == nil {
		t.Error("openDecision accepted an undeclared profile")
	}
}
