// Package catalog is the named factory registry for vesseld. It is
// the single seam between the declarative ref strings users write
// in YAML ("engine: { ref: graph-llm }", "provider: openai") and
// the live Go constructors that materialise those refs into
// runtime objects.
//
// Why a separate package: keeping the registry abstraction stable
// from v0.1.0 is what allows future plugin work (Go plugin loader
// in v0.2.0, out-of-process gRPC plugins later) to land without
// breaking already-deployed vesseld configurations. Every plugin
// path can implement against catalog.Catalog regardless of how it
// gets the factory functions into the daemon.
//
// What lives here:
//
//   - The Catalog struct + per-category Register* methods (one per
//     factory category: engine, probe, tool-pack, llm-provider,
//     history-store).
//   - The Builtin() function returning a Catalog pre-populated
//     with v0.1.0 in-tree implementations.
//   - The DepBundle struct that callers (resolver, fleet) supply
//     to factories so providers can pull resolved secrets / shared
//     LLM clients without coupling to package-level globals.
//
// Categories are deliberately closed: adding a new category
// requires a new Register method and the resolver knowing about
// it. Factories within a category are open: any string-ref
// registration is fine.
package catalog
