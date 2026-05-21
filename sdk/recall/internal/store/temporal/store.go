package temporal

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ErrNotFound is returned by Get / UpdateValidity / ReopenValidity
// when the fact does not exist in the requested scope. It aliases
// port.ErrNotFound so callers depend on the port contract.
var ErrNotFound = port.ErrNotFound

// ErrReopenConflict is returned by ReopenValidity when the fact's
// current CorrectedBy does not match the expected value supplied by
// the caller. This means another writer has legitimately closed the
// fact for a different reason and rollback must NOT clobber it.
//
// Classified as errdefs.Conflict so callers can distinguish the
// "guard failed, do not retry" case from a transient store error.
var ErrReopenConflict = errdefs.Conflict(errdefs.New("recall temporal store: reopen guard mismatch"))

// ErrValidityAlreadyClosed is returned by UpdateValidity when the
// target fact already carries a non-zero ValidTo and the caller
// supplies a (validTo, correctedBy) tuple that does not match the
// existing one. The store stays strict so callers that DO require
// exclusive close semantics (e.g. RebuildAll) see the conflict; the
// canonical Save pipeline treats it as a benign race signal because
// the desired post-state ("prior fact is closed") is already true.
//
// Classified as errdefs.Conflict so it remains a 409-shaped failure
// at the public boundary for any caller that has not opted in to the
// tolerant interpretation.
var ErrValidityAlreadyClosed = errdefs.Conflict(errdefs.New("recall temporal store: fact validity already closed"))

// TemporalStore and ListQuery live in internal/port/store.go.
// This package implements port.TemporalStore (see memory_store.go).
