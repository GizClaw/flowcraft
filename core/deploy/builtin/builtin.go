// Package builtin is the kind table core ships: every engine, protocol and layer
// source that has a registration in this module, wired in one call.
//
// It is opt-in on purpose, and it mirrors core/runtime/backends. core/deploy
// registers no implementation of its own — it owns the document, the Spec and
// the kind tables, and nothing else — so a host that wants exactly one engine
// imports exactly that engine's package:
//
//	import (
//		enginesgraph "github.com/GizClaw/flowcraft/core/deploy/engines/graph"
//		"github.com/GizClaw/flowcraft/core/deploy/sources"
//	)
//
//	kinds := deploy.Kinds{}.
//		WithEngines(enginesgraph.Engine()).
//		WithSources(sources.File())
//
// Importing this package instead is the convenience trade those call sites make:
// one line, and the binary carries the graph runtime and the A2A client and
// server. Neither choice is wrong; the point is that it is a choice, made by an
// import rather than by a hardcoded table inside the document layer.
package builtin

import (
	"github.com/GizClaw/flowcraft/core/deploy"
	enginesa2a "github.com/GizClaw/flowcraft/core/deploy/engines/a2a"
	enginesgraph "github.com/GizClaw/flowcraft/core/deploy/engines/graph"
	protocolsa2a "github.com/GizClaw/flowcraft/core/deploy/protocols/a2a"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
)

// Kinds returns the kinds every deployment starts with: the graph and a2a
// engines, the a2a protocol, the two layer sources that need nothing from the
// host (file and inline), and no hooks — a hook is the host's own policy, so the
// platform registers none.
//
// The two sources are registered because a deployment that names no source at all
// cannot describe a runtime: every stack has to read something. A stack read from
// an embedded tree or a secret store is a factory the host adds
// (sources.FS, sources.Func) rather than an entry of this table: it needs a value
// — an fs.FS, a loader — that no document can write.
//
// The table is a value, so WithEngines and friends return a copy and extending it
// never mutates what this returned.
func Kinds() deploy.Kinds {
	return deploy.Kinds{}.
		WithEngines(enginesgraph.Engine(), enginesa2a.Engine()).
		WithProtocols(protocolsa2a.Protocol()).
		WithSources(sources.File(), sources.Inline())
}
