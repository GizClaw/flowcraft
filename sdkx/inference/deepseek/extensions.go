package deepseek

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// rejectOtherExtensions marks every active field of every extension that
// does not apply to this operation as rejected on the ledger.
func rejectOtherExtensions(operation string, other []inference.Extension, ledger *ledger) {
	for _, extension := range other {
		for _, field := range extension.ActiveFields() {
			ledger.reject(field.Qualify(extension), fmt.Sprintf("extension %q does not apply to %s", extension.ExtensionID(), operation))
		}
	}
}
