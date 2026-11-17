---
layout: default
title: Context Registry
---

# Context Registry

`core` carries cross-layer facts on `context.Context`. Each one exists because a
fact has to cross a boundary that no seam carries: the run identity is stamped by
the assembly and read by a span opened inside the sandbox, the tool call id is
stamped by the executor and read by the delegation service, and neither pair may
import the other's package. `context` is the only channel that already runs
through every call in the platform.

That also makes each value invisible at the seam, so this page is their registry:
one row per value — the file that declares its key, what it carries, who writes
it, who reads it, how long it lives, whether it is mutated, and what a second
writer does to the first. The registry is enforced rather than remembered:
`core/internal/contextkeys` parses `core/**` and fails when a `context.WithValue`
call appears in a file the table does not name, when a named file stops calling
it, and when the table stops naming a file the gate allows.

## The values

| Key file                                                      | Value                                                                                | Written by                                                                                                                                                               | Read by                                                                                                               | Lifetime                                                                                             | Mutable                                                                        | Override                                                                      |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ | ----------------------------------------------------------------------------- |
| `core/correlation/run.go` (`runKey{}`)                        | `correlation.Run`, stored as the identity plus the span attributes it renders to     | `correlation.WithRun`: `Runtime.Context`, `agent-host` once per run, `agent` once per turn, `delegation.WithCall` for lineage                                            | `correlation.From` in the agent run record and in delegation; `correlation.Attrs` in every span a layer opens beneath | run — one `Run` call; task and turn are stamped again each turn                                      | no — the pair is replaced and `Attrs` returns a capped slice                   | merge — a field the writer sets wins, a zero field keeps the inherited one    |
| `core/inference/request_metadata.go` (`requestMetadataKey{}`) | `inference.RequestMetadata`, a `map[string]string` whose keys the deployment defines | `inference.WithRequestMetadata` — no production caller in this repository: the deployment supplies it                                                                    | `inference.RequestMetadataFrom` — likewise; a host's telemetry or driver reads it                                     | call — one generate or decision, across retries and streaming                                        | no — cloned on the way in, and the returned map is read-only                   | replace; a nil or empty map returns the context unchanged                     |
| `core/inference/route/target.go` (`traceKey{}`)               | `*route.Trace`, the attempt collector                                                | `route.WithTrace` — no production caller in this repository: the caller above the router creates it                                                                      | `route.TraceFrom` — the router's own dispatch loop, which records one `Attempt` per try                               | call — the attempts of one call across retries and targets                                           | yes, by design — it appends under a mutex, and the context carries the pointer | replace; there is nothing to merge                                            |
| `core/tool/call_context.go` (`callIDCtxKey{}`)                | `string` — the model-issued tool call id                                             | `tool.(*Executor).Execute`, before the middleware chain                                                                                                                  | `tool.CallIDFromContext` — `delegation.Service`, which derives the chain id from it                                   | call — one tool dispatch                                                                             | no                                                                             | replace                                                                       |
| `core/tool/scope.go` (`scopeKey{}`)                           | `tool.Scope` — the run's projection: the `CallRecorder` every dispatch reports to    | `tool.WithScope` / `tool.EnsureScope` — in practice `engine.WithCatalogView`, whose view implements the recorder, and `runtime.NewRunContext`; a host may attach its own | `tool.ScopeFrom` — the `RecordCalls` middleware, and `engine.CatalogViewFrom`, which asserts the recorder to a view   | run — a catalog view per run; session-scoped when a host reuses one view                             | no — the scope is set once; the recorder behind it accumulates                 | replace with `WithScope`; `EnsureScope` keeps the projection already attached |
| `core/tool/failure_kind.go` (`failureKey{}`)                  | `*tool.failureRecord` — one call's classification box                                | `tool.WithFailureRecorder` — the `Telemetry` middleware, before it calls inward                                                                                          | `tool.FailureKindFrom` — the same middleware, when it closes the span                                                 | call — written through by `tool.RecordFailure` from the executor and the short-circuiting middleware | yes, by design — a shared box, and the first classification wins               | replace — every call allocates a fresh box                                    |

**Lifetime** is one of four levels. Runtime: as long as the assembled runtime
the value came from. Run: one `Run` call. Turn: one model round-trip inside a
run. Call: one tool dispatch or one model call, retries and fallbacks included.
Two of the four are unused today: no value is attached for a runtime or for a
single turn, and the runtime id — a runtime-scoped fact — travels as a field of
the run-scoped value rather than as a value of its own.

## The rules

Four rules hold across the table, and they are why it has this shape.

**No dependencies in the context.** A value carries a fact about the work in
progress, not the machinery doing it: nothing here hands a lower layer a
provider, a runtime, a store or an executor to call. The two entries that do
point at an object point at a _sink_ — `tool.CallRecorder` is where a middleware
reports what it dispatched, `*route.Trace` is where the router reports each
attempt — because the seam they report through returns a response and has no
second return value. A fact survives any implementation of a seam; a component
read out of the context quietly makes the reader depend on whoever wrote it.

**No conversation content.** The run identity holds ids, the request metadata
holds deployment-defined correlation keys, the trace holds target names and
redacted error text, the failure record holds a closed vocabulary. Prompts, tool
arguments and tool results stay in `message` and the checkpoint. The values here
end up as span attributes, log fields and run records, so content placed in one
leaks into all three.

**Mutable collectors only.** A value that keeps changing after it is attached is
a collector, and everything else is a plain value a reader may keep. Three
entries lead to one: `*route.Trace` appends an attempt per try,
`*tool.failureRecord` keeps the first classification it is given, and the
recorder in `tool.Scope` accumulates the run's calls. In each case the
entry is a single pointer or interface set once, the mutation happens behind it
under the discipline its type documents — a mutex, first-write-wins, append-only
— and the reason is the same each time: the seam the collector serves returns a
response and has no second return value to carry the record. `WithRun` builds a
fresh pair, `WithRequestMetadata` clones the map, `WithCallID` stores a string:
those are replaced, never edited in place.

**One value per domain.** A domain gets one key, and a writer that knows more
about it extends that value instead of declaring a second. `delegation.Call` is
the worked example: depth and chain id are fields of `correlation.Run`, so
`delegation.WithCall` is a `WithRun` that sets two of them. `engine.WithCatalogView`
does the same from the other side — it stores its view in the `Recorder` field of
`tool.Scope` rather than opening a key of its own. A reader that had to consult
two keys would
have to know which layers wrote them, which is the coupling the carrier was
supposed to remove.

Every key is an unexported zero-size struct, declared beside the accessors that
read and write it and, where core owns the value type, in the file that type is
declared in. The value type itself is unexported wherever its shape is not part
of the API (`attached`, `failureRecord`). Neither is a style choice: no other
package can write the slot by hand, and the file that declares a value is the
file this table cites for it.

## What is not folded

Two domains are still unfolded, so the table is not a partition of the platform
into one value per domain and should not be read as one.

The tool-call domain has two keys and no single value. The id
(`call_context.go`) and the failure box (`failure_kind.go`) are facts about the
same call; a `tool.Call` value holding both would be the fold the last rule
points at, and it does not exist. The run's recorder used to be a third key; it
is now the `Recorder` field of `tool.Scope`, so a tool call no longer attaches a
projection of its own. A component that needs both remaining facts reads the
context twice and has to know which middleware installed each.

`inference/route`'s trace is the other half. The router reports its attempts into
a collector that is not part of `inference.RequestMetadata`, so the inference
domain carries two keys; and, like the metadata, the collector is only ever
created by a caller above the router — the package's own test is the sole caller
in this repository — so nothing in `core` fixes its lifetime. Both gaps are
stated here rather than hidden, so a reader knows what the registry does not yet
cover.
