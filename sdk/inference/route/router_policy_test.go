package route

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

func policyTargetRef(name string) inference.ModelRef {
	return inference.ModelRef{
		ID: inference.ModelID{Provider: "fake", Name: name},
	}
}

func TestPolicySelectorsFollowDeclaredOrder(t *testing.T) {
	policy := Policy{
		Generate: []Pool{
			{Tier: "premium", Targets: []Target{
				{Model: policyTargetRef("a")},
				{Model: policyTargetRef("b")},
			}},
			{Tier: "cheap", Targets: []Target{
				{Model: policyTargetRef("c")},
			}},
		},
	}
	selectors := policy.Selectors()

	decision, err := selectors.Generate.SelectGenerate(
		context.Background(),
		generateRequest("hello"),
	)
	if err != nil {
		t.Fatalf("SelectGenerate: %v", err)
	}
	if decision.Selected != policyTargetRef("a") || decision.Tier != "premium" {
		t.Fatalf("decision = %+v", decision)
	}

	attempt := func(target inference.ModelRef) Attempt {
		return Attempt{
			Target:    target,
			Phase:     AttemptPhaseExecute,
			Trigger:   AttemptTriggerSelection,
			Outcome:   AttemptOutcomeFailed,
			ErrorKind: inference.UnsupportedFeature,
		}
	}
	next, ok, err := selectors.GenerateFallback.NextGenerate(
		context.Background(),
		generateRequest("hello"),
		attempt(policyTargetRef("a")),
	)
	if err != nil || !ok || next != policyTargetRef("b") {
		t.Fatalf("first fallback = %+v/%v/%v, want b", next, ok, err)
	}
	next, ok, err = selectors.GenerateFallback.NextGenerate(
		context.Background(),
		generateRequest("hello"),
		attempt(policyTargetRef("b")),
	)
	if err != nil || !ok || next != policyTargetRef("c") {
		t.Fatalf("second fallback = %+v/%v/%v, want c (cross-pool)", next, ok, err)
	}
	next, ok, err = selectors.GenerateFallback.NextGenerate(
		context.Background(),
		generateRequest("hello"),
		attempt(policyTargetRef("c")),
	)
	if err != nil || ok || next != (inference.ModelRef{}) {
		t.Fatalf("exhausted fallback = %+v/%v/%v, want stop", next, ok, err)
	}
}

func TestPolicySelectorsStopOnForeignAttemptTarget(t *testing.T) {
	policy := Policy{
		Embed: []Pool{{Tier: "balanced", Targets: []Target{
			{Model: policyTargetRef("a")},
		}}},
	}
	next, ok, err := policy.Selectors().EmbedFallback.NextEmbed(
		context.Background(),
		inference.EmbedRequest{},
		Attempt{Target: policyTargetRef("foreign"), Outcome: AttemptOutcomeFailed},
	)
	if err != nil || ok || next != (inference.ModelRef{}) {
		t.Fatalf("foreign attempt fallback = %+v/%v/%v, want stop", next, ok, err)
	}
}

func TestPolicySelectorsFailNoRouteForUnconfiguredOperation(t *testing.T) {
	policy := Policy{
		Embed: []Pool{{Tier: "balanced", Targets: []Target{
			{Model: policyTargetRef("a")},
		}}},
	}
	_, err := policy.Selectors().Generate.SelectGenerate(
		context.Background(),
		generateRequest("hello"),
	)
	if !IsKind(err, NoRoute) {
		t.Fatalf("SelectGenerate error = %v, want NoRoute", err)
	}
}

func TestPolicySelectorsOwnFlattenedTargets(t *testing.T) {
	policy := Policy{
		Generate: []Pool{{Tier: "premium", Targets: []Target{
			{Model: policyTargetRef("a")},
		}}},
	}
	selectors := policy.Selectors()
	policy.Generate[0].Targets[0].Model = policyTargetRef("mutated")
	policy.Generate[0].Tier = "mutated"
	decision, err := selectors.Generate.SelectGenerate(
		context.Background(),
		generateRequest("hello"),
	)
	if err != nil {
		t.Fatalf("SelectGenerate: %v", err)
	}
	if decision.Selected != policyTargetRef("a") || decision.Tier != "premium" {
		t.Fatalf("selectors observe policy mutation: %+v", decision)
	}
}

func TestPolicySelectorsServeTranscriptionShapes(t *testing.T) {
	policy := Policy{
		Transcription: []Pool{{Tier: "balanced", Targets: []Target{
			{Model: policyTargetRef("whisper")},
		}}},
	}
	selectors := policy.Selectors()
	unary, err := selectors.Transcription.SelectTranscription(
		context.Background(),
		inference.TranscriptionRequest{},
	)
	if err != nil {
		t.Fatalf("SelectTranscription: %v", err)
	}
	session, err := selectors.TranscriptionSession.SelectTranscriptionSession(
		context.Background(),
		inference.TranscriptionSessionConfig{},
	)
	if err != nil {
		t.Fatalf("SelectTranscriptionSession: %v", err)
	}
	if unary.Selected != policyTargetRef("whisper") ||
		session.Selected != policyTargetRef("whisper") {
		t.Fatalf("unary/session decisions = %+v/%+v", unary, session)
	}
}

func TestPolicySelectorsRouteEndToEnd(t *testing.T) {
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"first":  {reject: inference.UnsupportedFeature},
		"second": {},
	})
	policy := Policy{
		Generate: []Pool{{Tier: "balanced", Targets: []Target{
			{Model: policyTargetRef("first")},
			{Model: policyTargetRef("second")},
		}}},
	}
	router, err := New(runtime, policy.Selectors())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Metadata.Model != policyTargetRef("second").ID {
		t.Fatalf("response model = %+v", response.Metadata.Model)
	}
	if trace.Executed != policyTargetRef("second") ||
		len(trace.Fallbacks) != 1 ||
		len(trace.Attempts) != 3 {
		t.Fatalf("trace = %+v", trace)
	}
}
