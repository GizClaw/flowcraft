package runtime

import "github.com/GizClaw/flowcraft/sdk/errdefs"

// ErrBuilderUsed reports an attempted registration or Build after a Builder's
// single construction attempt has started.
var ErrBuilderUsed = errdefs.Conflictf("runtime Builder has already been used")
