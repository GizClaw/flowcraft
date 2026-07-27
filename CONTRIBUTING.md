# Contributing to FlowCraft

Issues, focused pull requests, and draft RFCs are welcome. For substantial API
or architecture changes, open an issue first so the module boundaries and
compatibility impact can be agreed before implementation.

## Before opening a pull request

Run:

```sh
make ci
make test-e2e
make release-check
git diff --check
```

Add tests for changed behavior and format Go files with `gofmt`. Commit messages
use Conventional Commits such as `feat:`, `fix:`, `docs:`, `refactor:`,
`test:`, and `chore:`.

## Declaring a module release

The independently versioned library modules are `sdk`, `memory`, `sdkx`, and
`voice`. `cmd/claw` is built and tested from source but is not currently
published as a versioned binary.

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

When dependent modules release together, update their `go.mod` requirements to
the versions being planned in the same batch. The dependency order is
`sdk -> memory -> sdkx`, with `voice` after `sdk`. Check the exact versions with:

```sh
make release-plan
```

On `main`, the release workflow validates each planned module independently
with `GOWORK=off`, including tidy, build, vet, and race tests. Relevant
`sdk`/`memory`/`sdkx` batches also run retrieval E2E and hermetic evals. Only
after every required gate succeeds are all module tags pushed atomically.
Failed runs create no tags and are retried by the next push to `main` or a
manual workflow dispatch.
