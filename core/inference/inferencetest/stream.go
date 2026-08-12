package inferencetest

import (
	"context"
	"io"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

type GenerateCompileParitySuite struct {
	Model   inference.ModelRef
	Request func() inference.GenerateRequest
	Unary   inference.GenerateDriver
	Stream  inference.GenerateStreamDriver
}

// RunGenerateCompileParity proves both Generate execution shapes cover the
// same request fields while reporting their own execution field.
func RunGenerateCompileParity(t *testing.T, suite GenerateCompileParitySuite) {
	t.Helper()
	if suite.Request == nil || suite.Unary == nil || suite.Stream == nil {
		t.Fatal("GenerateCompileParitySuite requires request and both drivers")
	}
	request := suite.Request()
	unary, err := suite.Unary.Explain(context.Background(), suite.Model, request)
	if err != nil {
		t.Fatalf("unary Explain: %v", err)
	}
	stream, err := suite.Stream.Explain(context.Background(), suite.Model, request)
	if err != nil {
		t.Fatalf("stream Explain: %v", err)
	}
	unaryFields := decisionsWithoutShape(unary.Decisions)
	streamFields := decisionsWithoutShape(stream.Decisions)
	if !reflect.DeepEqual(unaryFields, streamFields) ||
		!hasDecision(unary.Decisions, inference.FieldGenerateExecutionUnary) ||
		!hasDecision(stream.Decisions, inference.FieldGenerateExecutionStream) {
		t.Fatalf(
			"unary and stream compiler decisions differ:\nunary:  %+v\nstream: %+v",
			unary.Decisions,
			stream.Decisions,
		)
	}
}

func decisionsWithoutShape(decisions []inference.Decision) []inference.Decision {
	filtered := make([]inference.Decision, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Field != inference.FieldGenerateExecutionUnary &&
			decision.Field != inference.FieldGenerateExecutionStream {
			filtered = append(filtered, decision)
		}
	}
	return filtered
}

func hasDecision(decisions []inference.Decision, field inference.FieldID) bool {
	for _, decision := range decisions {
		if decision.Field == field {
			return true
		}
	}
	return false
}

type GenerateStreamSuite struct {
	Model   inference.ModelRef
	Request func() inference.GenerateRequest
	Driver  inference.GenerateStreamDriver

	TransportCalls func() int64
	AssertEvent    func(*testing.T, inference.GenerateStreamEvent)
	AssertResult   func(*testing.T, inference.GenerateResponse)
	AssertClose    func(*testing.T, error)
}

func RunGenerateStream(t *testing.T, suite GenerateStreamSuite) {
	t.Helper()
	if suite.Request == nil || suite.Driver == nil || suite.TransportCalls == nil {
		t.Fatal("GenerateStreamSuite requires request, driver, and transport probe")
	}
	if err := suite.Model.Validate(); err != nil {
		t.Fatalf("Model: %v", err)
	}

	request := suite.Request()
	expected := request.Clone()
	before := suite.TransportCalls()
	explanation, err := suite.Driver.Explain(
		context.Background(),
		suite.Model,
		request,
	)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if suite.TransportCalls() != before ||
		explanation.Operation != inference.OperationGenerate ||
		len(explanation.Decisions) == 0 {
		t.Fatalf("Explain performed I/O or lost decisions: %+v", explanation)
	}
	assertUnchanged(t, expected, request.Clone())

	stream, err := suite.Driver.Stream(context.Background(), suite.Model, request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = stream.Close()
		}
	})
	if suite.TransportCalls() != before+1 {
		t.Fatalf("Stream transport calls = %d, want %d", suite.TransportCalls(), before+1)
	}
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if suite.AssertEvent != nil {
			suite.AssertEvent(t, event)
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if response.Metadata.Model != suite.Model.ID ||
		response.Metadata.Operation != inference.OperationGenerate ||
		len(response.Metadata.Decisions) == 0 {
		t.Fatalf("Result metadata = %+v", response.Metadata)
	}
	if suite.AssertResult != nil {
		suite.AssertResult(t, response)
	}
	closeErr := stream.Close()
	closed = true
	if suite.AssertClose != nil {
		suite.AssertClose(t, closeErr)
	} else if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	assertUnchanged(t, expected, request.Clone())
}
