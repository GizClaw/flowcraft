// Package assembly turns a single JSON/YAML manifest into a runnable
// vessel.Captain plus the supporting workspace, memory, knowledge and tool
// dependencies.
//
// The package is intentionally a Go-library assembly layer. It does not run a
// daemon, start HTTP servers, install signal handlers, or process multi-file
// inventories. Callers own those concerns and decide when to call Build,
// StartOps, Stop and Close.
package assembly
