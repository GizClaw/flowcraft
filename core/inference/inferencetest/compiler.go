// Package inferencetest is the conformance suite every driver runs.
//
// It pins the rules that are true of a driver regardless of which provider it
// speaks: that every active field is accounted for, that unsupported options are
// refused instead of dropped, that answers are decoded into a valid response, and
// that a stream assembles into the same answer a unary call would return.
//
// It deliberately does not know any provider's wire format. A driver supplies
// samples — canned native payloads and per-field requests — and the suite applies
// the rules to them. The suite therefore never has to be updated when a provider
// adds a parameter; the driver's own fixture carries that knowledge.
package inferencetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

// FieldCase opens exactly one canonical field on an otherwise minimal request and
// names a marker that must appear in the rendered wire. The marker is how the
// suite proves the field reached the wire rather than merely being reported.
type FieldCase struct {
	// Field is the canonical field this case exercises.
	Field inference.Field
	// Request is minimal plus that one field.
	Request inference.GenerateRequest
	// Marker is a distinctive substring expected in the rendered wire.
	Marker string
}

// Rejection is a request the driver must refuse, with the kind and field the
// caller needs in order to act on it.
type Rejection struct {
	Name    string
	Request inference.GenerateRequest
	// Kind is the expected error kind.
	Kind inference.ErrorKind
	// Field is the field the error must name, or empty when the refusal is not
	// about one field.
	Field inference.Field
}

// CompilerFixture describes one driver's compiler to the suite.
type CompilerFixture[Wire any] struct {
	// ProviderID is the identity the driver answers to; extensions addressed to
	// other providers are dropped before compilation.
	ProviderID string
	// Compile is the driver's compiler, exactly as bound into its pipeline.
	Compile func(
		ctx context.Context,
		request inference.GenerateRequest,
		shape protocol.Shape,
	) (protocol.Compiled[Wire], error)
	// Baseline is the smallest request this driver compiles.
	Baseline inference.GenerateRequest
	// Supported lists every canonical field the driver claims to accept. The suite
	// requires one FieldCase per entry, so a field cannot be claimed and untested.
	Supported []inference.Field
	// Cases exercises each supported field in isolation.
	Cases []FieldCase
	// Render turns a compiled wire into JSON text for marker assertions.
	Render func(Wire) (string, error)
	// Rejections are the refusals this driver promises.
	Rejections []Rejection
}

// RunCompiler runs the accounting and rejection suites.
//
// The accounting rules are the same ones the pipeline enforces at run time; the
// suite checks them against the driver's compiler directly so a regression names
// the field that broke instead of surfacing as a runtime contract violation.
func RunCompiler[Wire any](t *testing.T, fixture CompilerFixture[Wire]) {
	t.Helper()
	if fixture.Compile == nil || fixture.Render == nil {
		t.Fatalf("inferencetest: compiler fixture needs Compile and Render")
	}

	t.Run("baseline compiles and accounts for its fields", func(t *testing.T) {
		compiled, err := fixture.Compile(context.Background(), fixture.Baseline, protocol.ShapeUnary)
		if err != nil {
			t.Fatalf("baseline: %v", err)
		}
		checkAccounting(t, fixture.Baseline, compiled.Fields, compiled.Warnings)
	})

	t.Run("every supported field has a case", func(t *testing.T) {
		covered := make(map[inference.Field]int, len(fixture.Cases))
		for _, testCase := range fixture.Cases {
			covered[testCase.Field]++
		}
		for _, field := range fixture.Supported {
			switch covered[field] {
			case 1:
			case 0:
				t.Errorf("field %q is claimed but has no case", field)
			default:
				t.Errorf("field %q has %d cases, want 1", field, covered[field])
			}
		}
	})

	t.Run("each field alone reaches the wire", func(t *testing.T) {
		for _, testCase := range fixture.Cases {
			t.Run(string(testCase.Field), func(t *testing.T) {
				compiled, err := fixture.Compile(
					context.Background(), testCase.Request, protocol.ShapeUnary,
				)
				if err != nil {
					t.Fatalf("compile: %v", err)
				}
				checkAccounting(t, testCase.Request, compiled.Fields, compiled.Warnings)
				if !honored(testCase.Field, compiled.Fields, compiled.Warnings) {
					t.Fatalf("field %q is active but neither honored nor warned", testCase.Field)
				}
				rendered, err := fixture.Render(compiled.Wire)
				if err != nil {
					t.Fatalf("render: %v", err)
				}
				if !strings.Contains(rendered, testCase.Marker) {
					t.Fatalf("wire does not contain %q: %s", testCase.Marker, rendered)
				}
			})
		}
	})

	t.Run("inactive fields are absent from the wire", func(t *testing.T) {
		compiled, err := fixture.Compile(context.Background(), fixture.Baseline, protocol.ShapeUnary)
		if err != nil {
			t.Fatalf("baseline: %v", err)
		}
		rendered, err := fixture.Render(compiled.Wire)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		active := activeSet(fixture.Baseline)
		for _, testCase := range fixture.Cases {
			if _, ok := active[testCase.Field]; ok {
				continue
			}
			if testCase.Marker == "" {
				continue
			}
			if strings.Contains(rendered, testCase.Marker) {
				t.Errorf("baseline wire carries the marker for inactive field %q: %q",
					testCase.Field, testCase.Marker)
			}
		}
	})

	t.Run("rejections name what they refuse", func(t *testing.T) {
		for _, rejection := range fixture.Rejections {
			t.Run(rejection.Name, func(t *testing.T) {
				_, err := fixture.Compile(
					context.Background(), rejection.Request, protocol.ShapeUnary,
				)
				if err == nil {
					t.Fatalf("request accepted, want %v", rejection.Kind)
				}
				if !inference.IsKind(err, rejection.Kind) {
					t.Fatalf("kind = %v, want %v", err, rejection.Kind)
				}
				inferenceErr := &inference.Error{}
				ok := errors.As(err, &inferenceErr)
				if !ok {
					t.Fatalf("error is %T, want *inference.Error", err)
				}
				if rejection.Field != "" && inferenceErr.Field != rejection.Field {
					t.Fatalf("field = %q, want %q", inferenceErr.Field, rejection.Field)
				}
			})
		}
	})
}

// checkAccounting applies the contract rules to one compilation.
func checkAccounting(
	t *testing.T,
	request inference.GenerateRequest,
	fields []inference.Field,
	warnings []inference.Warning,
) {
	t.Helper()
	active := activeSet(request)
	for _, field := range fields {
		if !field.Valid() {
			t.Errorf("honored field %q is not a valid field identity", field)
		}
		if _, ok := active[field]; !ok {
			t.Errorf("honored field %q is not active on the request", field)
		}
	}
	for field := range active {
		if !honored(field, fields, warnings) {
			t.Errorf("active field %q is neither honored nor warned", field)
		}
	}
	for _, warning := range warnings {
		if warning.Field != "" && !warning.Field.Valid() {
			t.Errorf("warning names invalid field %q", warning.Field)
		}
	}
}

func activeSet(request inference.GenerateRequest) map[inference.Field]struct{} {
	active := request.ActiveFields()
	set := make(map[inference.Field]struct{}, len(active))
	for _, field := range active {
		set[field] = struct{}{}
	}
	return set
}

// honored reports whether the field was either compiled or explained.
func honored(
	field inference.Field,
	fields []inference.Field,
	warnings []inference.Warning,
) bool {
	for _, candidate := range fields {
		if candidate == field {
			return true
		}
	}
	for _, warning := range warnings {
		if warning.Field == field {
			return true
		}
	}
	return false
}
