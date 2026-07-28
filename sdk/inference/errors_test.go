package inference

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestErrorFormattingRedactsCause(t *testing.T) {
	err := NewError(
		InvalidRequest,
		OperationGenerate,
		FieldGenerateInputText,
		errors.New("prompt=secret"),
	)
	if got := err.Error(); got == "" || strings.Contains(got, "secret") {
		t.Fatalf("Error() leaked cause: %q", got)
	}
	if !errdefs.IsValidation(err) {
		t.Fatal("InvalidRequest must interoperate with errdefs validation")
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
