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
`extra_flags` (bwrap only) passes additional bwrap flags, with any flag
that could weaken the policy (e.g. `--ro-bind` or `--args`) rejected at
build time. The local runner is a no-isolation backend for trusted
workflows; bwrap/seatbelt enforce the isolation boundary and reject
policies they cannot honor.

## Per-exec write policy

`ExecOptions.Write` narrows the filesystem boundary for a single call
without changing the runner. `WriteReadOnly` keeps the runner root
read-only for that exec (explicit `writable_paths` and platform escape
hatches like `/dev/null` remain allowed); `WriteWorkspace` (zero value)
keeps the runner root writable — the current behavior. There is no
widening mode — a call can only request a stricter boundary than the
runner was constructed with, and `WithDefaults` follows the same rule
(either side read-only wins). The local runner has no OS boundary and
reports no write modes in `Capabilities`.

For read-only auto-approval, `ClassifySafeReadOnly` implements the
codex-rs-style heuristic (base read-only commands plus argument-aware
checks for `find` / `rg` / `git` / `sed`, with `sh -c` / `bash -lc`
unwrap). It is a caller-side helper — the host's `ApprovalFunc` decides:

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
