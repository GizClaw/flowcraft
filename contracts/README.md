# contracts/

Single source of truth for FlowCraft event envelopes, payloads, partitions and
HTTP API. Everything under `internal/eventlog/*_gen.go`, `web/src/api/*.gen.ts`
and `contracts/*.schema.json` is generated from this directory by
`cmd/eventgen`.

## Layout

| Path | Purpose |
|---|---|
| `events.yaml` | Top-level manifest: spec version, partitions, categories, lint config, includes. |
| `events/<domain>.yaml` | One file per business domain (task / cron / agent / chat / webhook / realm / audit). Defines events + their inline payloads. |
| `payloads/<file>.yaml` | Cross-domain shared payload definitions (e.g. `Actor`, `WebhookInboundBody`). |
| `envelope.schema.json` | **Generated.** JSON Schema for the runtime `Envelope` shape. |
| `payloads.schema.json` | **Generated.** JSON Schema for every payload struct. |
| `openapi.yaml` | HTTP surface (events read API, command write API). |

## Workflow

```
edit contracts/**       →   make events-gen      →   commit generated outputs
                            (~100ms; runs lint + codegen)

before each PR          →   make events-check    →   CI red line; checks lint +
                                                     evolution + codegen drift
```

A pre-commit hook (`make install-hooks`) wires `events-gen` to fire whenever
files under `contracts/` change locally, so the generated artefacts stay in
sync without manual intervention.

## CI red lines (DoD D.3 / D.4 / D.5)

| ID | What it checks | Where |
|---|---|---|
| D.3 | Naming lint: completed-tense whitelist, no imperative verbs, no `-ing`, partition + category registration, audit summary references payload | `make events-check` (lint phase) |
| D.4 | Evolution rules: deletion needs prior `deprecated: true`; same-version cannot drop fields, change types, tighten optional→required, change category/partition/payload_type | `make events-check` (snapshot diff vs `cmd/eventgen/testdata/baseline/`) |
| D.5 | Codegen drift: regenerates and `git diff --exit-code` so anyone changing contracts must commit the generated outputs | `make events-check` (gen + diff phase) |

The dedicated workflow `.github/workflows/events.yml` runs on every push and
PR that touches contracts/eventgen/eventlog. **Mark this workflow as a Required
Status Check in GitHub repo Settings → Branches → Branch protection rules**, so
PRs cannot merge while D.3 / D.4 / D.5 are red.

## Adding a new event

1. Pick the right domain file under `events/`. Add the event with a
   completed-tense verb (e.g. `task.cancelled`, not `task.cancel`).
2. Add or reuse a payload — inline under `payloads:` or in `payloads/*.yaml`.
3. Run `make events-gen`. Commit the generated `*_gen.go`, `*.gen.ts`, and
   `*.schema.json` files together with your contract change.
4. CI runs `make events-check`. If you broke evolution rules, fix the rules
   you broke, or bump `version`, then re-baseline:
   ```
   go run ./cmd/eventgen -mode=write-baseline
   ```
   Note: re-baselining is permitted only when the breakage is intentional and
   has been reviewed.

## Removing an event

1. PR #1: set `deprecated: true` on the event. Merge.
2. Wait at least one release cycle (or, in early-stage development, one
   reviewed PR).
3. PR #2: delete the event from the contract. `events-check` allows this only
   if the baseline still records `deprecated: true`.

See `cmd/eventgen/testdata/compat/` for executable specifications of every
allowed and forbidden contract change.
