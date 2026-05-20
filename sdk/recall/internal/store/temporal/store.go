package temporal

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrNotFound is returned by Get / UpdateValidity / ReopenValidity
// when the fact does not exist in the requested scope.
//
// Classified as errdefs.NotFound so the public boundary
// (sdk/recall.Memory) and HTTP shims map it to 404 without each
// caller re-checking message text. The sentinel identity is
// preserved via the wrapped inner error so existing
// errors.Is(err, ErrNotFound) checks keep working.
var ErrNotFound = errdefs.NotFound(errdefs.New("recall temporal store: fact not found"))

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
