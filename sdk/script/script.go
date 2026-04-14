// Package script defines a language-agnostic script execution interface.
// Implementations (e.g. script/jsrt for JavaScript, script/luart for Lua)
// provide concrete runtimes that satisfy the Runtime interface.
package script

import "context"

// Signal represents a control signal from a script back to the host.
type Signal struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// Runtime executes scripts with injected host bindings.
// Implementations must be safe for concurrent use.
type Runtime interface {
	Exec(ctx context.Context, name, source string, env *Env) (*Signal, error)
}

// Env carries per-execution configuration and host bindings.
type Env struct {
	// Config is script-level configuration, accessible as a global in the script.
	Config map[string]any

	// Bindings maps names to host objects injected into the script's global scope.
	// Each value is typically a map[string]any of Go functions callable from the script.
	Bindings map[string]any
}
