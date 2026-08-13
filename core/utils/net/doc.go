// Package net contains the shared network-policy and enforcement
// machinery used by the sandbox backends.
//
// It owns:
//
//   - NetPolicy and the NetMode / NetRule / NetAction / MITMPolicy
//     types that describe a sandbox's outbound-network posture;
//   - host-pattern compilation and matching (hostmatch plus the
//     higher-level Matcher over NetRule);
//   - the host-side enforcement Proxy used by bwrap and seatbelt for
//     allow-list and proxy modes;
//   - the MITM seam interfaces consumed by the proxy, with the
//     implementation living in the mitm subpackage.
//
// The package is deliberately independent of core/sandbox: sandbox
// imports net, never the reverse.
package net
