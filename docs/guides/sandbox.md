---
layout: default
title: Sandbox
---
# Sandbox Guide

`core/sandbox` is the command execution boundary. It defines `Runner`,
`ExecOptions`, session support, and policy validation. Concrete backends are
selected by deployment configuration.

## Runner interface

```go
type Runner interface {
    Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error)
}
```

Backends must reject any policy they cannot enforce rather than silently
downgrade.

## Deployment resource

The local runner is a no-isolation backend for trusted workflows:

```yaml
resources:
  box:
    kind: sandbox.Runner
    impl: local
    settings:
      root: ./sandbox
```

Platform backends are registered from core subpackages:

- `core/sandbox/bwrap` for Linux namespace isolation.
- `core/sandbox/seatbelt` for macOS confinement.
- `core/sandbox/windows` for Windows Job Object lifecycle and
  resource caps (phase 1: no OS-level filesystem or network
  confinement yet).

Both isolation backends share the same settings shape:

```yaml
resources:
  box:
    kind: sandbox.Runner
    impl: bwrap            # or seatbelt
    settings:
      root: ./sandbox
      binary: /usr/bin/bwrap    # optional; resolved against the root
      writable_paths: [./out]   # optional; paths the sandbox may write
      readonly_root: true       # optional; keep the runner root read-only
      extra_flags: [--die-with-parent]  # bwrap only; policy-downgrading flags are rejected
```

`root` is required and scopes the sandbox filesystem. `binary` overrides
the backend binary and is resolved against the root; `writable_paths`
opt into write access; `readonly_root` keeps the runner root read-only
for every exec (explicit `writable_paths` stay writable);
`writable_paths` entries that resolve to the runner root conflict with
`readonly_root: true` and are rejected at build time instead of being
silently dropped (without `readonly_root` such an entry is redundant
and ignored);
`extra_flags` (bwrap only) passes additional bwrap flags, with any flag
that could weaken the policy (e.g. `--ro-bind` or `--args`) rejected at
build time. The local runner is a no-isolation backend for trusted
workflows; bwrap/seatbelt enforce the isolation boundary and reject
policies they cannot honor.

## Platform support

| Backend | Platform | Isolation | Net modes | Write policy | Resource caps |
|---|---|---|---|---|---|
| `local` | all (interactive sessions unix-only) | none | `NetDefault` only | none enforced | memory / cpu via group watcher (unix) |
| `bwrap` | Linux | user / mount / pid / net namespaces | `NetDefault`, `NetDenyAll`, `NetAllowList`, `NetProxy` | root + `writable_paths`, `WriteReadOnly` | memory / cpu (watcher) |
| `seatbelt` | macOS | Seatbelt (SBPL) | `NetDefault`, `NetDenyAll`, `NetAllowList`, `NetProxy` | root + writable paths, `WriteReadOnly` | memory / cpu (watcher) |
| `windows` | Windows | Job Objects (process tree, memory / cpu caps) | `NetDefault` only (phase 1) | none enforced (phase 1) | memory / cpu (job limits) |

The `windows` backend is the phase-1 port that makes command execution
work on Windows: every child runs in its own job object, timeout /
cancel / close terminate the whole process tree, and memory / cpu
limits are enforced by the kernel. Filesystem write confinement
(restricted tokens + ACLs) and network filtering are planned follow-ups;
until then the backend fails closed with `errdefs.NotAvailable` for
policies it cannot enforce.

## Per-exec write policy

`ExecOptions.Write` narrows the filesystem boundary for a single call
without changing the runner. `WriteReadOnly` keeps the runner root
read-only for that exec (explicit `writable_paths` and platform escape
hatches like `/dev/null` remain allowed); `WriteWorkspace` (zero value)
keeps the runner root writable — the current behavior. There is no
widening mode — a call can only request a stricter boundary than the
runner was constructed with, and `WithDefaults` follows the same rule
(either side read-only wins; an unknown value on either side is
preserved so backend validation rejects it instead of silently
degrading to `WriteWorkspace`). The local runner has no OS boundary and
reports no write modes in `Capabilities`.

For read-only auto-approval, `ClassifySafeReadOnly` implements the
codex-rs-style heuristic (base read-only commands plus argument-aware
checks for `find` / `rg` / `git` / `sed` / `sort`, with `sh -c` /
`bash -lc` unwrap). `date` and `hostname` are deliberately not
auto-approved: `date -s` changes the system clock and
`hostname newname` changes the host name — non-file writes the OS
sandbox cannot block. It is a caller-side helper — the host's
`ApprovalFunc` decides:

```go
if req.Opts.Write == sandbox.WriteReadOnly && sandbox.ClassifySafeReadOnly(req.Exec) {
    return sandbox.Allow, nil
}
```

It never denies and never widens policy; unrecognized commands return
`false` and route to the human approver.

## Policy groups

`ExecOptions` carries:

- `WorkDir`, `Stdin`, `Timeout`;
- `Env` allow-list/inject policy;
- `Net` network policy;
- `Resources` memory/cpu/output caps.

See [workspace.md](workspace.md) for state storage.
