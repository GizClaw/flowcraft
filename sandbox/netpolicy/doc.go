// Package netpolicy is the shared outbound-network policy and its enforcement
// machinery, used by the sandbox backends. It decides whether a host may be
// reached and, when a proxy is installed, enforces that decision on live
// connections.
//
// # Roles
//
// [NetPolicy] is the declared posture — a [NetMode] plus ordered [NetRule]
// values with [NetAction]s, and the [MITMPolicy] that says whether TLS is
// intercepted. The matcher compiles host patterns (domains, wildcards, IP
// literals, CIDR) once and answers them purely. The enforcement proxy is one
// listener that multiplexes HTTP (plain requests and CONNECT) with SOCKS5
// (CONNECT only, no auth), so HTTP clients and SOCKS5-aware non-HTTP clients
// (ssh ProxyCommand, curl --socks5-hostname, a tool honoring
// ALL_PROXY=socks5://…) traverse the same allow-list and upstream policy. The
// MITM seam interfaces live here; their implementation is the mitm subpackage.
//
// # Invariants
//
// Rules are evaluated in order and the first match decides, so a deny placed
// before an allow is what makes an exception possible. A hostname that
// resolves to several addresses is decided per answer: the dial target is
// pinned to the first address an allow rule actually covers, a deny match
// anywhere in the answer denies outright, and a dial never re-resolves into an
// address the checks did not validate. A refused request fails closed — the
// client sees the denial, never an unfiltered pass-through — and the rule's
// reason travels with it into the denial body and the audit record.
//
// # Boundary
//
// This package is independent of core/sandbox by design: sandbox imports it,
// never the reverse. Installing the proxy, owning the child's lifetime and
// deciding when interception is on belongs to each backend.
package netpolicy
