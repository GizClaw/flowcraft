// Package permission holds the declarations that answer "what may this turn
// touch?" — files and network — plus the pure predicates that read them.
//
// One value, several interpreters. A Policy is a value: it can be built once
// from configuration, copied into a per-call context, and serialised across a
// process boundary. Who interprets it depends on what is being enforced:
//
//   - fsx interprets the file half in-process, right before touching the disk.
//   - sandbox projects the writable paths onto the kernel (bind mounts, a
//     seatbelt profile, Windows ACLs) and the network half onto its proxy and
//     netns machinery.
//
// Nothing here performs I/O. That is the rule that keeps this package from
// becoming a hub: policy is a value, enforcement is somebody else's package.
// If a change wants to open a file, listen on a socket, or parse configuration
// here, the logic belongs in the interpreter instead.
//
// What is deliberately absent:
//
//   - Globs. A mount can bind a path or a subtree; it cannot bind "anything
//     named tmp at any depth" without scanning the filesystem first. A
//     permission the kernel cannot enforce is not modelled here.
//   - Read restrictions that a sandbox would honour. Backends do not project
//     them (see Access), so they are tool-layer guards, not a boundary.
package permission
