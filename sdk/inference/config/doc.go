// Package config loads versioned inference deployment configuration and
// assembles inference.Runtime instances from it.
//
// The document is JSON at the protocol level; YAML is accepted as
// authoring sugar through sdk/config/utils, which detects JSON by the
// Kubernetes rule (first non-whitespace byte is an open brace) and
// converts YAML before strict decoding. Provider specs stay as opaque
// JSON subtrees and are decoded by the owning provider factory through
// [DecodeSpec].
//
// The package enforces a SecretRef-only boundary: documents never
// contain plaintext credentials, and every credential is resolved
// through an explicit SecretResolver catalog at build time. Provider
// packages own their Spec decoding and model rules through the Factory
// contract; this package owns envelope validation, secret resolution,
// and assembly.
package config
