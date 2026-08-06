package inference

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestErrorFormattingRedactsCause(t *testing.T) {
	cause := errors.New("prompt=secret")
	err := NewError(
		InvalidRequest,
		OperationGenerate,
		FieldGenerateInputText,
		cause,
	)
	if got := err.Error(); got == "" || strings.Contains(got, "secret") {
		t.Fatalf("Error() leaked cause: %q", got)
	}
	if !errdefs.IsValidation(err) {
		t.Fatal("InvalidRequest must interoperate with errdefs validation")
	}
	if !errors.Is(err, cause) {
		t.Fatal("error chain must retain its diagnostic cause")
	}
}

func TestProviderFailuresReceiveOneErrdefsClassification(t *testing.T) {
	tests := []struct {
		name         string
		transportErr error
		check        func(error) bool
		notCheck     func(error) bool
	}{
		{
			name:         "generic provider failure",
			transportErr: errors.New("connection failed"),
			check:        errdefs.IsNotAvailable,
			notCheck:     errdefs.IsInternal,
		},
		{
			name:         "context cancellation",
			transportErr: context.Canceled,
			check:        errdefs.IsAborted,
			notCheck:     errdefs.IsNotAvailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type wire struct{}
			driver, err := BindGenerate(
				nativeGenerateCompile(wire{}),
				func(context.Context, wire) (struct{}, error) {
					return struct{}{}, tt.transportErr
				},
				func(context.Context, struct{}) (GenerateResponse, error) {
					return GenerateResponse{}, nil
				},
			)
			if err != nil {
				t.Fatalf("BindGenerate: %v", err)
			}
			_, err = driver.Execute(
				context.Background(),
				ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
				validGenerateTextRequest(),
			)
			if !IsKind(err, ProviderFailure) || !errdefs.HasClassification(err) {
				t.Fatalf("Execute error = %v, want classified ProviderFailure", err)
			}
			if !tt.check(err) || tt.notCheck(err) {
				t.Fatalf("Execute error has wrong errdefs classification: %v", err)
			}
		})
	}
}

func TestNewProviderFailureDefaultsToNotAvailable(t *testing.T) {
	err := NewError(
		ProviderFailure,
		OperationGenerate,
		"",
		errors.New("unclassified provider error"),
	)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("NewError = %v, want errdefs.NotAvailable", err)
	}
}

// TestEveryErrorKindClassifies pins the Kind ↔ errdefs marker table in
// doc.go. Every ErrorKind declared in errors.go must appear in the table
// exactly once, and a plain probe cause must classify to the expected marker.
func TestEveryErrorKindClassifies(t *testing.T) {
	tests := map[ErrorKind]func(error) bool{
		InvalidRequest:            errdefs.IsValidation,
		UnsupportedOperation:      errdefs.IsNotAvailable,
		UnsupportedFeature:        errdefs.IsValidation,
		InvalidExtension:          errdefs.IsValidation,
		UnknownProvider:           errdefs.IsNotFound,
		UnknownModel:              errdefs.IsNotFound,
		UnknownProfile:            errdefs.IsNotFound,
		PolicyDenied:              errdefs.IsPolicyDenied,
		OperationInterrupted:      errdefs.IsAborted,
		CompilerContractViolation: errdefs.IsInternal,
		ProviderFailure:           errdefs.IsNotAvailable,
		InvalidProviderResponse:   errdefs.IsInternal,
	}

	probe := errors.New("probe")
	for kind, check := range tests {
		classified := kind.classify(probe)
		if !errdefs.HasClassification(classified) || !check(classified) {
			t.Errorf("%s.classify(probe) = %T, want classified with expected marker", kind, classified)
		}
	}

	declared := errorKindsDeclaredInSource(t)
	if len(declared) != len(tests) {
		t.Errorf("errors.go declares %d ErrorKinds, classification table covers %d", len(declared), len(tests))
	}
	for _, kind := range declared {
		if _, ok := tests[kind]; !ok {
			t.Errorf("ErrorKind %q is missing from the classification table", kind)
		}
	}
}

// errorKindsDeclaredInSource extracts the ErrorKind consts from errors.go so
// the classification table cannot silently drift when a kind is added.
func errorKindsDeclaredInSource(t *testing.T) []ErrorKind {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "errors.go", nil, 0)
	if err != nil {
		t.Fatalf("parse errors.go: %v", err)
	}
	var kinds []ErrorKind
	ast.Inspect(file, func(node ast.Node) bool {
		gen, ok := node.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			return true
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || len(valueSpec.Values) != 1 {
				continue
			}
			if ident, ok := valueSpec.Type.(*ast.Ident); !ok || ident.Name != "ErrorKind" {
				continue
			}
			lit, ok := valueSpec.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", valueSpec.Names[0].Name, err)
			}
			kinds = append(kinds, ErrorKind(value))
		}
		return true
	})
	return kinds
}
