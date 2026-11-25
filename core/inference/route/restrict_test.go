package route

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
)

// serving is a target that declares one model with the given modes and answers
// generations with vendor's name.
func serving(t *testing.T, vendor, name string, calls *[]string, modes ...model.Mode) Target {
	t.Helper()
	return servingNamed(t, vendor, vendor, name, calls, modes...)
}

// servingNamed is serving with a target label that differs from the vendor, for a fleet
// with two endpoints of one provider.
func servingNamed(t *testing.T, target, vendor, name string, calls *[]string, modes ...model.Mode) Target {
	t.Helper()
	gen := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		*calls = append(*calls, vendor)
		return inference.GenerateResponse{
			Message:      message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")},
			FinishReason: inference.FinishStop,
		}, nil
	}}
	return Target{
		Name: target,
		Provider: inference.Provider{
			Generator: gen,
			Catalog: testCatalog{models: []model.ModelDescriptor{{
				ID:           model.ModelID{Provider: vendor, Name: name},
				Capabilities: model.ModelCapabilities{Modes: modes},
			}}},
		},
	}
}

func refOf(vendor, name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: vendor, Name: name}}
}

// TestRestrictRefusesAModelOutsideTheList is the authority half of the router: a
// session's grant is the whole set, so a name it does not list is refused before any
// target is chosen.
func TestRestrictRefusesAModelOutsideTheList(t *testing.T) {
	var calls []string
	router, err := New([]Target{
		serving(t, "openai", "gpt-6", &calls, model.ModeText),
		serving(t, "anthropic", "opus-5", &calls, model.ModeText),
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restricted := router.Restrict([]Allow{{Model: refOf("openai", "gpt-6")}})

	request := generationRequest()
	request.Model = refOf("openai", "gpt-6")
	if _, err := restricted.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate for a granted model: %v", err)
	}
	if len(calls) != 1 || calls[0] != "openai" {
		t.Fatalf("calls = %v, want the granted vendor", calls)
	}

	before := len(calls)
	request.Model = refOf("anthropic", "opus-5")
	_, err = restricted.Generate(context.Background(), request)
	var classified *inference.Error
	if !errors.As(err, &classified) || classified.Kind != inference.PolicyDenied {
		t.Fatalf("err = %v, want PolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "model.not_granted") {
		t.Fatalf("error %q does not carry the stable detail", err)
	}
	if reason := errors.Unwrap(err); reason == nil ||
		!strings.Contains(reason.Error(), "opus-5") || !strings.Contains(reason.Error(), "openai/gpt-6") {
		t.Fatalf("cause %v does not name what was asked and what is granted", errors.Unwrap(err))
	}
	if len(calls) != before {
		t.Fatalf("an ungranted model reached a target: %v", calls)
	}
}

// TestRestrictRefusesAModeTheEntryDoesNotPermit is what the grant's modes are for: a
// model listed for text may not be used for an image.
func TestRestrictRefusesAModeTheEntryDoesNotPermit(t *testing.T) {
	var calls []string
	router, err := New([]Target{serving(t, "openai", "gpt-6", &calls, model.ModeText, model.ModeImage)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restricted := router.Restrict([]Allow{{Model: refOf("openai", "gpt-6"), Modes: []model.Mode{model.ModeText}}})

	request := generationRequest()
	request.Model = refOf("openai", "gpt-6")
	request.Output = inference.OutputImage
	_, err = restricted.Generate(context.Background(), request)
	var classified *inference.Error
	if !errors.As(err, &classified) || classified.Kind != inference.PolicyDenied {
		t.Fatalf("err = %v, want PolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "model.mode_not_permitted") {
		t.Fatalf("error %q does not carry the stable detail", err)
	}
	if reason := errors.Unwrap(err); reason == nil ||
		!strings.Contains(reason.Error(), "may not be used for image") || !strings.Contains(reason.Error(), "text") {
		t.Fatalf("cause %v does not name the mode and what is permitted", errors.Unwrap(err))
	}
	if len(calls) != 0 {
		t.Fatalf("a mode the grant excludes reached a target: %v", calls)
	}
}

// TestRestrictChoosesFromTheListInOrder is the point of the ordered list: a request that
// names no model gets the first entry that can serve the mode, and a later entry when the
// first cannot.
func TestRestrictChoosesFromTheListInOrder(t *testing.T) {
	var calls []string
	router, err := New([]Target{
		// Declares text only: unusable for an image request.
		serving(t, "openai", "gpt-6", &calls, model.ModeText),
		serving(t, "dashscope", "wan-2", &calls, model.ModeImage),
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restricted := router.Restrict([]Allow{
		{Model: refOf("openai", "gpt-6")},
		{Model: refOf("dashscope", "wan-2")},
	})

	request := generationRequest()
	request.Model = model.ModelRef{}
	request.Output = inference.OutputImage
	if _, err := restricted.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(calls) != 1 || calls[0] != "dashscope" {
		t.Fatalf("calls = %v, want the entry that declares image", calls)
	}

	// A text request takes the first entry, whatever its position.
	request.Output = ""
	if _, err := restricted.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(calls) != 2 || calls[1] != "openai" {
		t.Fatalf("calls = %v, want the first entry for text", calls)
	}
}

// TestRestrictReportsWhenNothingCanServe is the empty answer: no granted model declares the
// mode, so the call is refused with the list rather than sent to a vendor to fail.
func TestRestrictReportsWhenNothingCanServe(t *testing.T) {
	var calls []string
	router, err := New([]Target{serving(t, "openai", "gpt-6", &calls, model.ModeText)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restricted := router.Restrict([]Allow{{Model: refOf("openai", "gpt-6")}})

	request := generationRequest()
	request.Model = model.ModelRef{}
	request.Output = inference.OutputVideo
	_, err = restricted.Generate(context.Background(), request)
	var classified *inference.Error
	if !errors.As(err, &classified) || classified.Kind != inference.PolicyDenied {
		t.Fatalf("err = %v, want PolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "model.no_candidate") {
		t.Fatalf("error %q does not carry the stable detail", err)
	}
	if reason := errors.Unwrap(err); reason == nil || !strings.Contains(reason.Error(), "serves video") {
		t.Fatalf("cause %v does not name the mode", errors.Unwrap(err))
	}
	if len(calls) != 0 {
		t.Fatalf("nothing should have run: %v", calls)
	}
}

// TestRestrictPinsTheProviderOfAnExtension keeps a per-call option honest: the pipeline
// drops options addressed to another provider, so an automatic choice must land on the
// provider the option names.
func TestRestrictPinsTheProviderOfAnExtension(t *testing.T) {
	var calls []string
	router, err := New([]Target{
		serving(t, "openai", "gpt-6", &calls, model.ModeText),
		serving(t, "anthropic", "opus-5", &calls, model.ModeText),
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restricted := router.Restrict([]Allow{
		{Model: refOf("openai", "gpt-6")},
		{Model: refOf("anthropic", "opus-5")},
	})

	request := generationRequest()
	request.Model = model.ModelRef{}
	request.Extensions = inference.Extensions{optionFor{provider: "anthropic"}}
	if _, err := restricted.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(calls) != 1 || calls[0] != "anthropic" {
		t.Fatalf("calls = %v, want the provider the option is addressed to", calls)
	}
}

// optionFor is an extension that names one provider and carries nothing.
type optionFor struct{ provider string }

func (o optionFor) ProviderID() string                     { return o.provider }
func (optionFor) ExtensionID() string                      { return "option" }
func (optionFor) ActiveFields() []inference.ExtensionField { return nil }
func (optionFor) Validate() error                          { return nil }
func (o optionFor) Clone() inference.Extension             { return o }

// TestAnUnrestrictedRouterKeepsEverything is the compatibility half: a router built
// without a list behaves as it always has.
func TestAnUnrestrictedRouterKeepsEverything(t *testing.T) {
	var calls []string
	router, err := New([]Target{serving(t, "openai", "gpt-6", &calls, model.ModeText)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, r := range map[string]*Router{"plain": router, "empty list": router.Restrict(nil)} {
		request := generationRequest()
		request.Model = refOf("openai", "gpt-6")
		if _, err := r.Generate(context.Background(), request); err != nil {
			t.Fatalf("%s: Generate: %v", name, err)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want two", calls)
	}
	if (*Router)(nil).Restrict(nil) != nil {
		t.Fatal("restricting a nil router built one")
	}
}

// TestToolsRequireTheToolCallMode is what the mode replaced the boolean for: a request
// that offers tools needs the channel, so a model that declares text only is not a
// candidate — and a grant that narrows the model to text refuses the request before any
// target is picked.
func TestToolsRequireTheToolCallMode(t *testing.T) {
	var calls []string
	textOnly := serving(t, "openai", "gpt-6", &calls, model.ModeText)
	withTools := servingNamed(t, "openai-turbo", "openai", "gpt-6-turbo", &calls, model.ModeText, model.ModeToolCall)
	router, err := New([]Target{textOnly, withTools}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Unrestricted: the model that declares the channel is chosen for a tool request.
	request := generationRequest()
	request.Model = refOf("openai", "gpt-6-turbo")
	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "read_file",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	request.Tools = tools
	if _, err := router.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(calls) != 1 || calls[0] != "openai" {
		t.Fatalf("calls = %v", calls)
	}

	// The same model, narrowed to text by the grant, may not be asked to call tools.
	restricted := router.Restrict([]Allow{{
		Model: refOf("openai", "gpt-6-turbo"),
		Modes: []model.Mode{model.ModeText},
	}})
	before := len(calls)
	_, err = restricted.Generate(context.Background(), request)
	var classified *inference.Error
	if !errors.As(err, &classified) || classified.Kind != inference.PolicyDenied {
		t.Fatalf("err = %v, want PolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "model.mode_not_permitted") {
		t.Fatalf("error %q does not carry the stable detail", err)
	}
	if len(calls) != before {
		t.Fatalf("a tool request reached a model narrowed to text: %v", calls)
	}
}

// TestThePublishedCatalogObeysTheRestriction keeps enumeration honest: a session must not
// be offered a model its next call would refuse.
func TestThePublishedCatalogObeysTheRestriction(t *testing.T) {
	var calls []string
	router, err := New([]Target{
		serving(t, "openai", "gpt-6", &calls, model.ModeText),
		serving(t, "anthropic", "opus-5", &calls, model.ModeText),
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	restricted := router.Restrict([]Allow{{Model: refOf("openai", "gpt-6")}})

	provider := restricted.Provider()
	if provider.Catalog == nil {
		t.Fatal("the restricted router published no catalog")
	}
	models := provider.Catalog.Models()
	if len(models) != 1 || models[0].ID.Name != "gpt-6" {
		t.Fatalf("Models() = %+v, want only the granted model", models)
	}
	if _, found := provider.Catalog.Describe(model.ModelID{Provider: "anthropic", Name: "opus-5"}); found {
		t.Fatal("Describe reported an ungranted model")
	}
	if got := len(router.Provider().Catalog.Models()); got != 2 {
		t.Fatalf("the unrestricted router published %d models, want both", got)
	}
}
