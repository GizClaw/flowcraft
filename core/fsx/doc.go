// Package fsx is the file boundary of one session: the set of subtrees it may
// read, the set it may write, and the OS handles that enforce both.
//
// # Two sides, enforced the same way
//
// A Tree carries up to two root sets. Reads are confined to the read roots;
// writes are confined to the write roots. A path outside its side is refused
// before anything is opened, and once a handle is chosen the kernel — not this
// package — decides whether the relative path it is given stays inside.
//
// Each side is optional, and the two have deliberately different zero values:
//
//   - no read roots means "no restriction": an absolute path is handed to the
//     os package as-is.
//   - no write roots and [WithAllWrites] absent means "no write at all".
//
// The asymmetry is not an oversight. Reading is recoverable and auditable, and
// the common deployment is "let the agent look around"; writing is neither, so
// the zero value refuses it. A caller that wants unrestricted writes must say
// so with [WithAllWrites].
//
// # Why handles rather than a policy check
//
// A permission check on a string is a race: the path can be replaced with a
// symlink between the check and the open. An [os.Root] handle cannot be talked
// out of its subtree, so the boundary survives that race. A Tree therefore opens
// one handle per declared root and keeps it for the session's lifetime; Close
// releases them, and a host that opens Trees per session must Close them.
//
// # Reported paths are canonical
//
// Roots are resolved through symlinks once, when the Tree is opened, and every
// path the Tree reports back — readable roots, walk results, error messages —
// uses that resolved spelling. A caller that searches for its own unresolved
// spelling in a walk result will not find it; that is deliberate, because the
// resolved path is the one the boundary actually enforces.
//
// # What this package does not do
//
// It has no opinion about permissions. It takes root lists, not a policy type,
// because a boundary that imported the policy algebra would drag the model
// vocabulary and the digest rules into the layer that only needs to open files.
// The mapping from a policy (empty means unrestricted, readonly means no writes)
// lives one layer up.
//
// It also does not audit. Every operation here is one call; a session's audit
// ledger is emitted by the tools that make the calls, because only they know
// which conversation and which intent the call belongs to.
package fsx
