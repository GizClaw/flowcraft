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
- `module` must be `core`.
- `bump` must be `patch` or `minor`.
- A changeset cannot declare the same module more than once.
- Multiple pending changesets for one module use the highest bump (`minor`
  outranks `patch`).
- A changeset is consumed for a module when that file exists in the module's
  latest `module/vX.Y.Z` tag.

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
release gate runs. `preflight --write` applies the normalized tidy result to the
module's `go.mod`/`go.sum` (without the temporary replace directives), so a
coordinated release PR can commit the tidy state up front.

After a changeset reaches `main`, the `Release modules` workflow aggregates all
pending summaries, updates the module-version table and release sections in
`CHANGELOG.md`, and opens or refreshes the
`automation/release-changelog` Release PR. `make release-changelog` determines
that PR's content; feature PRs normally do not commit the generated output.

Merging the Release PR makes the workflow validate the changelog again and run
the module gates. It creates every planned tag atomically only after all checks
pass. An unmerged Release PR, a changelog mismatch, or any failed gate creates
no tags.

Pending sections converge before publication: if a failed batch receives more
changesets, releasegate replaces that module's untagged section with the newly
aggregated section. Once a real tag exists, its changelog section becomes
historical and is never rewritten. Changeset files themselves remain in the
repository because tag containment is the consumption ledger.
