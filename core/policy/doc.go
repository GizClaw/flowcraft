// Package policy is the permission vocabulary: what one session may do, in one
// value with one validator and one digest.
//
// # One type, one authority
//
// A [Grant] is stated by whoever opens a session, which is the application — the
// trusted side. It is a value, not a gate: the host reads it to build the
// boundary, and the tools read it to refuse a call with a correctable message.
// There is no second type and no second source, because two representations of the
// same facts disagree before anything else does.
//
// # Three defaults, and one reason for all of them
//
// [Files], [ToolSet]'s allow-list and Models are all empty by default, and all for
// the same reason: what exists at all is a property of the process, so "all of it"
// is the one meaningful default. A caller that wants less writes it down, and each
// set is enforced where it is used — the tree handles for files, the projection for
// tools, and a refusal before any provider sees a model call.
//
// [Files.ReadOnly] is the one field that expresses "none", because there is no way to
// spell an empty root list differently from an omitted one.
//
// # A widening is one command, not a new session
//
// [Grant.Widen] is what an approval turns into a temporary cell's authority. It
// checks the three invariants the feature depends on — the approved grant keeps
// every permit the session had, only files.write, net and exec.env may differ, and
// the result validates — because they are properties of the values rather than of
// the host that asks. The bound above the approval is what the machine can enforce,
// which the sandbox backend answers when the temporary cell is built.
//
// # This package does no I/O and emits no telemetry
//
// Everything here is a value with a validator. Refusals are returned as
// [Denial], which the caller records; a vocabulary package that opened handles or
// started spans would be the layer everyone has to import, and the one place
// nobody can test in isolation.
package policy
