package port

import "github.com/GizClaw/flowcraft/sdk/errdefs"

// ErrNotFound is the portable missing-entity sentinel for recall store
// ports. TemporalStore and EvidenceStore implementations should return
// (or wrap) this value so facades and pipelines do not import
// subsystem packages such as internal/store/temporal.
var ErrNotFound = errdefs.NotFound(errdefs.New("recall store: not found"))
