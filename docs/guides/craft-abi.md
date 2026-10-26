---
layout: default
title: Craft ABI
---
# Craft ABI (`flowcraft.craft/v1`)

**Status: normative.** This page and the proto it describes are the contract
between the platform and any craft (a sidecar that runs an agent loop).

- Source of truth: [`proto/flowcraft/craft/v1/craft.proto`](../../proto/flowcraft/craft/v1/craft.proto)
- Platform enforcement: `rust/flowcraft-platformd/src/abi.rs`
- Conformance suite: `rust/flowcraft-platformd/tests/conformance.rs`
- Launch/lifecycle: `rust/flowcraft-platformd/src/craft.rs`

## Three interfaces, three owners

| Interface | Owner | Freedom |
| --- | --- | --- |
| Lifecycle (`Craft` RPCs) | the platform | a craft implements it exactly; a mismatched version is refused |
| Capabilities (`workspace.Workspace/v1`, `sandbox.Runner/v1`, `secret.Store/v1`, `tool.v1.ToolSource`, …) | the platform | a craft may only consume them (with a grant), never claim them |
| Business payloads (`Run` input/events) | the craft | typed `message`/`delta`, or an opaque declared extension |

Checkpointing is deliberately not a platform capability: an agent owns the state
it can resume from, so a craft persists its own checkpoints.

The platform routes, replays, times out, and audits a turn without parsing the
craft's business payloads. Conversation data is not craft-defined: `message`
and `delta` reference `flowcraft.message.v1`, the same schema tool results use,
so the turn and the tool contract cannot drift. Only the extension body is
craft-private, and it is bounded by the types the craft declares.

## Lifecycle RPCs

| RPC | Caller → craft | Semantics |
| --- | --- | --- |
| `Describe` | platform | Returns `CraftInfo`. Not authenticated: it is how a platform learns what to expect. Must not start work. |
| `Start` | platform | `{instance, tenant, grants}` → `{session, grants}`. The craft stores the grants; they are the only authorization it has. |
| `Run` | platform | `{session, input, credit}` → server stream of `RunEvent`. Must end with exactly one `done` or `error` event, then close the stream. |
| `Steer` | platform | `{session, payload}`. Feeds a running turn; same envelope as `input`. |
| `Renew` | platform | `{session, grants}` → `{grants}`. Replaces the whole grant set. Sent at half the lease TTL. |
| `Cancel` | platform | `{session, reason}`. Stops the turn, keeps the session. Idempotent. |
| `Close` | platform | `{session}`. Releases the session; later `Run`/`Steer` must fail. Idempotent. |

## Business envelopes

`RunRequest.input`, `SteerRequest.payload`, and every `delta`/`message`
`RunEvent.payload` carry a `flowcraft.craft.v1.Envelope` whose oneof body is
one of:

| Body | Carries |
| --- | --- |
| `message` | a complete `flowcraft.message.v1.Message` (role + ordered parts) |
| `delta` | a stream fragment: `text`, `reasoning`, or `tool_call` |
| `extension` | `{type, data}`: craft-private bytes, carried opaquely |

The conversation model lives in
[`proto/flowcraft/message/v1/message.proto`](../../proto/flowcraft/message/v1/message.proto):
the craft ABI and `tool.v1` reference the same messages, so a tool result and a
turn's content share one schema.

- `message` and `delta` are platform vocabulary. A craft must not declare them
  in `input_types`/`event_types`.
- `extension.type` must be listed in `CraftInfo.input_types` (input/steer) or
  `CraftInfo.event_types` (events), **or** start with `x-`.
- The platform validates the body: known role and at least one typed part for
  `message`, a set kind for `delta`, and type naming/declaration for
  `extension`. It never inspects `extension.data`.
- `done` carries no payload. `error` must carry a non-empty message in the
  `error` field. `credit` is advisory flow control.

## Capability namespaces

- Platform capabilities are reserved: a craft may not list them in `provides`.
  The reserved prefixes are `workspace.`, `sandbox.`, `secret.`,
  `checkpoint.`, `inference.`, `tool.`, `memory.`, `event.`, `graph.`,
  `agent.`, `craft.`.
- A craft may only advertise private capabilities shaped
  `<vendor>.<craft>.<thing>/v<N>` (for example `acme.research.tools/v1`).
- `requires` may name either kind; `provided` endpoints are reached with the
  same grant mechanism as platform capabilities.

## Grant usage

Each `Grant` carries `capability`, `endpoint` (the resolved target, e.g.
`unix:///run/flowcraft/workspace.sock`), `token`, `expires_at_unix`, and
`scope` (JSON, frozen at issue time). Every call to an endpoint sends
`authorization: Bearer <token>` as metadata. The endpoint validates the token
offline and enforces the scope; the craft must not widen it.

## Bootstrap token and socket

- The platform mints one bootstrap token per craft, hands it to `exec` through
  `FLOWCRAFT_CRAFT_TOKEN`, and sends it as `x-flowcraft-craft-token` on every
  call **after** `Describe`. A craft should require it (the reference stub
  does).
- The platform owns the socket path: it creates the parent directory (0700),
  removes a stale socket, and sets the socket to 0600 once the craft binds.
  A craft must not unlink the socket on exit.

## Versioning

- `CraftInfo.abi_version` is the version the craft implements; `abi_versions`
  lists everything it can serve. The platform supports
  `flowcraft.craft/v1` and refuses to start a craft whose declared versions do
  not intersect (fail closed, `NotAvailable`).
- Within `v1`, changes are additive only: new RPCs, new fields, new event
  types. Removing or redefining anything requires `flowcraft.craft/v2`, served
  alongside `v1` during a migration window.

## Conformance checklist

A craft implementation must pass:

1. `Describe` reports `abi_version`, `input_types`/`event_types` that name only
   extension types (never `message`/`delta`), and only private `provides`
   names.
2. `Start` returns a non-empty session and keeps the grants.
3. `Run` emits `delta`/`message` envelopes with the matching typed body, then
   exactly one terminal event; the stream ends afterwards.
4. `Renew` replaces the grant set without dropping the session.
5. `Close` makes later `Run` calls fail.
6. Every call after `Describe` requires the bootstrap token.
7. `Steer` accepts the same typed envelope as `Run` input.

`tests/conformance.rs` implements these checks against a craft endpoint; the
`craft-stub` subcommand is the reference implementation.

## Not part of this ABI

- The craft's agent loop, graph, tools, memory, and model selection. Those are
  SDK internals; the platform never inspects them.
- Configuration documents. `platform.yaml`, `deploy.yaml`, and `craft.yaml` are
  human-authored documents validated by JSON Schema, not by this proto.
