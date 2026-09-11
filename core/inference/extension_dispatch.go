package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// ExtensionFor splits the extensions that reached this provider into the
// options value of one operation and the extensions that belong to another
// operation.
//
// T is the operation's own options type. Extensions of any other concrete type
// land in the second result in request order. When a request carries more than
// one extension of type T the last one wins: one request body holds one set of
// options, and the compiler reads the value the caller wrote last.
//
// Entries that are nil, including a typed nil behind the interface, belong to
// neither result.
//
// The caller rejects the second result rather than ignoring it (see
// [Ledger.RejectExtensions]): an extension addressed to this provider but
// meant for another operation is a configuration error, not a silent no-op.
func ExtensionFor[T Extension](extensions Extensions) (T, Extensions) {
	var options T
	var other Extensions
	for _, extension := range extensions {
		if ptr.IsNil(extension) {
			continue
		}
		if typed, ok := extension.(T); ok {
			options = typed
			continue
		}
		other = append(other, extension)
	}
	return options, other
}

// RejectExtensions records a rejection for every active field of every
// extension that does not belong to operation, qualifying each field with the
// identity of the extension it came from.
//
// It is the counterpart of [ExtensionFor]: pass the extensions that did not
// match, or the whole request's extensions when the operation consumes none.
// The rejection is per field, so the ledger still names the knob that was
// refused instead of collapsing a whole extension into one decision. Nil
// entries are skipped.
func (l *Ledger) RejectExtensions(operation string, extensions Extensions) {
	if l == nil {
		return
	}
	for _, extension := range extensions {
		if ptr.IsNil(extension) {
			continue
		}
		reason := fmt.Sprintf(
			"extension %q does not apply to %s",
			extension.ExtensionID(),
			operation,
		)
		for _, field := range extension.ActiveFields() {
			l.Reject(field.Qualify(extension), reason)
		}
	}
}
