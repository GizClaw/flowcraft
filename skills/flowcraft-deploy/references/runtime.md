# Process document

The process document says what _this process_ carries: the sandbox backends a
session can run in, the model targets it serves, the tool sources it aggregates,
the directories skill discovery walks, and how much of a turn it records. It is
read by `deploy.Loader.Process`, which returns a plan of settings plus factories and
builds nothing.

```yaml
version: 1

sandbox:
  - name: local # what an agent's grant.sandbox selects
    impl: local # a backend the binary registered
    settings: {} # the backend's own block, decoded by its factory

inference:
  providers:
    - name: openai # what a grant's models are checked against
      impl: openai
      settings: { api_key: ${OPENAI_API_KEY} }
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
  roots: [/srv/agent/skills]

# Both fields are required: "we record nothing" and "nobody said" are different
# facts, and the second one is how a deployment ends up with no trace at all.
observability: { trace: call, bodies: false }
```

## Fields

| Section                 | Rule                                                                                                                                                           |
| ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `version`               | Must be `1`. Any other value is refused instead of read optimistically.                                                                                        |
| `sandbox[]`             | `{name, impl, settings}`. `name` is the backend's address in a grant; two entries cannot share one.                                                            |
| `inference.providers[]` | `{name, impl, settings}`. `name` is what a grant's models are resolved against.                                                                                |
| `inference.route`       | `attempts_per_target`, `max_targets`, `base_delay`, `max_delay`. Selectors are Go values and stay out of the document.                                         |
| `tools.sources[]`       | `{name, impl, settings}`. Every source is aggregated into one tool table for the whole process.                                                                |
| `tools.middleware[]`    | `{name, impl, settings}`, outermost first. A link whose registration says it is only worth installing above the process's trace level is left out of the plan. |
| `skills.roots[]`        | Absolute directories scanned for `SKILL.md`. Discovered once for the process, because the index is a process fact.                                             |
| `observability.trace`   | Required: `none`, `run`, `call` or `node`.                                                                                                                     |
| `observability.bodies`  | Required, `true` or `false`. Retention of model response bytes, not a level.                                                                                   |

`sandbox`, `inference` and `tools` are optional as blocks, but a process with no
backend can serve no session and a process with no provider serves no model — the
first session or the first agent document is where that shows up.

Duplicate names within a section are refused: a name is how a session selects the
resource.

## `settings` belongs to the implementation

Every entry names an `impl` and carries that implementation's settings as an opaque
block. The schema of the block is the factory's, not the document's: the loader
hands the raw bytes to `Factory.Plan`, which decodes them strictly. That is why an
unknown key inside `settings` is a load error with the same weight as an unknown
key at the top level, and why a settings block this card cannot describe in general
— `examples/forge/internal/app` is where a real one is written.

The `impl` name must be in the process's own registration table. The error lists
what is there:

```text
deploy: provider "luna" selects impl "opnai", which is not registered (anthropic, openai)
```

## `observability`

| Trace level | Spans                                                                         |
| ----------- | ----------------------------------------------------------------------------- |
| `none`      | the run and its turns                                                         |
| `run`       | one per driver step, plus the turn's `checkpoint.save`                        |
| `call`      | the per-call spans: a model call, a tool call, one command                    |
| `node`      | whatever the driver considers a node of its own — the graph adds one per node |

`bodies: true` keeps each model response's bytes, which is what a replay answers
from. It is a retention decision about the user's content, so it is written down
rather than inferred from the level.

Both live on the process and not on an agent because everything they control — the
tool chain, the model access, the backends — is built once for the process. A level
two agents could disagree about would be a knob with no effect.

## `route`

`attempts_per_target` and `max_targets` bound retries and failover,
`base_delay`/`max_delay` bound the backoff; a negative value is refused. Which
target a call picks is a selector, and a selector is a Go value the host registers,
so it has no written form.

## Placeholders

Every string may name a placeholder, and expansion happens once, after the layers
merge:

| Form                | Resolves through                      |
| ------------------- | ------------------------------------- |
| `${NAME}`           | the built-in `env` scheme             |
| `${NAME:-fallback}` | `env`, with a default when unset      |
| `${scheme:arg:arg}` | a resolver the application registered |

Only `${env:…}` is built in; a scheme this process did not register is a load error
listing the schemes that are. `$$` is a literal `$`.

## What the loader checks

1. strict decoding — an unknown field anywhere, `settings` included, is an error;
2. `version` is `1`;
3. every section's names are present and unique;
4. every `impl` is registered in its group, and its `settings` decode;
5. `observability.trace` and `observability.bodies` are both present, and the level
   parses;
6. `route` values parse and are not negative.

It does not dial a provider, construct a backend or start anything: what a settings
block _means_, whether a credential works and whether an endpoint answers are the
host's business at start time.

## The other document

What an agent may do is not here: it is the agent document's `grant`, which is a
ceiling the process's resources are selected from. See
[deployment.md](deployment.md).
