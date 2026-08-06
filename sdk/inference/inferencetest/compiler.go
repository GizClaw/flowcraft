package inferencetest

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// CompilerRejection describes one canonical field a provider must reject
// explicitly rather than silently dropping.
type CompilerRejection[Request any] struct {
	Name    string
	Request func() Request
	Field   inference.FieldID
	Kind    inference.ErrorKind
}

// CompilerDrop describes one canonical field a provider intentionally
// discards while succeeding: the compile must succeed, the report must
// mark the field Dropped, and the decision must state the reason.
type CompilerDrop[Request any] struct {
	Name    string
	Request func() Request
	Field   inference.FieldID
}

// CompilerSuite verifies field-ledger completeness and provider-native wire
// compilation independently from transport.
type CompilerSuite[Request, Wire any] struct {
	Operation inference.Operation
	Model     inference.ModelRef
	Request   func() Request
	Snapshot  func(Request) any
	Fields    func(Request) []inference.FieldID
	Compile   inference.Compiler[Request, Wire]

	AssertWire func(*testing.T, Wire)
	Rejections []CompilerRejection[Request]
	Drops      []CompilerDrop[Request]
}

// GenerateCompilerSuite fixes the canonical request type while retaining the
// provider wire type selected by a conformance test.
type GenerateCompilerSuite[Wire any] struct {
	Model      inference.ModelRef
	Shape      inference.GenerateExecutionShape
	Request    func() inference.GenerateRequest
	Snapshot   func(inference.GenerateRequest) any
	Compile    inference.GenerateCompiler[Wire]
	AssertWire func(*testing.T, Wire)
	Rejections []CompilerRejection[inference.GenerateRequest]
	Drops      []CompilerDrop[inference.GenerateRequest]
}

// RunGenerateCompiler applies the common compiler completeness suite to the
// unified Generate operation.
func RunGenerateCompiler[Wire any](
	t *testing.T,
	suite GenerateCompilerSuite[Wire],
) {
	t.Helper()
	if err := suite.Shape.Validate(); err != nil {
		t.Fatalf("Shape: %v", err)
	}
	RunCompiler(t, CompilerSuite[inference.GenerateRequest, Wire]{
		Operation: inference.OperationGenerate,
		Model:     suite.Model,
		Request:   suite.Request,
		Snapshot:  suite.Snapshot,
		Fields: func(request inference.GenerateRequest) []inference.FieldID {
			return request.ActiveFieldsFor(suite.Shape)
		},
		Compile: func(
			ctx context.Context,
			model inference.ModelRef,
			request inference.GenerateRequest,
		) (inference.Compiled[Wire], error) {
			return suite.Compile(ctx, model, request, suite.Shape)
		},
		AssertWire: suite.AssertWire,
		Rejections: suite.Rejections,
		Drops:      suite.Drops,
	})
}

func RunCompiler[Request, Wire any](
	t *testing.T,
	suite CompilerSuite[Request, Wire],
) {
	t.Helper()
	if suite.Request == nil ||
		suite.Snapshot == nil ||
		suite.Fields == nil ||
		suite.Compile == nil {
		t.Fatal("CompilerSuite requires request, snapshot, active fields, and compiler")
	}
	if err := suite.Operation.Validate(); err != nil {
		t.Fatalf("Operation: %v", err)
	}
	if err := suite.Model.Validate(); err != nil {
		t.Fatalf("Model: %v", err)
	}

	t.Run("complete_success_ledger", func(t *testing.T) {
		request := suite.Request()
		expected := suite.Snapshot(request)
		active := append([]inference.FieldID(nil), suite.Fields(request)...)
		compiled, err := suite.Compile(context.Background(), suite.Model, request)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		if err := compiled.Report.ValidateSuccess(suite.Operation, active); err != nil {
			t.Fatalf("CompileReport: %v", err)
		}
		assertUnchanged(t, expected, suite.Snapshot(request))
		if suite.AssertWire != nil {
			suite.AssertWire(t, compiled.Wire)
		}
	})

	for _, rejection := range suite.Rejections {
		rejection := rejection
		t.Run("reject_"+rejection.Name, func(t *testing.T) {
			if rejection.Request == nil {
				t.Fatal("rejection request is required")
			}
			switch rejection.Kind {
			case inference.UnsupportedFeature,
				inference.InvalidExtension:
			default:
				t.Fatalf(
					"rejection kind %q is not accepted from provider compilers",
					rejection.Kind,
				)
			}
			request := rejection.Request()
			expected := suite.Snapshot(request)
			active := append([]inference.FieldID(nil), suite.Fields(request)...)
			compiled, err := suite.Compile(context.Background(), suite.Model, request)
			if err == nil {
				t.Fatal("Compile succeeded, want structured rejection")
			}
			if !inference.IsKind(err, rejection.Kind) {
				t.Fatalf("Compile error = %v, want %s", err, rejection.Kind)
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) ||
				inferenceErr.Operation != suite.Operation ||
				inferenceErr.Field != rejection.Field {
				t.Fatalf(
					"Compile error context = %+v, want operation=%s field=%s",
					inferenceErr,
					suite.Operation,
					rejection.Field,
				)
			}
			if reportErr := compiled.Report.ValidateFailure(
				suite.Operation,
				active,
			); reportErr != nil {
				t.Fatalf("rejection report: %v", reportErr)
			}
			if !compiled.Report.Rejects(rejection.Field) {
				t.Fatalf("report did not reject %q: %+v", rejection.Field, compiled.Report)
			}
			assertUnchanged(t, expected, suite.Snapshot(request))
		})
	}

	for _, drop := range suite.Drops {
		drop := drop
		t.Run("drop_"+drop.Name, func(t *testing.T) {
			if drop.Request == nil {
				t.Fatal("drop request is required")
			}
			request := drop.Request()
			expected := suite.Snapshot(request)
			active := append([]inference.FieldID(nil), suite.Fields(request)...)
			compiled, err := suite.Compile(context.Background(), suite.Model, request)
			if err != nil {
				t.Fatalf("Compile: %v, want success with a dropped field", err)
			}
			if err := compiled.Report.ValidateSuccess(
				suite.Operation,
				active,
			); err != nil {
				t.Fatalf("CompileReport: %v", err)
			}
			reason := ""
			for _, decision := range compiled.Report.Decisions {
				if decision.Field == drop.Field &&
					decision.Disposition == inference.Dropped {
					reason = decision.Reason
				}
			}
			if reason == "" {
				t.Fatalf(
					"report did not drop %q with a reason: %+v",
					drop.Field,
					compiled.Report,
				)
			}
			assertUnchanged(t, expected, suite.Snapshot(request))
		})
	}
}

func NativeReport(
	operation inference.Operation,
	fields ...inference.FieldID,
) inference.CompileReport {
	decisions := make([]inference.Decision, len(fields))
	for index, field := range fields {
		decisions[index] = inference.Decision{
			Field:       field,
			Disposition: inference.Native,
		}
	}
	return inference.CompileReport{Operation: operation, Decisions: decisions}
}
