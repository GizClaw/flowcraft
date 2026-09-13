package inference

import (
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/message"
)

// Declaration checks enforce what a model *declares* about itself before any
// provider work starts. They are deliberately distinct from compiler checks:
//
//   - the declaration check reads the frozen ModelDescriptor and answers
//     "is this request even in this model's declared envelope?";
//   - the compiler check reads the provider's own catalog facts and answers
//     "can this provider surface carry this request?".
//
// Both rejections are shaped the same way — an Error naming the canonical
// request field with a kind the route layer treats as transport-safe — so a
// request that exceeds one model's declaration falls back to another target
// instead of failing outright.

const (
	// declarationDetailMaxOutputTokens is the stable, redacted stage label of
	// a declared output-budget rejection (see Error.Detail).
	declarationDetailMaxOutputTokens = "declaration.max_output_tokens"
	// declarationDetailInputPrefix labels a declared input-capability
	// rejection, followed by the content kind the request carried.
	declarationDetailInputPrefix = "declaration.input."
)

// declarationGatedInputKinds are the content kinds whose wire form is not
// universal: a surface carries one only when the model declares it, which is
// how a driver decides to reject it. Text and structured data are the baseline
// every compiler lowers, and tool calls, tool results and reasoning traces are
// structural parts whose role and surface rules belong to the driver, so this
// check leaves those to the compiler.
var declarationGatedInputKinds = []message.PartKind{
	message.PartImage,
	message.PartAudio,
	message.PartVideo,
	message.PartFile,
}

// checkGenerateDeclaration applies the declaration checks: the request must
// fit the output budget the model declares and carry only the input kinds it
// declares.
func checkGenerateDeclaration(
	descriptor ModelDescriptor,
	request GenerateRequest,
) error {
	if err := checkDeclaredOutputLimit(descriptor, request); err != nil {
		return err
	}
	return checkDeclaredInputKinds(descriptor, request)
}

// checkDeclaredOutputLimit rejects a generate request whose requested output
// budget exceeds what the model declares it can emit.
//
// The limit is a promise, not a clamp: silently lowering the caller's budget
// would hide both the caller's intent and the routing opportunity. An
// undeclared limit means the provider catalog claimed no bound, so nothing is
// rejected on its behalf.
func checkDeclaredOutputLimit(
	descriptor ModelDescriptor,
	request GenerateRequest,
) error {
	text := request.Input.Content.Intent.Text
	if text == nil || text.MaxOutputTokens == nil {
		return nil
	}
	limit := descriptor.Limits.MaxOutputTokens
	if limit == nil || *text.MaxOutputTokens <= *limit {
		return nil
	}
	err := NewError(
		UnsupportedFeature,
		OperationGenerate,
		FieldGenerateIntentTextMaxOutputTokens,
		fmt.Errorf(
			"requested max output tokens %d exceeds the declared limit %d",
			*text.MaxOutputTokens,
			*limit,
		),
	)
	err.Detail = declarationDetailMaxOutputTokens
	return err
}

// checkDeclaredInputKinds rejects a generate request carrying a content kind
// the model does not declare, which is what the driver's own compiler would do
// once it ran. Rejecting here means it never opens the model's drivers: the
// rejection costs no provider client and no credential resolution, and the
// route layer treats it as transport-safe, so the request falls back to a
// target that declares the part.
//
// An undeclared input list is unknown, not "accepts nothing": drivers publish
// part kinds only where they can reach the wire, so filtering on an empty
// declaration would reject every request to a model that never published one.
// The compiler stays the arbiter for everything this check does not cover —
// the baseline kinds, and structural parts whose role rules are per surface.
func checkDeclaredInputKinds(
	descriptor ModelDescriptor,
	request GenerateRequest,
) error {
	declared := descriptor.Capabilities.Inputs
	if len(declared) == 0 {
		return nil
	}
	for _, turn := range request.Context {
		if err := rejectUndeclaredInputKinds(
			descriptor, declared, turn.Content.Parts, GenerateContextPartField,
		); err != nil {
			return err
		}
	}
	return rejectUndeclaredInputKinds(
		descriptor, declared, request.Input.Content.Parts, GenerateInputPartField,
	)
}

// rejectUndeclaredInputKinds returns the rejection for the first carried part
// whose kind the model does not declare, or nil. field maps one content kind
// onto the canonical request field this position activates, so the rejection
// names the field the report uses.
func rejectUndeclaredInputKinds(
	descriptor ModelDescriptor,
	declared []message.PartKind,
	parts []message.Part,
	field func(message.PartKind) (FieldID, bool),
) error {
	for _, part := range parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		kind := normalized.Kind()
		if !slices.Contains(declarationGatedInputKinds, kind) ||
			slices.Contains(declared, kind) {
			continue
		}
		canonical, ok := field(kind)
		if !ok {
			continue
		}
		rejection := NewError(
			UnsupportedFeature,
			OperationGenerate,
			canonical,
			fmt.Errorf(
				"request carries %s input, which model %q does not declare",
				kind, descriptor.ID.Name,
			),
		)
		rejection.Detail = declarationDetailInputPrefix + string(kind)
		return rejection
	}
	return nil
}
