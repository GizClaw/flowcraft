package inference

import "testing"

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
