package minimax

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// partField resolves a part kind to its ledger field through core's table, so
// the driver cannot drift from the field list the runtime activates. A miss
// cannot happen — core's TestGenerateLedgerCoversPartKinds pins the table
// against message.PartKinds — and panics rather than returning an empty field,
// because a decision recorded against "" never reaches the report.
func partField(
	lookup func(message.PartKind) (inference.FieldID, bool),
) func(message.PartKind) inference.FieldID {
	return func(kind message.PartKind) inference.FieldID {
		field, ok := lookup(kind)
		if !ok {
			panic("minimax: no ledger field for part kind " + string(kind))
		}
		return field
	}
}

var (
	contextPartField = partField(inference.GenerateContextPartField)
	inputPartField   = partField(inference.GenerateInputPartField)
)

// rejectTextControls rejects the text-only intent controls (tools, sampling,
// reasoning) for a non-text operation, one decision per active field so the
// report stays field-precise. Callers reject the text intent itself after.
func rejectTextControls(
	text *inference.TextIntent,
	ledger *inference.Ledger,
	toolsReason, samplingReason, reasoningReason string,
) {
	if len(text.Tools) > 0 {
		ledger.Reject(inference.FieldGenerateIntentTools, toolsReason)
	}
	if text.ToolChoice != nil {
		ledger.Reject(inference.FieldGenerateIntentToolChoice, toolsReason)
	}
	if text.Temperature != nil {
		ledger.Reject(inference.FieldGenerateIntentTemperature, samplingReason)
	}
	if text.TopP != nil {
		ledger.Reject(inference.FieldGenerateIntentTopP, samplingReason)
	}
	if text.ReasoningEnabled != nil {
		ledger.Reject(inference.FieldGenerateIntentReasoningEnabled, reasoningReason)
	}
	if text.ReasoningEffort != "" {
		ledger.Reject(inference.FieldGenerateIntentReasoningEffort, reasoningReason)
	}
}
