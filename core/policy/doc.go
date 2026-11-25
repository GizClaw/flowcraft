// Package policy is the permission vocabulary: what an agent may do as a
// ceiling, what a session actually does, and the one function that turns the
// first into the second.
//
// # One type, two instances
//
// A [Grant] is both a ceiling and a fact. The deployment parses an agent's
// ceiling; [Grant.Narrow] produces a session's version, and it only ever
// narrows. There is no second "bind" type, because a second type would mean a
// second validator, a second digest and a second rendering of the same facts —
// and the first thing two representations do is disagree.
//
// # Three defaults, chosen one at a time
//
// The fields of a Grant do not share a default policy, and the difference is
// deliberate:
//
//   - [Files] read and write are empty by default: unrestricted. Reading is
//     recoverable and auditable, and "let the agent look around" is the ordinary
//     deployment. A caller narrows by listing roots, and [Files.ReadOnly] is the
//     one field that expresses "none".
//   - [ToolSet] allow is empty by default: every wired tool. Which tools exist is
//     a property of the process, so "all of them" is a meaningful default.
//   - Models are empty by default: every model the process serves. A model call
//     spends money, so a deployment that wants a subset writes one down; the
//     host enforces it at the call, which is what makes the empty set mean
//     "unrestricted" rather than "none".
//
// # A widening is a second layer, not a second ceiling
//
// [Grant.Escalate] is what an operator declared above the ceiling, and [Grant.Widen]
// is the only function that turns it into authority: for one command, a session may
// be run in a boundary the declaration covers, never in the ceiling it was narrowed
// from. Both invariants — the declaration is inside the ceiling's reach, and a
// widening never drops what the bind already had — are checked here, because they
// are properties of the values rather than of the host that asks.
//
// # This package does no I/O and emits no telemetry
//
// Everything here is a value with a validator. Refusals are returned as
// [Denial], which the caller records; a vocabulary package that opened handles or
// started spans would be the layer everyone has to import, and the one place
// nobody can test in isolation.
package policy
