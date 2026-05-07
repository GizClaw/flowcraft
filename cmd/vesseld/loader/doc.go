// Package loader walks a list of --config inputs (each a file or
// directory) and turns them into the apispec.Object slice the
// resolver consumes. The loader handles three concerns:
//
//  1. Filesystem walking with optional recursion.
//  2. Multi-document YAML / JSON parsing via the apispec router.
//  3. Per-document source-location annotation so error messages
//     can point at "<file>:doc[<idx>]".
//
// The loader does NOT do cross-document validation (duplicate
// names across files, missing references) — that is the resolver's
// job. Keeping the loader narrow means swapping it out (e.g. for
// a future ConfigMap watcher) only requires preserving the input
// → []apispec.Object contract.
package loader
