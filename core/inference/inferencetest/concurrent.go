package inferencetest

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// GenerateConcurrentSuite exercises the concurrency contract of a
// provider's bound Generate pipelines: compilers, transports, and
// decoders must be safe for concurrent calls.
type GenerateConcurrentSuite struct {
	Model   inference.ModelRef
	Request func() inference.GenerateRequest
	Unary   inference.GenerateDriver
	Stream  inference.GenerateStreamDriver

	// Goroutines is the number of concurrent workers per operation.
	// Non-positive uses the default (4).
	Goroutines int
}

// RunGenerateConcurrent runs Explain/Execute and Explain/Stream
// concurrently against whichever drivers are configured.
func RunGenerateConcurrent(t *testing.T, suite GenerateConcurrentSuite) {
	t.Helper()
	if suite.Request == nil {
		t.Fatal("GenerateConcurrentSuite requires a request constructor")
	}
	if suite.Unary == nil && suite.Stream == nil {
		t.Fatal("GenerateConcurrentSuite requires at least one generate driver")
	}
	if err := suite.Model.Validate(); err != nil {
		t.Fatalf("Model: %v", err)
	}

	workers := suite.Goroutines
	if workers <= 0 {
		workers = 4
	}
	var wg sync.WaitGroup
	errorsCh := make(chan error, workers*2)

	if suite.Unary != nil {
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				request := suite.Request()
				if _, err := suite.Unary.Explain(
					context.Background(),
					suite.Model,
					request,
				); err != nil {
					errorsCh <- err
					return
				}
				if _, err := suite.Unary.Execute(
					context.Background(),
					suite.Model,
					request,
				); err != nil {
					errorsCh <- err
					return
				}
			}()
		}
	}

	if suite.Stream != nil {
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				request := suite.Request()
				if _, err := suite.Stream.Explain(
					context.Background(),
					suite.Model,
					request,
				); err != nil {
					errorsCh <- err
					return
				}
				stream, err := suite.Stream.Stream(
					context.Background(),
					suite.Model,
					request,
				)
				if err != nil {
					errorsCh <- err
					return
				}
				defer func() { _ = stream.Close() }()
				for {
					_, err := stream.Next(context.Background())
					if err == io.EOF {
						break
					}
					if err != nil {
						errorsCh <- err
						return
					}
				}
				if _, err := stream.Result(); err != nil {
					errorsCh <- err
					return
				}
			}()
		}
	}

	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent generate: %+v", err)
	}
}
