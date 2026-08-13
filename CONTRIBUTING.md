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

The independently versioned library modules are `core`, `driver/*`,
`backends/*`, and `memory`.
The Go workspace also includes `examples/forge` (the runnable local demo, not
released), while `tools/releasegate` builds with `GOWORK=off` against pinned
releases.

Module dependency order:

```text
core -> driver/* / backends/* -> memory
```

- `core` is the platform module and depends on nothing in-tree.
- `driver/*` provides provider inference adapters over `core`.
- `backends/*` provides platform-specific sandbox/object-store/SQLite
  backends over `core`.
- `memory` is one implementation of the `core/memory` contracts.

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
correct release intent.

### Coordinated releases

When dependent modules release together, update their `go.mod` requirements to
the versions being planned in the same batch. Check the exact versions with:

```sh
make release-plan
```

The release gate rejects a module whose same-batch dependency pins do not match
the planned versions.

Before opening a coordinated release PR, run `make release-preflight`. It tidies
each planned module against the same-batch versions (using temporary local
`replace` directives), so new indirect requirements introduced by the dependency
bump are committed with the release instead of failing the gate after the first
tag is published. `make release-preflight-write` applies the tidy results to the
module's `go.mod`/`go.sum`.

## Release automation

After a changeset reaches `main`, the `Release modules` workflow aggregates its
summaries into `CHANGELOG.md` and opens or updates the
`automation/release-changelog` Release PR. Feature PRs should not commit this
generated changelog update. Maintainers can reproduce it locally with
`make release-changelog`.

When the Release PR merges, the release workflow validates each planned module
independently with `GOWORK=off`, including tidy, build, vet, and race tests.
Only after the generated changelog and every required gate succeed are all
module tags pushed atomically. Failed runs create no tags and are retried by
the next push to `main` or a manual workflow dispatch.

## Working in the workspace

- `make ci` runs `vet` + `test` across `core`, `driver/*`, `backends/*`,
  `memory`, and
  `examples/forge`.
- `make fmt` / `make tidy` normalize formatting and module files everywhere.
- Changes to `core` contracts may break `driver/*`, `backends/*`,
  `examples/forge`, or `memory`;
  `make ci` covers them in-tree.
- The forge demo's scenarios are native deployment documents; changes to the
  assembly or runtime surface should be exercised with a demo run
  (`cd examples/forge && go run . test -test werewolf/opening_setup`).

For larger work, please open a discussion or draft RFC issue first — it's much
faster than reviewing a 5k-line PR cold.
