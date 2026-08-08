// Package config is the shared build protocol for FlowCraft: the
// unified factory contract every build step implements, plus the JSON
// wire protocol every factory-owned settings subtree shares.
//
// The protocol is JSON by design. YAML is authoring sugar: documents
// may be written as YAML, but they are converted to JSON at the parsing
// boundary by sdk/config/utils before anything in this package sees
// them. That mirrors Kubernetes, where JSON is the canonical form and
// YAML is converted at the client boundary.
//
// The protocol lives in sdk so module configs can depend on it without
// importing any particular assembly engine (such as sdkx/deploy). An
// assembly engine consumes this protocol: it registers factories,
// resolves dependencies between them, and owns the assembled values'
// lifecycle. This package deliberately contains no engine: no builder,
// no document shape, no lifecycle, and no domain types — every value
// crosses the protocol boundary as `any`.
//
// # Build protocol
//
// One [Factory] contract covers every build step — resources, engine
// kinds, and lifecycle hooks:
//
//   - [Spec] declares the factory's (Kind, Impl) key, its named
//     dependencies, and the item type container factories expose;
//   - [Input] carries the factory-owned settings subtree (raw JSON)
//     plus already-built dependencies and the shared document
//     resolution capability;
//   - [Factory] builds one value from an [Input];
//   - [Catalog] is the unified registry that an assembly engine keeps
//     every factory in, keyed by (Kind, Impl);
//   - [ItemResolver] lets a container factory expose named items so a
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
//   - [DecodeSettings] decodes raw settings into a typed value with
//     unknown fields rejected, so a configuration typo fails the build
//     instead of silently dropping policy;
//   - [Source] is the "content or reference" document type: a plain
//     string or nested object is literal content, while {"file": ...} /
//     {"embed": ...} reference an external file or a build-time
//     embedded asset;
//   - [Loader] resolves a [Source] or [Ref] into bytes at build time,
//     applying path constraints, a size bound, and error classification
//     in one place;
//   - [Ref] is the "location, not content" reference (file / embed)
//     used by fields whose inline form is the enclosing structure;
//   - [Input.Resolve] is how the assembly host injects that resolution
//     capability into every factory ([Input.ResolveSource] surfaces a
//     missing resolver as a validation error; [Input.ResolveDocument]
//     decodes settings as a [Source] and materializes it).
//
// # Generic registries
//
// [Registry] is the generic named-factory registry behind every
// module's internal extension point: workspace drivers, tool middleware
// kinds, sandbox backends, and inference providers all register a name
// and build by name. Extensions own the decoding of their settings; the
// registry owns lookup and validation only.
//
// # Syntax
//
// Parsing a whole document (a deployment file or a module's own
// sub-document) is not this package's job. Use sdk/config/utils.Decode,
// which detects JSON by the Kubernetes rule (first non-whitespace byte
// is an open brace) and converts YAML to JSON before strict decoding.
package config
