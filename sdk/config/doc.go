// Package config is the shared configuration protocol for FlowCraft
// modules: the contracts a module config package implements so its
// built values can be bound by a deployment document, plus the JSON
// wire protocol every factory-owned settings subtree shares.
//
// The protocol is JSON by design. YAML is authoring sugar: documents
// may be written as YAML, but they are converted to JSON at the parsing
// boundary by sdk/config/utils before anything in this package sees
// them. That mirrors Kubernetes, where JSON is the canonical form and
// YAML is converted at the client boundary.
//
// The protocol lives in sdk so module configs can depend on it without
// importing any particular assembly engine (such as sdkx/deploy). A
// deployment engine consumes this protocol: it registers factories,
// resolves dependencies between them, and owns the assembled values'
// lifecycle. This package deliberately contains no engine: no builder,
// no document shape, no lifecycle.
//
// # Two halves
//
// The resource half declares how one shared, named object is built:
//
//   - [ResourceFactory] builds a value from its [ResourceSpec] and a
//     [Input] of already-built dependencies;
//   - [ItemResolver] lets a container resource expose named items so a
//     dep may address one item inside it ("resource/item");
//   - [SourceFunc] resolves a dep reference to a value the HOST owns,
//     which the document borrows rather than constructs.
//
// The settings half is the JSON wire protocol shared by every factory
// settings subtree:
//
//   - [Opaque] captures a subtree verbatim without decoding it, so a
//     deployment envelope can stay strictly checked while factory-owned
//     payloads stay opaque;
//   - [DecodeSettings] decodes an opaque subtree into a typed value with
//     unknown fields rejected, so a configuration typo fails the build
//     instead of silently dropping policy;
//   - [SubDocument] is the common "file or inline" envelope used by
//     resource impls that wrap a module's own document loader.
//
// The hook factories ([PreparerFactory], [ObserverFactory],
// [RefereeFactory], [CommitterFactory]) complete the protocol: they let
// a deployment document attach lifecycle behavior that reaches into the
// same dependency area as resources.
//
// # Catalogs
//
// [Input] is the universal factory input (opaque settings plus
// dependencies), and [Catalog] is the generic named-factory registry
// behind every module's extension point: workspace drivers, tool
// middleware kinds, sandbox backends, and inference providers all
// register a name and build by name. Extensions own the decoding of
// their settings; the catalog owns lookup and validation only.
//
// # Syntax
//
// Parsing a whole document (a deployment file or a module's own
// sub-document) is not this package's job. Use sdk/config/utils.Decode,
// which detects JSON by the Kubernetes rule (first non-whitespace byte
// is an open brace) and converts YAML to JSON before strict decoding.
package config
