---
layout: default
title: Inference Runtime
---
# Inference Runtime Guide

`core/inference` is the contract for model calls, and three packages around it are
the runtime: `core/inference/model` (identity, capabilities, limits, usage
vocabulary), `core/inference/route` (selection, retry, fallback), and `core/runtime`
(assembly from a deployment document). `core/inference-openai` and
`core/inference-anthropic` are the two protocol families that ship with the
platform; every other vendor is a `driver/` module.

```go
type Provider struct {
    Generator            // required: Generate and GenerateStream
    Embedder  Embedder   // nil when the provider does not serve embeddings
    Decider   Decider    // nil when the provider does not serve judgments
    Catalog   Catalog    // nil when the deployment declared no models
}
```

`Provider.Validate()` is what turns "declared but uncallable" into a deployment
error: a model that declares an embedding width while no `Embedder` is bound, or
decision support while no `Decider` is bound, would be a capability a caller can
discover and never use.

Transcription and image generation are not seams here — they are extensions the
provider driver offers (`core/inference-openai` has both). A capability that only
one vendor sells stays with the vendor.

## Exact addressing

Every call names a concrete model:

```go
ref := model.ModelRef{
    ID:      model.ModelID{Provider: "openai", Name: "gpt-5"},
    Profile: "luna",        // the deployment's provider block
}
```

The runtime never picks or replaces a model on its own. Routing is optional and
explicit: it is a policy over the declared line-up, and an empty `route.selector`
means declaration order.

## The document

```yaml
inference:
  providers:
    - name: luna                   # deployment identity, secret-free
      driver: openai               # a name the host compiled in
      settings:                    # the driver's own written form (see below)
        base_url: https://api.openai.com/v1
        api_key: "${OPENAI_API_KEY}"
        api: responses
  models:
    - provider: luna
      model: gpt-5
      weight: 100
      # Fill in only what the driver's catalog does not declare; a
      # contradiction is an assembly error, not an override.
      capabilities: {inputs: [text, image], outputs: [text]}
      limits: {max_input_tokens: 200000, max_output_tokens: 32000}
  route:
    selector: weighted             # a name the host registered
    attempts_per_target: 2
    max_targets: 3
    base_delay: 200ms
    max_delay: 2s
```

| Block | Rule |
| --- | --- |
| `providers[].name` | The deployment's identity for the account or endpoint; it is what `models[].provider` refers to. |
| `providers[].driver` | Must be registered on the `Runtime`; an unknown one fails with the offered names. |
| `providers[].settings` | Opaque to core: the driver's constructor decodes it. |
| `models[]` | Position is priority — the first model is tried first. Declaring a model is the only way one exists. |
| `route` | Optional. `attempts_per_target`, `max_targets`, `base_delay`, `max_delay`, `selector`, `selector_settings`. |

## Drivers and their settings

The runtime imports no driver, so the adapter that maps a written settings block
onto a driver's typed `Settings` lives in the host:

```go
runtime.WithDrivers(
    runtime.Driver{Name: openai.ProviderID, Build: buildOpenAI},
    runtime.Driver{Name: anthropic.ProviderID, Build: buildAnthropic},
)
```

Every driver's `Settings` is a plain Go value **without JSON tags** — a JSON block
is not its wire format, and pretending otherwise would freeze the contract. The
host writes the mapping, which means a deployment's `settings` keys are the
host's vocabulary: `examples/forge/internal/app/drivers.go` is the reference
implementation, including duration parsing and the `wire` sub-block the Anthropic
driver takes.

That indirection is deliberate. It lets a driver rename a field without a
breaking change to every deployment file, and it keeps a credential's spelling
(`api_key`) a host decision rather than a vendor one.

## Model declarations

A model exists because a deployment declares it. Nothing is inherited from a
same-named model, so a generate model has to state its text output, and every
published capability is written out.

Two sources meet here:

- the **driver's catalog** (`Settings.Models`) — the facts the driver knows about
  a model it serves;
- the **document's `models[]`** — the facts this deployment wants to publish.

A contradiction between them is an assembly error, not an override. What the
document adds and the catalog omits becomes the published descriptor
(`model.ModelDescriptor`: id, label, capabilities, limits, lifecycle).

### Limits are promises, not clamps

A `Generate` request whose `max_output_tokens` exceeds the target's declared
maximum is rejected **before** the driver is opened, and the rejection is
transport-safe — so a routed request falls back to a target that declares room for
it instead of failing. The caller's budget is never silently lowered, and an
undeclared limit rejects nothing.

`max_input_tokens` is not enforced: the module has no tokenizer, and an
approximate count would be worse than none. A host that owns the conversation
history reads the declared window from the catalog and trims where it already
trims.

### Capabilities route the call

Declared capabilities are consulted on both the selection and the fallback path:
a target whose declared output kinds cannot serve the request intent is skipped,
while a model with undeclared capabilities is treated as undeclared — not
unsupported — and the request is attempted. The provider's own validation remains
the final arbiter.

A capability declaration is also a promise checked per surface: a model declaring
toggleable reasoning must accept `reasoning_enabled=false` on that surface, and one
declaring `always` must reject it. Discovery bits such as hosted web search ride on
the descriptor, so a host can surface per-model options without vendor-specific
knowledge.

## Routing

Selection, retry and fallback live in `core/inference/route`:

```go
router, err := route.New(targets, route.Policy{
    Generate: mySelector{},          // nil = every target, in declaration order
    Retry: route.RetryPolicy{
        AttemptsPerTarget: 2,
        MaxTargets:        3,
        BaseDelay:         200 * time.Millisecond,   // doubles per attempt
        MaxDelay:          2 * time.Second,
    },
})
```

A `route.Target` is one candidate: its provider, weight (for selectors that use
one), the model it serves, and the deployment profile it belongs to. Dispatch
stamps the winning target's model identity onto the request, so a caller names a
model and the answer carries the provider that actually served it.

| Field | Meaning |
| --- | --- |
| `Generate`, `Decision` | One selector per seam; a nil selector means every target in order. |
| `Retry.AttemptsPerTarget` | Tries of one target for a transient failure; zero means one. |
| `Retry.MaxTargets` | How many candidates are tried; zero means all the selector returned. |
| `Retry.BaseDelay`, `Retry.MaxDelay` | Backoff, doubling per attempt, capped. |

A **selector** is a Go value, and `runtime.Selector` is only the name that lets a
document refer to one — two typed slots, because
`route.Selector[inference.GenerateRequest]` and
`route.Selector[inference.DecisionRequest]` are different Go types and one factory
cannot produce both:

```go
runtime.WithSelectors(runtime.Selector{
    Name:     "weighted",
    Generate: func(json.RawMessage) (route.Selector[inference.GenerateRequest], error) {
        return weighted{}, nil
    },
})
```

There are no built-in selectors, and an unregistered name fails the build with the
offered list. A selector has one method — `Select(ctx, request, targets)` — because
a selector answering for several seams would be the operation table this package
exists to avoid. Pure Go callers skip the name table entirely and put the value in
`route.Policy` directly.

## Extensions

Provider-specific options travel as typed extensions, not as a raw map the driver
merges blindly:

```yaml
request:
  extensions:
    - {name: reasoning, settings: {effort: high}}
```

An extension name is resolved against the `ExtensionDecoder`s the host registered
(`nodes.WithModelDecoders`, `runtime` options), so unknown settings are a build
error rather than a silently dropped field, and the driver receives a typed value.
`inference.Extensions` also carries `Metadata`, which is passed through untouched
and is the place for a trace tag rather than a wire field.

## Streaming

`GenerateStream` returns a `Stream` the caller must close. The stream carries
incremental events — text deltas, reasoning deltas, tool-call fragments, usage —
and one terminal event; a consumer that stops early must close it, or the provider
connection leaks. A node that does not need deltas calls `Generate` instead: a call
whose answer is data should not pay for an event stream.

## Media

Media is a first-class part kind (see [media.md](media.md)). A request's parts and
the model's declared `inputs`/`outputs` are checked against each other during
routing, which is what keeps "this turn has an image, this target has no vision"
from reaching a provider. Audio and video parts follow the same rule; a provider
that does not accept a kind refuses the request, and flattening it into text is the
host's decision, not the runtime's.

## Judgment

`Decider.Decide` answers a set of questions in request order and returns one answer
per question:

| Type | Is |
| --- | --- |
| `Question` | One question with a `QuestionKind`: a choice among labelled options, a probability, a free answer. |
| `ChoiceValue`, `Probability`, `Answer` | The shapes an answer takes. |
| `DecisionRequest`, `DecisionResponse` | The batch and its results, with usage and the same error vocabulary as generation. |

Routing applies to judgment too — `runtime.Selector.Decision` is the other typed
slot — because "which model judges this" is the same question as "which model
answers this".

## Errors

Every seam shares one vocabulary — an `inference.ErrorKind` plus a `Field` naming
the request member at fault: `InvalidRequest`, `UnsupportedOperation`,
`UnsupportedFeature`, `InvalidExtension`, `UnknownModel`, `PolicyDenied`,
`ProviderFailure`, `InvalidProviderResponse`, `UndefinedTool`, …

A routing decision classifies from that vocabulary, not from a string, and it has
three outcomes rather than two:

| Outcome | Kinds | Meaning |
| --- | --- | --- |
| retry the same target | `ProviderFailure`, `ProviderTruncated` | Transient, or a target worth one more attempt. |
| move to the next target | `UnknownModel`, `UnsupportedOperation`, `UnsupportedFeature`, and anything unclassified | This target cannot serve the request; another may. |
| stop | `InvalidRequest`, `InvalidExtension`, `PolicyDenied`, `UndefinedTool`, `CompilerContractViolation`, `OperationInterrupted`, and a cancelled context | Repeating it anywhere produces the same answer. |

That is what makes a fallback safe: a wrong request is not retried three times
against three vendors, and a cancelled call is not retried at all.

## What this is not

| Not here | Where |
| --- | --- |
| A provider registry with `init()` registration | The host's `runtime.WithDrivers` table: "which drivers exist" is a fact of the binary. |
| A vendor's model line-up | The deployment declares it; drivers ship none. |
| Transcription, image generation | Driver extensions (`core/inference-openai`). |
| Realtime | Reserved; no seam yet. |
| Token counting | No tokenizer in core. |
