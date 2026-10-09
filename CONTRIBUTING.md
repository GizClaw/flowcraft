# Contributing to FlowCraft

Issues, focused pull requests, and draft RFCs are welcome. For substantial API
or architecture changes, open an issue first so the module boundaries and
compatibility impact can be agreed before implementation.

## Before opening a pull request

Run from the repository root:

```sh
make ci
make release-check
git diff --check
```

Add tests for changed behavior and format Go files with `gofmt`. Commit
messages use Conventional Commits with a module scope, e.g.
`feat(driver/openai): add qwen provider` or `fix(core): harden streaming`.
Keep changes within module boundaries.

## Modules and dependency order

The independently versioned library modules are `core`, `craft`, `driver/*`,
and `backends/*`.
The Go workspace also includes `examples/forge` and `examples/anvil` (runnable
local demos, not released), while `tools/releasegate` builds with `GOWORK=off`
against pinned releases.

Module dependency order:

```text
core -> craft / driver/* / backends/*
```

- `core` is the platform module and depends on nothing in-tree.
- `craft` assembles an application over `core`: the `craft.yaml` definition,
  compile-time capabilities, the MCP plugin host and host primitives, and the
  process-level manager. It is the second module the release gate manages.
- `driver/*` provides provider inference adapters over `core`.
- `backends/*` provides the platform-side implementations over `core` (the
  SQLite checkpoint store and the memory backend today); sandbox backends live
  in `core/sandbox`.

## Declaring a module release

A pull request may merge without a release intent. When its changes should
publish one or more library modules, add a new immutable
`.release/<descriptive-name>.json` file:

```json
{
  "summary": "Add streaming retries",
  "releases": [
    {
      "module": "core",
      "bump": "patch"
    }
  ]
}
```

Allowed bumps are `patch` and `minor`; pre-1.0 breaking changes use `minor`.
Multiple pending changesets for one module are aggregated to the highest bump.
Never edit, rename, or delete a merged changeset. Add another changeset to
correct release intent. `module` is `core` or `craft`; the release gate rejects
any other value, and a changeset may name both when they release together. A
module that has never been tagged has no version cell in `CHANGELOG.md` yet: its
row carries `-` until its first release fills it in.

### Coordinated releases

When dependent modules release together, update their `go.mod` requirements to
the versions being planned in the same batch. Check the exact versions with:

```sh
make release-plan
```

Move each dependent's `go.mod` requirement to the version planned in the same
batch; a pin left on the previous tag ships a release built against the old
dependency, and `craft` requires `core`, so a core + craft batch is tagged
`core` first. The release gate rejects a module whose same-batch dependency pins
do not match the planned versions, and rejects an untidy `go.mod`.

A dependent that imports packages no published dependency tag contains cannot
be tagged on its own at all: `GOWORK=off go build ./...`, which the gate runs
per module, fails before the pin is ever consulted. Ship it in the same batch
as the dependency release that adds those packages (`craft` and `core` are the
current pair; the details are in `.release/README.md`).

Before opening a coordinated release PR, run `make release-preflight`. It tidies
each planned module against the same-batch versions (using temporary local
`replace` directives), so new indirect requirements introduced by the dependency
bump are committed with the release instead of failing the gate after the first
tag is published. `make release-preflight-write` applies the tidy results and
moves a same-batch requirement still sitting on the previous tag to the planned
version. The `Validate release intent` lane runs the same preflight, so a stale
pin or an untidy `go.mod` fails the PR that declares the release.

## Release automation

After a changeset reaches `main`, the `Release modules` workflow aggregates its
summaries into `CHANGELOG.md` and opens or updates the
`automation/release-changelog` Release PR. Feature PRs should not commit this
generated changelog update. Maintainers can reproduce it locally with
`make release-changelog`.

When the Release PR merges, the release workflow validates each planned module
independently with `GOWORK=off` — tidy, build, vet, and race tests — in
dependency order, and pushes each tag as soon as that module's own gate passes,
so a later module can resolve the dependency released earlier in the same run.
A module tagged earlier in a batch keeps its tag if a later gate fails; the next
push to `main` or a manual workflow dispatch plans only the modules whose tags
are still missing.

## Working in the workspace

- `make ci` runs `vet` + `test` across `core`, `craft`, `driver/*`,
  `backends/*`, `examples/forge`, and `examples/anvil`.
- `make fmt` / `make tidy` normalize formatting and module files everywhere.
- Changes to `core` contracts may break `craft`, `driver/*`, `backends/*`,
  `examples/forge`, or `examples/anvil`; `make ci` covers them in-tree.
- The forge demo's scenarios are native deployment documents; changes to the
  assembly or runtime surface should be exercised with a demo run
  (`cd examples/forge && go run . test -test werewolf/opening_setup`).

For larger work, please open a discussion or draft RFC issue first — it's much
faster than reviewing a 5k-line PR cold.
