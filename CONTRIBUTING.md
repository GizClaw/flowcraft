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
`feat(sdkx/inference): add qwen provider` or `fix(memory): harden streaming`.
Keep changes within module boundaries.

## Modules and dependency order

The independently versioned library modules are `sdk`, `memory`, and `sdkx`.
The Go workspace also includes `examples/forge` (the runnable local demo, not
released), while `tests/conformance` and `tools/releasegate` build with
`GOWORK=off` against pinned releases.

Module dependency order:

```text
sdk -> sdkx -> memory
```

- `sdk` is the contract layer and depends on nothing in-tree.
- `sdkx` provides adapters and generic assembly over `sdk`.
- `memory` is one implementation of the `sdk/memory` contracts and depends on
  both `sdk` and `sdkx` (its worker runtime integration implements
  `sdkx/runtime` interfaces).

The retired `cmd/claw` CLI remains in the repository for reference only; it is
not part of the workspace, build, or CI.

## Declaring a module release

A pull request may merge without a release intent. When its changes should
publish one or more library modules, add a new immutable
`.release/<descriptive-name>.json` file:

```json
{
  "summary": "Add streaming retries",
  "releases": [
    {
      "module": "sdk",
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
the versions being planned in the same batch. Because the order is
`sdk -> sdkx -> memory`, a coordinated batch pins `sdkx` on `sdk` and `memory`
on both. Check the exact versions with:

```sh
make release-plan
```

The release gate rejects a module whose same-batch dependency pins do not match
the planned versions.

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

- `make ci` runs `vet` + `test` across `sdk`, `memory`, `sdkx`, and
  `examples/forge`, then the `GOWORK=off` conformance suites.
- `make fmt` / `make tidy` normalize formatting and module files everywhere.
- Changes to `sdk` or `sdkx` contracts may break `examples/forge` or `memory`;
  `make ci` covers them in-tree.
- The forge demo's scenarios are native deployment documents; changes to the
  assembly or runtime surface should be exercised with a demo run
  (`cd examples/forge && go run . chat --workspace <ws>`).

For larger work, please open a discussion or draft RFC issue first — it's much
faster than reviewing a 5k-line PR cold.
