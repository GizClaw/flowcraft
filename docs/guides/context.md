---
layout: default
title: Context Registry
---

# Context Registry

`core` carries a handful of cross-layer facts on `context.Context`. Each one exists
because a fact has to cross a boundary that no seam carries: the run identity is
stamped by the assembly and rendered by every span a layer beneath opens, the turn's
record level is set once and read by every producer that asks whether a span is worth
opening, and the package that writes one may not import the layer that reads it.
`context` is the only channel that already runs through every call in the platform.

Two things deliberately do **not** travel there, and they are the first thing a reader
who came looking for them needs to know. **Authority** is a parameter: a tool call
receives a `tool.Binding` — the session's `policy.Grant`, its capability pocket and its
view — from the dispatcher, so a tool cannot be handed permission its caller never
granted. **The tool projection** is a field of that binding, `tool.Binding.View`, a
per-session `tool.View` built by the deployment's `tool.ViewFactory`; there is no
context key for it. A projection in ambient state would make one session's prompt
depend on what another session did.

That also makes each value here invisible at the seam, so this page is their registry:
one row per value — the file that declares its key, what it carries, who writes it, who
reads it, how long it lives, whether it is mutated, and what a second writer does to the
first.

## The values

| Key file                                        | Value                                                                                                 | Written by                                                                                                                                                                                      | Read by                                                                                                                                                         | Lifetime | Mutable                                                                        | Override                                                                                                                  |
| ----------------------------------------------- | ----------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------- | ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------- |
| `core/identity/identity.go` (`runKey{}`)        | `identity.Run` — the tenant, agent, authority, session, bind, task, turn and chain one run belongs to | `identity.Attach`, called only from `core/identity/inject`: `inject.Session` (the host, per run), `inject.Turn` (`agent.RunTurn`, per turn), `inject.Chain` (the host, for a delegated session) | `identity.Lookup` — rendered as span attributes through `telemetry.Attrs`/`AttrsFor` in every layer that opens one; `agent` reads the tenant for the run record | run      | no — the value is replaced and `Run.Pairs` returns a fresh slice               | refuse: a second injection at the same level is an error, and `identity.Detach` is the one way to strip an outer identity |
| `core/telemetry/tracelevel.go` (`traceKey{}`)   | `telemetry.TraceLevel` — how much of this turn is recorded                                            | `telemetry.WithTrace`, once per turn by `agent.RunTurn`, from the deployment                                                                                                                    | `telemetry.Records` and `telemetry.Start`, which every producer asks before opening a span                                                                      | turn     | no — replaced, never edited                                                    | replace: a nested turn may legitimately differ, and the innermost answer is the one in force                              |
| `core/agent/failure.go` (`failureKey{}`)        | `*failureBox` — the turn's failure classification, a closed vocabulary                                | `agent.WithFailureBox`, once per turn by `agent.RunTurn`                                                                                                                                        | `agent.RecordFailure` (every stage) and `agent.FailureKindFrom` (the run record, the host's span)                                                               | turn     | yes, by design — the first classification wins, under a mutex                  | keep: a context that already carries a box is returned unchanged; `agent.WithoutFailureBox` removes it for a nested run   |
| `core/tool/record.go` (`recordKey{}`)           | `record` — the active tool call id plus the per-call classification box                               | `tool.WithCallID` and `tool.WithFailureRecorder`, from `tool/dispatch` before the middleware chain                                                                                              | `tool.CallIDFromContext`, `tool.RecordFailure`, `tool.FailureKindFrom` — the middleware chain and the dispatch telemetry                                        | call     | yes — the box keeps the first classification it is given                       | `WithCallID` replaces the id and keeps the box it found; the recorder keeps an existing box                               |
| `core/inference/request_metadata.go`            | `inference.RequestMetadata`, a `map[string]string` whose keys the deployment defines                  | `inference.WithRequestMetadata` — no production caller in this repository: the deployment supplies it                                                                                           | `inference.RequestMetadataFrom` — likewise; a host's telemetry or driver reads it                                                                               | call     | no — cloned on the way in, and the returned map is read-only                   | replace; a nil or empty map returns the context unchanged                                                                 |
| `core/inference/route/target.go` (`traceKey{}`) | `*route.Trace`, the attempt collector                                                                 | `route.WithTrace` — no production caller in this repository: the caller above the router creates it                                                                                             | `route.TraceFrom` — the router's own dispatch loop, which records one `Attempt` per try                                                                         | call     | yes, by design — it appends under a mutex, and the context carries the pointer | replace; there is nothing to merge                                                                                        |

**Lifetime** is one of three levels. Run: one `Agents.Run` call — the identity is
attached there rather than at open, because a turn is what carries a trace and the
injection refuses a second attach, so a value attached at open would fail the
session's second turn. Turn: one `RunTurn` inside the run. Call: one tool dispatch or
one model call, retries and fallbacks included. Nothing is attached for the life of the
process: a value that outlived the run describing it would be one conversation leaking
into the next.

## The rules

Four rules hold across the table, and they are why it has this shape.

**No dependencies in the context.** A value carries a fact about the work in progress,
not the machinery doing it: nothing here hands a lower layer a provider, a process, a
store or an executor to call. The one entry that does point at an object points at a
_sink_ — `*route.Trace`, where the router reports each attempt — because the seam it
reports through returns a response and has no second return value. A fact survives any
implementation of a seam; a component read out of the context quietly makes the reader
depend on whoever wrote it. Identity is the worked example of the cost: the package
imports nothing from `core` at all, so a layer that only needs a session id does not
link the telemetry SDK or the policy vocabulary.

**No conversation content.** The run identity holds ids, the request metadata holds
deployment-defined correlation keys, the trace holds target names and redacted error
text, and the two classification boxes hold a closed vocabulary. Prompts, tool
arguments and tool results stay in `message` and the checkpoint. The values here end up
as span attributes, log fields and run records, so content placed in one leaks into all
three.

**Mutable collectors only.** A value that keeps changing after it is attached is a
collector, and everything else is a plain value a reader may keep. Two entries lead to
one: `*route.Trace` appends an attempt per try, and the failure boxes keep the first
classification they are given. In both cases the entry is a single pointer or interface
set once, the mutation happens behind it under the discipline its type documents — a
mutex, first-write-wins — and the reason is the same: the seam the collector serves
returns a response and has no second return value to carry the record. `Attach` stores
a flat struct, `WithRequestMetadata` clones the map, and the tool record is a struct
carrying its box: those are replaced, never edited in place.

**One value per domain.** A domain gets one key, and a writer that knows more about it
extends that value instead of declaring a second. Delegation is the worked example:
depth and chain id are fields of `identity.Run`, so `inject.Chain` is an attach that
sets two of them, and `platform.DelegationOutcome` reports them back out. The tool
domain does the same from the other side — the call id and the classification share one
`record` key because they are one call's diagnostics. A reader that had to consult two
keys would have to know which layers wrote them, which is the coupling the carrier was
supposed to remove.

Every key is an unexported zero-size struct, declared beside the accessors that read and
write it. The value type itself is unexported wherever its shape is not part of the API
(`failureBox`, `failureRecord`, `record`). Neither is a style choice: no other package
can write the slot by hand, and the file that declares a value is the file this table
cites for it.

One write path is enforced rather than remembered. `identity.Attach` is exported for
`core/identity/inject` and for tests only, and the layout guard L-INJECT
(`TestInjectIsCalledFromTheAssembly`) fails when a package other than `host`, `agent` or
`identity/inject` imports the injector — so a new layer cannot start naming identities
by accident.

## What is not folded

One domain is still unfolded, so the table is not a partition of the platform into one
value per domain and should not be read as one.

`inference/route`'s trace is it. The router reports its attempts into a collector that
is not part of `inference.RequestMetadata`, so the inference domain carries two keys;
and, like the metadata, the collector is only ever created by a caller above the router
— the package's own test is the sole caller in this repository — so nothing in `core`
fixes its lifetime. The gap is stated here rather than hidden, so a reader knows what
the registry does not yet cover.
