// Package mitm implements the MITM engine the sandbox backends use to enforce
// network policy on intercepted TLS: it terminates the client's TLS
// connection with a locally issued certificate, inspects the request, asks the
// policy and hooks whether it may proceed, and forwards it upstream when it
// may.
//
// # Roles
//
// [CA] issues the leaf certificates; [NewCA] builds one, [WriteBundle] writes
// the PEM bundle a sandboxed child trusts. [New] builds the engine from the
// policy, the hooks, a dialer for the upstream leg, and the root pool used to
// verify it. The contract it satisfies — MITMPolicy, MITMHooks, MITMEngine —
// belongs to core/utils/net, so the sandbox backends and this implementation
// agree on one interface.
//
// # Invariants
//
// A refused request fails closed: a hook error becomes errBlocked and the
// client gets 403, never an unfiltered pass-through. The hooks observe the
// client request's own context, so their deadline and span belong to the
// connection being inspected.
//
// Every upstream connection, response body and inspection artifact is closed
// on the failure paths as well as the success path; a leaked body would keep
// the upstream socket alive after the sandbox call returned.
//
// # Boundary
//
// Deciding whether a host is allowed is core/utils/net's job (the matcher and
// the policy); this package only enforces the decision on a live connection.
// The sandbox backends (bwrap, seatbelt, windows net isolation) own when the
// engine is installed.
package mitm
