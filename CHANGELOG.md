# Changelog

All notable changes to this repository are documented here. The active
published module is `core`; releases use the `core/vX.Y.Z` tag prefix.

Pending changesets are aggregated into module release sections by the automated
Release PR before their tags are published.

## Current Published State

| Module | Latest tag | Notes |
| --- | --- | --- |
| `core` | `core/v0.4.7` | Unified platform module: contracts, deploy, runtime, and built-in resources. |

## [Unreleased]

_No pending changes._

<!-- releasegate:releases -->

## `core/v0.4.7` - 2026-09-22

### Changed

- feat(core): add narrow board channel reads for hot script paths — Board.ChannelLen(name) reads the count from the channel header without allocating, Board.LastMessage(name) returns one message as (message.Message, bool) matching PopChannelMessage, Board.ChannelTail(name, count) returns the last count messages in channel order, Board.ChannelView(name) hands out a private slice over the shared messages so Channel projects through it and drops the defensive deep copy it paid before, and Board.AppendChannelMessages(name, msgs) lands a batch under one lock (AppendChannelMessage is now its one-message case) so a concurrent reader sees all of the batch or none of it; "a message on a channel is immutable" is now the Board type's contract rather than a note on ChannelView, so core must never mutate a message the board holds and a middleware that skips a copy is a documented violation instead of a surprise; core/agent/bindings adds board.channelLen(name) (must not allocate), board.lastMessage(name) (null on an empty channel) and board.channelTail(name, count) to the script surface, board.appendChannel(name, msg) accepts a message object or an array of them validated as a whole so a bad entry lands none of it, an empty table is the empty list under Lua (its single table type spells a batch and a message alike), so appendChannel(name, {}) is an empty batch instead of a message without a role and setChannel(name, {}) can clear a channel — without which a Lua script had no way to say "no messages" at all — a rejected append throws under JS while Lua reads the reason from the return value, and board.channel() stays detached from the board; call sites that only measured a channel read it through ChannelLen (core/graph execute.go seed length, validateReads / validateWrites / channelLengths) and core/runtime/session's sessionHistoryPreparer appends through the batch API over ChannelView, dropping a copy it paid before; docs/guides/graph.md and the flowcraft-config skill reference document the new surface, including that the canonical steer node binds host.drainSteer() before appending — a host.* call in argument position expands to both of its return values under Lua ("expected 2 arguments, got 3") — and what each runtime does with a rejected append.

## `core/v0.4.6` - 2026-09-20

### Changed

- feat(core): add an inbound steer channel for running turns — agent.SteerSource (DrainSteer() []message.Message) names the optional pull-based capability next to EventBusProvider and is discovered through CapabilityFromHost / SteerFromHost, so a running turn can be corrected instead of only interrupted; session.Turn.Steer bounds the turn-owned queue (8 pending messages, 32 KiB per message, measured as the length of its JSON encoding) and reports every rejection instead of dropping it — errdefs.Validation for a malformed message, ErrSteerTooLarge / ErrSteerQueueFull as errdefs.BudgetExceeded, agent.Interrupted(...) once an interrupt was requested (it classifies as interrupted and stays recoverable through errors.As on agent.InterruptedError, so a caller routing by errdefs.IsInterrupted must not read that refusal as the barge-in itself), and ErrSteerClosed as errdefs.NotAvailable once the turn is terminal; validation, encoding and the size check run before the lock, which also fixes the payload error as the winner for a message that is both oversized and addressed to a closed turn; Turn.DrainSteer is a non-blocking, concurrency-safe take-all that never returns a message twice and stays callable after the turn is terminal, and Turn.PendingSteer() / result.State["session.pending_steer"] keep undelivered messages observable — the count is turn result state, not an event (the run-end envelope is published before the turn settles, so an event-only consumer never sees it), and it is an int in process but a JSON number after a round trip; queues are turn-scoped and never persisted, so a resumed run starts empty; the session wraps every turn host in a steerHost (outermost, UnwrapHost preserved) so SteerSource is present exactly when the run is steerable, and a HostFactory product that already implements it fails the start with errdefs.Conflict rather than being silently shadowed, which means the engine-visible host is always wrapped and optional capabilities must be read through CapabilityFromHost (SteerFromHost, EventBusFromHost, delegation.ServiceFromHost) instead of asserting the concrete host type; agent.HostCapabilityMask is the new decorator contract that withholds exactly one optional capability from that traversal while the rest of the chain stays readable, and core/delegation applies it on the legacy (manager-less) sync path so a delegated run inherits the caller's host minus its turn-owned steer queue — a delegated document that drains steer now sees errdefs.NotAvailable instead of the parent's corrections, while ServiceProvider and EventBusProvider still traverse and a sub-run started against a bound session.Manager keeps its own queue; core/agent/bindings exposes host.drainSteer() on the standard script surface, returning the board.appendChannel message wire shape, errdefs.NotAvailable when the host cannot accept steer, and answering with whatever arrived since the last drain instead of latching like checkInterrupt; agent/agenttest grows MockHost steer support and an opt-in HostSuite SteerSourceContract case; core never injects text mid-stream (no executor change and no first-class steer node type — delivery happens at a boundary the document placed, leaving each deployment free to decide what the text becomes), and docs/guides/runtime.md, docs/guides/graph.md and docs/guides/delegation.md document the channel, the rejection classes and the inherited-host rules; fix(core/inference): state the cache-inclusive prompt-total contract — Usage.Input / InputTokenUsage now document InputTokens as the inclusive prompt total (every prompt token, cache reads and cache writes included) whose split buckets are disjoint subsets of it, warn hosts not to add cache read / write to InputTokens because that is the arithmetic of a provider's raw, cache-exclusive wire counter and double counts here, and describe the nil ("zero, or not reported") and clamp semantics, so for cached calls Anthropic InputTokens / TotalTokens grow by the cache buckets and TotalTokens - OutputTokens is the prompt size on every provider.

## `core/v0.4.5` - 2026-09-19

### Changed

- fix(core/tool/mcp): close dead sessions before reconnecting and stop pinging modern peers — Source.watch no longer drops a session whose liveness probe failed without closing it, so the transport (and the stdio child it spawned) is released before the reconnect is scheduled instead of leaking one process tree per interval; sessions negotiated at protocol 2026-07-28 or later are watched through the transport connection (ClientSession.Wait) instead of a ping that SEP-2575 removed, legacy sessions keep the periodic ping, a new per-server liveness override (WithServerLiveness in Go, liveness: 30s | off in the tool.Source spec) lets one server disable probing while still reconnecting when its connection closes, and tool or resource calls that fail with ErrConnectionClosed asynchronously tear the dead session down and schedule a reconnect so a transport that never surfaces closure still recovers.

## `core/v0.4.4` - 2026-09-17

### Changed

- fix(core/tool): report per-round visibility from tool_search — Definitions and Discover now share one visibleSet helper, so Discover reports Exposed only for the names the next Definitions call really sends and answers visible_budget for a pooled name the per-round budget drops (unknown or undiscoverable names keep their own reason), the byte walk skips an entry that does not fit instead of stopping so one oversized definition no longer starves the candidates behind it while the highest-priority entry is still kept, a discovered entry carries its rank inside the discovering batch and the per-round cut prefers the better-ranked hit within one round (pool eviction keeps its LRU/seq semantics, and entries discovered by separate calls still fall back to MRU order), and unset discovery.max_tools/max_bytes follow the effective budget.max_definitions/budget.max_bytes so raising the visible budget raises the pool with it; fix(core/inference): skip output-incompatible targets in generate fallback — policyRoute.NextGenerate walks the effective fallback order through the shared remainingTargets and skips candidates whose declared outputs cannot serve the request exactly as selection already does, so a preflight rejection no longer hands the request to a target that provably cannot answer it and an exhausted walk surfaces the failed attempt's error instead of a capability-mismatched target's (undeclared outputs stay compatible, InspectModel errors propagate), with a dismissed candidate recorded as a preflight/fallback skipped attempt carrying skip_reason output_capability and reported as a telemetry span attribute while selection-time skips stay silent and embed/transcription fallback stays order-based; refactor(core,driver)!: drop the image aspect-ratio intent field — ImageIntent loses aspect_ratio, its size/ratio mutual exclusion and the FieldGenerateIntentImageAspectRatio ledger field so size is the only canonical shape control, driver/openai and driver/bytedance drop their rejection branches, driver/minimax moves its shape-only mode to the new ImageOptions.AspectRatio extension registered as image_options and rejects it when a canonical size is set just like bytedance's size_token, and a deployment that still sets intent.image.aspect_ratio fails to decode with an unknown-field error, so name an explicit size or use the extension (breaking for Go callers that read or set the field and for anything keyed on the ledger field id)

## `core/v0.4.3` - 2026-09-17

### Changed

- feat(core/message/media): publish xhigh and max as canonical image quality tiers — media.ImageQuality gains the two tiers the newest GPT image models accept (xhigh above high, max on top) and Validate accepts them, so a generate request can name them and reach the provider that serves the model; providers with no such tier keep reporting the field per request rather than failing core validation, with bytedance and minimax still dropping any quality value with a reason

## `core/v0.4.2` - 2026-09-17

### Changed

- fix(core/sandbox): unwrap cmd and PowerShell script wrappers — NormaliseExec replaces the POSIX-only unwrap with a shell-profile table (bare /c carrying exactly one plain-word script, and the exact -NoProfile -Command pair), so Windows shell-syntax commands normalise to the underlying program for allowlist matching and read-only auto-approval on every platform while pipes, redirections, quoting, abbreviated parameters, -EncodedCommand and profile-loading wrappers stay raw and approval-bound, pinned by accept/reject matrices and differential witnesses that run the claimed tokens through a real sh and a real cmd; fix(core): bump google.golang.org/grpc to v1.83.2 past GO-2026-6348 (heap memory exhaustion via HTTP/2 DATA frame fragmentation) and its GO-2026-6443 successor, raising workspace floors for go.opentelemetry.io/otel v1.44.0, golang.org/x/net v0.58.0, x/sys v0.47.0, x/sync v0.22.0, x/text v0.41.0 and genproto 20260526163538

## `core/v0.4.1` - 2026-09-15

### Changed

- feat(core)!: expose the script surface as a deployment resource — core/agent/bindings publishes the provider contract (Provider, LateProvider, Binding, Invocation), a single assembly path (Assemble, with duplicate and identifier validation) and Chain for composition, core/graph/nodes/script serves the standard surface as StandardBindings (the node/stream/parallel bridges plus the late runtime bridge) with per-execution state arriving through Invocation so one provider is built once and shared across runs, and the new agent.ScriptBindings resource kind (impl: standard, impl: none, or a host-registered impl) carries bridge policy from the document (tools.allow/allow_all, fs.max_read_bytes/max_write_bytes, shell.allow) validated against the wired capabilities at build time, wired as an optional single script_bindings dep that core/graph/resource hands to every script execution through ExecutionContext and that replaces the standard surface (an unwired engine keeps today's per-node-type fallback); breaking Go API: bindings.EnvBuilder/NewEnvBuilder are removed, BuildEnv returns (env, error), assembly rules live only in Assemble, global names must be identifiers legal in both bundled runtimes and must not be a JS/Lua keyword, and the node span reports script.bindings.count/source with a debug record of the node identity and global names; feat(core)!: honor build.max_iterations 0 as unlimited and bound runs end to end — WithMaxIterations(n) caps at n > 0, lifts the guard at 0 and fails Build below zero while the deployment knob forwards an explicit 0 (an absent key keeps the default 100), agent.WithRunTimeout and agent.Policy.RunTimeout add a run-level wall clock spanning the seeder, every revise attempt, Referees and Committers so the shorter of the run and per-Execute deadlines wins, the resource factory logs every build warning, and the budget error names the nodes routed, the rejected wave's size, the cap and the remediation while the timeout error carries the effective deadline (breaking: 0 now means unlimited rather than "use the default" and negative values are rejected instead of ignored), with docs/guides/graph.md documenting the three budgets and a copyable build-budget sample in the config-skill references and the werewolf scenario; fix(core/sandbox): harden the Windows isolation paths — the AppContainer verifier matches probe results line-by-line against a trimmed line and reports stdout and stderr raw on timeout, exit error and mismatch, three confirmed defects in the raw Win32 bindings are fixed (GetSecurityDescriptorDacl takes 4-byte BOOLs with the descriptor kept alive, the mandatory-label ACL uses a real 256-byte buffer instead of the 8-byte ACL header, and WFP filter conditions hold their masks and FWP_UINT64 weight alive across FwpmFilterAdd0), with a new probe-output test and a nightly, manually dispatchable windows-sandbox-stress lane that runs the isolation tests under -race with GODEBUG=invalidptr=1,clobberfree=1; fix(core/sandbox): make the Windows stress lane pass checkptr — the conpty environment block is built as a []uint16 and passed as &block[0] so the helper decodes a slice instead of walking a raw pointer; docs(guides): keep the script bindings example out of Liquid's way

## `core/v0.4.0` - 2026-09-14

### Changed

- feat(core/inference)!: own the model vocabulary as a leaf package and close the declaration loop — core/inference/model carries identity, descriptor, capabilities, limits, lifecycle, reasoning and operations with deprecated re-exports from core/inference, the CapabilitiesPatch/ReasoningPatch overlay vocabulary is deleted because a declaration is the whole fact, generate requests are checked against the model's declared output budget and input kinds before any provider work (both rejections stay fallback-eligible), and tool results become multimodal (message.Content through tool execution, middleware redaction and result limits, MCP, delegation and the script bridge); feat(core/inference): a compiled attempt becomes a value — Binding opens a model's drivers once, Prepared compiles one request against them, the graph inference node and script bridge reuse an LRU BindingCache, route keeps one attempt engine, and shared helpers (Ledger, ExtensionFor, RejectExtensions, LogProvider, ClassifyStatus, ptr) replace the per-driver copies; fix(core/inference): stamp reasoning traces with a verification scope and replay a stored trace only where the target can verify it, name the failed contract check in Error.Detail, classify 402/403/405, treat an interim streamed image snapshot as a contract violation, and keep result budgets exact with non-text tool results bounded by default

## `core/v0.3.1` - 2026-09-09

### Changed

- feat(core/tool): add a per-session discovery pool with an independent byte/count budget — Session.Select becomes Discover with per-name Exposed/Reason and Evicted outcomes, dynamic.discovery gains max_tools/max_bytes/idle_rounds (defaults 32 tools, 16 KiB, 10 rounds) with idle sweep and least-recently-used overflow eviction (Deferred before Direct on recency ties), per-round dynamic.budget still caps what reaches the model with tool_search (Always) surviving pruning, tool_search drops select for query+limit and reports hits/exposed/failed/evicted instead of skipping unknown or unloadable names, and dynamic assemblies auto-record while RecordCalls and selected_retention/recent_window are deprecated; feat(core/tool): expose telemetry as a configurable assembly middleware — FromSettings appends middleware.Telemetry() after recover when telemetry.enabled is set, and the resource factory settings decode accepts the telemetry entry

## `core/v0.3.0` - 2026-09-08

### Changed

- feat(core/inference): surface generate-stream truncation and finish integrity to callers — streams that end before a terminal finish classify as ProviderTruncated (NotAvailable/503, never auto-retried by core; attempt-void semantics live at the caller), failures carry stable Error.Detail stage labels plus provider request/response ids captured at stream open via the optional ProviderStreamMetadata contract (x-request-id/apim-request-id headers, envelope/message ids), error spans mirror inference.error.detail and llm ids, and adapter-synthesized chat finishes are marked FinishSynthesized end to end on the generate event and response and mirrored as inference.finish.synthesized on success spans, with core rejecting a synthesized flag that carries no finish reason; feat(core/graph,core/agent): make stream failure handling a first-class inference-node policy — MessageStream gains an explicit Drop terminal, the inference node exposes stream_failure_policy {on_error,on_interrupt} with commit_partial (default) or discard actions classified per termination category (errdefs aborted/interrupted markers), recoverable undefined-tool streams discard partial text at the drain site so recovered rounds keep a clean transcript, partial usage reporting stays policy-independent, and stream-delta events are documented as advisory previews with board channels authoritative

## `core/v0.2.8` - 2026-09-07

### Changed

- feat(core/runtime): add WithResolver for custom expansion schemes (closes #513) — the runtime builder can now register custom ${scheme:ref} expansion schemes via WithResolver, passed through unchanged to deploy.WithResolver so per-workspace configuration sources resolve inside resource, agent-engine, and agent-hook settings subtrees before factories decode them; custom schemes shadow same-named built-ins while ${secret:...} always wins, the built-in env/base/home schemes stay available unless overridden, Reload keeps the resolver across generations, and runtime-layer tests pin engine-settings expansion rather than leaving that contract to deploy-layer tests alone

## `core/v0.2.7` - 2026-09-07

### Changed

- feat(core/inference): unify model capability declarations and tighten the reasoning contract — new CapabilitiesPatch/ReasoningPatch give provider specs leaf-level overlay semantics (written leaves replace, unstated leaves inherit, and the legacy `reasoning: "toggle"` string form stays backward compatible and now patches only the kind), ModelCapabilities gains CustomEmbedDimensions as a published per-model capability bit alongside hosted web search, ModelLimits grows WithMaxInputTokens/WithMaxOutputTokens/Values so resolved catalog limits share one type with descriptors, and ReasoningKind semantics are made explicit: a model published as `toggle` must compile reasoning_enabled=false on its provider surface while `always` rejects it, enforced by a shared ReasoningOffProbe conformance helper and conformance tests across every driver; drivers (anthropic, azure, bytedance, deepseek, kimi, minimax, openai, qwen) migrated catalog merges to the shared overlay and limits type and removed driver-private control flags (openai/azure effort_none, per-driver top-level dimensions, deepseek per-model responses), so deployment specs changed accordingly: removed keys are rejected by strict decoding, custom_embed_dimensions lives under capabilities, reasoning off needs no knob, and capability/limits/guard behavior is covered by contract tests

## `core/v0.2.6` - 2026-09-07

### Changed

- feat(core): add runtime external dependency injection — a new external_deps channel lets deploy/runtime accept caller-owned dependency values consumed only as resource/engine/hook dependencies: runtime.external_deps document declarations carry a name and contract, runtime.Builder.WithExternalResource(s), deploy.WithExternalResources, and resource.Graph.AddExternal provide the wiring, and externals are borrowed rather than owned so deploy/runtime never construct, wire, deployment-bind, or close them; Reload never rebuilds or replaces them so every generation receives the same injected object, and they stay hidden from Runtime.Resource and Result.Names; Reload may shrink the declared set but cannot require values that were not injected, the declared contract must match the consuming factory's DepSpec.Type, external item refs are rejected in the v1 deploy schema, and dynamic RegisterAgent engine/hook dependencies may reference externals including after Reload re-binds dynamic agents; feat(core/inference): publish model max output token limits — ModelLimits.MaxOutputTokens declares the maximum tokens a model may emit in a single response alongside the existing max input limit, with JSON round-trip, Clone, positive-value validation, and bridge round-trip coverage, while provider catalogs can now publish provider-documented max-output values on generated model descriptors

## `core/v0.2.5` - 2026-09-05

### Changed

- feat(core): add runtime drain API for offline replacement — Runtime.Drain(ctx) quiesces a running Runtime for offline replacement, serialized with RegisterAgent/UnregisterAgent/Reload/Close; it refuses new session leases and new Starts on already-open leases through session.Manager.Idle/WaitIdle/Drain and ErrManagerDraining, waits for active turns to finish naturally within ctx without interrupting them, stays drained after success or timeout so callers can retry or proceed to Close, and closes the race where a Start that already acquired an epoch could install a new turn after Drain observed the manager idle; runtime and manager drain tests plus the runtime guide cover the flow; fix(core/inference): accept dropped decisions in failed compile reports — CompileReport.ValidateFailure no longer treats a Dropped disposition as a contract violation, so a driver that drops one field with a reason while rejecting another in the same compile surfaces its structured rejection instead of CompilerContractViolation, unreasoned drops in failed reports stay rejected, and the Dropped/ValidateSuccess docs are aligned

## `core/v0.2.4` - 2026-09-02

### Changed

- fix(core): close out core security and performance review findings — LocalWorkspace pins an os.Root handle with kernel-enforced symlink containment and bounded fs reads, one-shot sandbox Exec keeps first-max-bytes truncation under small caps, read-only classifier rejects git -c/--config-env/login-shell combined flags and PATH/GIT_/LD_/DYLD_ env assignments, Linux process-group sampling reads /proc instead of forking ps, run ids and checkpoint suffixes fail closed on entropy failure, fs bridge I/O is capped and script nodes expose FSBridgeOptions, proxy/MITM classifier peeks/upstream CONNECT reads/MITM handshakes and headers are bounded with HTTP/1.1 tunnels served by http.Server and capped leaf-cert cache, template rendering stops at a byte budget, media materialization and tool-result truncation are capped, and golang.org/x/mod is bumped to v0.40.0; feat(core): carry request metadata through inference drivers — GenerateRequest.RequestMetadata enters the compile ledger as generate.request.metadata, graph inference nodes expose request_metadata node config, DeepSeek/OpenAI/Azure forward the opaque map under a configurable arbitrary top-level envelope (typed metadata or passthrough JSON), and Anthropic/MiniMax/Bytedance/Kimi/Qwen report dropped when they cannot forward it

## `core/v0.2.3` - 2026-09-01

### Changed

- feat(core/inference): per-model reasoning effort maps — ModelCapabilities.Reasoning becomes ReasoningCapability{Kind, EffortMap} with the canonical ladder extended to minimal/low/medium/high/xhigh, and each model declares how those levels map to its own wire tokens (built-in OpenAI, Anthropic, Kimi, Qwen, Bytedance, DeepSeek, and MiniMax catalogs migrated); legacy reasoning: "toggle" strings stay backward compatible and re-serialize identically when no effort map is present; DeepSeek v4 flash/pro align with the official low/high/max thinking ladder and spec-declared reasoning models without an effort dial now explicitly enable thinking instead of silently dropping the effort request; breaking Go API: ModelCapabilities.Reasoning assignments must migrate from ReasoningKind to ReasoningCapability

## `core/v0.2.2` - 2026-08-31

### Changed

- feat(core): windows sandbox backend — job-object process-tree lifecycle (kill-on-close, job-wide memory and CPU budgets, completion-port kill classification), ConPTY interactive sessions, opt-in write confinement with restricted low-integrity tokens, windows workspace path adaptation (case-insensitive matching, delete retry, rename guard, advisory permission-bit semantics), and MCP windows stdio transport with ctrl-break graceful shutdown; AppContainer-backed network policy on windows — NetDenyAll and NetAllowList/NetProxy via a capability-less lowbox token, WFP bind-layer (ALE_RESOURCE_ASSIGNMENT) TCP permit/block filters closing UDP/ICMP egress, hard ALE_AUTH_CONNECT / ALE_AUTH_RECV_ACCEPT permits pinning the container to a host-side loopback enforcement proxy (IsLoopback-scoped, deny reasons, host:port allow rules), a SOCKS5 proxy channel with HTTP(S)_PROXY/ALL_PROXY env injection, behavioral WFP fence verification, and a windows concurrency/stress test suite

## `core/v0.2.1` - 2026-08-29

### Changed

- fix(core): roll back the failed turn's partial text from the messages channel on undefined-tool recovery, so the recovered round cannot leave adjacent assistant messages that violate provider reasoning round-trip rules (DeepSeek thinking mode 400 'reasoning_text must be passed back'); adds Board.PopChannelMessage

## `core/v0.2.0` - 2026-08-28

### Changed

- feat(core): pluggable settings reference expansion, declarative secret stores, and a unified reference grammar — resource.Scheme/ReferenceResolver open ${scheme:path[:default]} expansion (env/base/home/secret built in, \${...} escaping, strict errors, did-you-mean hints for dotted scheme typos), the deploy builder centralizes expansion across resources/engines/hooks with an all-open env/home/base default (breaking behavior: every literal ${ must be escaped as \${, a leading ~ expands to the user home directory, and {file:}/{embed:} content is expanded too), secret.Store resources (env/file impls) feed a lazy ${secret:NAME}/${secret:store.NAME} scheme with TTL caching and <secret> redaction, resource.Secret carries literals or refs resolved at request time, driver profile secrets migrate to typed values, and agent board references join the unified grammar (breaking: ${board:user.name} replaces ${board.user.name}); breaking API: Expand/DecodeSettings/DecodeTyped/DecodeConfig/ParseSpec/BindAgent gain context parameters

## `core/v0.1.35` - 2026-08-28

### Changed

- fix(core): keep undefined-tool recovery feedback out of the messages channel — recoverUndefinedTool no longer appends a user-role rejection to the transcript; the feedback text is stored on the board under the optional recover_feedback_key config (defaulting to a reserved per-node __recover_feedback.<node id> var), the recovered inference round consumes it as its current input with the whole channel as context, and a successful round clears the pending marker and deletes the feedback var; recovery-enabled nodes get isolated per-node slots and unrelated nodes never delete another node's pending feedback

## `core/v0.1.34` - 2026-08-27

### Changed

- feat(core): image generation streaming and quality — ImagePartDelta gains Interim progress snapshots that replace rather than accumulate at the same part index, so partial-image previews stream as progress while the terminal result keeps only the final image per index (GeneratedImages stays correct); ImageIntent gains a canonical Quality field (auto|low|medium|high via media.ImageQuality) wired through the generate ledger, giving azure/openai image generation a quality knob and letting bytedance/minimax reject it at compile time

## `core/v0.1.33` - 2026-08-27

### Changed

- fix(core): bump OTLP HTTP exporters past GO-2026-4985 — otlptracehttp/otlpmetrichttp to v1.43.0 and otlploghttp to v0.19.0 (with otel/log and sdk/log at v0.19.0, otlptrace at v1.43.0) so oversized collector response bodies are size-limited instead of exhausting memory, aligning the same dependencies across all drivers, backends, examples, and the flowcraft-config validator

## `core/v0.1.32` - 2026-08-25

### Changed

- fix(core/graph): undefined-tool recovery appends a user-role text feedback message instead of replaying a synthetic assistant tool_call paired with a tool result — no fabricated conversation turn, no provider reasoning round-trip exposure (DeepSeek thinking mode rejects assistant tool-call turns without reasoning_content), and no undefined function reference left in context; recovery markers and the llm->compact->llm loop are unchanged

## `core/v0.1.31` - 2026-08-25

### Changed

- feat(core): per-exec write policy and read-only sandbox mode — sandbox.WritePolicy (WriteWorkspace / WriteReadOnly) on ExecOptions narrows the filesystem boundary per call, WithReadOnlyRoot() / settings.readonly_root provide the runner-level default, seatbelt omits the runner root from the SBPL writable set and bwrap ro-binds rootDir in read-only mode, sandbox/local rejects WriteReadOnly as unavailable, backends advertise the write modes they enforce, and ClassifySafeReadOnly supplies the codex-rs-style read-only command heuristic for caller-owned auto-approval; review hardening closes classifier gaps (date/hostname dropped from the base set, sort -o, rg --search-zip/--hostname-bin, git diff/log/show write variants, and sed address-form writes rejected), pins unknown default write policies so they reach backend validation instead of being silently swallowed, rejects writable_paths resolving to the runner root when readonly_root is set, and unifies polarity naming with extracted per-call write merging covered by direct tests

## `core/v0.1.30` - 2026-08-25

### Changed

- feat(core): complete delegation stream lineage and async stream delivery — the delegate tool schema now steers long-running work toward async and documents sync/async lifecycle semantics; delegation run lineage is stamped end-to-end: the tool executor exposes the active call id on the execution context, agent.Request carries ParentRunID and Attributes, new parent_run_id / tool_call_id event headers with typed helpers are projected by graph and agent envelope mint points from run identity (top-level runs stay header-free), and sync delegations derive caller run id and delegate call id at the boundary so nested delegations compose per hop; async delegations now stream real-time envelopes to the caller's live sinks across the queue: AsyncRequest gains ParentRunID / CallID / Stream fields, an in-process stream escrow (pre-submit ref, WithStreamEscrowTTL backstop, released at terminal completion / submit failure / Close, refreshed on claim) restores the caller's observer-downgraded sinks worker-side, a whitelisted WithStreamTargetResolver fallback re-materializes serializable StreamTargets cross-process, lineage is injected into async request metadata so kanban cards and delegation_status surface it, and kanban NoteRunID correlates subagent run ids onto claimed cards; the runtime supplies both whitelisted stream-target halves via StreamExportRegistry (conversation-scoped live sinks and named-bus forwarders resolving exactly the conversation/bus kinds) plus delegation.WithStreamTargetExporter persisting the first describable target on async submit; review hardening completes lineage on session-level envelopes (logical run end and prompt requested/resolved now stamp parent_run_id / tool_call_id — the engine's run end was previously consumed as an attempt delimiter), A2A engine run/step events stamp lineage with ambient RunInfo injected at the run boundary, and the async escrow lifecycle is hardened with a periodic TTL sweep, post-Submit re-arm that re-stores attachments the sweep already dropped, release on failed Complete, and warnings when an expected stream attachment cannot be materialized

## `core/v0.1.29` - 2026-08-25

### Changed

- fix(core): inherit caller stream sinks into delegated subagent sessions — session turns now stamp their attached stream sinks as an inheritable StreamPolicy on the run context (new session.StreamPolicy/WithStreamPolicy/StreamPolicyFromContext, with SinkSpec documenting that a shared sink must tolerate concurrent OnDelta calls), and delegation attaches the caller's sinks to subagent turns when the policy is inheritable; inherited specs are downgraded to observers with ack-on-delivery and no unacked window because the subagent turn is never exposed to the inherited sink (an unacked authoritative attachment would otherwise be detached with BudgetExceeded once its window fills), while visibility, queue size, and delivery timeout are preserved, nested delegations propagate transitively, non-inheritable policies and async worker runs (whose caller context does not cross the backend queue) are not inherited, and tests cover sync delegation delivery, the non-inheritable skip, observer downgrade field preservation, and a 40-delta authoritative sink staying attached

## `core/v0.1.28` - 2026-08-24

### Changed

- fix(core/runtime/session): preserve optional host capabilities in ephemeral sessions — ephemeralHost now implements agent.HostUnwrapper so agent.CapabilityFromHost traversal reaches the wrapped turn host's optional capabilities (delegation service, event bus) instead of stopping at the opaque wrapper, which previously made delegation.ServiceFromHost fail and delegate/delegation_status report 'host has no delegation service' (and broke nested delegation, since non-persistent subagent turns also run ephemeral); checkpoint suppression is unchanged, and a regression test asserts both delegation-service-style capability resolution and checkpoint suppression through the ephemeral wrapper

## `core/v0.1.27` - 2026-08-24

### Changed

- feat(core/runtime/session): add Manager.DeleteSession for by-key removal of a session's durable state — drop the checkpoint-store record (committed board/history, parked run marker, resumable request) and the parked run's checkpoint for one agent+context key, drain and close any live session with RemoveAgent-style rollback semantics (opens refused while in flight, ctx-bounded drain with full rollback on timeout, idempotent retries), and only delete durable state after the session confirms its active turn stopped so a budgeted close timeout returns a retryable error instead of letting afterTurn resurrect state; feat(core/inference): per-call model hint for router generate selection — GenerateRequest gains an optional model_hint (provider/name or bare name) consumed only by the route selector, the inference graph node gains model_hint config resolving board references per invocation (${board.model}) so a per-conversation choice can ride agent.Request inputs, a hinted target is tried first and falls back to the default chain on failure (execute/preflight/circuit-open, never re-attempting the failed hint), unknown/ambiguous/output-incompatible hints fall back to the default policy, and hint matching is profile-blind; docs updated in guides/graph.md and guides/inference.md

## `core/v0.1.26` - 2026-08-22

### Changed

- feat(core): recover from undefined tool calls instead of failing the turn — a model response that names a tool absent from the exposed definitions now rejects with a distinguishable undefined_tool inference error (deterministic and non-retryable) instead of invalid_provider_response, and the inference graph node gains opt-in undefined_tool_recovery config ({enabled, max_per_run}, default 2) that replays the rejected call as an assistant tool_call paired with a tool result telling the model the tool is not exposed and to use tool_search, sets recover_pending_key/recover_count_key board vars with tool_pending_key false, and returns success so the graph can route the round back to inference; recovery is bounded per graph run, strict deployments keep hard-failing, named/required tool_choice violations are never recovered, and one rejection discards the round's other tool calls; docs and graph-level tests cover the loop-back routing contract

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

