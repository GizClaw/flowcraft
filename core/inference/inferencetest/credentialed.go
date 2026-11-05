package inferencetest

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
)

// CredentialedFixture describes the real-provider lane. It is skipped unless the
// environment variable is set, so `go test ./...` stays hermetic.
type CredentialedFixture struct {
	// EnvKey is the variable that enables the lane, for example OPENAI_API_KEY.
	EnvKey string
	// NewProvider builds a provider from the environment's credentials.
	NewProvider func(t *testing.T) (inference.Provider, error)
	// NewProviderWithKey builds a provider with an explicit key, used to prove a
	// rejected credential is classified instead of retried.
	NewProviderWithKey func(t *testing.T, key string) (inference.Provider, error)
	// Request builds a minimal request for one output modality.
	Request func(t *testing.T, output inference.Output) inference.GenerateRequest
	// Outputs lists the modalities to smoke.
	Outputs []inference.Output
	// Validate adds driver-specific assertions beyond the generic ones.
	Validate func(t *testing.T, output inference.Output, response inference.GenerateResponse)
	// Concurrency is how many parallel calls the race check makes. Zero uses 4.
	Concurrency int
	// NoStreaming skips the streaming check for a surface that cannot stream.
	// Refusing streaming is a legitimate answer; the suite just needs to know
	// which surfaces it applies to.
	NoStreaming bool
}

// RunCredentialed runs the real-provider lane: one call per output, one stream,
// one rejected credential and a concurrent burst.
func RunCredentialed(t *testing.T, fixture CredentialedFixture) {
	t.Helper()
	key := os.Getenv(fixture.EnvKey)
	if key == "" {
		t.Skipf("%s not set", fixture.EnvKey)
	}
	if fixture.NewProvider == nil || fixture.Request == nil {
		t.Fatalf("inferencetest: credentialed fixture needs NewProvider and Request")
	}

	t.Run("each output produces a usable answer", func(t *testing.T) {
		for _, output := range fixture.Outputs {
			t.Run(string(output), func(t *testing.T) {
				provider, err := fixture.NewProvider(t)
				if err != nil {
					t.Fatalf("new provider: %v", err)
				}
				response, err := provider.Generate(context.Background(), fixture.Request(t, output))
				if err != nil {
					t.Fatalf("generate: %v", err)
				}
				if len(response.Message.Content.Parts) == 0 {
					t.Fatal("response carries no content")
				}
				if err := response.Usage.Validate(); err != nil {
					t.Fatalf("usage is inconsistent: %v", err)
				}
				if fixture.Validate != nil {
					fixture.Validate(t, output, response)
				}
			})
		}
	})

	if !fixture.NoStreaming {
		t.Run("streaming produces the same shape as unary", func(t *testing.T) {
			provider, err := fixture.NewProvider(t)
			if err != nil {
				t.Fatalf("new provider: %v", err)
			}
			stream, err := provider.GenerateStream(context.Background(), fixture.Request(t, inference.OutputText))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer stream.Close()
			drain(t, stream)
			response, err := stream.Result()
			if err != nil {
				t.Fatalf("result: %v", err)
			}
			if len(response.Message.Content.Parts) == 0 {
				t.Fatal("streamed response carries no content")
			}
			if err := response.Usage.Validate(); err != nil {
				t.Fatalf("streamed usage is inconsistent: %v", err)
			}
		})
	}

	if fixture.NewProviderWithKey != nil {
		t.Run("a rejected credential is classified, not retried", func(t *testing.T) {
			provider, err := fixture.NewProviderWithKey(t, "sk-definitely-invalid")
			if err != nil {
				t.Fatalf("new provider: %v", err)
			}
			_, err = provider.Generate(context.Background(), fixture.Request(t, inference.OutputText))
			if err == nil {
				t.Fatal("call with a bad key succeeded")
			}
			if !inference.IsKind(err, inference.ProviderFailure) {
				t.Fatalf("kind = %v, want ProviderFailure", err)
			}
			if cause := errors.Unwrap(err); cause != nil && !errdefs.IsUnauthorized(cause) {
				t.Logf("cause classified as %v (provider-dependent)", cause)
			}
		})
	}

	t.Run("concurrent calls share no state", func(t *testing.T) {
		provider, err := fixture.NewProvider(t)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		workers := fixture.Concurrency
		if workers <= 0 {
			workers = 4
		}
		var wait sync.WaitGroup
		// Results travel over a channel: each worker owns its own outcome, so
		// there is no shared write to reason about.
		results := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := provider.Generate(
					context.Background(), fixture.Request(t, inference.OutputText),
				)
				results <- err
			}()
		}
		wait.Wait()
		close(results)
		index := 0
		for err := range results {
			if err != nil {
				t.Errorf("worker %d: %v", index, err)
			}
			index++
		}
	})
}
