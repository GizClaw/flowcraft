# Changelog

All notable changes to this repository are documented here. The active
published module is `core`; releases use the `core/vX.Y.Z` tag prefix.

Pending changesets are aggregated into module release sections by the automated
Release PR before their tags are published.

## Current Published State

| Module | Latest tag | Notes |
| --- | --- | --- |
| `core` | `core/v0.1.20` | Unified platform module: contracts, deploy, runtime, and built-in resources. |

## [Unreleased]

_No pending changes._

<!-- releasegate:releases -->

## `core/v0.1.20` - 2026-08-20

### Changed

- fix(core): skip session drain wait without sinks and surface missing run-end — the stream coordinator no longer waits the drain timeout when a turn has no sinks, and a timed-out drain records session.finalize_error plus a telemetry warning instead of silently swallowing the missing run-end; feat(core): record inference request ids and token usage in OTel telemetry — per-call Assembly instrumentation emitting executions.total, errors.total, duration.seconds, tokens.input, tokens.output, and tokens.input.cached metrics plus llm.request.id / llm.response.id, llm.tokens.*, llm.latency.ms, llm.cost.micros, inference.error_kind, inference.embed.items, and inference.audio.duration_ms span attributes, with usage Model/LatencyMs stamped at the Assembly boundary and the graph inference node mirroring ids and usage onto its node span on success

## `core/v0.1.19` - 2026-08-19

### Changed

- feat(core): custom graph node types as deployment resources — new graph.NodeType resource kind with a script impl (declarative type/source/reads/writes and the full script bridge surface via the shared RunScript helper), the agent.Engine/graph factory mounts custom types through an optional node_type Many dep and registers them before Build with typed-nil and duplicate-mount guards, NodeTypeRegistrar/ConfigFileRefFields contracts let host/plugin Go node types opt into registration and config file-ref materialization, and Role gains JSON tags for declarative I/O roles

## `core/v0.1.18` - 2026-08-19

### Changed

- feat(core): add transcription session finisher — transcription sessions expose the optional TranscriptionSessionFinisher capability: FinishInput signals end-of-input so continuous sessions reach io.EOF after multiple final events, decoded sessions pass it through as a no-op when the provider lacks the capability, TranscribeStream performs the handshake automatically after the source stream ends, and the script bridge inference.transcribeSession handle gains finish() with a Validation error when the provider lacks the capability

## `core/v0.1.17` - 2026-08-19

### Changed

- feat(core): add first-class transcription with live part-stream input — unary Transcribe and duplex TranscribeSession join Generate/Embed as core/inference workloads with ModelRef addressing, route pools, retries, circuit breakers, session telemetry, provider SPI via Openers.Transcribe, agent bindings (transcribe/explainTranscribe/transcribeSession), and live message.Stream[Part] input through FeedTranscription/TranscribeStream, backed by the live media-stream transport (media.Stream/media.Pipe, stream-backed audio/video sources, MaterializeContent conversion) with stream subscription projection; feat(core): first-class finish and provider_outputs script emit types decode into typed StreamDeltaFinish/StreamDeltaProviderOutputs envelopes, with invalid payloads skipping emission; fix(core): keep raw payloads on script bridge host.emit deltas (StreamDeltaPayload.payload) instead of dropping them, and skip invalid tool_call/tool_result/part payloads rather than publishing empty deltas; chore(core): lower module Go version to 1.25 to match the dependency floor

## `core/v0.1.16` - 2026-08-18

### Changed

- feat(core): support env scalars in settings decode — add resource.Int/Bool/Float64/Int64 scalar settings types that accept JSON literals or strings, so ${env:NAME} expansion can feed numeric and boolean fields; runtime session config, MCP tool server specs, workspace scoped settings, and A2A history_length now decode through the env-aware settings path with strict unknown-field rejection and clear validation errors for unparseable strings; also harden env restore error checks in runtime config tests

## `core/v0.1.15` - 2026-08-18

### Changed

- feat(core): lift board ref syntax into agent.Board — ${board.<path>} dot-path resolution through nested string-keyed maps and exported struct fields, typed JSON-literal defaults (${board.x:default}), fail-fast validation for missing references with \${board.x} escaping, and json.Decoder.UseNumber config decoding to preserve integer precision; agent.Board gains Resolve/ResolveString, ContainsBoardRef, ExtractBoardRefs, and BoardRefMarker, the board bridge exposes board.resolve/board.resolveString, and graph.ContainsRef/graph.ExtractRefs become deprecated wrappers

## `core/v0.1.14` - 2026-08-18

### Changed

- feat(core): add runtime generation reload with multi-bus ownership and epoch-pinned sessions (Runtime.Reload, per-generation host factories/catalogs, dynamic agent drain/rebind); expose built deployment resources via Runtime.Resource; move delegation host wrapping out of runtime to an opt-in deployment-aware host factory decorator (WithResultHostFactory + core/delegation/hostwrap), so delegation services are no longer auto-exposed on turn hosts

## `core/v0.1.13` - 2026-08-17

### Changed

- feat(core): expose inference explain, embed, and model-catalog entry points in the script bridge — inference.models/inspect, the embed family (embed/routeEmbed/explainEmbed/routeExplainEmbed), stream preflight (explainStream/routeExplainStream with a public Router.ExplainGenerateStream), with route-level explain responses carrying the selected model's declared limits

## `core/v0.1.12` - 2026-08-17

### Changed

- feat(core): declare per-model max input token limits through ModelDescriptor.ModelLimits and fill driver catalogs from official documentation; fix(core): make sandbox watcher close race-safe

## `core/v0.1.11` - 2026-08-15

### Changed

- feat(core): dynamic agent registry for runtime agent register/unregister with lifecycle-aware cleanup; feat(core): expose response_format on graph inference node config

## `core/v0.1.10` - 2026-08-15

### Changed

- feat(core): provider-carried extension decoders for graph inference node extensions — move the extension wire types into core/inference, carry decoders on ProviderDefinition, and aggregate them per configured provider so graph yaml extensions work without host-side registration

## `core/v0.1.9` - 2026-08-15

### Changed

- fix(core): make MCP Source.AddServer non-blocking so hung servers cannot stall host startup, with WithRequired and WaitReady for hosts that need readiness

## `core/v0.1.8` - 2026-08-15

### Changed

- feat(core): runtime tool publication with background MCP reconnect; fix(core): make sandbox.Runner lifecycle part of the contract; fix(core): bound SessionRegistry.Close and wait out start races; fix(core): tighten MCP session lifecycle and tool projection

## `core/v0.1.7` - 2026-08-14

### Changed

- fix(core): run Agent.Prepare hooks in Execute

## `core/v0.1.6` - 2026-08-14

### Changed

- fix(core): fresh run ids per turn, session-scoped committed history, explicit resume, and immediate Turn.Cancel

## `core/v0.1.5` - 2026-08-13

### Changed

- feat(core): runtime prompt event subscription, prompt lifecycle resolved events, and sandbox allowlist approval composition

## `core/v0.1.4` - 2026-08-13

### Changed

- Treat already-closed sandbox stdin as a no-op close to fix the release-gate exec race

## `core/v0.1.3` - 2026-08-13

### Changed

- Add stream terminal signals and request-id telemetry, stamp usage model/latency, and enrich driver usage and audio support

## `core/v0.1.2` - 2026-08-13

### Changed

- Add inference and agent conformance suites and expand workspace settings references

## `core/v0.1.1` - 2026-08-13

### Changed

- Fold sandbox backends into core/sandbox, split local runner into core/sandbox/local, and move net policy/proxy/mitm into core/utils/net

## `core/v0.1.0` - 2026-08-13

### Changed

- Introduce core as the single platform module by folding sdk and sdkx contracts, deploy/runtime assembly, built-in resources, and tooltest support into core

