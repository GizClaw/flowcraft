package inference

import "fmt"

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

// declarationDetailMaxOutputTokens is the stable, redacted stage label of a
// declared output-budget rejection (see Error.Detail).
const declarationDetailMaxOutputTokens = "declaration.max_output_tokens"

// checkGenerateDeclaration rejects a generate request whose requested output
// budget exceeds what the model declares it can emit.
//
// The limit is a promise, not a clamp: silently lowering the caller's budget
// would hide both the caller's intent and the routing opportunity. An
// undeclared limit means the provider catalog claimed no bound, so nothing is
// rejected on its behalf.
func checkGenerateDeclaration(
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
