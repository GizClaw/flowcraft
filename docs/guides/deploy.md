---
layout: default
title: Deployment Documents
---

# Deployment Documents Guide

`core/deploy` turns deployment documents into _values_: settings and closures,
never handles. It decodes both document shapes strictly, validates every entry
against the implementation table the application registered, and answers the two
questions a deployment can get wrong before anything is built — "does this
document hold together" and "can this process serve it".

Whoever builds is `core/host`, and only the host closes what it built. That is not
ceremony: a loader that returned handles would own their lifecycle, and "only the
assembly layer holds the process" would stop being true the first time a test
called `Load`.

## Two documents

A process is described twice, and neither document can name a resource the other
one owns:

| Document           | Says                                                                                                                                       |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------ |
| a process document | what _this process_ carries: the sandbox backends, the model targets, the tool sources, the skill roots, and how much of a turn it records |
| an agent document  | what _one agent_ is: its name, its program, its session table, its hooks                                                                   |

A session selects from the process's resources through the `policy.Grant` its caller
states when it is opened; a document cannot add a provider or a backend. Adding a
resource means editing the process document and restarting — a different action from
deploying an agent, and the error messages keep the two apart.

`examples/forge` calls the process document `runtime.yaml` and the agent document
`deploy.yaml`. The names are the application's, not the loader's: the loader takes
a file for what it is by decoding it as one or the other.

### The process document

```yaml
version: 1

sandbox:
  - name: local
    impl: local

inference:
  providers:
    - name: openai
      impl: openai
      settings: { api_key: "${env:OPENAI_API_KEY}" }
  route:
    attempts_per_target: 2
    max_targets: 1
    base_delay: 250ms
    max_delay: 5s

tools:
  sources:
    - { name: files, impl: files }
    - { name: exec, impl: exec }
  middleware:
    - { name: telemetry, impl: telemetry }

skills:
  roots: ["/srv/agent/skills"]

# Both fields are required: "we record nothing" and "nobody said" are different
# facts, and the second one is how a deployment ends up with no trace at all.
observability: { trace: call, bodies: false }
```

| Section                 | Rule                                                                                                                         |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `version`               | Must be `1` (`deploy.SupportedVersion`). Another version is refused instead of read optimistically.                          |
| `sandbox[]`             | `{name, impl, settings}`. `name` is what a session's `grant.sandbox` selects; `impl` is a backend registered on the process. |
| `inference.providers[]` | `{name, impl, settings}`. `name` is what a grant's models are checked against, `impl` a registered provider.                 |
| `inference.route`       | `attempts_per_target`, `max_targets`, `base_delay`, `max_delay`. Selectors are Go values and stay out of the document.       |
| `tools.sources[]`       | `{name, impl, settings}`. Every source is aggregated into one tool table for the process.                                    |
| `tools.middleware[]`    | `{name, impl, settings}`. The chain every tool call passes through, outermost first.                                         |
| `skills.roots[]`        | Absolute directories scanned for `SKILL.md`. Discovered once for the process, so a session is not paying for it per turn.    |
| `observability.trace`   | Required. `none`, `run`, `call` or `node`.                                                                                   |
| `observability.bodies`  | Required, `true` or `false`. Retention of model response bytes, not a level.                                                 |

Duplicate names within a section are refused: a name is how an agent selects the
resource, so two entries answering to one name would make the selection ambiguous.

The `settings` block is opaque here on purpose: its schema belongs to the factory
the application registered, which is the only thing that knows what a backend or a
provider needs. The loader hands each block to its factory's `Plan` at load time,
so a typo inside one is a document error with the rest of them.

### The agent document

```yaml
version: 1

agents:
  - name: assistant
    program:
      impl: graph
      settings:
        model: { provider: openai, name: gpt-4o-mini }
        path: /srv/agent/graphs/assistant.yaml
    limits: { idle: 30m, max_sessions: 8 }
    hooks:
      - impl: memory
        settings: { dir: /srv/agent/memory, retain: 40 }
```

| Field              | Rule                                                                                                      |
| ------------------ | --------------------------------------------------------------------------------------------------------- |
| `version`          | Must be `1`.                                                                                              |
| `agents[].name`    | Required, unique in the document, and what a host addresses the agent by. Layer merging also keys on it.  |
| `agents[].program` | Required. `impl` names a registered program (`graph` in `examples/forge`); `settings` are that program's. |
| `agents[].limits`  | **Required.** `idle > 0` and optionally `max_sessions`. The session table's policy, not a permission.     |
| `agents[].hooks`   | `{impl, settings}`, chained in declaration order. An impl nobody registered is a document error.          |

The document has no runtime section on purpose: what an agent may use is the
process's, not a private copy of it. It also has no permission section: what a
session may do is stated by whoever opens it (see [Authority](#authority)).

## Authority

`policy.Grant` is what one session may do. It is a required argument to
`host.Open` — a session with no stated authority has not been authorised by
anybody — and it is built by the application, which is the trusted side. The
value is validated by `core/policy`, not by the host, so the code that builds one
and the code that enforces it agree by construction.

| Field        | Meaning                                                                                                               | Zero value                                                                          |
| ------------ | --------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `sandbox`    | The backend this session runs in, by the name the process gave it.                                                    | The one backend the process configured; a name it does not carry is `NotAvailable`. |
| `files`      | `read[]`, `write[]`, `readonly`. Absolute roots, no globs, no symlink resolution.                                     | Neither side restricted.                                                            |
| `net`        | `mode` (`default`, `deny_all`, `allow_list`, `proxy`), `allow_hosts[]`, `rules[]`, `proxy`, `unix_sockets[]`, `mitm`. | `default`: the host decides.                                                        |
| `exec`       | `timeout`, `env`, `shell`.                                                                                            | No timeout, the host's environment, the backend's shell.                            |
| `resources`  | `cpu_millicores`, `memory_bytes`, `disk_bytes`, `max_output_bytes`.                                                   | Unbounded — and a backend that cannot enforce a non-zero value must refuse.         |
| `models`     | `[{id: {provider, name}, profile?, modes?}]`, in priority order: the first entry a request can use answers it.        | Empty means **every model the process serves**, in the process's own order.         |
| `tools`      | `allow[]`, `hide[]`, `exposure{}`, `budget{}`.                                                                        | `allow` unset means every tool the process assembled.                               |
| `delegation` | `targets[]`, `modes[]` (`sync`, `async`), `max_depth`, `max_concurrent`, `timeout`, `max_input_bytes`.                | Absent means no delegation at all.                                                  |

Three defaults are deliberately equal, and the equality is the design: files are
open because a read is recoverable and auditable; tools are open because which
tools exist is a property of the process; models are open because which models
exist is the process's too. Each one is enforced where it is used — the tree
handles the files, the projection the tools, and the router refuses a model call
outside the set before any provider sees it.

A model entry may narrow what the model may be asked for:

```yaml
models:
  - { id: { provider: openai, name: gpt-6 } } # any mode it declares
  - { id: { provider: openai, name: deep-research }, modes: [text] }
```

The modes are `text`, `image`, `audio`, `video`, `tool_call`, `embed` and `decide`.
`tool_call` is what a request that offers tools needs, so narrowing a model to
`text` is how a session is denied tools on that model. A request that names no model
gets the first entry, in order, that can serve it.

`Grant.Normalized()` is the gate and the canonical form at once. It normalizes
`files`, `tools` and `net`, validates `exec`, `resources` and `delegation`,
de-duplicates models while keeping declaration order (that order is the failover
priority), and deep-copies the slices the value owns.

Three `files` invariants are refused rather than repaired:

- `readonly: true` together with a non-empty `write`;
- a narrowed read set beside an unrestricted write set — "you may look here and
  write anywhere" is never what an author meant;
- a write root that no read root covers.

A root must be absolute, non-empty, free of surrounding whitespace and not a glob.
`/` collapses a side to "unrestricted", because listing everything and saying
nothing are the same permission and must produce the same digest. Normalization is
lexical: nothing is resolved, so a symlink and its target are two different roots.

`readonly` is refused unless the backend can bound writes at the OS level: it is a
confine, not a wish. `exec("echo x > /tmp/f")` never goes through `fsx`, so a
backend that only checks paths inside the file tools cannot honour it, and
`ValidateConfine` refuses such a confine at open rather than accepting a promise it
does not keep.

### Widening a refused command

A backend that refuses a command with `sandbox.ErrConfineDenied` has said the command is outside
the session's authority. A process may offer that refusal to an operator
(`host.WithEscalation`), and what comes back is an approval for **one command in a temporary
cell** — never a change to the session's own boundary.

```go
host.WithEscalation(myEscalator)   // an implementation of host.Escalator
host.WithEscalationTTL(5 * time.Minute)
```

- The answer must be a widening: `policy.Grant.Widen` refuses one that drops a permit the session
  already had, changes anything but `files.write`, `net` and `exec.env`, or does not validate. An
  answer that replaces the authority instead of widening it does not run.
- The bound above the approval is what the machine can enforce: the temporary cell is built from
  the widened grant through the same backend factory, and `sandbox.ValidateConfine` is what says
  whether that backend can honour it.
- The attempt is retried once. A second refusal inside the widened cell is the answer, so a model
  cannot summon a prompt by walking into whatever boundary it is given.
- `ttl` belongs to the process rather than to a document: a temporary cell has to end, whatever
  the command asked for.
- Every widening that reached a cell leaves an audit record: the command hash, the two digests
  (`from_bind`, `to_bind`) and the decision. The command text, its arguments and the paths are
  never written.

With no escalator installed the refusal stands; the feature is opt-in, like approval.

## Registering implementations

Core carries no implementation table: the shipped providers, backends, tool
sources and middleware live in the `inference`, `sandbox` and `tool` modules, and
`core` is forbidden from importing them. The application registers what it
compiles in:

```go
impls := deploy.NewImpls().
    // A provider factory's Plan decodes the written settings; Make returns *inference.Provider,
    // because that is what the host builds and what its catalog reports models from.
    Provider(deploy.Impl("openai", buildOpenAI)).
    // A backend is built for one session's confine, so the factory receives it. This one
    // declares no enforcement, which is why a session that narrows writes onto it is refused.
    Backend(deploy.BackendImpl("local", func(_ struct{}, confine sandbox.Confine) (sandbox.Runner, error) {
        return local.New(confine)
    })).
    ToolSource(deploy.Impl("files", builtinFiles)).
    Middleware(deploy.MiddlewareImpl("telemetry", func(struct{}, tool.Catalog) (tool.Middleware, error) {
        return middleware.Telemetry(), nil
    }).AtTrace(telemetry.TraceCall)).
    Hook(deploy.Impl("memory", newMemory)).
    Program(deploy.Impl("graph", graphProgram))
if err := impls.Err(); err != nil { /* a name was registered twice, or was empty */ }

loader := deploy.NewLoader(impls)
```

`buildOpenAI`, `builtinFiles`, `newMemory` and `graphProgram` are the application's
settings-plus-constructor pairs; `examples/forge/internal/app` is a complete table
written this way.

Six groups exist — provider, backend, tool_source, middleware, hook, program — and
a name only means something inside its group: an implementation called `local` is
a different thing as a provider and as a backend. `deploy.Group.String()` is how a
document section spells each one.

A `Factory` is split into the two phases a deployment needs. `Plan` decodes the
settings strictly and must have no side effects; `Make` builds the object from what
`Plan` returned. Keeping them apart is what lets a document be validated in a test
or a CI job without dialling anything.

Two registration-wide declarations travel with the factory:

| Declaration | Answers                                                                                                                                                                                                                                                                                                                  |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `AtTrace`   | The least trace level the link is worth installing at. A middleware below the process's level is left out of the plan, not built and disabled.                                                                                                                                                                           |
| `Enforcing` | What a backend can enforce on this platform, as a `sandbox.Enforcement`. The host reads it when a session is opened, because the per-session instance does not exist until then — and a backend that declares nothing bounds nothing, so a session that narrows writes onto it is refused. `sandbox/local` is that case. |

## Loading

```go
ctx := context.Background()

// A process: what this binary carries.
plan, err := loader.Process(ctx, sources.File("/srv/agent/runtime.yaml"))

res, err := host.Open(ctx, plan, host.Options{
    Capabilities: capSet,     // the slots this process can hand a tool
    Dispatcher:   dispatcher, // the tool executor this binary compiles in
    View:         view,       // the projection: one per deployment, one view per session
})
defer res.Close()

// Then the agents, resolved against what the built process reports.
deployments, err := loader.Deployments(ctx, res.Capabilities(),
    sources.File("/srv/agent/deploy.yaml"))
```

`Loader.Process` returns a `deploy.ProcessPlan`: for every entry the decoded
settings and the factory that will build it. `Loader.Deployments` returns
`[]deploy.Deployment`: name, planned program, validated grant, planned hooks.
Neither builds anything, and both take `config.Layer` values, so one file on disk
and a stack of overrides go through the same code.

`deploy.Capabilities` is the process's answer to an agent document, and it holds
only what the _process_ decides — `Backends`, `Enforcement` and `Models` as
`"provider/name"`. Registered implementation names are checked against the kind
table instead, which the loader already holds.

### Layers and placeholders

Layers are `core/config`'s. `deploy/sources` is the four constructors over
`config.Layer` — `File`, `Bytes`, `FS`, `Func` — plus `Over` for priority:

```go
layers := []config.Layer{
    sources.File("base.yaml"),
    sources.Over(sources.Bytes("local.yaml", override), 10),
}
```

- Layers merge in ascending priority; duplicate priorities are refused.
- Mappings merge key by key, **sequences are replaced whole**, scalars are
  replaced. An `agents` list in a higher layer replaces the list, so an agent it
  wants to change is written out in full; there is no per-agent overlay.
- Placeholders are resolved once, after the merge, so a value an overriding layer
  replaced is never looked up. Forms: `${NAME}`, `${NAME:-fallback}` (both the
  built-in `env` scheme) and `${scheme:args}` for a resolver the application
  registered with `config.WithResolver`.
- `$$` is a literal `$`. A `<<` merge key is expanded when the layer is parsed.

Resolvers travel with the load, not with a document:

```go
loader := deploy.NewLoader(impls).
    WithConfigOptions(config.WithResolver("workspace", resolveWorkspace))
```

Only `${env:…}` is built in. `examples/forge` registers `${workspace:root}` so one
workspace directory stays relocatable; a key is `settings.api_key: ${env:OPENAI_API_KEY}`.

To print a startup checklist instead of a restart loop, ask first:

```go
req, err := config.RequiredPlaceholders(ctx, layers...)
missing := req.Missing(os.LookupEnv) // every unset variable, in one pass
```

## Two kinds of failure

| Class          | Means                                                                                                                                                                                           | Fixed by                          |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------- |
| `Validation`   | The document does not hold together: unknown field, wrong type, missing required value, a duplicate name, a name nobody registered.                                                             | Editing the document.             |
| `NotAvailable` | The document holds together but asks for something this process lacks: a program or hook kind it did not register, a session whose grant names a backend or a model the process does not carry. | Changing how the process started. |

```text
deploy: an agent document needs at least one agent
deploy: every agent needs a name
deploy: agent "assistant" must state limits (idle, and optionally max_sessions)
deploy: agent "assistant" selects sandbox "bwrap", which this process does not configure (local)
deploy: agent "assistant" names model "openai/gpt-5", which this process does not serve (openai/gpt-4o-mini)
# the last two are raised when a session is opened: the document no longer names either
deploy: observability.trace is required (none, run, call or node)
deploy: observability.bodies is required (true or false)
```

Use `errdefs.IsValidation` / `errdefs.IsNotAvailable` to branch, which is what the
skill's validator does.

## Installing and running deployments

`host` turns values into live objects:

```go
agents := host.NewAgents(res,
    host.WithStore(store),                    // nil keeps a turn in memory only
    host.WithFingerprint(deploy.Fingerprint(deployments)),
    host.WithHarness("forge@1.2.3"),
    host.WithApproval(approval),              // the policy that decides a command
    host.WithEscalation(escalator),           // who agrees to widen a refused command
    host.WithEscalationTTL(5*time.Minute),    // how long that widened cell may live
    host.WithRunSink(func(record agent.RunRecord) { ... }),
)
if err := agents.Add(deployments...); err != nil { ... }
defer agents.Close(ctx)

session, err := agents.Open(ctx, sessionID, "assistant", authority)
for event, err := range agents.Run(ctx, sessionID, msg) { ... }
```

| Call                                           | Use                                                                                                    |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `Agents.Add` / `Remove` / `Reload`             | Install, retract, swap a deployment. `Remove` refuses while sessions are open.                         |
| `Agents.Open`                                  | Admit a session under the `policy.Grant` its caller states; `WithChain` attaches a delegation lineage. |
| `Agents.Run`                                   | One turn, as an iterator of `agent.Event`. One turn per session at a time.                             |
| `Agents.Steer` / `Cancel`                      | Address the turn that is in flight.                                                                    |
| `Agents.Sessions` / `Session` / `CloseSession` | The session table.                                                                                     |
| `Agents.EvictIdle`                             | Evict by the agent's `limits.idle`. Explicit, and it skips a running session however long it has run.  |
| `Agents.Names` / `Deployed`                    | What is installed.                                                                                     |
| `Agents.Close` / `Resource.Close`              | The two exits: the deployment table first, then the process.                                           |

A reload retires the old program and closes it when its last session is gone: a
session runs against what it was opened under.

`deploy.Fingerprint(deployments)` is the identity of a deployment set — name, program
kind and hook kinds, per deployment, in order. It lives in `deploy` because two
processes must compute it identically to be comparable at all; a checkpoint written
under a different fingerprint is refused on resume.

Two things are deliberately outside it. **Authority**, because a deployment no longer
carries one: the checkpoint records the manifest the session ran under instead, and
that is what refuses a conversation that changed authority. And the **observability
settings**, because a record level decides how much evidence a run leaves, not whether
it can continue.

## Where each piece lives

| Package               | Role                                                                                                      |
| --------------------- | --------------------------------------------------------------------------------------------------------- |
| `core/deploy`         | Both document shapes, the implementation table, loading, plans, `Fingerprint`. Imports no implementation. |
| `core/deploy/sources` | `File`, `Bytes`, `FS`, `Func`, `Over`: the four constructors over `config.Layer`.                         |
| `core/host`           | `Resource` (one process), `Agents` (one deployment table), `Session` (one conversation), one turn.        |
| `core/policy`         | `Grant`, `Widen`, `Diff`, `Denial`: authority in one value.                                               |
| `core/config`         | The layer stack, the merge, placeholders and provenance.                                                  |
| `core/agent`          | The turn decorator, the driver contract, the checkpoint and record shapes.                                |
| `agent/graph`         | The `graph` program's implementation and its document; see [graph.md](graph.md).                          |
| `sandbox/…`           | The backends (`local`, `remote`) and the confine they enforce.                                            |
| `inference/…`         | The providers and the request pipeline.                                                                   |
| `tool/…`              | The tool sources and the dispatch/middleware chain.                                                       |

## What was removed, and why

The previous assembly model — one `deploy.yaml` with `engine`, `runtime.layers`,
`catalog`, `protocol` and `delegation` per agent, a `Loader.Load` returning
`[]deploy.Spec`, and a `core/agent-host` builder over a `runtime.Config` — is
gone, together with the `runtime` and `correlation` packages and the
`deploy/protocols/a2a` kind. Each agent carried its own substrate, so two agents
in one process could disagree about which backend existed, and a document could
only be understood by reading its layer stack. The current model has two
documents, one implementation table per process, and a grant as the only thing an
agent says about what it may touch.
