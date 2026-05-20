// Package projection houses the rebuildable derived views of the
// temporal ledger plus the fanout that dispatches writes to them.
// The canonical Projection contract lives in internal/port; this
// file re-exports the Consistency enum constants so projection
// implementations can use the short Required / Optional names.
package projection

import "github.com/GizClaw/flowcraft/sdk/recall/internal/port"

// Re-exported port.Consistency constants. The interface itself is
// port.Projection — concrete projection implementations satisfy it
// via compile-time assertions in their own files.
const (
	Required = port.Required
	Optional = port.Optional
)
