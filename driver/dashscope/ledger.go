package dashscope

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
			panic("dashscope: no ledger field for part kind " + string(kind))
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

// turnParts collects the text prompt and media parts of a request whose
// surface has no conversation channel: user context turns and the input join
// in order, and any other role is rejected once rather than silently
// flattened into the prompt. The parts callback receives the ledger field
// resolver for the turn it came from, so a decision lands on the field the
// runtime activated.
func turnParts(
	request inference.GenerateRequest,
	ledger *inference.Ledger,
	visit func(part message.Part, fields func(message.PartKind) inference.FieldID),
	roleReason string,
) (prompt []string) {
	collect := func(parts []message.Part, fields func(message.PartKind) inference.FieldID) {
		for _, part := range parts {
			if text, ok := part.(message.TextPart); ok {
				prompt = append(prompt, text.Text)
				continue
			}
			visit(part, fields)
		}
	}
	for _, turn := range request.Context {
		if turn.Role != message.RoleUser {
			ledger.Reject(inference.FieldGenerateContextRole, roleReason)
			continue
		}
		collect(turn.Content.Parts, contextPartField)
	}
	collect(request.Input.Content.Parts, inputPartField)
	return prompt
}

// rejectPromptLength rejects a prompt that exceeds the declared character
// limit, landing on the most precise active text field: the input text when
// the request input carries text, otherwise a context text field, otherwise
// the input role.
func rejectPromptLength(
	request inference.GenerateRequest,
	ledger *inference.Ledger,
	length, limit int,
	reason string,
) {
	if limit <= 0 || length <= limit {
		return
	}
	if containsTextPart(request.Input.Content.Parts) {
		ledger.Reject(inference.FieldGenerateInputText, reason)
		return
	}
	for _, turn := range request.Context {
		if containsTextPart(turn.Content.Parts) {
			ledger.Reject(inference.FieldGenerateContextText, reason)
			return
		}
	}
	ledger.Reject(inference.FieldGenerateInputRole, reason)
}

func containsTextPart(parts []message.Part) bool {
	for _, part := range parts {
		if part != nil && part.Kind() == message.PartText {
			return true
		}
	}
	return false
}

// rejectionTarget answers which of a rejection's candidate fields the request
// actually turned on. A decision only reaches the report for an active field,
// and [inference.CompileReport.ValidateFailure] requires a failed compile to
// reject at least one, so rejecting an inactive field fails the compile with no
// decision to explain it.
//
// A setting a surface requires but the request left out — an input image on an
// editing model, a prompt the caller only declared as an intent — has no field
// of its own, because a field is active exactly while the request carries a
// value for it. Callers therefore hand [rejectionTarget.reject] the candidates
// in precision order and let the request decide where the rejection lands.
type rejectionTarget map[inference.FieldID]struct{}

func newRejectionTarget(
	request inference.GenerateRequest,
	shape inference.GenerateExecutionShape,
) rejectionTarget {
	target := make(rejectionTarget)
	for _, field := range request.ActiveFieldsFor(shape) {
		target[field] = struct{}{}
	}
	return target
}

// reject records reason against the first candidate the request activates. The
// input role is the backstop: GenerateInput.Validate requires a user or tool
// role, so a request that reached a compiler always carries that field.
func (t rejectionTarget) reject(
	ledger *inference.Ledger,
	reason string,
	candidates ...inference.FieldID,
) {
	for _, field := range candidates {
		if _, ok := t[field]; ok {
			ledger.Reject(field, reason)
			return
		}
	}
	ledger.Reject(inference.FieldGenerateInputRole, reason)
}
