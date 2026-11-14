package inferencetest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Sample is one canned provider answer.
type Sample struct {
	// Name labels the subtest.
	Name string
	// Status is the HTTP status the fake endpoint returns. Zero means 200.
	Status int
	// Body is the response body.
	Body string
	// Check asserts on the decoded response of a success sample.
	Check func(t *testing.T, response inference.GenerateResponse)
	// WantKind, when set, expects the call to fail with that error kind instead.
	WantKind inference.ErrorKind
}

// ProviderFixture describes how to build a provider against a local endpoint.
type ProviderFixture struct {
	// ProviderID is the driver's identity.
	ProviderID string
	// NewProvider builds a provider pointed at baseURL. It must not perform any
	// I/O during construction.
	NewGenerator func(t *testing.T, baseURL string) (inference.Provider, error)
	// Request builds the request used for decoding assertions.
	Request func(t *testing.T) inference.GenerateRequest
	// Samples are canned answers, grouped by the output they serve.
	Samples map[inference.Output][]Sample
}

// RunProvider runs the decode and error-classification suites.
//
// Every sample is served by a local HTTP endpoint that the driver really talks to,
// so the assertions cover the driver's own SDK usage, its status mapping and its
// decoder — not a mock of them.
func RunGenerator(t *testing.T, fixture ProviderFixture) {
	t.Helper()
	if fixture.NewGenerator == nil || fixture.Request == nil {
		t.Fatalf("inferencetest: provider fixture needs NewGenerator and Request")
	}
	if len(fixture.Samples) == 0 {
		t.Fatalf("inferencetest: provider fixture needs at least one sample")
	}

	t.Run("decode", func(t *testing.T) {
		for output, samples := range fixture.Samples {
			for _, sample := range samples {
				t.Run(string(output)+"/"+sample.Name, func(t *testing.T) {
					provider, request := fixture.serve(t, sample)
					response, err := provider.Generate(context.Background(), request)
					if sample.WantKind != "" {
						if err == nil {
							t.Fatalf("call succeeded, want %v", sample.WantKind)
						}
						if !inference.IsKind(err, sample.WantKind) {
							t.Fatalf("kind = %v, want %v", err, sample.WantKind)
						}
						return
					}
					if err != nil {
						t.Fatalf("generate: %v", err)
					}
					if err := response.Validate(); err != nil {
						t.Fatalf("response invalid: %v", err)
					}
					if response.Metadata.Model.Provider != fixture.ProviderID {
						t.Errorf("metadata provider = %q, want %q",
							response.Metadata.Model.Provider, fixture.ProviderID)
					}
					if sample.Check != nil {
						sample.Check(t, response)
					}
				})
			}
		}
	})

	t.Run("cancellation is an interruption", func(t *testing.T) {
		blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			w.WriteHeader(http.StatusGatewayTimeout)
		}))
		t.Cleanup(blocking.Close)

		provider, err := fixture.NewGenerator(t, blocking.URL)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err = provider.Generate(ctx, fixture.Request(t))
		if err == nil {
			t.Fatal("cancelled call succeeded")
		}
		if !inference.IsKind(err, inference.OperationInterrupted) {
			t.Fatalf("kind = %v, want OperationInterrupted", err)
		}
	})

	t.Run("an unparsable body is never a silent success", func(t *testing.T) {
		provider, request := fixture.serve(t, Sample{Body: "not json at all"})
		_, err := provider.Generate(context.Background(), request)
		if err == nil {
			t.Fatal("garbage body accepted")
		}
		// Either classification is defensible: a driver that decodes in its own
		// Decoder reports the response as unusable, while one whose SDK parses
		// inside the transport can only report the call as failed — the SDK wraps
		// the parse error without preserving a type to distinguish it. What must
		// never happen is a zero-value response reported as success.
		if !inference.IsKind(err, inference.InvalidProviderResponse) &&
			!inference.IsKind(err, inference.ProviderFailure) {
			t.Fatalf("kind = %v, want InvalidProviderResponse or ProviderFailure", err)
		}
	})
}

// serve starts an endpoint that always answers with the sample, and returns a
// provider pointed at it together with the request to use.
func (f ProviderFixture) serve(
	t *testing.T,
	sample Sample,
) (inference.Generator, inference.GenerateRequest) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		status := sample.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(sample.Body))
	}))
	t.Cleanup(server.Close)

	provider, err := f.NewGenerator(t, server.URL)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return provider, f.Request(t)
}
