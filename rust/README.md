# FlowCraft platform (Rust)

`flowcraftd` is the platform daemon: a code-first kernel, a local composition
graph, and the capability endpoints other processes call. The agent loop lives
in the Go `craft` SDK, not here; the two sides speak protobuf.

Status: **four capabilities are end to end** — workspace, secret, sandbox, and
tool — each with a provider, a grant-authenticated gRPC endpoint, and a remote
client presenting the same contract. Checkpointing stays in the craft SDK: an
agent owns the state it can resume from, so the platform does not provide a
checkpoint store. Endpoints are unix sockets by
default and TLS/mTLS over TCP when a `dns://` target is configured. `SIGHUP`
reloads the deployment as a new generation; a bad document never disturbs the
running one, and new TLS material is picked up by the reload. `craft.Craft/v1`
is the ABI to the craft sidecars that run the agent loop, and `admin.sock` is the
operator surface. `${secret:...}` resolves
against a `secret.Store` built in the same deployment graph. Everything else is
listed under [Not implemented yet](#not-implemented-yet).

## Layout

Two packages, because the published unit is the library:

```text
rust/
  Cargo.toml                 workspace, shared deps, lints
  rust-toolchain.toml        pinned toolchain (1.93)
  flowcraft/                 the library — everything a consumer depends on
    src/lib.rs               module map and the feature table
    src/error/               error taxonomy: stable kinds + status mapping
    src/telemetry/           telemetry pipeline: field vocabulary, text/JSON
                             rendering, the stderr split, the subscriber host
    src/proto/               wire bindings for every contract under proto/,
                             committed under generated/ (see `make rust-proto-gen`)
    src/message/             the hand-written half of `flowcraft.message.v1`:
                             shape rules, text parts, definition size
    src/search/              text retrieval primitives: tokenizer + BM25
    src/resource/            deployment document, strict decode, and the
                             composition kernel: Plan / Build / Start / Close
    src/kernel/              control plane: config, catalog, grants, listeners,
                             bootstrap secrets, TLS, transport
    src/client/              client plumbing: grant-carrying `Wire`, gRPC
                             auth/status helpers, the craft ABI client
    src/caps/workspace/      workspace: local provider (cap-std confined), scoped
                             and prefixed decorators, helper operations, endpoint
                             server, remote client
    src/caps/secret/         secret: memory/env/file/keychain providers, endpoint,
                             remote client
    src/caps/sandbox/        sandbox: local process runner, endpoint, remote client
    src/caps/tool/           tool: capability (endpoint/remote/MCP) + agent-side
                             framework (registry, assembly, middleware, exposure,
                             discovery, executor)
  flowcraft-platformd/       the `flowcraftd` binary, and the supervisor, admin
                             and craft-hosting surface only it needs
  tools/proto-gen/           regenerates `flowcraft/src/proto/generated/` from
                             `proto/` (`publish = false`, a repository tool)
  examples/workspace/        a runnable platform.yaml + deploy.yaml
  examples/capabilities/     workspace, secret, sandbox, tool + `${secret:...}`
```

The layers inside `flowcraft` used to be crates of their own. They are modules
now, because the crate — not the layer — is the unit `cargo add` and crates.io
see: one version, one API page, one publish. A split whose only job was to prove
which layer another layer may use cannot justify eleven version numbers.

The one-way edges survive in two places: the `dep:` entries of the feature table,
and `flowcraft/tests/layering.rs`, which reads the `crate::` paths each module
reaches for and compares them to the table below.

```text
error                leaf, always compiled (its only dependency is `thiserror`)
telemetry            → error
proto                leaf (generated, committed)
message              → error, proto
search               → error
resource             → error, telemetry
kernel               → error, telemetry, resource
client               → error, telemetry, proto, kernel
caps::*              → error, resource, kernel, client, proto (+ search in tool)
flowcraft-platformd  → flowcraft (features = ["caps"])
```

Public API, unchanged for consumers:

```rust
use flowcraft::error::{Error, ErrorKind};
use flowcraft::resource::{Builder, Deployment};
use flowcraft::client::Wire;
use flowcraft::caps::workspace::Handle;
```

One rule held: nothing depends on the library except the daemon. Inside the
library a module may reach *down* only — and `proto` and `error` reach nowhere.

### Features

`default = ["platform"]`: the composition plane, which is what a program that
hosts FlowCraft wants.

| feature | brings |
| --- | --- |
| `platform` | `telemetry`, `proto`, `search`, `resource`, `kernel`, `client` |
| `caps` | `platform` + `workspace`, `secret`, `sandbox`, `tool`, `wire` |
| `workspace` | the provider, decorators and helpers; **no** control plane |
| `secret` / `sandbox` / `tool` | one capability, endpoint and remote client included |
| `wire` | the gRPC endpoints, the remote clients, the grant-scope checks |
| `otel` / `otlp` | trace context; shipping it to a collector |
| `telemetry`, `proto`, `message`, `search`, `resource`, `kernel`, `client` | one layer each |

Two shapes are load-bearing enough to be gated by tests: `otel` without `otlp`
must carry no runtime and no HTTP stack (`flowcraft/tests/telemetry_deps.rs`),
and `--no-default-features --features workspace` must build without the control
plane (`make rust-windows-check`, plus the `rust-windows` CI lane).

### Platform support

The kernel's transport is unix-domain sockets, so the platform as a whole is
unix-only. One capability is portable in isolation, and it is the one whose
provider is most likely to be reused: `--no-default-features --features
workspace` drops `wire` (the endpoint, the remote client and the grant scope) and
leaves the provider, its decorators and the helper operations — a shape with no
control-plane dependency that builds for a Windows target.

`make rust-windows-check` cross-compiles it locally (it needs the
`x86_64-pc-windows-gnu` target and a mingw gcc), which is the fast feedback loop;
the `rust-windows` CI lane runs the same shape on a Windows host, so the
`cfg(windows)` branches — the sharing-violation retry, the refusal to replace a
directory with `MoveFileEx` — are executed, not merely compiled. That is still
not a Windows support claim: the daemon, its transport and the other capabilities
remain unix-only.

## Quickstart

Prerequisites: the pinned toolchain (`rust-toolchain.toml`; `rustup` installs it
on first use). Not `protoc`: the wire bindings under `flowcraft/src/proto` are
committed, so a build — yours, docs.rs', or `cargo install`'s — reads them as
ordinary source. `make rust-proto-gen` regenerates them from `proto/`, and
`make rust-proto-check` fails if they went stale.

```bash
cd rust
cargo test                       # 778 tests, no network, no credentials

cargo run -p flowcraft-platformd -- catalog
cargo run -p flowcraft-platformd -- check \
  --config examples/workspace/platform.yaml \
  --deploy examples/workspace/deploy.yaml

cargo run -p flowcraft-platformd -- serve \
  --config examples/capabilities/platform.yaml \
  --deploy examples/capabilities/deploy.yaml
# SIGTERM / Ctrl-C shuts it down in reverse construction order
# SIGHUP reloads deploy.yaml as a new generation
```

`check` is the preflight: it parses both documents, injects the platform-owned
external dependencies, and runs the whole plan — types, dependency edges,
settings, cycles — without opening a socket. Exit codes: `0` ok, `2` invalid
document or configuration, `3` unreadable file, `1` internal.

## Logging

One record is one line. `flowcraft::telemetry` owns the vocabulary, the rendering,
and the routing; a host installs the pipeline once and keeps the returned guard
until exit.

```bash
flowcraftd serve ... --log-format json   # one JSON object per line
flowcraftd serve ... -v                  # debug
flowcraftd serve ... --profile client    # client-daemon defaults: text + ring
flowcraftd check ... -q                  # warnings and errors only
RUST_LOG="info,flowcraft::kernel=debug" flowcraftd serve ...   # wins over flags
```

A target is a module path inside the library — `flowcraft::caps::sandbox`, not
`flowcraft_cap_sandbox` — so `targets:` in `platform.yaml` and `RUST_LOG` spell
the module. `flowcraft::logerr` is the crate-root macro every layer reports
failures through.

Durable policy belongs in `platform.yaml`, under `log:` — the level there is
applied at startup, while `format`, `file` and `ring_bytes` decide how the
process was built, so a change to those asks for a restart instead of being
silently ignored:

```yaml
log:
  level: info
  targets:
    flowcraft::caps::sandbox: debug
  format: json
  file: /var/log/flowcraftd/flowcraftd.log
  file_max_bytes: 8388608     # rotate at 8 MiB (0 disables rotation)
  file_backups: 3             # keep .1 … .3, then overwrite the oldest
  ring_bytes: 262144          # recent output kept in memory for `diag`
  redact: true                # replace credential-looking field values
  craft_stdio: ""             # "" = capture for `exec`, inherit for `systemd`
  throttle_per_minute: 60     # cap for repeated warnings/errors; 0 disables
```

The profile follows the command, so an operator command stays quiet by default:

| | `serve` / `craft-stub` | `check` / `catalog` / `ctl` |
| --- | --- | --- |
| level | `info` | `warn` |
| format | JSON, text when stderr is a terminal | text, no timestamp or target |
| stream | `< warn` on stdout, `>= warn` on stderr | same |

Every record carries the resource fields — `service.name`, `service.version`,
`flowcraft.install`, `flowcraft.install.scope`, `flowcraft.session`,
`flowcraft.principal` — so a line from a user's machine can be tied back to a
machine without the machine name ever being recorded. The install identity is a
random ULID in one machine-wide file (`/var/lib/flowcraft/identity.json`, written
once by the installer); `flowcraftd` reads it, and falls back to a per-user file
labelled `flowcraft.install.scope=user` when it is not there.

## Trace context

Every record emitted inside a span carries the W3C `trace_id` and `span_id` of
that span, and a call carries them to the next process:

```text
... INFO flowcraft_platformd::supervisor generation swapped flowcraft.gen=2 \
    trace_id=522ca6a8f6d48a6cd3511c1f15878dba span_id=98e21d9247a61f87
```

- `flowcraftd` spans `generation.reload` and `craft.start`; every endpoint wraps
  a request in an `rpc` span.
- `flowcraft::client` stamps `traceparent` next to the grant on every capability
  and craft call; the endpoint middleware attaches it as the parent, so a
  capability call is part of the operation that made it.
- A request without the header starts a new root trace rather than inventing a
  parent.

The ids, the span lifecycle and the `traceparent` encoding are the
**OpenTelemetry SDK's** (`opentelemetry`, `opentelemetry_sdk`,
`tracing-opentelemetry`). `flowcraft::telemetry` supplies the resource attributes
(`flowcraft.install`, `flowcraft.session`, …) and reads the current span's ids
for the record renderer; it does not implement the protocol.

### Shipping to a collector (`--features otlp`)

```toml
# flowcraft-platformd/Cargo.toml
[features]
otlp = ["flowcraft/otlp"]
```

```yaml
log:
  otlp:
    endpoint: "http://otel-collector.monitoring:4318"   # base URL; paths appended
    headers: { "x-honeycomb-team": "${secret:HONEYCOMB_KEY}" }
    insecure: false
```

That adds `opentelemetry-otlp` (HTTP/protobuf), the log bridge and a runtime,
and ships both signals:

```text
POST /v1/logs    559 bytes
POST /v1/traces  892 bytes
```

The feature is **off by default** because a client binary has nowhere to send to,
and `flowcraft/tests/telemetry_deps.rs` pins both halves: the build with `otel`
and without `otlp` has no runtime, no HTTP stack and no exporter, and
`--features otlp` has them.

A field whose *name* looks like a credential (`token`, `secret`, `password`,
`authorization`, …) has its value replaced by `<redacted>` before it is rendered,
so an accidental leak at a call site does not become a leak in the file. That is
a backstop, not a licence: a secret must still not be passed under a name that
hides what it is.

When an `error!` is recorded, the ring is written once to `<log>.last-error`
(at most once a minute), because the records that explain a failure are exactly
the ones a crashing process cannot hand over later.

`flowcraftd diag` writes a support bundle from a running daemon: the in-memory
ring, the pipeline manifest, and the capability catalog. It contains no grant
token, no secret value and not the identity salt.

```bash
$ flowcraftd diag --socket /run/flowcraft/admin.sock --out ./bundle
support code: 01M5KE3M
bundle: ./bundle          # README.txt, telemetry.log, manifest.json, catalog.json
```

### Collecting without the exporter

Records are already structured (JSON, one object per line, dotted keys, W3C
`trace_id`), so a deployment can collect them the way it collects any file or
stdout stream — an OTel Collector `filelog`/`stdout` receiver maps them without a
bespoke exporter, and that path needs neither the feature flag nor a runtime.
The OTLP exporter above is the other option, and it stays opt-in for the reason
given there: the pipeline's default build carries no async runtime and no HTTP
stack, and `flowcraft/tests/telemetry_deps.rs` pins both halves of that split —
nothing in the `otel` closure, the exporter and the log bridge under
`--features otlp`.

The support code is the first eight characters of the machine identity, short
enough to read out and specific enough to find that machine's records.

## How the pieces fit

```text
platform.yaml   host config     operator-owned, restart to change
   │            endpoints, target allow-list, grant TTL
   ▼
Platform        kernel          catalog → issuer → listener factory
   │
deploy.yaml     composition     resources + entries + externals
   │
Builder.plan    pure check      every error at once, nothing constructed
Builder.deploy  topological     build → start → (reverse) shutdown
   │
capability      endpoint        unix socket, grant-authenticated gRPC
   │
remote client   resource        same contract, different transport
```

Two rules carry most of the design:

- **The kernel is not a document.** A tenant document cannot replace the
  catalog, widen the target allow-list, or change a grant's lifetime.
- **Capabilities are services.** A remote provider is not a factory registered
  from the far side; it is a local client resource (`impl: remote`) talking to
  a service, with a grant minted by the platform.

## What is implemented

| Piece | State |
| --- | --- |
| Deployment document (JSON + YAML, strict, single document) | ✅ |
| YAML key-collision rejection, YAML 1.2 scalar semantics | ✅ |
| Plan: kind lookup, declared/supplied deps, type + contract checks, cycles, all-errors | ✅ |
| `Phase::Pre` ordering, `Startable`/shutdown lifecycle | ✅ |
| `${env:...}` expansion (string form) | ✅ |
| `${secret:NAME}` / `${secret:ID.NAME}` expand to a lazy reference, resolved at use time | ✅ |
| Secret stores: `id` / `default: true`, several stores per deployment | ✅ |
| Secret values never print: `Secret` renders `<secret>` in `Debug`/`Display`/serde | ✅ |
| Resolved secrets cached per store (TTL + bounded, negative results included) | ✅ |
| Host-injected secret store (a store the process owns, not the document) | ✅ |
| Externals (typed, injected, contract-checked) | ✅ |
| Catalog (serializable, from the registry) | ✅ |
| Kernel config: `strict`/`trusted`, `allow_targets`, endpoint catalog, TTL | ✅ |
| Grants: Ed25519 compact tokens, expiry, per-grant and per-session revocation | ✅ |
| Unix endpoint listeners, owner-only socket permissions, stale-socket recovery | ✅ |
| Workspace: local provider, gRPC endpoint, remote client, scope (root/write) | ✅ |
| Secret: memory/env/file/keychain providers, gRPC endpoint, remote client, scope (prefix/prefixes) | ✅ |
| Secret writes: `Mutable` (set/delete/delete-prefix) and the app-side keychain `Manager` | ✅ |
| Checkpoint: local directory store, gRPC endpoint, remote client, scope (prefix) | ✅ |
| Sandbox: local runner, byte-cursor output with replay and gaps, SIGTERM→grace→SIGKILL, six exit reasons | ✅ |
| Sandbox sessions: blocking `read`, `write`, `close_input`, `resize`, `signal`, `watch`, `close`, per-session features | ✅ |
| Sandbox pty: ConPTY is not wired yet; unix ptys are (merged stream, resize, Ctrl-C as `0x03`) | ⚠️ |
| Sandbox write boundary: `root` + `writable_paths` + `readonly_root`, symlink-safe containment, `bwrap`/`seatbelt` profiles | ✅ |
| Sandbox limits: cgroup v2 memory ceiling and cpu budget (probed; elsewhere reported as unavailable) | ✅ |
| Sandbox policy: allow-list with shell unwrapping, approval predicates, tightening defaults, read-only classifier, escalation | ✅ |
| Sandbox network: deny-all, allow-list/proxy via the enforcement proxy (terminates CONNECT + HTTP + SOCKS5 for the child; upstream chaining is not implemented), bwrap bridge | ✅ |
| Sandbox file journal: net-state folding, retention ring with gaps, three RPCs, remote reader | ✅ |
| Sandbox endpoint: describe/start/read/write/close-input/resize/signal/watch/wait/list/terminate/close-runner/journal, scope (commands, timeout ceiling, output bound) | ✅ |
| Shared endpoint plumbing: bearer auth, capability/tenant check, status mapping, lazy client | ✅ |
| TLS / mTLS: per-endpoint cert/key/client_ca, bootstrap secrets from env or a directory | ✅ |
| Rotation: changes under `bootstrap.dir` reload automatically (debounced, off while draining) | ✅ |
| Generation reload (`SIGHUP`): parse → build → start → swap → drain, one at a time | ✅ |
| `craft.Craft/v1`: Describe/Start/Run(stream)/Steer/Cancel/Renew/Close client | ✅ |
| ABI enforcement: version negotiation, envelope types, capability namespaces, bootstrap token, 0600 sockets | ✅ |
| ABI conformance suite (`tests/conformance.rs`, reusable against any craft) | ✅ |
| `ExecSupervisor`: spawn a craft, wait for its socket, open a session, stop it | ✅ |
| `flowcraftd craft-stub`: a minimal craft for development and tests | ✅ |
| Hosted crafts: `crafts:` config, per-craft grants, start/stop lifecycle | ✅ |
| Craft supervisors: `exec` (own the process), `external` (connect only), `systemd` (unit) | ✅ |
| Host-level preflight: unknown endpoints and capability mismatches fail `check` | ✅ |
| Layered deployment documents: repeated `--deploy`, later layers win | ✅ |
| `entries:` surfaced: `serve` logs them, admin `status` reports them | ✅ |
| Admin socket + `flowcraftd ctl`: status / reload / drain / crafts / revoke, owner-only | ✅ |
| Drain: readiness off and no new grants minted; existing leases keep working | ✅ |
| Sandbox confinement: `bwrap` (Linux) and `seatbelt` (macOS) process wrappers, with the enforcement proxy | ✅ |
| Graceful drain: listeners stop accepting and finish in-flight requests on shutdown | ✅ |
| Grant renewal: remote clients re-mint before expiry, hosted crafts push `Renew` | ✅ |
| `flowcraftd serve` / `check` / `catalog` | ✅ |
| Graceful SIGTERM/SIGINT shutdown | ✅ |

## Not implemented yet

1. **Packaging** — static musl builds, a systemd unit, a distroless image, and
   Helm. The binary already runs under a service manager by hand.
2. **Windows** — the library is feature-split so it *builds* there, but no Windows
   provider exists yet: no job objects, no ConPTY, no token-based write
   confinement, no AppContainer. `Capabilities` reports the gap honestly (a
   `tty` request on Windows is `NotAvailable`, not an empty session). The planned
   phases are in the local Windows working note.
3. **Upstream proxy chaining** — a `sandbox` net policy with `proxy: http://…`
   is refused at build time rather than silently dialling direct. `allow_list`
   and `proxy` mode work as a deny-list proxy with the enforcement proxy.

## Design notes and deviations from the Go side

- **Typed edges are checked by `TypeId` plus an optional `contract` string.**
  Go compares interface satisfaction; Rust cannot, so a factory records its
  concrete output type at registration and `Plan` compares it to the
  requirement before anything is built.
- **Capability values are wrapped in handles** (`workspace.Handle`). Go's
  "concrete type satisfies interface" lets one requirement name an interface; a
  Rust `TypeId` is exact, so the requirement names the handle and every provider
  returns it. Substitution still works; the wrapper is the price.
- **Every built value implements `Resource`** with no-op `start`/`shutdown`
  defaults, instead of Go's optional `Startable` interface assertion. It costs a
  trait impl per type and removes the "forgot to forward the capability" class.
- **Remote clients connect lazily.** Build runs before start, so connecting at
  construction would race the endpoint's listener. `kernel::Wire` owns the
  grant, the lazy channel, and the bearer metadata for every capability.
- **`${secret:...}` stays a reference until it is used.** Secret providers
  declare `Phase::Pre`, the kernel reports every built value to a build
  observer, and the daemon attaches it to one `SecretTable` with its settings
  `id` and `default` flag. Expansion emits a `{"$secret": {...}}` marker, which
  a settings field typed `Secret` decodes; the value is looked up when the
  consumer needs it and cached per store (60 s, 256 names, negative results
  included). `${secret:NAME}` uses the one store marked `default: true`,
  `${secret:ID.NAME}` names a store by `id` (or its resource name), and a dotted
  name whose prefix is not a store stays a name in the default store. Anything
  that reads a secret is therefore `main` phase and builds later. A host that
  owns a store of its own injects it with
  `assembly::BuildOptions::with_default_secret`.
- **Documents layer; the merge is last-wins.** `--deploy` is repeatable: each
  later file replaces a resource or external by name, appends entries, and may
  bump the version. Each layer is decoded strictly but validated only after the
  merge, so a partial overlay is legal — and a reload re-reads every layer.
- **Drain stops minting, not serving.** The admin `drain` command turns
  readiness off and makes `Platform::grant` return `NotAvailable`; leases that
  were already issued stay valid until they expire or are revoked, so in-flight
  work finishes instead of failing mid-step.
- **Capabilities are reported, not defaulted.** A remote provider that cannot
  reach its endpoint returns the error; an empty capability set would silently
  downgrade what a caller thinks is guaranteed.
- **Leases are renewed, not permanent.** A remote client re-mints its grant once
  it is within 60s of expiry and stamps the fresh token on the next request;
  a hosted craft gets a `Renew` push at half the platform TTL. Without this a
  daemon would simply stop working when the first lease elapsed — the TTL is a
  security property, not a lifetime.
- **Confinement is a wrapper, not a second runner.** `bwrap`/`seatbelt` rewrite
  the argv and leave env filtering, timeouts, process-group kill, and output
  bounds to the local runner. Only what the wrapper actually enforces is
  promoted into capabilities: filesystem bounds, `deny-all` networking, and
  read-only writes. `seatbelt` is deprecated upstream but remains the only
  per-process confinement macOS ships without entitlements.
- **Shutdown drains.** A listener stops accepting and waits for in-flight
  requests (`serve_with_incoming_shutdown`), bounded by a 10s grace; only then
  does the process exit. Reload takes the same path for the generation it
  replaces.- **The admin surface is a socket, not a port.** `admin.sock` is owner-only
  (0700 directory, 0600 socket) and speaks one JSON line per request. `drain`
  clears readiness without tearing anything down, `reload` is the same path
  `SIGHUP` takes, and `revoke` takes a session id. A failure to bind the admin
  socket warns and continues: it is an operator convenience, not a dependency.
- **Preflight is split in two on purpose.** The resource kernel checks types,
  dependencies, and cycles; the daemon checks what only the host knows — whether
  a `settings.endpoint` name resolves and whether that endpoint serves the
  capability the resource claims. Both run in `check`, so a typo no longer
  passes the preflight and fails with the daemon already up.
- **Certificate rotation is a reload, and the directory is watched.** TLS
  material is read when an endpoint binds, and a reload binds a new generation,
  so replacing the files is enough. When `bootstrap.dir` is set the daemon
  watches it by default (`bootstrap.watch: false` disables): events are
  debounced by 500ms so a cert+key pair written separately is one rotation, and
  changes are ignored once the daemon is draining. `SIGHUP` remains the manual
  equivalent.
- **Crafts are hosted, not part of a generation.** `crafts:` is host config: on
  boot the daemon launches each process, waits for its socket, opens a session,
  and mints one grant per requested capability (validated against the catalog at
  startup). A `SIGHUP` swaps resources but leaves crafts alone, and shutdown
  closes every session before revoking the platform session.
- **Three interfaces, three owners.** The lifecycle ABI (`Craft`) is the
  platform's: a craft implements it exactly, and the platform refuses a craft
  whose `abi_version` or capability namespace does not fit (private
  capabilities must be `<vendor>.<craft>.<thing>/vN`). The *business* interface
  is the craft's, but only as typed `flowcraft.message.v1` messages/deltas, or
  inside an extension whose `type` is declared in `CraftInfo.input_types` /
  `event_types` (`x-` is the extension escape hatch). The platform never
  parses extension bytes; it validates the envelope, routes, replays, and
  audits.
- **Every call after Describe carries a bootstrap token.** The platform mints
  one per craft, hands it to `exec` through the environment, and sends it as
  `x-flowcraft-craft-token`; the socket is chmod 0600 and its directory 0700,
  so the craft's control plane is not open to other local users.
- **The craft boundary is an ABI, not a shared process.** The platform owns the
  session and mints the grants; the craft owns its agent loop and internal
  graph. `CraftHandle::run` is a server-streaming turn with application-level
  credit. Conversation payloads are typed (`flowcraft.message.v1`), so the turn
  and tool results share one schema; only an `Extension` body is opaque, and a
  craft must declare its type before the platform carries it.
- **Reload is a generation swap, not a mutation.** `SIGHUP` parses and plans
  first (nothing is created), builds and starts the new graph, swaps it in, then
  drains the old one under a bounded grace period — every resource's `shutdown`
  runs, not just the listeners. A parse or plan failure keeps the running
  generation untouched; a drain that exceeds the grace is logged and reported as
  `drained: false` in the reload outcome (and by `ctl`), retried once at
  shutdown, and never fails the reload, because the swap already happened.
  `platform.yaml` changes still require a restart, because endpoints, TLS, and
  grant policy are kernel state.
- **TLS material never enters a document.** A `tls` block holds
  `${secret:...}` references only; the bytes come from the host's bootstrap
  provider (environment or a directory). The kernel refuses inline PEM at
  startup, and a `dns://` endpoint without TLS is rejected rather than served in
  the clear. One block configures both directions: `cert`/`key`/`client_ca` for
  the listener, `ca`/`client_cert`/`client_key`/`server_name` for the client.
- **A sandbox runner declares only what it enforces.** The local runner reports
  `env_allow_list` and a pty, but no `filesystem_bounds`, and a spec that asks for
  a policy it cannot enforce is rejected with `NotAvailable` — never quietly
  downgraded. Capabilities are fetched lazily: a remote runner asks its server on
  first use and refuses to fall back to an empty set.
- **Sandbox output is pulled, not pushed.** `read(after_seq, max_bytes)` returns
  sequenced chunks, so a slow consumer cannot stall the child. `wait` joins the
  output pumps before marking EOF, so the last chunks are never lost to a race.
- **The tool framework lives with the capability, in modules.** `cap-tool`
  carries both the cross-process contract (endpoint/remote/scope/MCP) and the
  in-process framework ported from `core/tool`: registry with conflict policy,
  exposure/visibility, the bounded discovery pool, `tool_search`, and a
  middleware executor with panic recovery. The ranking kernel (tokenizer +
  BM25) lives in `flowcraft::search`, because skills and memory use the same
  splitter and scoring. The layers stay separate: the wire
  side never calls the policy side, and `LocalSource` is the seam between them.
- **MCP is an edge adapter, not the internal wire.** `tool.Source/mcp` attaches
  stdio or streamable-HTTP servers and maps `tools/list` / `tools/call` onto the
  local contract: names are prefixed `<server>__`, text/image/audio/resource
  blocks keep their typed part, and anything else falls back to a JSON data part
  instead of being dropped. Optional servers (`required: false`), per-call
  deadlines (`http_timeout`) and the MCP resource bridge (`resources: true`)
  are supported. Connection is lazy: a required server is awaited for up to
  `connect_timeout` (default 30s) during build, an optional one connects in the
  background and publishes its tools through the registrar, and a tool whose
  server has not connected yet connects on first use. A failed call reconnects
  the server once, and a successful reconnect re-lists its tools and reconciles
  the registry (new and changed entries replace their old ones, removed ones
  disappear). Liveness pings run every 15s by default (`liveness: off`
  disables); peers on protocol 2026-07-28+ are skipped because SEP-2575 removed
  the method, and a failed ping reconnects.
- **Assembly is the composition point.** `Assembly` builds a registry, appends
  `tool_search` when dynamic injection is enabled, and installs the session
  recorder. An assembly is long-lived and stateless per run: the per-run session
  travels in `CallContext`, the Rust counterpart of Go's ambient context.
- **A registry is a resource.** `tool.Registry/memory` merges many
  `tool.Source` deps (`tool`, `tool.*`) into one registry and exposes it as a
  `tool.ToolSource`, so a document can wire sources to an endpoint without code.
- **Middleware is a set of small policy layers.** Recover, telemetry, result
  limit, recoverable truncation, timeout, concurrency, rate limit, redaction,
  audit and approval live in `middleware/`, with a settings parser for the
  declarative subset.
- **A tool source speaks the conversation schema.** `List` returns
  `flowcraft.message.v1.ToolDefinition` and `Execute` returns
  `flowcraft.message.v1.Content` — the same messages a craft turn carries. The
  endpoint validates the catalog it serves (names, schemas, duplicates) and
  returns it in name order; `Execute` checks that the arguments are a JSON
  object before charging the call budget, so a malformed request never reaches
  a provider or spends a lease. The grant scope is an allow-list plus a
  `max_calls` budget; the budget is keyed by the session, so renewing the lease
  (a fresh token) does not refill it, `List` is free, and an `Execute` attempt
  counts even when it fails. The registry is programmatic (a tool is code, not
  YAML), so there is no `tool.Source/local` document factory.
- **Grants are validated offline** by the endpoint (no introspection call).
  Revocation is local to the daemon; cross-daemon revocation is deliberately
  left to short TTLs.
- **The YAML stack is `saphyr` plus a hand-written conversion.** A serde YAML
  backend was rejected: the maintained forks silently last-win on duplicate
  keys, and non-string keys collapse onto one JSON key in a way that depends on
  iteration order. Collisions are rejected here.
- **Unix only for now.** The config parser accepts `dns://` targets but the host
  refuses to serve them, so an unsupported deployment fails loudly rather than
  degrading.

## Publishing

Two packages, one version: `version` is written once, in
`[workspace.package]`, and both packages read it.

```bash
cd rust
cargo publish -p flowcraft --dry-run     # unpacks the package and rebuilds it
cargo publish -p flowcraft
cargo publish -p flowcraft-platformd
```

`--dry-run` is the check that matters. It builds from the tarball, which is the
only thing that proves the crate is self-contained — and it is why the wire
bindings are committed rather than generated: a `build.rs` reading `../../proto`
would publish a crate that cannot compile, and every consumer would need
`protoc`. `cargo install flowcraft-platformd` therefore needs nothing but a
Rust toolchain.

Order is not a suggestion: the daemon names the library by version, so the
library has to be on the registry first. A second publish that fails with "no
matching package named `flowcraft`" means the index has not caught up — wait and
retry, never `--no-verify`.

Three consequences of "one version, two packages":

- a patch to the daemon alone still bumps the library, because the number lives
  in one place;
- the wire contract is part of the library's public API (`flowcraft::proto`), so
  a breaking `.proto` change is a breaking library change; `0.x` minors are
  where that is allowed to happen;
- `flowcraft` ships no binaries, so `cargo install flowcraft` has nothing to
  install — the daemon is the installable thing. The in-namespace bridge is not a
  second artefact either: `flowcraftd` re-executes itself with
  `--flowcraft-sandbox-bridge`, and a host that embeds the sandbox capability does
  the same by calling `bridge::run_if_invoked()` before it looks at `argv` (see
  `caps::sandbox::net::bridge`).

## Tests

```text
cargo test          # unit + integration (real UDS, real grants)
cargo clippy --all-targets
cargo fmt --check
```

The integration tests cover the properties that matter most:

- plan reports *every* problem at once and constructs nothing;
- a failed build shuts down what it already built, in reverse;
- `pre`-phase resources build first and cannot consume `main`;
- the remote client reads and writes the same bytes as the local provider;
- scope root and read-only scope are enforced per call;
- a revoked session cannot reach the endpoint;
- `check` exits `2` and names every bad path.

## References

- Wire contracts: `proto/flowcraft/…`, generated into `flowcraft::proto` and
  committed there (`make rust-proto-gen`).
- Go side: `core/resource`, `core/platform`, `core/workspace`.
