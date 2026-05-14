// Package tlsconfig builds the *crypto/tls.Config that vesseld's
// TCP listener uses to terminate mutual TLS. It is the bridge
// between the resolver-side DaemonMTLSPlan (URL-keyed reference
// strings) and the api-side listener (raw key material).
//
// # Scope
//
// The package owns three responsibilities:
//
//  1. Resolve the Cert / Key / ClientCA references through a
//     [secrets.Provider]. Every reference goes through the same
//     daemon-wide provider so a vault://, file://, or env://
//     reference is honoured uniformly across the daemon.
//  2. Parse the returned bytes as PEM, build the X.509 server
//     certificate and the client-CA pool, and assemble a
//     [crypto/tls.Config] with ClientAuth = RequireAndVerifyClientCert.
//  3. Map the resolver's "1.2" / "1.3" minVersion string into the
//     stdlib's tls.VersionTLS{12,13} constants, defaulting to TLS
//     1.3 when unspecified.
//
// # Why a separate package
//
// The crypto-material handling is mechanical but easy to get
// subtly wrong (forgetting RequireAndVerifyClientCert is the
// canonical "we thought we had mTLS, we didn't" bug). Carving it
// out of cmd/vesseld/runtime keeps that codepath single-purpose
// and unit-testable without spinning up a full daemon.
//
// # Non-scope
//
// The package does NOT itself listen, accept connections, or
// renegotiate certificates. It also does NOT watch the underlying
// secret references for rotation — Build is a one-shot snapshot
// taken at startup, mirroring how the token-file path reads its
// content once. Live rotation is a future RFC.
package tlsconfig
