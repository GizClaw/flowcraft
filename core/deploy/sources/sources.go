// Package sources is the layer sources core ships, and the factories for the ones
// that need a value only the host has.
//
// It is opt-in on purpose, and it mirrors core/runtime/backends. core/deploy
// registers none of these, so a host that reads its layers from disk links only
// [File]; deploy/builtin is the host that wants the standard set in one line.
//
// # What can be a registration
//
// The split is whether the document can say everything. [File] and [Inline] are
// registrations because the document carries the whole address — a path, or the
// text itself. [FS] and [Func] are factories because their address is a Go value
// (an fs.FS, a loader) that no document can write, so the host names the kind and
// hands it over.
//
// Everything else a host might want — HTTP, a vault, a row in a table, a document
// generated at startup — is [Func] with five lines of the host's own policy. That
// is deliberate: TLS, retries, size limits and credentials are decisions a
// platform should not make on a deployment's behalf.
//
// # How a source names its layer
//
// A factory names its layers "<kind>:<reference>" — "assets:runtime/base.yaml",
// "vault:openai/luna" — so an error or a provenance entry says which store the
// layer came from. [File] is the exception: its reference is already a location,
// and the path is what a reader needs to see.
package sources
