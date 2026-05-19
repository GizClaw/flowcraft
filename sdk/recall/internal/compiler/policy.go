package compiler

import "github.com/GizClaw/flowcraft/sdk/recall/internal/governance"

// Policy is the write-path governance hook. Aliased to governance so
// compiler and governance share one contract (docs §10.2).
type Policy = governance.WritePolicy

// NopPolicy allows every fact through unchanged.
type NopPolicy = governance.NopWritePolicy
