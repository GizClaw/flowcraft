// Package sandbox is the command execution boundary: the contract every
// backend implements, and nothing else.
//
// # What the contract promises
//
// A [Runner] starts interactive or streaming process sessions under a policy and
// manages their lifecycle. A [Session] is the unified handle to one running
// process — output is an append-only log read by cursor, input is a stream, and
// exit is a cached value. The one-shot form is derived, not part of the
// interface: [Exec] drains a session, so a new backend gets one-shot execution
// for free and has no second code path to keep in step.
//
// Two rules make a backend honest, and both are enforced by review rather than
// by the type system:
//
//   - [Capabilities] is the explicit, wire-safe declaration of what the backend
//     can do. Callers and tools never discover features through interface
//     assertions; an unsupported operation fails with errdefs.NotAvailable, and
//     Capabilities says up front why. A backend that advertises a capability it
//     does not enforce turns the declaration into a lie.
//   - A policy a backend cannot enforce is rejected, never silently downgraded.
//     Narrowing is allowed everywhere — a call may ask for a stricter boundary
//     than the runner was built with — but nothing widens a boundary.
//
// # Layering
//
// Three layers, in dependency order:
//
//  1. This package: the contract, the values it is made of, and the three derived
//     pieces every backend would otherwise re-implement — [Exec] (the one-shot
//     form), [ValidateConfine] (the construction-time check that the instance's
//     permission is enforceable, with [Confine.Narrow] resolving a call's budget
//     against the instance's ceiling), and [DefaultShell]/[ShellFor] (which shell
//     exists on this host). It spawns nothing itself: Exec goes through a
//     [SessionStarter], and the shell picker only probes.
//
//  2. The sandbox module (github.com/GizClaw/flowcraft/sandbox), which owns how
//     the contract is carried out: journal (filesystem event sources),
//     netpolicy (proxy, MITM, SOCKS5), and sessionkit (process spawning, the
//     session registry, the group resource sampler). They depend on this package;
//     this package never depends on them, which is why they are a module rather
//     than a subdirectory: the contract is versioned with the platform, the
//     implementations with the backends they serve.
//
//     The split is also a stability statement. This package is the compatibility
//     surface: its exported API is what a backend implements, so breaking it is a
//     deliberate, recorded change. The sandbox module is not — its machinery may be
//     reshaped between minor versions, because every backend that exists today
//     lives in this repository. The first backend from outside freezes that surface
//     too.
//
//  3. Backends, in that same module: sandbox/local, sandbox/bwrap,
//     sandbox/seatbelt, sandbox/windows — and sandbox/remote, which wraps another
//     runner instead of spawning processes. A backend is a sibling package of the
//     machinery, not a subpackage of it, because it is an implementation of the
//     contract rather than part of the contract.
//
// The machinery is in the sandbox module today; no backend is yet — the batch
// that rebuilds one is where the module gains its first.
//
// Command gating is deliberately absent. Asking a human, or a rule, whether one
// command may run — and asking again whether a refused command may leave the
// boundary — is host policy, not a sandbox capability: it decorates the narrow
// [SessionStarter], and "leave the boundary" means opening a session with a wider
// grant rather than widening this one.
//
// A backend may import any of the machinery beside it; nothing above it may
// import a backend except the host that selects one.
//
// # Verifying a backend
//
// A backend is verified against sandbox/sandboxtest, not against its own reading
// of this documentation. The suite lives beside this package, not in the
// implementations module, because it is the contract's executable specification:
// a backend author outside this repository should be able to depend on the
// contract and its suite without pulling in the machinery built here. It drives a
// real runner and asserts the promises
// made here (capability honesty, cursor semantics, EOF, idempotent lifecycle,
// the /dev/null stdin rule). A one-shot Exec, a new transport, or a new
// platform does not need a new interpretation of the contract — it needs the
// suite to pass.
package sandbox
