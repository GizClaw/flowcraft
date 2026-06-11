// Package namespace centralises retrieval namespace construction.
//
// Retrieval namespaces are storage identifiers, not user-facing IDs. They must
// stay inside the conservative [A-Za-z0-9_] alphabet accepted by every in-tree
// backend while still encoding higher-level ownership such as user scopes or
// corpus datasets.
//
// New writes should use the V2 helpers on Prefix. Legacy V1 user namespaces of
// the form "<prefix>_<runtime>__u_<user>" are intentionally non-injective when
// the sane'd runtime or user contains the delimiter; use
// Prefix.LegacyUserScopeV1 only from migration tooling.
package namespace
