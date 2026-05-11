// Package script defines a language-agnostic script execution interface.
// Implementations (e.g. script/jsrt for JavaScript, script/luart for Lua via gopher-lua)
// provide concrete runtimes that satisfy the Runtime interface.
// Host APIs for [Env] are built with script/bindings; see bindings/doc.go for layout and conventions.
package script

import "context"

// Signal represents a control signal from a script back to the host.
//
// Type is always one of "interrupt" / "error" / "done" — the three
// outcomes a script can choose to surface besides "ran to completion".
//
// Kind is a per-Type sub-classifier:
//
//   - For Type "error" it carries an errdefs category name (see
//     [ErrorKind]). [SignalToError] maps it onto the matching errdefs
//     wrapper; unknown values degrade to [ErrorKindInternal] so a
//     typo in script land cannot escape as an unclassified error.
//   - For Type "interrupt" it carries an [engine.Cause] string. Unknown
//     values degrade to [engine.CauseCustom] for the same reason.
//   - For Type "done" Kind is unused.
//
// Message is the human-readable detail. Detail is freeform structured
// metadata scripts may attach; it is preserved across host translation
// but is not inspected by [SignalToError] today.
type Signal struct {
	Type    string         `json:"type"`
	Kind    string         `json:"kind,omitempty"`
	Message string         `json:"message,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
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
