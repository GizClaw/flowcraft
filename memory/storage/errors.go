// Package storage defines the canonical storage contracts for the memory
// module: an append-only Log for ordered, idempotent event streams and a
// key-value Store for current values and keyed state. Contracts live here,
// inside the consuming module, and not in sdk; sdk/workspace is one adapter
// implementation, never the contract itself.
package storage

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Common storage errors. They carry errdefs classification so callers can
// use either errors.Is (identity) or errdefs.IsNotFound / IsConflict
// (category) without translating at the package boundary.
var (
	// ErrNotFound reports a missing key, stream, or event. Delete-style
	// operations are idempotent and do not return it.
	ErrNotFound = errdefs.NotFound(errors.New("storage: not found"))

	// ErrConflict reports an immutable write to an existing key or an
	// idempotency key replayed with different content.
	ErrConflict = errdefs.Conflict(errors.New("storage: conflict"))
)
