package typesafe

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// The driver ships no model line-up: a declaration is the whole fact about a
// model, so the tests carry the declarations they exercise.

// evidenceFixture is the state every decision test starts from — the vendor's
// own example, so a failure reads against the documented shape.
const evidenceFixture = "Help! My payouts have been failing for 3 days."

// decisionCapabilities is the declaration the family contract requires for a
// decision model: text evidence in, because the wire carries one state value,
// and no output kind at all, because a decision model answers questions rather
// than producing content.
func decisionCapabilities() model.ModelCapabilities {
	return model.ModelCapabilities{Inputs: []message.PartKind{message.PartText}}
}

// decisionSpec declares one decision model named name.
func decisionSpec(name string) ModelSpec {
	return ModelSpec{
		Name:         name,
		Kind:         string(kindDecision),
		Capabilities: decisionCapabilities(),
	}
}

// providerSettingsJSON renders one provider settings document over the given
// declarations, with the profile every deployment needs.
func providerSettingsJSON(t *testing.T, spec string) string {
	t.Helper()
	return `{
		"id": "typesafe",
		"spec": ` + spec + `,
		"profiles": [{
			"id": "default",
			"operations": ["decision"],
			"secrets": {"api_key": "sk-test"}
		}]
	}`
}

// modelsSpec renders the spec document of a deployment that serves models.
func modelsSpec(t *testing.T, models ...ModelSpec) string {
	t.Helper()
	declared, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("marshal declarations: %v", err)
	}
	return `{"models": ` + string(declared) + `}`
}

// newProvider is the deployment path under test: a settings document in, a
// provider definition out. It returns the factory's error so a test can assert
// a rejection instead of only its absence.
func newProvider(t *testing.T, settings string) (inference.ProviderDefinition, error) {
	t.Helper()
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: []byte(settings),
	})
	if err != nil {
		return inference.ProviderDefinition{}, err
	}
	provider, ok := value.(inference.ProviderDefinition)
	if !ok {
		t.Fatalf("New returned %T, want inference.ProviderDefinition", value)
	}
	return provider, nil
}

// mustBuildProvider builds one provider definition from a settings document,
// the way a deployment does: the resource factory decodes the settings
// strictly, resolves the profiles, and binds each declared model to the opener
// its kind serves. It is named apart from the production buildProvider the
// factory calls, because both live in this package.
func mustBuildProvider(t *testing.T, settings string) inference.ProviderDefinition {
	t.Helper()
	provider, err := newProvider(t, settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

// typesafeModel is the model ref a deployment resolves for a declaration.
func typesafeModel(name string) model.ModelRef {
	return typesafeModelInProfile(name, "default")
}

// typesafeModelInProfile is the ref a deployment resolves when the declared
// profile is not the default one.
func typesafeModelInProfile(name, profile string) model.ModelRef {
	return model.ModelRef{
		ID:      model.ModelID{Provider: providerID, Name: name},
		Profile: profile,
	}
}

// decisionRequestFixture is the request the compile tests start from: the three
// question kinds, one described and one undescribed option, and the vendor's
// own support-message state. It contains nothing this wire refuses, so the
// shared compiler suite can drive it as a complete success.
func decisionRequestFixture() inference.DecisionRequest {
	return inference.DecisionRequest{
		Input: inference.DecisionInput{Text: evidenceFixture},
		Questions: []inference.DecisionQuestion{
			predicateQuestion(),
			choiceQuestion(),
			scoreQuestion(),
		},
	}
}

// The three questions, one constructor each, so a test that exercises a single
// kind can build a request around it without restating the others.

func predicateQuestion() inference.DecisionQuestion {
	return inference.DecisionQuestion{
		Name:         "is_urgent",
		Kind:         inference.DecisionQuestionPredicate,
		Instructions: "Does this convey urgency?",
	}
}

func choiceQuestion() inference.DecisionQuestion {
	return inference.DecisionQuestion{
		Name:         "department",
		Kind:         inference.DecisionQuestionChoice,
		Instructions: "Which team should handle this?",
		Choices: []inference.DecisionChoice{
			{
				Value:       inference.NewDecisionStringChoice("billing"),
				Description: "Payments, invoicing, refunds",
			},
			{Value: inference.NewDecisionStringChoice("technical")},
			{Value: inference.NewDecisionStringChoice("sales")},
		},
	}
}

func scoreQuestion() inference.DecisionQuestion {
	return inference.DecisionQuestion{
		Name:         "frustration",
		Kind:         inference.DecisionQuestionScore,
		Instructions: "How frustrated is the customer?",
		Levels: []inference.DecisionLevel{
			{Label: "Calm"},
			{Label: "Frustrated"},
			{Label: "Very angry"},
		},
	}
}

// wireResponseFixture is one response in the route's own shape. Its answer keys
// are deliberately in another order than the request's questions: the wire
// contracts no order, so a test that reads the canonical list is also the test
// that the driver restored the request's.
func wireResponseFixture() string {
	return `{
		"model": "jev-1.13.0",
		"answers": {
			"frustration": {
				"type": "score",
				"score": 1.05,
				"legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
				"probabilities": {"0": 0.0, "1": 0.95, "2": 0.05},
				"confidence": 0.92
			},
			"is_urgent": {"type": "noul", "noul": 0.99},
			"department": {
				"type": "choice",
				"choice": "billing",
				"probabilities": {"billing": 0.88, "technical": 0.12, "sales": 0.0},
				"confidence": 0.81
			}
		},
		"usage": {"input_tokens": 318, "output_tokens": 34}
	}`
}

// compileForTest compiles one request the way the provider does for the default
// model, so a test can drive the transport or the decoder without building a
// whole provider.
func compileForTest(
	t *testing.T,
	request inference.DecisionRequest,
) systemOneParams {
	t.Helper()
	compiled, err := compileDecision(DefaultModel)(
		context.Background(), typesafeModel(DefaultModel), request)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return compiled.Wire
}

// singleQuestionParams compiles a request whose only question is question, so a
// decode test can script exactly the answers it asserts.
func singleQuestionParams(
	t *testing.T,
	question inference.DecisionQuestion,
) systemOneParams {
	t.Helper()
	request := decisionRequestFixture()
	request.Questions = []inference.DecisionQuestion{question}
	return compileForTest(t, request)
}

// systemOneBody renders one compiled request the way it reaches the wire.
func systemOneBody(t *testing.T, params systemOneParams) map[string]any {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("params are not JSON: %v", err)
	}
	return body
}

// decodeWireBytes hands a scripted answer body to the decoder the way the
// transport does: the body as it arrived, beside the placement that carries the
// ids. A decode test can therefore script bytes instead of a server and still
// exercise the fold the decode depends on.
func decodeWireBytes(
	t *testing.T,
	body string,
	params systemOneParams,
) (inference.DecisionResponse, error) {
	t.Helper()
	return decodeDecision(context.Background(), decisionRaw{
		placement: params.placement,
		body:      []byte(body),
	})
}

// rejectionFor returns the rejection the report recorded for field.
func rejectionFor(
	report inference.CompileReport,
	field inference.FieldID,
) (inference.Decision, bool) {
	for _, decision := range report.Decisions {
		if decision.Field == field && decision.Disposition == inference.Rejected {
			return decision, true
		}
	}
	return inference.Decision{}, false
}

// foreignExtension is an extension addressed to this driver's provider id but
// meant for an operation it does not serve — the shape a deployment produces
// when one provider id carries settings for several operations. This driver
// ships no extension at all, so the compiler has to refuse it rather than
// ignore settings the caller believes are in effect.
type foreignExtension struct{}

func (foreignExtension) ProviderID() string {
	return providerID
}

func (foreignExtension) ExtensionID() string {
	return "custom_options"
}

func (foreignExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"mode"}
}

func (foreignExtension) Validate() error {
	return nil
}

func (foreignExtension) Clone() inference.Extension {
	return foreignExtension{}
}

// foreignExtensionField is the ledger field the extension's active field
// qualifies to, which is what the compiler reports.
var foreignExtensionField = inference.ExtensionField("mode").Qualify(foreignExtension{})
