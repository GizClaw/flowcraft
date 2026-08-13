# Changelog

All notable changes to this repository are documented here. The active
published module is `core`; releases use the `core/vX.Y.Z` tag prefix.

Pending changesets are aggregated into module release sections by the automated
Release PR before their tags are published.

## Current Published State

| Module | Latest tag | Notes |
| --- | --- | --- |
| `core` | `core/v0.1.0` | Unified platform module: contracts, deploy, runtime, and built-in resources. |

## [Unreleased]

_No pending changes._

<!-- releasegate:releases -->

## `core/v0.1.0` - 2026-08-13

### Changed

- Introduced the `core` platform module (`github.com/GizClaw/flowcraft/core`)
  containing agent, message, errdefs, resource, deploy, runtime, session,
  event, graph, inference, memory contracts, tool, telemetry, workspace, and
  sandbox packages.
- Moved provider adapters to per-provider `driver/*` modules and
  platform-specific sandbox/object-store/SQLite implementations to
  `integrations/*`.
- `deploy` now builds resources through `core/resource.Factory` and wires
  agents/hooks after resource construction.
- `runtime` now owns `core/runtime` and `core/runtime/session`.

### Removed

- Active `sdk` and `sdkx` documentation and examples were replaced by the
  `core`/`driver`/`integrations` layout with no compatibility shims.

Historical `sdk` / `sdkx` / `memory` release notes are archived in [CHANGELOG.legacy.md](CHANGELOG.legacy.md).
