---
---
# Wire contracts

Everything that crosses a process boundary is defined here, and only here.
Proto files do not live in a language module: they are shared by the Rust
platform and the Go SDK, and a proto owned by one module inherits that module's
release cadence.

## Layout

```
proto/
├── buf.yaml                       # lint + breaking-change policy
├── buf.gen.yaml                   # Go codegen (Rust generates at build time)
├── generate.py                    # buf generate + copy next to the Go packages
└── flowcraft/
    ├── error/v1/error.proto       # the structured error every endpoint carries
    ├── message/v1/message.proto   # shared content model (parts, tools, deltas)
    ├── craft/v1/craft.proto       # platform ↔ craft lifecycle ABI
    ├── workspace/v1/workspace.proto
    ├── sandbox/v1/sandbox.proto
    ├── secret/v1/secret.proto
    └── tool/v1/tool.proto         # a capability crafts/plugins provide
```

The directory path matches the proto package (`flowcraft.<area>.v<major>`), which
is what `buf lint` enforces.

`error/v1` and `message/v1` declare messages only — no service. They describe
what other contracts put on the wire, so they have no endpoint of their own.

## Errors on the wire

Every endpoint reports failure as a gRPC status, and every FlowCraft endpoint
fills `Status.details` with one `flowcraft.error.v1.Error`. The category travels
in the payload, not only in the status code: two pairs of categories share a code
(`BUDGET_EXCEEDED`/`RATE_LIMIT` on `ResourceExhausted`, `POLICY_DENIED`/
`FORBIDDEN` on `PermissionDenied`), so a peer reading only the code would treat a
permanent budget failure as retryable and a policy refusal as a permission
problem.

Two layers, on purpose:

- `Kind` — the fourteen shared categories (`flowcraft-error` / `core/errdefs`).
  Authoritative when present; a peer falls back to the status code when a server
  sends no payload, so old peers and non-FlowCraft servers keep working.
- `Detail{domain, code, fields}` — a domain's own vocabulary. `(domain, code)` is
  the machine-readable key, `fields` holds bounded, redacted scalars.

Deliberately absent: any opaque or binary channel. Model output, prompts, and
credentials are not "error detail", and an error payload that can carry them
would be a leak surface by construction. A value that does not fit a short
string is a modelling problem, not a transport problem.

The `code` values are added, never renamed — a rename is a new code. `fields`
keys and `code` spellings are therefore part of the contract.

## What belongs here

- Contract between the platform and anything in another process (crafts,
  capability endpoints, plugins that speak gRPC).
- Anything whose wire compatibility must outlive a single module version.

## What does not

- In-process SDK types (agent, graph, tool registry): those are language types.
- Configuration documents (`platform.yaml`, `deploy.yaml`, `craft.yaml`): those
  are human-authored YAML and belong in `schema/` as JSON Schema.

## Generate

```bash
make proto-gen      # buf generate + copy into the Go packages
make proto-check    # regenerate and fail on drift
make proto-lint     # buf lint
make proto-breaking # buf breaking against main
```

Rust commits its bindings: `rust/flowcraft/src/proto/generated/` is produced by
`rust/tools/proto-gen` (`make rust-proto-gen`). They are checked
in so that anyone building the `flowcraft` crate — a consumer, docs.rs, `cargo
install` — needs neither `protoc` nor a build script. `make rust-proto-check`
fails on drift, the way `make proto-check` guards the Go stubs.

The Go stubs land next to their Go packages (the directory named by
`option go_package`), so adding this tree did not change any import path.

## Changing a contract

- Additive within `v1`: new fields, new RPCs, new message types. Never renumber
  or reuse a field; reserve removed ones (`reserved 7; reserved "old";`).
- Breaking: add `v2` next to `v1` (`flowcraft/<area>/v2/`), serve both during a
  migration window, then remove `v1`. `buf breaking` (`PACKAGE` mode) enforces
  this.
