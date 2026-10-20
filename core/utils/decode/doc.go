// Package decode is the boundary between authoring formats and the JSON core
// protocol. It detects JSON the way Kubernetes does — the first non-whitespace
// byte is an open brace — and treats everything else as YAML, converting it to
// JSON before strict decoding, so protocol packages never see YAML.
//
// # Roles
//
// It answers one question — what format is this document — and offers one
// operation per format: [ToJSON] for YAML, [Decode] for a typed value read
// strictly out of either. The diagnostics ([FormatOf], [KeyCollisions],
// [UnusableKeys]) exist because a strict decode that rejects an unknown key is
// only half the story: a key that differs from the expected one by case or by
// spelling looks like a missing field, and a YAML key that is not a string
// (`on`, `yes`, `1.5`) silently changes meaning.
//
// # Invariants
//
// Decoding is strict: an unknown field is an error rather than a silently
// dropped value, and exactly one document is accepted — a second document in
// the stream is an error, not the first one winning. Format detection is
// content-first: a name never decides the format.
//
// # Boundary
//
// Schema validation, defaults and bounds belong to the package that owns the
// settings (core/resource); this package only makes the bytes decodable.
package decode
