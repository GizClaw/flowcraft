package inference

import "strings"

// Field identifies one knob of a canonical request. Drivers report which
// fields they could not honor, and [Warning] names the field so a caller can
// tell "the output was adjusted" from "the tools were dropped".
//
// The set is deliberately flat and small: one name per knob that appears on
// [GenerateRequest], not one name per provider parameter. Provider-specific
// options are addressed through [ExtensionField], which qualifies itself with
// its provider and extension identity. Deployment knobs — sampling, response
// format, reasoning, image size, voice — are not named here because they never
// cross this package's request boundary.
type Field string

const (
	FieldModel      Field = "model"
	FieldContext    Field = "context"
	FieldInput      Field = "input"
	FieldRole       Field = "role"
	FieldContent    Field = "content"
	FieldOutput     Field = "output"
	FieldTools      Field = "tools"
	FieldToolChoice Field = "tool_choice"
	FieldExtensions Field = "extension"
	FieldStream     Field = "stream"

	// Fields of the embedding seam.
	FieldDimensions Field = "dimensions"
	FieldVectors    Field = "vectors"

	// Fields of the decision seam.
	FieldQuestions Field = "questions"
	FieldAnswers   Field = "answers"
)

const extensionFieldPrefix = "extension."

// IsExtension reports whether the field addresses a provider-specific option
// rather than a canonical knob.
func (f Field) IsExtension() bool { return strings.HasPrefix(string(f), extensionFieldPrefix) }

// Valid reports whether f is a canonical field or a qualified extension field.
func (f Field) Valid() bool {
	switch f {
	case FieldModel, FieldContext, FieldInput, FieldRole, FieldContent,
		FieldOutput, FieldTools, FieldToolChoice, FieldExtensions,
		FieldStream:
		return true
	}
	return f.IsExtension() && len(f) > len(extensionFieldPrefix)
}
