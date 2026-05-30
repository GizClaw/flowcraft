// Package namespace centralises retrieval namespace construction.
//
// Retrieval namespaces are storage identifiers, not user-facing IDs. They must
// stay inside the conservative [A-Za-z0-9_] alphabet accepted by every in-tree
// backend while still encoding higher-level ownership such as recall scopes or
// knowledge datasets.
//
// New writes should use the V2 helpers on Prefix. Legacy V1 recall namespaces
// of the form "<prefix>_<runtime>__u_<user>" are intentionally non-injective
// when the sane'd runtime or user contains the delimiter; use
// Prefix.LegacyUserScopeV1 only from migration tooling.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/namespace instead.
// This package will be removed in v0.5.0.
package namespace
