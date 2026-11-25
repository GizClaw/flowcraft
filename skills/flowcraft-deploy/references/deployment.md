# Agent document

The agent document says what one agent is: a name, a program, a grant and hooks.
It is read by `deploy.Loader.Deployments`, which resolves it against a built
process and still builds nothing. One document carries a list of agents, because a
fleet is described once and then tuned per seat — and a document with one agent is
just a list of one.

```yaml
version: 1

agents:
  - name: assistant # the address a host uses, and what layer merging keys on
    program:
      impl: graph # a program the binary registered
      settings: # that program's own block
        model: { provider: openai, name: gpt-4o-mini }
        path: ${WORKSPACE_ROOT}/graphs/assistant.yaml
    grant:
      sandbox: bwrap # the process must configure it; escalate needs a boundary
      files:
        read: [${WORKSPACE_ROOT}]
        write: [${WORKSPACE_ROOT}/out]
      net: { mode: allow_list, allow_hosts: [api.openai.com] }
      exec: { timeout: 60s }
      resources: { cpu_millicores: 2000, memory_bytes: 2147483648 }
      models:
        - id: { provider: openai, name: gpt-4o-mini }
      tools: { allow: [read_file, write_file] }
      escalate:
        # An escalation only widens, so each dimension it names covers what the
        # grant already allows: here, its write roots plus one more.
        files: { write: [${WORKSPACE_ROOT}/out, ${WORKSPACE_ROOT}/cache] }
        net: { mode: allow_list, allow_hosts: [api.openai.com, "*.github.com"] }
        ttl: 5m
        max_per_session: 3
      limits: { idle: 30m, max_sessions: 8 }
    hooks:
      - impl: memory
        settings: { dir: ${WORKSPACE_ROOT}/memory }
```

## Fields

| Field              | Rule                                                                                               |
| ------------------ | -------------------------------------------------------------------------------------------------- |
| `version`          | Must be `1`.                                                                                       |
| `agents[].name`    | Required, unique in the document. It is what a host addresses and what an overlay rewrites.        |
| `agents[].program` | Required. `impl` names a registered program; `settings` are decoded by its factory.                |
| `agents[].grant`   | The ceiling this agent's sessions run under. Validated by `core/policy`.                           |
| `agents[].hooks`   | `{impl, settings}`, chained in declaration order. An `impl` nobody registered is a document error. |

There is no `engine`, no `runtime.layers`, no `protocol`, no `catalog` and no
agent-level `observability`: what an agent may use is its grant, and how much of a
turn is recorded is the process's. A document that still carries those keys fails
the strict decode rather than half-loading.

## The grant

One type serves two purposes — the ceiling a document declares and the facts of one
session after narrowing — because two types with the same fields would mean two
validators and two digests of the same facts.

| Field        | Meaning                                                                                                               | Zero value                                                              |
| ------------ | --------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| `sandbox`    | The backend this agent's sessions run in, by the name the process gave it.                                            | The runtime's default; a name the process lacks is `NotAvailable`.      |
| `files`      | `read[]`, `write[]`, `readonly`. Absolute roots, no globs.                                                            | Neither side restricted.                                                |
| `net`        | `mode` (`default`, `deny_all`, `allow_list`, `proxy`), `allow_hosts[]`, `rules[]`, `proxy`, `unix_sockets[]`, `mitm`. | `default`: the host decides.                                            |
| `exec`       | `timeout`, `env` (`allow[]`, `inject{}`), `shell`.                                                                    | No timeout, the host's environment, the backend's shell.                |
| `resources`  | `cpu_millicores`, `memory_bytes`, `disk_bytes`, `max_output_bytes`.                                                   | Unbounded — and a backend that cannot enforce a non-zero value refuses. |
| `models`     | `[{ id: {provider, name}, profile? }]`, in failover order.                                                            | Empty means **none**: a call spends money, so there is no "all".        |
| `tools`      | `allow[]`, `hide[]`, `exposure{}`, `budget{ max_definitions, max_bytes }`.                                            | `allow` unset means every tool the process assembled.                   |
| `delegation` | `targets[]`, `modes[]` (`sync`, `async`), `max_depth`, `max_concurrent`, `timeout`, `max_input_bytes`.                | Absent means no delegation at all.                                      |
| `escalate`   | `files.write[]`, `net`, `exec.env`, `ttl`, `max_per_session`.                                                         | Absent means a refused command stays refused.                           |
| `limits`     | `idle`, `max_sessions`.                                                                                               | **Required**: `limits.idle` must be positive.                           |

The three open/closed defaults are deliberate: files are open because a read is
recoverable and auditable, tools are open because which tools exist is a property of
the process, and models are closed because a call names a resource with no
"everything" spelling.

### `files`

Three combinations are refused rather than repaired:

- `readonly: true` together with a non-empty `write`;
- a narrowed `read` beside an unrestricted `write` — "look here, write anywhere" is
  never what an author meant;
- a `write` root that no `read` root covers.

A root must be absolute, non-empty, free of surrounding whitespace and not a glob.
`/` collapses that side to unrestricted, because listing everything and saying
nothing are the same permission and must produce the same digest. Normalization is
lexical, so a symlink and its target are two different roots.

`readonly` is a **tool-level** permission, not a mount: `exec("echo x > /tmp/f")`
does not go through the file tree. A stronger promise has to come from a backend
that mounts read-only, or one that refuses a write-restricting confine it cannot
enforce.

### `escalate`

A declared widening: the most a session may be widened to, for one command, after
somebody agrees. It sits above the ceiling rather than inside it, which is why a
narrowed session never carries it.

```yaml
grant:
  sandbox: bwrap # must be a backend that confines writes
  files: { read: [/srv/agent], write: [/srv/agent/out] }
  net: { mode: allow_list, allow_hosts: [api.openai.com] }
  exec: { env: { allow: [PATH] } }
  escalate:
    files: { write: [/srv/agent/out, /srv/agent/cache] }
    net: { mode: allow_list, allow_hosts: [api.openai.com, "*.github.com"] }
    exec: { env: { allow: [PATH, HTTPS_PROXY] } }
    ttl: 5m
    max_per_session: 3
```

| Rule                                               | Why                                                                                                                                                                                      |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Only `files.write`, `net`, `exec.env`              | Widening anything else changes what the session _is_, not where one command runs.                                                                                                        |
| `files.read`/`readonly` refused                    | An escalation widens writes only.                                                                                                                                                        |
| `files.write` must sit inside `files.read`         | Otherwise the widened grant is one the validator itself refuses.                                                                                                                         |
| `files.write` on an unrestricted write set refused | There is nothing to widen, and a list of roots would shrink it.                                                                                                                          |
| Every named dimension must cover the grant's own   | An escalation only widens. On a grant with a restricted write set, `files.write` restates those roots and adds to them — a declaration that widens only `net` still names the write set. |
| `ttl` and `max_per_session` required, positive     | A widening with no end is a new ceiling; an uncapped one is a prompt the model can summon.                                                                                               |
| The backend must confine writes                    | `local` does not: `escalate` on it is `NotAvailable`, not a warning.                                                                                                                     |

### `limits`

`limits.idle` is required and positive; `max_sessions` is optional. They are parsed
with the grant but are not permissions: narrowing a session clears them and the
digest ignores them. An idle timeout that nobody wrote is a session table that keeps
every conversation forever, which is why it has no default.

## What the loader checks

1. `version` is `1`, there is at least one agent, names are present and unique;
2. the grant normalizes — `files` invariants, `exec`, `resources`, `limits`,
   `delegation`, and `escalate` against the grant it sits on;
3. `grant.limits` is present and `limits.idle` is positive;
4. the program's `impl` is registered and its `settings` decode;
5. every hook's `impl` is registered and its `settings` decode;
6. against the built process: the sandbox exists, every named model is served, and
   an `escalate` block sits on a backend that confines writes.

Steps 1–5 are `Validation` errors — the document does not hold together. Step 6 is
`NotAvailable` — the document is fine, this process cannot serve it.

## Errors

```text
deploy: an agent document needs at least one agent
deploy: every agent needs a name
deploy: agent "assistant" is declared twice; a document names each agent once
deploy: agent "assistant" must state grant.limits (idle, and optionally max_sessions)
deploy: agent "assistant": grant.limits.idle must be a positive duration
deploy: agent "assistant": policy.escalate: ttl must be a positive duration
deploy: agent "assistant": program selects impl "grpah", which is not registered (graph)
deploy: agent "assistant" selects sandbox "bwrap", which this process does not configure (local)
deploy: agent "assistant" names model "openai/gpt-5", which this process does not serve (openai/gpt-4o-mini)
deploy: agent "assistant" declares grant.escalate on sandbox "local", which does not confine writes; there is no boundary to widen
agent: json: unknown field "engine"
```

The last one is the strict decode, named after the document the loader was reading
(`agent:` for this one, `runtime:` for the process document).
