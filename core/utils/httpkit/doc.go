// Package httpkit builds HTTP clients and transports for provider and host
// traffic: a connection-pooling RoundTripper, an optional bounded retry for
// transient failures, and TLS/HTTP-2 hardening behind options. It issues no
// requests itself and knows nothing about any provider's wire format.
//
// # Roles
//
// [Config] is the whole surface — protocol family, timeouts, pool size, TLS,
// retry policy — built through [DefaultConfig] plus [Option] values
// ([WithTimeout], [WithTLSClientConfig], [WithRetry], [WithConnectionPool], …).
// [NewRoundTripper] builds the transport, [NewHTTPClient] wraps it in a client.
// [RetryCountOf] reads back how many attempts a response took, so a caller can
// report it without depending on the retry implementation.
//
// # Invariants
//
// A retry is bounded and only ever applies to failures the policy considers
// transient; the response that finally surfaces carries its attempt count.
// A protocol value outside [ProtocolHTTP1]/[ProtocolHTTP2] is a programmer
// error and panics rather than silently building a default transport. The
// client's Transport is owned by the client: close idle connections through
// it when the host is done.
//
// # Boundary
//
// Credentials, endpoints and error taxonomies belong to each driver; network
// policy (which host may be reached at all) belongs to core/utils/netpolicy.
// This package only decides how the bytes travel.
package httpkit
