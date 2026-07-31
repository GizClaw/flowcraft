// Package seatbelt implements sdk/sandbox.Runner on top of Apple's
// sandbox-exec (Seatbelt / SBPL) — the only built-in confinement
// primitive on macOS. It is the macOS sibling of sdkx/sandbox/nsjail:
// same policy surface, different enforcement kernel.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete adapters
// that integrate with external systems. sandbox-exec is an external
// binary shipped with macOS, wrapped the same way nsjail is wrapped on
// Linux. The Runner type implements the generic sandbox.Runner
// interface, so a caller can be retargeted between LocalRunner, this
// backend, and nsjail without changing call sites.
//
// # macOS-only
//
// sandbox-exec exists only on macOS. The Runner type is therefore only
// constructible on darwin; on other platforms [New] returns
// errdefs.NotAvailable so callers can import the package for type
// references without build-tag gymnastics.
//
// # Capability matrix vs. sandbox.LocalRunner
//
// Mapping of sandbox.ExecOptions fields onto the Seatbelt profile:
//
//	WorkDir                     -- chdir; writes confined to the root
//	Stdin                       piped via os/exec
//	Timeout                     Go-side ctx deadline + process-group kill
//	Env.Allow / Env.Inject      filtered in Go (c.Env); Seatbelt has no env concept
//	Net.Mode == NetDefault      no network rules (host posture)
//	Net.Mode == NetDenyAll      (deny network*)
//	Net.Mode == NetAllowList    errdefs.NotAvailable (hostname rules need a proxy)
//	Net.Mode == NetProxy        errdefs.NotAvailable (no redirect primitive)
//	Resources.MemoryBytes       group watcher (sdk/sandbox.GroupCapsWatcher)
//	Resources.CPUMillicores     group watcher, cpu-time = Timeout x millicores/1000
//	Resources.DiskBytes         errdefs.NotAvailable (no quota mechanism)
//	Resources.MaxOutputBytes    truncated in Go (limitedBuffer)
//
// # Blast-radius policy shape
//
// The generated profile reads as "allow everything, deny all writes,
// re-allow the workspace": reads and process execution are unrestricted
// (a local agent must reach the real toolchain), file writes are denied
// machine-wide except the runner root and /dev/null. Dedicated temp
// or cache paths may be added explicitly with WithWritablePaths. This
// is the containment posture the local-sandbox PRD
// calls blast-radius: not total isolation, but an honest boundary
// around the workspace.
//
// Note: sandbox-exec is formally deprecated by Apple yet remains
// functional and is the same primitive Chrome and Anthropic's
// sandbox-runtime rely on. A future Virtualization.framework backend
// can supersede this package without changing the Runner seam.
package seatbelt
