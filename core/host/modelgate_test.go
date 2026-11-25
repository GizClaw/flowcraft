package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

func modelID(provider, name string) model.ModelID {
	return model.ModelID{Provider: provider, Name: name}
}

// gated is a provider whose catalog declares two models, one of which the session
// may call.
func gated(t *testing.T) (*inference.Provider, policy.Grant) {
	t.Helper()
	base := &inference.Provider{
		Generator: generator{},
		Catalog: catalog{models: []model.ModelDescriptor{
			{ID: modelID("openai", "gpt-4o")},
			{ID: modelID("openai", "gpt-4o-mini")},
		}},
	}
	grant := policy.Grant{Models: []model.ModelRef{{ID: modelID("openai", "gpt-4o-mini")}}}
	return base, grant
}

// TestGateModelsRefusesWhatTheGrantDoesNotName is the whole point of the gate: a
// model outside the session's set is a policy refusal, not a provider error.
func TestGateModelsRefusesWhatTheGrantDoesNotName(t *testing.T) {
	base, grant := gated(t)
	got := gateModels(base, grant)

	// The allowed model reaches the process provider, whose stub reports itself.
	if _, err := got.Generate(context.Background(), inference.GenerateRequest{
		Model: model.ModelRef{ID: modelID("openai", "gpt-4o-mini")},
	}); err == nil || !strings.Contains(err.Error(), "not used") {
		t.Fatalf("an allowed model did not reach the provider: %v", err)
	}

	for name, call := range map[string]func() error{
		"generate": func() error {
			_, err := got.Generate(context.Background(), inference.GenerateRequest{
				Model: model.ModelRef{ID: modelID("openai", "gpt-4o")},
			})
			return err
		},
		"stream": func() error {
			_, err := got.GenerateStream(context.Background(), inference.GenerateRequest{
				Model: model.ModelRef{ID: modelID("openai", "gpt-4o")},
			})
			return err
		},
	} {
		denial, ok := policy.AsDenial(call())
		if !ok {
			t.Fatalf("%s: an ungranted model was not a policy denial", name)
		}
		if denial.Check != policy.CheckModels {
			t.Fatalf("%s: Check = %q, want %q", name, denial.Check, policy.CheckModels)
		}
		if !strings.Contains(denial.Reason, "gpt-4o-mini") {
			t.Fatalf("%s: the reason does not name what the session may call: %q", name, denial.Reason)
		}
	}
}

// TestGateModelsHidesUngrantedModelsFromTheCatalog keeps the offered list honest:
// a session cannot enumerate what its next call would refuse.
func TestGateModelsHidesUngrantedModelsFromTheCatalog(t *testing.T) {
	base, grant := gated(t)
	got := gateModels(base, grant)

	visible := got.Catalog.Models()
	if len(visible) != 1 || visible[0].ID.Name != "gpt-4o-mini" {
		t.Fatalf("visible models = %+v, want only the granted one", visible)
	}
	if _, found := got.Catalog.Describe(modelID("openai", "gpt-4o")); found {
		t.Fatal("Describe reported an ungranted model")
	}
	if len(base.Catalog.Models()) != 2 {
		t.Fatal("the process provider was narrowed in place")
	}
}

// TestModelAccessWithoutASetSharesTheProvider pins the empty-set default: no
// wrapper, because there is nothing to enforce.
func TestModelAccessWithoutASetSharesTheProvider(t *testing.T) {
	base, _ := gated(t)
	agents := &Agents{res: &Resource{models: base}}
	if got := agents.modelAccess(policy.Grant{}); got != base {
		t.Fatal("an unrestricted session did not get the process provider")
	}
}

// declarer is a program that names its models, the way the graph program does.
type declarer struct {
	models []model.ModelRef
}

func (d declarer) DeclaredModels() []model.ModelRef           { return d.models }
func (declarer) Open(agent.SessionInfo) (agent.Driver, error) { return nil, nil }
func (declarer) Close() error                                 { return nil }

// TestAnOpenRefusesAProgramThatCallsWhatTheSessionMayNot is the early half of the model check: a
// program whose default or node model is outside the session's authority fails at open, where the
// error reads as a permission, instead of on every turn, where it reads as a broken model.
func TestAnOpenRefusesAProgramThatCallsWhatTheSessionMayNot(t *testing.T) {
	program := declarer{models: []model.ModelRef{
		{ID: modelID("openai", "gpt-4o")},
		{ID: modelID("openai", "gpt-4o-mini")},
	}}
	grant := policy.Grant{Models: []model.ModelRef{{ID: modelID("openai", "gpt-4o-mini")}}}

	err := checkDeclaredModels("referee", program, grant)
	denial, ok := policy.AsDenial(err)
	if !ok {
		t.Fatalf("err = %v, want a policy denial", err)
	}
	if denial.Check != policy.CheckModels {
		t.Fatalf("Check = %q, want %q", denial.Check, policy.CheckModels)
	}
	if !strings.Contains(denial.Reason, "gpt-4o") || !strings.Contains(denial.Reason, "gpt-4o-mini") {
		t.Fatalf("the reason does not name both sides: %q", denial.Reason)
	}
}

// TestAnOpenWithoutAModelSetAcceptsAnyProgram keeps the empty-set rule: an unrestricted session
// checks nothing, because there is nothing to check against.
func TestAnOpenWithoutAModelSetAcceptsAnyProgram(t *testing.T) {
	program := declarer{models: []model.ModelRef{{ID: modelID("openai", "gpt-4o")}}}
	if err := checkDeclaredModels("referee", program, policy.Grant{}); err != nil {
		t.Fatalf("an unrestricted session refused a program: %v", err)
	}
}

// TestAProgramThatCannotDeclareIsNotRefusedAtOpen: a remote driver has no static list, so the
// call-time gate is the only check — and the open must not invent one.
func TestAProgramThatCannotDeclareIsNotRefusedAtOpen(t *testing.T) {
	if err := checkDeclaredModels("referee", generatorProgram{}, policy.Grant{
		Models: []model.ModelRef{{ID: modelID("openai", "gpt-4o-mini")}},
	}); err != nil {
		t.Fatalf("a program with no declaration was refused: %v", err)
	}
}

// generatorProgram is a program that says nothing about its models.
type generatorProgram struct{}

func (generatorProgram) Open(agent.SessionInfo) (agent.Driver, error) { return nil, nil }
func (generatorProgram) Close() error                                 { return nil }

// embedder is a provider seam that reports itself when reached.
type embedder struct{}

func (embedder) Embed(context.Context, inference.EmbedRequest) (inference.EmbedResponse, error) {
	return inference.EmbedResponse{}, errReached
}

// decider is the same for judgments.
type decider struct{}

func (decider) Decide(context.Context, inference.DecisionRequest) (inference.DecisionResponse, error) {
	return inference.DecisionResponse{}, errReached
}

// errReached means the call passed the gate and arrived at the process provider.
var errReached = errors.New("reached")

// TestEveryModelSeamIsGated is the completeness rule: a session that may not call a model cannot
// embed with it or ask it to judge either, so the gate covers all three seams rather than the one a
// turn happens to use.
func TestEveryModelSeamIsGated(t *testing.T) {
	base := &inference.Provider{
		Generator: generator{},
		Embedder:  embedder{},
		Decider:   decider{},
	}
	grant := policy.Grant{Models: []model.ModelRef{{ID: modelID("openai", "gpt-4o-mini")}}}
	gated := gateModels(base, grant)

	ungranted := model.ModelRef{ID: modelID("openai", "gpt-4o")}
	granted := model.ModelRef{ID: modelID("openai", "gpt-4o-mini")}

	if _, err := gated.Embedder.Embed(context.Background(), inference.EmbedRequest{Model: ungranted}); !isDenial(err) {
		t.Fatalf("embed with an ungranted model = %v, want a denial", err)
	}
	if _, err := gated.Embedder.Embed(context.Background(), inference.EmbedRequest{Model: granted}); !errors.Is(err, errReached) {
		t.Fatalf("embed with a granted model = %v, want the provider", err)
	}
	if _, err := gated.Decider.Decide(context.Background(), inference.DecisionRequest{Model: ungranted}); !isDenial(err) {
		t.Fatalf("decide with an ungranted model = %v, want a denial", err)
	}
	if _, err := gated.Decider.Decide(context.Background(), inference.DecisionRequest{Model: granted}); !errors.Is(err, errReached) {
		t.Fatalf("decide with a granted model = %v, want the provider", err)
	}
}

// isDenial reports whether err is a model refusal.
func isDenial(err error) bool {
	denial, ok := policy.AsDenial(err)
	return ok && denial.Check == policy.CheckModels
}
