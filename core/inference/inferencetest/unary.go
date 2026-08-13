package inferencetest

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// UnarySuite verifies contracts shared by Generate and Embed drivers.
type UnarySuite[Request, Response any] struct {
	Operation inference.Operation
	Model     inference.ModelRef

	Request func() Request
	// Snapshot returns an owned, comparable representation of Request.
	Snapshot func(Request) any
	Explain  func(
		context.Context,
		inference.ModelRef,
		Request,
	) (inference.Explanation, error)
	Execute  func(context.Context, inference.ModelRef, Request) (Response, error)
	Metadata func(Response) inference.Metadata

	TransportCalls func() int64
	AssertResponse func(*testing.T, Response)
}

type GenerateUnarySuite struct {
	Model   inference.ModelRef
	Request func() inference.GenerateRequest
	Driver  inference.GenerateDriver

	TransportCalls func() int64
	AssertResponse func(*testing.T, inference.GenerateResponse)
}

func RunGenerateUnary(t *testing.T, suite GenerateUnarySuite) {
	t.Helper()
	if suite.Driver == nil {
		t.Fatal("GenerateUnarySuite requires a driver")
	}
	assertResponse := suite.AssertResponse
	RunUnary(t, UnarySuite[inference.GenerateRequest, inference.GenerateResponse]{
		Operation: inference.OperationGenerate,
		Model:     suite.Model,
		Request:   suite.Request,
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Explain:        suite.Driver.Explain,
		Execute:        suite.Driver.Execute,
		Metadata:       func(response inference.GenerateResponse) inference.Metadata { return response.Metadata },
		TransportCalls: suite.TransportCalls,
		AssertResponse: func(t *testing.T, response inference.GenerateResponse) {
			if response.Usage.Model != suite.Model {
				t.Fatalf(
					"Usage.Model = %+v, want %+v",
					response.Usage.Model,
					suite.Model,
				)
			}
			if response.Usage.LatencyMs < 0 {
				t.Fatalf(
					"Usage.LatencyMs = %d, want non-negative",
					response.Usage.LatencyMs,
				)
			}
			if assertResponse != nil {
				assertResponse(t, response)
			}
		},
	})
}

func RunUnary[Request, Response any](
	t *testing.T,
	suite UnarySuite[Request, Response],
) {
	t.Helper()
	validateUnarySuite(t, suite)

	t.Run("explain_without_provider_io", func(t *testing.T) {
		request := suite.Request()
		expected := suite.Snapshot(request)
		before := suite.TransportCalls()
		explanation, err := suite.Explain(context.Background(), suite.Model, request)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if calls := suite.TransportCalls(); calls != before {
			t.Fatalf("Explain transport calls = %d, want %d", calls, before)
		}
		if explanation.Model != suite.Model ||
			explanation.Operation != suite.Operation ||
			len(explanation.Decisions) == 0 {
			t.Fatalf("Explanation = %+v", explanation)
		}
		assertUnchanged(t, expected, suite.Snapshot(request))
	})

	t.Run("execute_returns_compiler_metadata", func(t *testing.T) {
		request := suite.Request()
		expected := suite.Snapshot(request)
		before := suite.TransportCalls()
		response, err := suite.Execute(context.Background(), suite.Model, request)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if calls := suite.TransportCalls(); calls != before+1 {
			t.Fatalf("Execute transport calls = %d, want %d", calls, before+1)
		}
		metadata := suite.Metadata(response)
		if metadata.Model != suite.Model.ID ||
			metadata.Operation != suite.Operation ||
			len(metadata.Decisions) == 0 {
			t.Fatalf("Metadata = %+v", metadata)
		}
		assertUnchanged(t, expected, suite.Snapshot(request))
		if suite.AssertResponse != nil {
			suite.AssertResponse(t, response)
		}
	})
}

func validateUnarySuite[Request, Response any](
	t *testing.T,
	suite UnarySuite[Request, Response],
) {
	t.Helper()
	if err := suite.Operation.Validate(); err != nil {
		t.Fatalf("Operation: %v", err)
	}
	if err := suite.Model.Validate(); err != nil {
		t.Fatalf("Model: %v", err)
	}
	if suite.Request == nil ||
		suite.Snapshot == nil ||
		suite.Explain == nil ||
		suite.Execute == nil ||
		suite.Metadata == nil ||
		suite.TransportCalls == nil {
		t.Fatal("UnarySuite requires request, snapshot, driver, metadata, and transport probe")
	}
}

func assertUnchanged[T any](t *testing.T, expected, actual T) {
	t.Helper()
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("provider mutated caller-owned request:\nexpected: %#v\nactual:   %#v", expected, actual)
	}
}
