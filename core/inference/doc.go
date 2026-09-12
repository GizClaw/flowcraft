// Package inference is the inference resource. Files are grouped by
// responsibility:
//
//   - Model vocabulary: model_alias.go re-exports core/inference/model
//     (identity, descriptor, capabilities, limits, lifecycle, reasoning,
//     the catalog patch language) so existing callers keep compiling.
//     New code imports core/inference/model directly.
//   - Shared contract: usage.go, decision.go, errors.go, extension.go,
//     extension_registry.go, extension_dispatch.go, metadata.go,
//     internal_helpers.go
//   - Attempt compilation: ledger.go (the shared compile ledger),
//     generate_ledger.go (the generate part-to-field tables), provider.go
//     (the compiler/binding pipeline),
//     provider_definition.go (ProviderDefinition / Openers),
//     generate_driver.go, prepared.go (a compiled attempt), binding.go
//     (opened drivers), binding_cache.go (the host-side reuse cache),
//     declaration.go (declared-limit preflight), provider_log.go
//   - Generate domain: generate_request.go, generate_input.go,
//     generate_intent.go, generate_output.go, generate_response.go,
//     generate_stream.go, generate_stream_accumulator.go,
//     generate_stream_delta.go
//   - Embed domain: embedding.go
//   - Transcribe domain: transcribe_request.go, transcribe_driver.go
//     (unary whole-file recognition and the duplex TranscriptionSession),
//     transcribe_session.go, transcribe_stream.go (live message.Stream
//     input pumped into a session)
//   - Resource layer: resource.go (the resource factory), assembly.go
//     (the inference.Assembly resource with execution), telemetry.go,
//     route/ (the inference.Router decorator: tiers, selectors,
//     retry/backoff, circuit breaker, trace)
//
// The operation axis is declared once, by model.Operations(); a workload
// without a request/session surface is not enumerated, so it cannot be
// declared, routed, or advertised. Realtime remains unimplemented and is
// therefore absent from the vocabulary until its surface lands.
package inference
