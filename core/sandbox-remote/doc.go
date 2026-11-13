// Package remote runs a sandbox.Runner across a process or host boundary: the
// client is a [sandbox.Runner] whose processes execute on the server, and the
// server is a thin adapter over any other runner.
//
// # The shape
//
// One server exposes one runner. It is not a scheduler and holds no fleet: the
// runner is built by whoever starts the server, and the server's job is to carry
// sessions over the wire and keep the handles it needs to answer by id.
//
//	inner := local.New(root)                    // or bwrap, seatbelt, windows
//	server, err := remote.NewServer(inner, remote.WithServerToken(token))
//	http.ListenAndServe(addr, server)
//
//	runner, err := remote.New(ctx, "https://sandbox.internal:8443",
//		remote.WithToken(token))
//	composed := engine.ComposeLocal(runner, policy) // decorators unchanged
//
// The client side is a proxy, so nothing above it changes: the tool set, the
// approval chain and the escalation path all see the same interface they see for
// a local backend. Policy stays on this side of the wire; the server re-validates
// what arrives and can only narrow it, because the caller is not the authority on
// what the far end can enforce.
//
// # What crosses the boundary, and what does not
//
// Session ids are opaque and minted by the server. Nothing here parses one, and
// the width is the server's business — which is what leaves room for an instance
// layer (one sandbox per session) without changing the paths later.
//
// Output is a cursor, never a stream: a read carries after_seq and answers with a
// sequence range. That is what makes a dropped connection cheap — a reconnecting
// client re-reads from its last cursor instead of losing what it missed — and it
// is why the transport can stay request/response.
//
// Files do not cross. The built-in file tools work on a local fsx tree, so a
// deployment that puts commands on a remote host needs that host's filesystem to
// be mounted where the tools run, or those tools turned off. This package moves
// the process boundary only.
//
// # Capabilities are narrowed, never invented
//
// The client reports the server's declaration verbatim with one exception:
// [sandbox.SessionFeatures].Events is cleared, because the push stream is not
// proxied yet. A caller reads Capabilities and knows not to call Watch; the
// alternative — advertising the feature and failing on use — is exactly what
// Capabilities exists to prevent.
//
// # Security
//
// The server refuses to be built without an authentication decision: either a
// bearer token or an explicit [WithoutAuth] for a listener that is already
// behind an authenticator. The token travels only in the Authorization header,
// never in a query string, because query strings end up in proxy logs. Transport
// security is the deployment's job (TLS or a unix socket); the client accepts a
// TLS configuration through [WithHTTPOptions].
package remote
