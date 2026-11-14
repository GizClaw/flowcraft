# Pitfalls

Every message below is one the loader prints. `make check-skill` runs this skill's
validator over the templates, so a card that drifts from the code fails CI rather
than in somebody's deployment.

## Document-level

| Message | Cause |
| --- | --- |
| `manage: version 2 is not supported; this build reads 1` | `version` must be the integer `1`, not `"v1"` and not `2`. |
| `manage: the document declares no agents` | `agents` is an empty list. |
| `manage: agents[0]: name is required` | An agent has no `name`. |
| `manage: agents[0]: duplicate name "forge"` | Two agents share a name; a name is an address. |
| `manage: agents[forge].engine: kind is required` | The engine block exists but names no kind. There is no default. |
| `manage: agents[forge].engine: kind "grpah" is not registered; offered: a2a, graph` | The host did not register that kind. Registration is `deploy.BuiltinKinds()` plus the host's own. |
| `manage: agents[forge].runtime: at least one layer is required` | A local engine needs a runtime; a remote one must not have one. |
| `manage: agents[forge].runtime.layers[0]: path is required` | A layer entry without `path`. |
| `manage: catalog.scope "per_call" is not supported; use "run" or "session"` | Two values, not an open set. |
| `config: decode yaml config: json: unknown field "resources"` | The document is the current shape: `agents[]`, not the removed `resources:` map. Any unknown field is an error. |

## Runtime layer

| Message | Cause |
| --- | --- |
| `sandbox.backend is required; one of local, seatbelt, bwrap, windows, remote` | No backend. A boundary picked by guessing is a boundary nobody chose. |
| `sandbox.backend: unknown backend "docker"` | Five values; `remote` is how another machine is reached. |
| `files.roots is required: at least one absolute directory the tools may write` | Missing or empty roots. |
| `files.roots: "out" is not an absolute path` | Relative roots are refused: the meaning of a config must not depend on where the process started. |
| `sandbox.writes: permission: writable path 0 ("out") must be absolute` | Same rule for the write list. |
| `net.mode: unknown mode "denyall"; one of default, none, allow_list, proxy` | Note the underscore: `allow_list` (alias `allowlist`), `none`/`deny`/`deny_all`. |
| `net.allow_hosts is only meaningful with net.mode: allow_list (got "proxy")` | A host list beside a mode that does not use it. |
| `sandbox.cpu_millicores: backend "local" cannot enforce a cpu budget; drop it or pick a backend that samples the process group` | `local` enforces no cpu or memory cap. Use `bwrap`/`seatbelt`/`windows`, or drop the budget. |
| `sandbox.cpu_millicores` without `timeout` | The cpu budget is derived from the deadline, so the deadline is required. |
| `sandbox.bwrap is only meaningful with sandbox.backend: bwrap (got "local")` | A backend's own block on another backend. Same for `seatbelt` and `windows`. |
| `sandbox.remote is only meaningful with sandbox.backend: remote` | The reverse direction. |
| `sandbox.remote.endpoint is required with sandbox.backend: remote` | `transport: http` needs an `http(s)` URL. |
| `sandbox.remote.socket is only meaningful with sandbox.remote.transport: unix` | A socket path beside an endpoint is configuration that does nothing. |
| `sandbox.journal.ops: unknown op "chmod" (want create, write, rename or remove)` | Attribute changes and reads are never journal events. |
| `tools.enable: unknown tool "read_file"; one of files, exec, skills` | `enable` names tool **groups**, not individual tools. |
| `inference.providers[luna].driver: "opnai" is not available; offered: openai, anthropic` | The host compiles drivers in; the name must be one of them. |
| `inference.providers[0]: name is required` | A provider block needs the deployment identity that `models[].provider` refers to. |
| `inference.models[0]: model is required` | The API model name is missing. |

## Graph document

| Message | Cause |
| --- | --- |
| `graph: entry is required` | Nothing to start from. |
| `graph: nodes[0]: id is required` | A node without an id cannot be an edge endpoint. |
| `graph: node "model": kind "modle" is not registered` | An unknown node kind; the registry is built by the host. |
| `graph: invalid condition "len(pending) >": ...` | Conditions compile at build. |
| `graph: output: key is required` | An `output` block without a key. |
| `graph: read ./graphs/assistant.yaml: no such file or directory` | `settings.path` is resolved against the process working directory, not the deployment document. Run the host (and the validator) from the directory the document lives in. |

## Shapes that load but do not work

| Symptom | Cause |
| --- | --- |
| A turn runs but the model has no tools | `tools.enable` names groups; the group's projection (`tools.dynamic`) may also hide them. |
| `writes` has no effect | `backend: local` has no boundary and `backend: remote` enforces it on the server — both warn. |
| A graph loops forever | `limits.max_steps` is unset, so only the caller's context bounds it. |
| A carried state key grows every turn | `carry: true` re-reads the key from the store each turn; a long conversation belongs in one task, not a carried key. |
| A journal reports nothing | Either the platform has no watch source (the build fails instead) or `exclude` covers everything. |
