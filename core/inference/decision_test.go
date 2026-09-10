package inference

import (
	"errors"
	"testing"
)

func TestValidateFailureAllowsDroppedAlongsideRejection(t *testing.T) {
	active := []FieldID{
		FieldGenerateInputImage,
		FieldGenerateIntentReasoningEffort,
	}
	report := CompileReport{
		Operation: OperationGenerate,
		Decisions: []Decision{
			{
				Field:       FieldGenerateIntentReasoningEffort,
				Disposition: Dropped,
				Reason:      `model maps reasoning effort "medium" to "high"`,
			},
			{
				Field:       FieldGenerateInputImage,
				Disposition: Rejected,
				Reason:      "model does not accept image input",
			},
		},
	}
	if err := report.ValidateFailure(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateFailure rejected a valid drop+reject report: %v", err)
	}
}

func TestValidateFailureRejectsUnreasonedDrop(t *testing.T) {
	report := CompileReport{
		Operation: OperationGenerate,
		Decisions: []Decision{
			{
				Field:       FieldGenerateIntentReasoningEffort,
				Disposition: Dropped,
			},
			{
				Field:       FieldGenerateInputImage,
				Disposition: Rejected,
				Reason:      "model does not accept image input",
			},
		},
	}
	err := report.ValidateFailure(
		OperationGenerate,
		[]FieldID{FieldGenerateInputImage, FieldGenerateIntentReasoningEffort},
	)
	if err == nil {
		t.Fatal("ValidateFailure accepted a dropped field without a reason")
	}
}

// Every contract violation names the check that failed: the field path
// alone cannot tell a driver bug from a caller bug, and production triage
// reads Error() rather than the cause chain.
func TestContractViolationNamesTheFailedCheck(t *testing.T) {
	cases := []struct {
		name   string
		report CompileReport
		active []FieldID
		want   string
	}{
		{
			name: "active field has no disposition",
			report: CompileReport{
				Operation: OperationGenerate,
				Decisions: []Decision{
					{Field: FieldGenerateContextText, Disposition: Native},
				},
			},
			active: []FieldID{
				FieldGenerateContextText,
				FieldGenerateContextReasoning,
			},
			want: "compiler_contract_violation during generate at " +
				"generate.context.*.content.parts.reasoning: active field has no disposition",
		},
		{
			name: "decision covers an inactive field",
			report: CompileReport{
				Operation: OperationGenerate,
				Decisions: []Decision{
					{Field: FieldGenerateContextText, Disposition: Native},
					{Field: FieldGenerateContextReasoning, Disposition: Dropped, Reason: "stale"},
				},
			},
			active: []FieldID{FieldGenerateContextText},
			want: "compiler_contract_violation during generate at " +
				"generate.context.*.content.parts.reasoning: decision covers an inactive field",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.report.ValidateSuccess(OperationGenerate, testCase.active)
			if err == nil {
				t.Fatal("ValidateSuccess accepted an invalid report")
			}
			if got := err.Error(); got != testCase.want {
				t.Fatalf("Error() = %q, want %q", got, testCase.want)
			}
			var inferenceErr *Error
			if !errors.As(err, &inferenceErr) {
				t.Fatalf("error = %v, want an inference error", err)
			}
			if inferenceErr.Detail != testCase.name {
				t.Fatalf("Detail = %q, want %q", inferenceErr.Detail, testCase.name)
			}
			// The cause keeps the chain for errors.Is/errors.As consumers.
			if cause := errors.Unwrap(err); cause == nil ||
				cause.Error() != testCase.name {
				t.Fatalf("cause = %v, want %q", cause, testCase.name)
			}
		})
	}
}

// The failure-side validator carries its reason the same way.
func TestContractViolationOnFailedCompileNamesTheFailedCheck(t *testing.T) {
	report := CompileReport{
		Operation: OperationGenerate,
		Decisions: []Decision{
			{Field: FieldGenerateInputText, Disposition: Dropped},
		},
	}
	err := report.ValidateFailure(OperationGenerate, []FieldID{FieldGenerateInputText})
	if err == nil {
		t.Fatal("ValidateFailure accepted a dropped field without a reason")
	}
	want := "compiler_contract_violation during generate at generate.input.content.parts.text: " +
		"dropped field carries no reason"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
