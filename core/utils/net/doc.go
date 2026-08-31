// Package net contains the shared network-policy and enforcement
// machinery used by the sandbox backends.
//
// It owns:
//
//   - NetPolicy and the NetMode / NetRule / NetAction / MITMPolicy
//     types that describe a sandbox's outbound-network posture;
//   - host-pattern compilation and matching (hostmatch plus the
//     higher-level Matcher over NetRule);
//   - the host-side enforcement Proxy used by the bwrap, seatbelt, and
//     windows backends for allow-list and proxy modes. One listener
//     multiplexes HTTP (plain requests and CONNECT) with SOCKS5
//     (CONNECT only, no auth), so HTTP clients and SOCKS5-aware
//     non-HTTP clients (ssh ProxyCommand, curl --socks5-hostname, tools
//     honoring ALL_PROXY=socks5://...) all traverse the same allow-list
//     / upstream policy;
//   - model-facing deny reasons on rules (NetRule.Reason), surfaced in
//     HTTP denial bodies and audit records;
//   - the MITM seam interfaces consumed by the proxy, with the
//     implementation living in the mitm subpackage.
//
// The package is deliberately independent of core/sandbox: sandbox
// imports net, never the reverse.
package net
