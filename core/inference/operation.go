package inference

import "fmt"

// Operation names the kind of model call. It appears in [Metadata] and in
// errors so that a caller can report what failed without inspecting the
// request.
type Operation string

const (
	OperationGenerate   Operation = "generate"
	OperationEmbed      Operation = "embed"
	OperationTranscribe Operation = "transcribe"
)

// Validate reports whether the operation is one the contract names.
func (o Operation) Validate() error {
	switch o {
	case OperationGenerate, OperationEmbed, OperationTranscribe:
		return nil
	default:
		return fmt.Errorf("unknown operation %q", o)
	}
}
