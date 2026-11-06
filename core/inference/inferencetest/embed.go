package inferencetest

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// EmbedderFixture is what an embedding driver hands the suite.
type EmbedderFixture struct {
	// ProviderID is the driver's identity.
	ProviderID string
	// NewEmbedder builds an embedder pointed at baseURL.
	NewEmbedder func(t *testing.T, baseURL string) (inference.Embedder, error)
	// Request builds the request used for every sample.
	Request func(t *testing.T) inference.EmbedRequest
	// Samples are the canned provider answers.
	Samples []EmbedSample
}

// EmbedSample is one canned answer and what it must decode to.
type EmbedSample struct {
	Name   string
	Status int
	Body   string
	// WantKind, when set, asserts the call fails with that classified kind.
	WantKind inference.ErrorKind
	// Check asserts on a successful answer; the suite has already run the
	// contract's own ValidateFor, so this covers what only the seam knows.
	Check func(t *testing.T, request inference.EmbedRequest, response inference.EmbedResponse)
}

// RunEmbedder runs the embedding conformance suite against a real HTTP
// endpoint, so the assertions cover the driver's own decoding and status
// mapping rather than a mock of them.
func RunEmbedder(t *testing.T, fixture EmbedderFixture) {
	t.Helper()
	if fixture.NewEmbedder == nil || fixture.Request == nil {
		t.Fatalf("inferencetest: embedder fixture needs NewEmbedder and Request")
	}
	if len(fixture.Samples) == 0 {
		t.Fatalf("inferencetest: embedder fixture needs at least one sample")
	}

	for _, sample := range fixture.Samples {
		t.Run(sample.Name, func(t *testing.T) {
			server := serve(t, sample.Status, sample.Body)

			embedder, err := fixture.NewEmbedder(t, server.URL)
			if err != nil {
				t.Fatalf("NewEmbedder: %v", err)
			}
			request := fixture.Request(t)
			response, err := embedder.Embed(context.Background(), request)

			if sample.WantKind != "" {
				if err == nil {
					t.Fatalf("Embed = nil error, want kind %q", sample.WantKind)
				}
				if !inference.IsKind(err, sample.WantKind) {
					t.Fatalf("Embed kind = %v, want %q", err, sample.WantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if sample.Check != nil {
				sample.Check(t, request, response)
			}
		})
	}
}
