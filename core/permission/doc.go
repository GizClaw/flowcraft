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
//   - Reads: not restricted, by decision. A read rule protects a secret, and a
//     secret belongs outside the tree the agent works in. Enforcing one here
//     would also mean two projections that must stay in sync forever — the
//     files a tool opens in this process, and whatever a sandboxed command
//     opens — for a failure that does not persist. Reads are bounded by where
//     the host places its data, not by a rule.
//
// # What is deliberately absent
//
//   - Globs. A mount binds a path or a subtree; it cannot bind "anything named
//     tmp at any depth" without scanning the filesystem first. A permission the
//     kernel cannot enforce is not modelled here.
//   - Read and deny entries. They existed and nothing configured them. A rule
//     nothing configures does not encode a decision; it encodes the hope that
//     somebody will, and in the meantime it reads as protection.
//
// Nothing here performs I/O. That rule is what keeps the package from becoming
// a hub: policy is a value, enforcement is somebody else's package.
package permission
