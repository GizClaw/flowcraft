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
//   - Resource layer: assembly.go (the inference.Assembly resource
//     with execution), route/ (the inference.Router decorator:
//     tiers, selectors, retry/backoff, circuit breaker, trace)
//
// The execution runtime (Runtime over a provider registry) and the
// Router resource are deferred; they land here in later milestones.
package inference
