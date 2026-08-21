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
      extra_flags: [--die-with-parent]  # bwrap only; policy-downgrading flags are rejected
```

`root` is required and scopes the sandbox filesystem. `binary` overrides
the backend binary and is resolved against the root; `writable_paths`
opt into write access; `extra_flags` (bwrap only) passes additional
bwrap flags, with any flag that could weaken the policy (e.g. `--ro-bind`
or `--args`) rejected at build time. The local runner is a no-isolation
backend for trusted workflows; bwrap/seatbelt enforce the isolation
boundary and reject policies they cannot honor.

## Policy groups

`ExecOptions` carries:

- `WorkDir`, `Stdin`, `Timeout`;
- `Env` allow-list/inject policy;
- `Net` network policy;
- `Resources` memory/cpu/output caps.

See [workspace.md](workspace.md) for state storage.
