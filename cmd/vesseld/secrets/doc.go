// Package secrets defines the SecretProvider abstraction vesseld uses
// to resolve opaque reference strings (TLS material, LLM API keys,
// SecretBoxes, etc.) into the plaintext bytes the daemon hands to
// its runtime components.
//
// # Scope
//
// This package owns the daemon-side bootstrap concern of "where does
// a credential live, and how do I read it without coupling to the
// storage backend". It is intentionally narrow:
//
//   - vesseld bootstraps a [Provider] (typically [Default]) at start
//     and threads it into mTLS setup, LLMProfile credential
//     resolution, and the upcoming kind: Secret apispec resolver.
//   - All references are opaque URL-shaped strings — the scheme picks
//     the backend, the rest is backend-specific. Callers do not parse
//     references themselves; they hand them to a Provider and trust
//     the registered backend to interpret them.
//   - Backends are simple [Provider] implementations registered into
//     a [Multi] router. Adding a new source (AWS Secrets Manager,
//     Kubernetes secrets, file with envelope decryption, …) is a
//     matter of registering one more Provider.
//
// # Reference syntax
//
//	env://NAME           — process environment variable NAME
//	file:///abs/path     — absolute file path; contents are returned
//	                       verbatim sans the trailing newline
//	vault://server/path?key=k
//	                     — placeholder for the HashiCorp Vault
//	                       backend; currently returns NotAvailable
//
// Reference strings live in operator-authored YAML / CLI flags. The
// strict URL parsing rejects ambiguous or malformed inputs early so
// a typo surfaces at startup rather than at first credential use.
//
// # Why a new package instead of extending resolver/secret.go
//
// cmd/vesseld/resolver already resolves apispec.ValueRef (Env / File
// / SecretRef forms) into plaintext for LLMProfile credentials. That
// path is BC-pinned to v1alpha1 and consumes a different syntactic
// shape (typed fields, not URL scheme). The two coexist deliberately:
//
//   - resolver/secret.go: legacy ValueRef contract — feeds the
//     apispec → catalog wiring chain.
//   - secrets.Provider:   forward-looking URL-keyed provider — fed
//     to mTLS, the upcoming kind: Secret resolver, and any future
//     subsystem that wants a single Get(ref) call without per-source
//     branching.
//
// resolver/secret.go MAY in a future PR delegate Env / File handling
// to a secrets.Provider so duplication shrinks, but that consolidation
// is out of scope here — this PR ships the new surface only.
//
// # Security posture
//
// The default [FileProvider] enforces "owner-readable only" file
// permissions (0o600 / 0o400) and refuses world / group readability
// to catch the common "secret committed to a repo with mode 0644"
// mistake at boot. Callers that need to opt out (test harnesses,
// shared sealed volumes) can construct a FileProvider with
// EnforceMode = false; the daemon never disables enforcement
// automatically.
//
// The Multi router treats an unknown scheme as
// errdefs.Validation — operators see a clear error at start instead
// of a mysterious "credential not found" at first request. Network-
// touching backends (vault://) are reachable only after they are
// explicitly registered; the stub VaultProvider in this package
// returns errdefs.NotAvailable so callers can wire vault:// refs
// today and pick up real decoding when the implementation lands.
package secrets
