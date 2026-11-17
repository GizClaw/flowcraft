// Package config turns a stack of YAML layers into one validated value, and
// describes who supplied what.
//
// It is the mechanism, not the vocabulary: nothing here knows what a runtime, a
// deployment or a graph is. A caller supplies the layer sources and a decoder,
// and gets back the decoded value plus the merged document it came from.
//
// # The three seams
//
// Where a layer's bytes come from is a [Source]: [File], [Bytes], [FS] for any
// io/fs filesystem — an //go:embed tree among them — and [Func] for anything
// else, such as a secret store or a generated document.
//
// How the merged document becomes a value is a [DecodeFunc]. [Strict] is the
// default discipline: JSON semantics, unknown fields rejected, so a typo in a
// permission section is an error instead of a silently ignored key. A caller
// that needs different semantics — defaults, normalization, a second view of
// the same document — passes its own function.
//
// How a caller reads the result is a [Doc]: the merged document, its canonical
// bytes, and the provenance of any value in it.
//
// # What a layer may be
//
// One YAML document per layer: a second `---` is refused, because YAML would
// otherwise stop at the first and drop the rest in silence. A layer that is
// only comments contributes nothing and is skipped; a stack in which every
// layer is like that describes an empty document and is refused.
//
// JSON needs no second parser: it is a YAML subset, so a .json layer merges,
// expands and decodes through the same path. Everything else — TOML, INI, an
// envelope of several documents — is not supported, for the reason below.
//
// # What is deliberately not a seam
//
// The merge rules are fixed: mappings merge key by key, sequences are replaced
// whole, scalars are replaced. A second policy would have to be declared by the
// document and reported by [Doc], or a reader cannot tell whether a list grew
// or was overwritten — the failure mode that made the previous resource-and-DAG
// assembly model unreadable. YAML is likewise the merge substrate: merging is
// defined on a document tree, so a second syntax would need its own tree, not
// another seam here.
package config
