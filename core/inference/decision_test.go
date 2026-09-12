package inference

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestComponentNotesFoldOntoFieldDisposition locks the contract that lets a
// field report per-component detail without lying about the whole field: a
// dropped image inside a tool result must not leave the field reading
// native, and the notes must be retrievable in encounter order.
func TestComponentNotesFoldOntoFieldDisposition(t *testing.T) {
	report := CompileReport{
		Operation: OperationGenerate,
		Decisions: []Decision{
			{
				Field:       FieldGenerateContextToolResult,
				Disposition: Dropped,
				Reason:      "tool output omitted image (model does not accept image input)",
				Components: []ComponentNote{
					{Kind: message.PartText, Disposition: Native, Index: 0},
					{
						Kind:        message.PartImage,
						Disposition: Dropped,
						Index:       1,
						Reason:      "model does not accept image input",
					},
					{Kind: message.PartText, Disposition: Native, Index: 2},
				},
			},
		},
	}
	active := []FieldID{FieldGenerateContextToolResult}
	if err := report.ValidateSuccess(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateSuccess rejected a folded component report: %v", err)
	}
	notes := report.Components(FieldGenerateContextToolResult)
	if len(notes) != 3 ||
		notes[1].Kind != message.PartImage ||
		notes[1].Disposition != Dropped ||
		notes[1].Index != 1 {
		t.Fatalf("components = %+v", notes)
	}
	// The accessor hands out a copy: mutating it must not reach the report.
	notes[0].Kind = message.PartAudio
	if report.Decisions[0].Components[0].Kind != message.PartText {
		t.Fatal("Components returned a view into the report")
	}
}

// TestComponentNotesMustFoldOntoFieldDisposition rejects the two ways a
// report could contradict itself: a field reading native while a component
// was dropped, and a component without a reason.
func TestComponentNotesMustFoldOntoFieldDisposition(t *testing.T) {
	cases := []struct {
		name    string
		report  CompileReport
		wantErr string
	}{
		{
			name: "field claims native while a component was dropped",
			report: CompileReport{
				Operation: OperationGenerate,
				Decisions: []Decision{{
					Field:       FieldGenerateContextToolResult,
					Disposition: Native,
					Components: []ComponentNote{{
						Kind:        message.PartImage,
						Disposition: Dropped,
						Index:       0,
						Reason:      "model does not accept image input",
					}},
				}},
			},
			wantErr: "do not fold onto the field disposition",
		},
		{
			name: "dropped component without a reason",
			report: CompileReport{
				Operation: OperationGenerate,
				Decisions: []Decision{{
					Field:       FieldGenerateContextToolResult,
					Disposition: Dropped,
					Reason:      "tool output omitted image",
					Components: []ComponentNote{{
						Kind:        message.PartImage,
						Disposition: Dropped,
						Index:       0,
					}},
				}},
			},
			wantErr: "dropped component carries no reason",
		},
		{
			name: "duplicate component position",
			report: CompileReport{
				Operation: OperationGenerate,
				Decisions: []Decision{{
					Field:       FieldGenerateContextToolResult,
					Disposition: Dropped,
					Reason:      "tool output omitted image",
					Components: []ComponentNote{
						{Kind: message.PartText, Disposition: Native, Index: 0},
						{
							Kind:        message.PartImage,
							Disposition: Dropped,
							Index:       0,
							Reason:      "model does not accept image input",
						},
					},
				}},
			},
			wantErr: "duplicate component index",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.report.ValidateSuccess(
				OperationGenerate,
				[]FieldID{FieldGenerateContextToolResult},
			)
			if err == nil {
				t.Fatalf("ValidateSuccess accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

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
