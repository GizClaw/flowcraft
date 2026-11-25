package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

// recordingGenerator reports which vendor served a call.
type recordingGenerator struct {
	vendor string
	served *[]string
}

func (g recordingGenerator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	*g.served = append(*g.served, g.vendor)
	return inference.GenerateResponse{}, nil
}

func (g recordingGenerator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	*g.served = append(*g.served, g.vendor)
	return nil, errors.New("stream not used")
}

// requestRef is one model reference a request names.
func requestRef(providerName, name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: providerName, Name: name}}
}

// authorityHost builds a fleet of two vendors and a session opened under grant.
func authorityHost(t *testing.T, grant policy.Grant) *Session {
	t.Helper()
	var served []string
	planned := func(vendor, name string) deploy.PlannedProvider {
		return deploy.PlannedProvider{Name: vendor, Impl: "fake",
			New: func() (*inference.Provider, error) {
				return &inference.Provider{
					Generator: recordingGenerator{vendor: vendor, served: &served},
					Catalog:   catalog{models: []model.ModelDescriptor{modelRef(vendor, name)}},
				}, nil
			}}
	}
	log := &journal{}
	res, err := Open(t.Context(), deploy.ProcessPlan{
		Providers: []deploy.PlannedProvider{
			planned("openai", "gpt-6"),
			planned("anthropic", "opus-5"),
		},
		Backends: []deploy.PlannedBackend{boundBackend("local", &boundary{log: log, filesystemBounds: true})},
	}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	agents := NewAgents(res)
	dep, _ := sessionDeployment(t, "referee", policy.Grant{}, log, &boundary{log: log, filesystemBounds: true})
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	session, err := agents.Open(t.Context(), "s1", "referee", grant)
	if err != nil {
		t.Fatalf("Open session: %v", err)
	}
	return session
}

// TestAModelRefusalReachesTheCallerAsAPolicyDenial is the translation this layer owns:
// the router classifies, and the host restates the refusal so the operator — and the
// model, on its next turn — reads which model was refused and what the session may call.
func TestAModelRefusalReachesTheCallerAsAPolicyDenial(t *testing.T) {
	session := authorityHost(t, policy.Grant{Models: []policy.ModelGrant{
		{ModelRef: requestRef("anthropic", "opus-5")},
	}})

	_, err := session.models.Generate(t.Context(), inference.GenerateRequest{
		Model: requestRef("openai", "gpt-6"),
	})
	denial, ok := policy.AsDenial(err)
	if !ok {
		t.Fatalf("err = %v, want a *policy.Denial", err)
	}
	if denial.Check != policy.CheckModels {
		t.Fatalf("check = %q, want %q", denial.Check, policy.CheckModels)
	}
	if !strings.Contains(denial.Reason, "openai/gpt-6") || !strings.Contains(denial.Reason, "anthropic/opus-5") {
		t.Fatalf("reason = %q, want the model asked for and the one granted", denial.Reason)
	}
	if strings.HasPrefix(denial.Reason, "route: ") {
		t.Fatalf("reason = %q, want the package prefix stripped", denial.Reason)
	}
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("err = %v, want the errdefs classification the turn reads", err)
	}
	if denial.Cause == nil {
		t.Fatal("the router's classification was dropped")
	}

	// The session cannot enumerate a model it may not call.
	for _, descriptor := range session.models.Catalog.Models() {
		if descriptor.ID.Provider == "openai" {
			t.Fatalf("the session enumerated a model it may not call: %+v", descriptor)
		}
	}
}

// TestAGrantedModelStillReachesItsProvider is the accepting half: the translation is a
// wrapper, not a policy of its own.
func TestAGrantedModelStillReachesItsProvider(t *testing.T) {
	session := authorityHost(t, policy.Grant{Models: []policy.ModelGrant{
		{ModelRef: requestRef("openai", "gpt-6")},
	}})
	if _, err := session.models.Generate(t.Context(), inference.GenerateRequest{
		Model: requestRef("openai", "gpt-6"),
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

// TestANonPolicyErrorIsNotRestated keeps the translation narrow: a provider failure is
// not reported as a permission problem.
func TestANonPolicyErrorIsNotRestated(t *testing.T) {
	failure := inference.NewError(inference.ProviderFailure, inference.FieldModel, errors.New("upstream 500"))
	if got := asPolicyDenial(failure); got == nil || got.Error() != failure.Error() {
		t.Fatalf("a provider failure was restated: %v", got)
	}
	if _, refused := policy.AsDenial(asPolicyDenial(failure)); refused {
		t.Fatal("a provider failure became a policy denial")
	}
	if asPolicyDenial(nil) != nil {
		t.Fatal("a nil error became a denial")
	}
}
