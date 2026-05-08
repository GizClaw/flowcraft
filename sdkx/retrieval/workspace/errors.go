package workspace

import "github.com/GizClaw/flowcraft/sdk/errdefs"

// errNilWorkspace is returned when [New] is called with a nil
// Workspace. Wrapped via errdefs so HTTP / RPC adapters can map it
// to a 400-class status without unwrapping by string.
var errNilWorkspace = errdefs.Validationf("retrieval/workspace: Workspace is nil")
