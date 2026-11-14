package inferencetest

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// DeciderFixture is what a judgment driver hands the suite.
type DeciderFixture struct {
	ProviderID string
	NewDecider func(t *testing.T, baseURL string) (inference.Decider, error)
	Request    func(t *testing.T) inference.DecisionRequest
	Samples    []DecisionSample
}

// DecisionSample is one canned answer and what it must decode to.
type DecisionSample struct {
	Name     string
	Status   int
	Body     string
	WantKind inference.ErrorKind
	Check    func(t *testing.T, request inference.DecisionRequest, response inference.DecisionResponse)
}

// RunDecider runs the judgment conformance suite against a real HTTP endpoint.
func RunDecider(t *testing.T, fixture DeciderFixture) {
	t.Helper()
	if fixture.NewDecider == nil || fixture.Request == nil {
		t.Fatalf("inferencetest: decider fixture needs NewDecider and Request")
	}
	if len(fixture.Samples) == 0 {
		t.Fatalf("inferencetest: decider fixture needs at least one sample")
	}

	for _, sample := range fixture.Samples {
		t.Run(sample.Name, func(t *testing.T) {
			server := serve(t, sample.Status, sample.Body)

			decider, err := fixture.NewDecider(t, server.URL)
			if err != nil {
				t.Fatalf("NewDecider: %v", err)
			}
			request := fixture.Request(t)
			response, err := decider.Decide(context.Background(), request)

			if sample.WantKind != "" {
				if err == nil {
					t.Fatalf("Decide = nil error, want kind %q", sample.WantKind)
				}
				if !inference.IsKind(err, sample.WantKind) {
					t.Fatalf("Decide kind = %v, want %q", err, sample.WantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if sample.Check != nil {
				sample.Check(t, request, response)
			}
		})
	}
}
