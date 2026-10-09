# Release changesets

Each `.release/*.json` file is an immutable release intent. After it reaches
`main`, do not modify, rename, or delete it. Add another changeset to correct
the intent.

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

- `summary` must be a non-empty single line and must not contain the reserved
  releasegate marker.
- `releases` must contain at least one entry.
- `module` must be `core` or `craft`. A changeset may declare both when they
  release together: `craft` requires `core`, so the batch is tagged
  `core` first, and the `craft` gate expects the new `core` checksums to
  appear in `go.sum`. A same-batch dependent must already require the version
  being tagged, or the preflight fails.
- `bump` must be `patch` or `minor`.
- A changeset cannot declare the same module more than once.
- Multiple pending changesets for one module use the highest bump (`minor`
  outranks `patch`).
- A changeset is consumed for a module when that file exists in the module's
  latest `module/vX.Y.Z` tag.

`craft`'s first release has to ship in the same batch as the `core` release it
depends on: the module imports `core/hooks` and `core/utils/lock`, and no
published `core` tag contains them, so `GOWORK=off go build ./...` — what the
release gate runs per module — cannot resolve them. Declare both modules in one
changeset, run `make release-preflight` (or `-write`) so `craft`'s `core` pin
moves to the version this batch tags, and let the workflow tag `core` first. A
standalone `craft` changeset fails the gate with `no required module provides
package ...`, which does not point at the cause.

The CLI is a standalone Go module. Run these commands from the repository root:

```sh
make release-check
make release-check BASE=origin/main
make release-plan
make release-preflight
make release-preflight-write
make release-changelog
```

`plan --json` prints the module plan, GitHub Actions matrix, and tags to create.
With no pending release intent, it succeeds with empty arrays.

`preflight` runs `go mod tidy` (with `GOWORK=off`) for every planned module. When
the module releases in the same batch as an in-tree dependency, the dependency is
replaced with its local directory so the tidy result reflects the versions that
will exist after tagging. This catches new indirect requirements before the
release gate runs. The same pass requires every same-batch dependency to be
pinned at the version this batch tags: the temporary replace resolves either pin,
so a dependency left on the previous tag would otherwise pass and the tag would
be built against the old release. `preflight --write` applies the normalized tidy
result to the module's `go.mod`/`go.sum` (without the temporary replace
directives) and moves stale same-batch pins to the planned versions, so a
coordinated release PR can commit the tidy state up front. The
`Validate release intent` lane runs the read-only preflight on every pull
request.

After a changeset reaches `main`, the `Release modules` workflow aggregates all
pending summaries, updates the module-version table and release sections in
`CHANGELOG.md`, and opens or refreshes the
`automation/release-changelog` Release PR. `make release-changelog` determines
that PR's content; feature PRs normally do not commit the generated output.

Merging the Release PR makes the workflow validate the changelog again and gate
the planned modules in dependency order, pushing each tag once its own gate
passes. A module that has never been tagged has no version cell yet; its row in
the published-state table carries `-` until its first release fills it in. An
unmerged Release PR, a changelog mismatch, or any failed gate creates no tags
beyond the ones already pushed in that run: a partial batch is retried by the
next push to `main` or a manual workflow dispatch, which plans only the modules
whose tags are still missing.

Pending sections converge before publication: if a failed batch receives more
changesets, releasegate replaces that module's untagged section with the newly
aggregated section. Once a real tag exists, its changelog section becomes
historical and is never rewritten. Changeset files themselves remain in the
repository because tag containment is the consumption ledger.
