// Package inference is the inference resource. Files are grouped by
// responsibility:
//
//   - Shared contract: model.go, usage.go, decision.go, errors.go,
//     extension.go, internal_helpers.go
//   - Provider SPI: provider.go (compiler/binding pipeline),
//     provider_definition.go (ProviderDefinition / Openers)
//   - Generate domain: generate.go, generate_stream.go,
//     generate_input.go, generate_intent.go, generate_output.go,
//     generate_driver.go
//   - Embed domain: embedding.go
//   - Transcribe domain: transcribe.go (unary whole-file recognition and
//     the duplex TranscriptionSession), transcribe_stream.go (live
//     message.Stream input pumped into a session)
//   - Resource layer: assembly.go (the inference.Assembly resource
//     with execution), route/ (the inference.Router decorator:
//     tiers, selectors, retry/backoff, circuit breaker, trace)
//
// Realtime — the fourth workload — is reserved in the operation enum and
// field ledger but has no request/session surface yet; it lands in a later
// milestone.
package inference
