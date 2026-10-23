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
    ├── craft/v1/craft.proto       # platform ↔ craft lifecycle ABI
    ├── workspace/v1/workspace.proto
    ├── sandbox/v1/sandbox.proto
    ├── secret/v1/secret.proto
    ├── checkpoint/v1/checkpoint.proto
    └── tool/v1/tool.proto         # a capability crafts/plugins provide
```

The directory path matches the proto package (`flowcraft.<area>.v<major>`), which
is what `buf lint` enforces.

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

Rust has nothing to commit: `rust/crates/flowcraft-proto/build.rs` compiles the
same files with `tonic-build` at build time.

The Go stubs land next to their Go packages (the directory named by
`option go_package`), so adding this tree did not change any import path.

## Changing a contract

- Additive within `v1`: new fields, new RPCs, new message types. Never renumber
  or reuse a field; reserve removed ones (`reserved 7; reserved "old";`).
- Breaking: add `v2` next to `v1` (`flowcraft/<area>/v2/`), serve both during a
  migration window, then remove `v1`. `buf breaking` (`PACKAGE` mode) enforces
  this.
