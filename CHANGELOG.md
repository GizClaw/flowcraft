# Changelog

All notable changes to this repository are documented here. The active
published module is `core`; releases use the `core/vX.Y.Z` tag prefix.

Pending changesets are aggregated into module release sections by the automated
Release PR before their tags are published.

## Current Published State

| Module | Latest tag | Notes |
| --- | --- | --- |
| `core` | `core/v0.1.25` | Unified platform module: contracts, deploy, runtime, and built-in resources. |

## [Unreleased]

_No pending changes._

<!-- releasegate:releases -->

## `core/v0.1.25` - 2026-08-22

### Changed

- feat(core/delegation): resume parked delegated runs on identical retry — persistent-session delegations now check Session.ParkedRequest and replay the parked run from its checkpoint under the original run id when the retried request matches (same message and metadata inputs), with any resume or probe failure (missing checkpoint, unreadable parked state, transient store errors, non-resumable engine) degrading to a fresh start with a warning instead of failing the retry, and a successful run clearing the parked marker; fix(core/delegation): delegation_targets accepts empty-object and null arguments as a no-argument call — the parameterless tool now admits "", whitespace, {}, and JSON null, rejects non-empty objects, arrays, and scalars as no-arguments-expected, and reports malformed or trailing JSON as a parse error

## `core/v0.1.24` - 2026-08-21

### Changed

- feat(core): expose runtime-registered agents as delegation targets — the delegation LocalDirectory merges the session manager's live dynamic view through a set-once TargetSource (dynamic-first resolve, deduped sorted List, freeze/unfreeze pinning), AgentRegistry implements the target source, and Build/Reload bind it to every delegation directory so RegisterAgent targets appear immediately, unregister yields TargetNotFound for new delegates while in-flight runs drain, and reload rollback unfreezes the still-current directory; fix(core): harden teardown and error surfacing — kanban card panics become returned errors, complete retries are interruptible, session close bounds turn drain, sandbox close bounds the wait after SIGKILL, bwrap bridge helpers thread stderr, CA bundle write and proxy teardown errors surface, nil-board seed failure is logged, and canceled status spelling is unified; feat(core): add telemetry log records across runtime paths — agent run lifecycle, revise attempts and close failures, runtime reload, agent lifecycle and session stream events, graph run publish, node retries and script stream events, inference route retries, fallbacks and circuit skips, delegation worker failures and kanban lifecycle, tool panics, lazy loads and MCP teardown, sandbox enforcement kills and session teardown, event subscription close and unobserved drops, deploy partial build cleanup, and resource loader base-dir close; refactor(core): deallocate memory bus drop summary and derive graph span/log scope attrs from one source

## `core/v0.1.23` - 2026-08-21

### Changed

- feat(core): wire delegation subagents through session lifecycle — the delegation service and tool source resolve a per-generation delegation.Directory dependency (new directory and session-provider resource factories), the directory binds the assembled deployment during the deploy wire phase, and the runtime hands its single session manager to the service via session.ManagerBinder (set-once, before the generation swap); delegated runs execute through the session lifecycle with a minted ContextID, refused ask-user, ephemeral sessions for non-persistent identities (no state, checkpoint, or resume), and caller/depth/timeout metadata crossing the session boundary, while the legacy bare-run path is preserved when no manager is bound; feat(core): emit delegation run telemetry and reuse resolved target — runAt logs the resolved subagent target through OpenTelemetry (agent.id, delegation.target/mode/depth/caller attributes) and passes the directory-resolved instance into the legacy path, removing a duplicate lookup

## `core/v0.1.22` - 2026-08-20

### Changed

- feat(core): declare model content capabilities and reasoning kind — ModelCapabilities gains explicit input/output content kinds (message.PartKind), a ReasoningKind control capability, and fluent With* composition, with PartKind validation and Intent.OutputKinds centralizing intent-to-content mapping while undeclared providers keep order-based routing; feat(core): route generate by declared output capabilities — the unified generate selector skips targets whose declared outputs cannot serve the request intent, and repeated model references across tiers collapse at build time so fallback never reattempts a failed target; feat(core): expose canonical intent envelope on graph inference node — the node accepts the canonical inference.Intent wire form (text/image/audio/video) through an intent config field, removing the TextIntent sugar knobs in favor of intent.text.* (BREAKING CHANGE: legacy text knobs are rejected at build time)

## `core/v0.1.21` - 2026-08-20

### Changed

- fix(core): allow array-rooted structured generate responses — json_schema output validation no longer forces the top-level JSON value to be an object; schemas rooted at other shapes such as arrays of objects now validate output against the schema directly, while json_object continues to require a top-level object

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

