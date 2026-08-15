# Changelog

All notable changes to this repository are documented here. The active
published module is `core`; releases use the `core/vX.Y.Z` tag prefix.

Pending changesets are aggregated into module release sections by the automated
Release PR before their tags are published.

## Current Published State

| Module | Latest tag | Notes |
| --- | --- | --- |
| `core` | `core/v0.1.8` | Unified platform module: contracts, deploy, runtime, and built-in resources. |

## [Unreleased]

_No pending changes._

<!-- releasegate:releases -->

## `core/v0.1.8` - 2026-08-15

### Changed

- feat(core): runtime tool publication with background MCP reconnect; fix(core): make sandbox.Runner lifecycle part of the contract; fix(core): bound SessionRegistry.Close and wait out start races; fix(core): tighten MCP session lifecycle and tool projection

## `core/v0.1.7` - 2026-08-14

### Changed

- fix(core): run Agent.Prepare hooks in Execute

## `core/v0.1.6` - 2026-08-14

### Changed

- fix(core): fresh run ids per turn, session-scoped committed history, explicit resume, and immediate Turn.Cancel

## `core/v0.1.5` - 2026-08-13

### Changed

- feat(core): runtime prompt event subscription, prompt lifecycle resolved events, and sandbox allowlist approval composition

## `core/v0.1.4` - 2026-08-13

### Changed

- Treat already-closed sandbox stdin as a no-op close to fix the release-gate exec race

## `core/v0.1.3` - 2026-08-13

### Changed

- Add stream terminal signals and request-id telemetry, stamp usage model/latency, and enrich driver usage and audio support

## `core/v0.1.2` - 2026-08-13

### Changed

- Add inference and agent conformance suites and expand workspace settings references

## `core/v0.1.1` - 2026-08-13

### Changed

- Fold sandbox backends into core/sandbox, split local runner into core/sandbox/local, and move net policy/proxy/mitm into core/utils/net

## `core/v0.1.0` - 2026-08-13

### Changed

- Introduce core as the single platform module by folding sdk and sdkx contracts, deploy/runtime assembly, built-in resources, and tooltest support into core

