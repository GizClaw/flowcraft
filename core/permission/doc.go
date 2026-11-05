// Package permission holds the declarations that answer "what may this turn
// touch?" — writes and network — plus the path primitives those declarations
// are judged with.
//
// # What is enforced, and by whom
//
//   - Writes: projected onto the kernel (a bind mount, a seatbelt profile, a
//     Windows label) and enforced there. The process that performs a write is
//     the one the kernel confines, so the policy is not a suggestion.
//   - Network: projected onto the sandbox's proxy and namespace machinery.
//   - Reads: not restricted. This is a decision, not an omission. A read
//     restriction would have to be enforced by the kernel to mean anything, and
//     the only place it could be projected is the same projection that already
//     handles writes; without it, an in-process check would be a boundary that
//     stops the model but not a command it runs.
//
// # What is deliberately absent
//
//   - Globs. A mount binds a path or a subtree; it cannot bind "anything named
//     tmp at any depth" without scanning the filesystem first. A permission the
//     kernel cannot enforce is not modelled here.
//   - Read and deny entries. They existed, nothing configured them, and their
//     enforcement could only ever be advisory.
//
// Nothing here performs I/O. That rule is what keeps the package from becoming
// a hub: policy is a value, enforcement is somebody else's package.
package permission
