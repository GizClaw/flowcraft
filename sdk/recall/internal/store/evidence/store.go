// Package evidence is the secondary-lookup boundary for raw source
// material attached to canonical facts.
//
// Per docs §7.2 evidence stays *embedded* on TemporalFact as the
// source of truth (EvidenceRefs / EvidenceText / SourceMessageIDs).
// This package provides a thin adapter so callers that need
// scope-keyed evidence lookup (UI surfaces, eval repair, audit
// trails) do not have to reload the whole canonical fact. The
// adapter MUST stay rebuildable from the canonical store — it
// never becomes a second truth layer.
package evidence

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrNotFound is returned by Get when the evidence id is missing
// in the requested scope.
//
// Classified as errdefs.NotFound so the public boundary maps to
// 404 without each caller pattern-matching the message; identity
// stays compatible with errors.Is(err, ErrNotFound).
var ErrNotFound = errdefs.NotFound(errdefs.New("recall evidence store: evidence not found"))

// EvidenceStore lives in internal/port/store.go.
// This package implements port.EvidenceStore (see memory_store.go).
