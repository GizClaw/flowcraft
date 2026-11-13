// Package sandbox is the command execution boundary: the contract every
// backend implements, and nothing else.
//
// # What the contract promises
//
// A [Runner] starts interactive or streaming process sessions under a policy and
// manages their lifecycle. A [Session] is the unified handle to one running
// process — output is an append-only log read by cursor, input is a stream, and
// exit is a cached value. The one-shot form is derived, not part of the
// interface: engine.Exec drains a session, so a new backend gets one-shot
// execution for free and has no second code path to keep in step.
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
// Four layers, in dependency order:
//
//  1. This package: contracts and the value types they are made of. Zero
//     behavior: no OS probing, no process spawning, no HTTP, no filesystem
//     access. If a symbol would need any of those, it belongs one layer down.
//  2. engine: the SDK over the contract — policy decorators (command gates,
//     approval, defaults), escalation, resource-limit validation, the one-shot
//     Exec, and platform shell selection.
//  3. Support packages that a backend needs but that are not the contract:
//     journal (filesystem event sources), netpolicy (proxy, MITM, SOCKS5), and
//     sessionkit (process spawning, the session registry, the group resource
//     sampler).
//  4. Backends: local, bwrap, seatbelt, windows — and remote, which wraps
//     another runner instead of spawning processes.
//
// A backend is an implementation, not part of the contract. It may import
// engine and any support package; nothing above it may import a backend except
// the host that selects one (core/runtime).
//
// # Why the support packages are public
//
// Go's internal rule bounds visibility by directory: core/sandbox/internal/...
// is importable only from inside core/sandbox. Every shared mechanism a backend
// needs therefore has to live in a public package under core/sandbox — which is
// why sessionkit is public rather than internal/session. Keeping a mechanism in
// internal/ is a promise that no backend will ever live outside this tree; that
// promise is not one this package makes.
//
// # Verifying a backend
//
// A backend is verified against [sandboxtest], not against its own reading of
// this documentation: the suite drives a real runner and asserts the promises
// made here (capability honesty, cursor semantics, EOF, idempotent lifecycle,
// the /dev/null stdin rule). A one-shot Exec, a new transport, or a new
// platform does not need a new interpretation of the contract — it needs the
// suite to pass.
//
// The behavioural guide is docs/guides/sandbox.md; journal semantics are
// documented on [JournalProvider] and in the journal package.
package sandbox
