package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// arbiterSettings renders a deployment document with the maintenance arbiter
// turned on. The rest is the smallest document that builds: no derivation, no
// summaries, no runner. The fake is taken by pointer because it carries the
// transport's lock, so a copy would stand for a transport it does not answer for.
func arbiterSettings(fake *inferencetest.DecisionFake) string {
	ref := inferencetest.DefaultFakeDecisionModel
	if fake != nil && fake.Model.ID.Provider != "" {
		ref = fake.Model
	}
	if ref.ID.Provider == "" {
		ref = inferencetest.DefaultFakeDecisionModel
	}
	return fmt.Sprintf(`{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "maintain": {"arbiter": {"provider": %q, "name": %q, "profile": %q, "threshold": 0.8,
	    "batch": 64, "concurrency": 2, "budget": 4, "remember": 300000}},
	  "interval": "0"
	}`, ref.ID.Provider, ref.ID.Name, ref.Profile)
}

func newArbiterAssembly(t *testing.T, settings string, engine *inference.Assembly) (*Assembly, error) {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	deps := map[string]any{"workspace": ws}
	if engine != nil {
		deps["inference"] = engine
	}
	value, err := NewFactory(WithClock(func() time.Time { return testNow })).New(
		context.Background(), resource.NewInput([]byte(settings), deps, nil, nil))
	if err != nil {
		return nil, err
	}
	if err := value.Wire(context.Background()); err != nil {
		return nil, err
	}
	return value, nil
}

// TestArbiterSettingsReachTheMaintenancePass wires the arbiter through the
// factory: the settings name a model and a threshold, the pass names them in
// its stamp, and the store that serves the overlay names the same pass -- a
// service and a store that disagreed about the arbiter would judge a scope and
// then refuse to read what it judged.
func TestArbiterSettingsReachTheMaintenancePass(t *testing.T) {
	fake := inferencetest.DecisionFake{}
	assembly, err := newArbiterAssembly(t, arbiterSettings(&fake), fake.Assembly(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assembly.Close() })

	stamp := assembly.maintain.Config.ProducerVersion()
	want := fmt.Sprintf("arbiter=%s/%s@supersede-v1,p=0.8",
		inferencetest.DefaultFakeDecisionModel.ID.Provider,
		inferencetest.DefaultFakeDecisionModel.ID.Name)
	if !strings.Contains(stamp, want) {
		t.Fatalf("pass stamp = %q, want it to carry %q", stamp, want)
	}
	if assembly.maintain.Config.ArbiterBudget != 4 {
		t.Fatalf("arbiter budget = %d", assembly.maintain.Config.ArbiterBudget)
	}
	// The ceiling reaches the pass, and not the stamp: it bounds what is
	// remembered rather than what is judged.
	if assembly.maintain.Config.ArbiterRemember != 300000 {
		t.Fatalf("arbiter remember = %d", assembly.maintain.Config.ArbiterRemember)
	}
	if strings.Contains(stamp, "remember") {
		t.Fatalf("the ceiling entered the stamp: %q", stamp)
	}
	// The schedule reaches the call site, and not the stamp: concurrency moves
	// the wall clock, so two deployments that differ only there serve the same
	// overlay.
	arbiter, ok := assembly.maintain.Config.Arbiter.(*maintain.DecisionArbiter)
	if !ok {
		t.Fatalf("arbiter = %T", assembly.maintain.Config.Arbiter)
	}
	if arbiter.Batch != 64 || arbiter.Concurrency != 2 {
		t.Fatalf("arbiter batch = %d, concurrency = %d", arbiter.Batch, arbiter.Concurrency)
	}
	if strings.Contains(stamp, "concurrency") || strings.Contains(stamp, "batch") {
		t.Fatalf("the schedule entered the stamp: %q", stamp)
	}
	status, err := assembly.OverlayStatus(context.Background(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	if status.ExpectedProducer != stamp {
		t.Fatalf("store serves %q, pass stamps %q", status.ExpectedProducer, stamp)
	}
	// A pass over an empty scope proposes nothing, so the arbiter is never
	// called: the model is asked about facts, not about the deployment.
	plan, err := assembly.Maintain(context.Background(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	if plan.ArbiterAsked != 0 || fake.TransportCalls() != 0 {
		t.Fatalf("plan = %+v, calls = %d", plan, fake.TransportCalls())
	}
	if plan.ProducerVersion != stamp {
		t.Fatalf("plan stamp = %q, want %q", plan.ProducerVersion, stamp)
	}
}

// TestMaintenanceWithoutAnArbiterKeepsItsStamp pins the upgrade path: a
// deployment that does not configure an arbiter stamps exactly what it stamped
// before the arbiter existed, so turning the feature on re-judges the scope and
// leaving it off keeps the overlay the scope already has.
func TestMaintenanceWithoutAnArbiterKeepsItsStamp(t *testing.T) {
	assembly, _ := newTestAssemblyWith(t, "")
	t.Cleanup(func() { _ = assembly.Close() })
	if stamp := assembly.maintain.Config.ProducerVersion(); strings.Contains(stamp, "arbiter=") {
		t.Fatalf("a deployment without an arbiter stamps one: %q", stamp)
	}
}

// TestArbiterSettingsAreRefusedWhenTheyCannotMeanAnything pins the validation
// boundary: a half-configured arbiter, a probability that is not one, and a
// bound that cannot hold anything are refused where the document is read.
func TestArbiterSettingsAreRefusedWhenTheyCannotMeanAnything(t *testing.T) {
	fake := inferencetest.DecisionFake{}
	engine := fake.Assembly(t)
	cases := map[string]string{
		"no provider": `{"provider": "", "name": "gpt-6-luna"}`,
		"no name":     `{"provider": "openai", "name": ""}`,
		"threshold":   `{"provider": "openai", "name": "gpt-6-luna", "threshold": 1.5}`,
		"batch":       `{"provider": "openai", "name": "gpt-6-luna", "batch": -1}`,
		"concurrency": `{"provider": "openai", "name": "gpt-6-luna", "concurrency": -1}`,
		"budget":      `{"provider": "openai", "name": "gpt-6-luna", "budget": -1}`,
		"remember":    `{"provider": "openai", "name": "gpt-6-luna", "remember": -1}`,
		"knobs alone": `{"threshold": 0.8}`,
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			settings := fmt.Sprintf(`{
			  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
			  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
			  "fact": {"strategy": "none"},
			  "summary": {"disabled": true},
			  "maintain": {"arbiter": %s},
			  "interval": "0"
			}`, block)
			if _, err := newArbiterAssembly(t, settings, engine); err == nil {
				t.Fatalf("settings %s were accepted", block)
			} else if !strings.Contains(err.Error(), "arbiter") {
				t.Fatalf("the refusal does not name the setting: %v", err)
			}
		})
	}
}

// TestArbiterRequiresTheInferenceDocument pins the dependency: an arbiter is a
// model call, so a deployment that configures one without an inference
// assembly is refused at build time rather than at the first pass.
func TestArbiterRequiresTheInferenceDocument(t *testing.T) {
	fake := inferencetest.DecisionFake{}
	_, err := newArbiterAssembly(t, arbiterSettings(&fake), nil)
	if err == nil {
		t.Fatal("an arbiter without an inference document was accepted")
	}
	if !strings.Contains(err.Error(), "inference") {
		t.Fatalf("the refusal does not name the dependency: %v", err)
	}
}
