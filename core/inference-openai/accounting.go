package openai

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// preflight rejects what this driver cannot express at all, before any mapping
// runs. Both dialects call it, so the two never drift on what they refuse.
func (d *driver) preflight(request inference.GenerateRequest) error {
	if request.Output.Kind() != inference.OutputText {
		return inference.NewError(
			inference.UnsupportedFeature, inference.OperationGenerate, inference.FieldOutput,
			fmt.Errorf("openai: output %q is not served by this driver", request.Output.Kind()),
		)
	}
	return d.checkDeclarations(request)
}

// honoredFields lists the canonical fields this driver consumes. Both dialects
// consume the same set, which is why it lives here rather than in each mapping:
// the contract check compares this against the request's active fields, and a
// divergence between the two dialects would be a silent drop on one of them.
//
// It is computed from the request rather than declared, so a field cannot be
// claimed as honored when the request never set it.
func honoredFields(request inference.GenerateRequest) []inference.Field {
	fields := []inference.Field{inference.FieldInput, inference.FieldRole}
	if len(request.Context) > 0 {
		fields = append(fields, inference.FieldContext)
	}
	if len(request.Input.Content.Parts) > 0 {
		fields = append(fields, inference.FieldContent)
	}
	if request.Output != "" {
		fields = append(fields, inference.FieldOutput)
	}
	if request.Tools.Len() > 0 {
		fields = append(fields, inference.FieldTools)
	}
	if request.Tools.Choice() != nil {
		fields = append(fields, inference.FieldToolChoice)
	}
	if len(request.Extensions) > 0 {
		fields = append(fields, inference.FieldExtensions)
	}
	return fields
}

// reasoningWarnings reports reasoning parts that were carried for display and
// not replayed to the provider. They are not silently lost: a caller sees why
// the round trip did not include them.
func reasoningWarnings(request inference.GenerateRequest) []inference.Warning {
	dropped := 0
	count := func(item message.Message) {
		for _, part := range item.Content.Parts {
			if _, ok := part.(message.ReasoningPart); ok {
				dropped++
			}
		}
	}
	for _, item := range request.Context {
		count(item)
	}
	count(request.Input.Message())
	if dropped == 0 {
		return nil
	}
	return []inference.Warning{{
		Field:   inference.FieldContent,
		Code:    "reasoning_not_replayed",
		Message: fmt.Sprintf("%d reasoning part(s) were not sent back to the provider", dropped),
	}}
}

// unsupportedOption rejects a per-call option the selected wire surface cannot
// express, instead of dropping it.
func unsupportedOption(option, surface string) error {
	return inference.NewError(
		inference.UnsupportedFeature,
		inference.OperationGenerate,
		inference.FieldExtensions,
		fmt.Errorf("openai: option %q is only available on the %s surface", option, surface),
	)
}

// rejectUnsupportedParts fails when a message carries a content kind the dialect
// cannot express. Skipping it would be the silent drop the contract check exists
// to prevent.
func rejectUnsupportedParts(item message.Message, allowed map[message.PartKind]bool) error {
	for _, part := range item.Content.Parts {
		if allowed[part.Kind()] {
			continue
		}
		return unsupportedPart(part.Kind())
	}
	return nil
}
