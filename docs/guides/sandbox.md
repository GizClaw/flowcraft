---
layout: default
title: Sandbox
---
# Sandbox Guide

`sdk/sandbox` is the agent's execution boundary: where commands run,
what they can reach (network), what they can see (env), how much they
can consume (resources). Sandbox is **daemon-level shared policy**;
per-run state lives in [`sdk/workspace`](workspace.md).

The split is deliberate: **sandbox is policy**, **workspace is state**.
A sandbox executes commands under a fixed policy; a workspace holds
the data those commands read and write. Scripts in a graph bind a
sandbox for command execution and a workspace for filesystem
operations — they are not interchangeable.

## Concepts

### The Runner interface

```go
type Runner interface {
    Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error)
}
```

One method. Concrete runners differ in **where** the work happens
(local process, bubblewrap namespace, seatbelt/macOS, container,
microVM) but share the same `ExecOptions` surface so a caller can be
retargeted between backends without changing call sites.

Implementations MUST:

- Honour `ExecOptions.Timeout`.
- Surface non-zero exits as `ExitCode` on `ExecResult` (return
  `err == nil` for that case).
- Reject any policy they cannot enforce with `errdefs.NotAvailable`
  rather than silently downgrading the request.

### Policy groups

`ExecOptions` carries three policy groups beyond the obvious
`WorkDir` / `Stdin` / `Timeout` knobs:

| Group       | Type             | What it controls                               |
| ----------- | ---------------- | ---------------------------------------------- |
| `Env`       | `EnvPolicy`      | allow-list of host env vars + `Inject` map     |
| `Net`       | `NetPolicy`      | mode (default, deny, allow via proxy — future) |
| `Resources` | `ResourceLimits` | CPU / memory / disk caps + `MaxOutputBytes`    |

`EnvPolicy.Allow` replaces the historical "inherit everything"
behaviour: when `Allow` is nil, behaviour is the legacy "inherit";
when it is non-nil, only listed variables pass through plus the
`Inject` map. This is the recommended posture for any multi-tenant
agent harness.

`NetPolicy.Mode` is enforced at the runner level. `LocalRunner`
only accepts `NetDefault`; non-default modes require a sandboxing
backend (namespace-based, container-based, or microVM-based) that
can actually enforce the policy at the kernel level. Trying to set
`Net: NetDenyAll` on a `LocalRunner` returns `errdefs.NotAvailable` at
`Exec` time, not a silent downgrade.

`ResourceLimits` covers `MemoryBytes`, `CPUMillicores`,
`DiskBytes`, and `MaxOutputBytes`. On unix, `LocalRunner` enforces
group-wide memory and CPU-time caps with a sampling watcher and
kills the whole process group on overflow. `DiskBytes` still needs
a quota-capable backend and is rejected with `NotAvailable`.

### Enforcement

`EnforcementOf(r)` returns an `Enforcement` struct describing what
the runner actually enforces vs what it would have to enforce
under request. Inspect the honest policy surface before execution:

```go
e := sandbox.EnforcementOf(r)
if !slices.Contains(e.NetModes, sandbox.NetDenyAll) {
    return errors.New("sandbox cannot enforce net policy on this backend")
}
```

`Enforcement` is informative, not authoritative — the only way to
verify behaviour is to run the command. Use it to fail fast on
misconfiguration, not to assert safety.

## First sandbox

```go
runner := sandbox.NewLocalRunner("/var/agent/root")

result, err := runner.Exec(ctx, "go", []string{"test", "./..."}, sandbox.ExecOptions{
    WorkDir: "./project",
    Timeout: 2 * time.Minute,
    Env: sandbox.EnvPolicy{
        Allow: []string{"PATH", "HOME", "GOFLAGS"},
        Inject: map[string]string{"CI": "1"},
    },
    Net:       sandbox.NetPolicy{Mode: sandbox.NetDefault},
    Resources: sandbox.ResourceLimits{
        MemoryBytes:    2 << 30,    // 2 GiB
        CPUMillicores:  2000,       // 2 cores
        MaxOutputBytes: 10 << 20,   // 10 MiB
    },
})
if err != nil { return err }
if result.ExitCode != 0 {
    return fmt.Errorf("tests failed: exit=%d stderr=%s", result.ExitCode, result.Stderr)
}
```

`LocalRunner` runs the command in a child process group rooted at
the configured root directory, with the env / net / resources
policies you provided.

## Runners

| Runner                  | Platform             | Enforced                                                 | Use when                                                       |
| ----------------------- | -------------------- | -------------------------------------------------------- | -------------------------------------------------------------- |
| `LocalRunner`           | unix (process group) | env allow-list, memory + CPU-time group caps, output cap | per-host dev, single-tenant trusted code, fast iteration       |
| `sdkx/sandbox/seatbelt` | macOS                | env allow-list, file/network confinement                 | macOS production, multi-tenant code                            |
| `sdkx/sandbox/bwrap`    | Linux                | env allow-list, namespace-level FS/net, soft CPU/memory caps, network policy (allow_list/proxy) | Linux production, strong isolation, network policy enforcement |
| Custom                  | any                  | depends                                                  | remote runners, microVM-backed runners, …                      |

The choice is not "which is more secure" — every shipped runner
makes the same trade-off about what it can enforce on its
platform. The choice is "what does the host's kernel offer, and
what policy axes matter to the workload."

## Decorators and approval

`Runner` is small enough that policy composes through decorators:

```go
runner := sandbox.AllowCommands(
    sandbox.WithApproval(
        sandbox.WithDefaults(local, defaults),
        approveFunc,
        myPredicates...,
    ),
    []string{"go", "pytest", "node"},
)
```

`WithApproval` injects a predicate chain in front of every call:
if any predicate matches, the host asks the approver; the
approver returns `Allow` or `Deny` (approver errors are fail-closed).
`AllowCommands` is
the canonical predicate — only listed commands pass.

`WithDefaults` merges a fixed `ExecOptions` into every call, and the merge is
security-biased: policy fields (`Env.Allow`, `Net`, `Resources`) belong to
the defaults and always win; `WorkDir` / `Stdin` fall back to defaults when
unset; `Timeout` takes the smaller of the two; `Env.Inject` is a union with
the caller winning on key collision.

## Workspace vs Sandbox

|             | Workspace                            | Sandbox                             |
| ----------- | ------------------------------------ | ----------------------------------- |
| Models      | files (data)                         | commands (behaviour)                |
| Reaches     | persistence layer                    | kernel / namespace / microVM        |
| Policy axis | path allow / deny                    | env / net / resources / approval    |
| Bind by     | `deps.workspace: ws/<name>` in graph | `deps.sandbox: box/<name>` in graph |
| Where       | `sdk/workspace`                      | `sdk/sandbox`                       |

A graph script can read and write workspace paths; it executes
commands through the sandbox. A script that needs both binds
both `workspace` and `sandbox` deps.

## Deploy integration

`sandbox.Registry` is a first-party resource in deployment
documents. The registry holds named sandboxes (each with a backend
and settings) and exposes them as `ItemResolver` so graph agents
bind `box/<name>`:

```yaml
resources:
  sandboxes:
    kind: sandbox.Registry
    impl: yaml
    deps: { workspaces: workspaces }
    settings:
      file: ./sandboxes.yaml
```

```yaml
# sandboxes.yaml
version: v1
sandboxes:
  coding:
    backend: local
    workspace: project # → bind to workspaces/<name>
    defaults:
      timeout: 2m
      env:
        allow: [PATH, HOME, GOCACHE]
        inject: { CI: "1" }
      net: { mode: deny_all }
      resources:
        memory_bytes: 2147483648
        cpu_millicores: 2000
        max_output_bytes: 10485760
```

`local` is the only backend registered by default. Platform backends
(`seatbelt` on macOS, `bwrap` on Linux) register themselves from their own
packages; a document naming one without registering it fails the build.

Graph binding:

```yaml
deps:
  sandbox: sandboxes/coding
```

The graph factory's `sandbox` dep contract is "optional, used by
scripts that need command execution." See
[deploy.md#engines](deploy.md#engines) for the full engine dep
contract.

## Testing

`LocalRunner` is the natural choice for unit tests — fast, hermetic,
and configurable. Test against a `t.TempDir()` and assert on the
`ExecResult` exit code and output. For policy tests, build a
`Runner` chain (predicate + approval + defaults + local) and assert
on the per-layer behaviour: does the predicate match? does the
approver's decision flow through? do the defaults apply when
caller doesn't override?

```go
runner := sandbox.NewLocalRunner(t.TempDir())
out, err := runner.Exec(ctx, "sh", []string{"-c", "echo hi"}, sandbox.ExecOptions{})
if err != nil { t.Fatal(err) }
if out.ExitCode != 0 || !strings.Contains(string(out.Stdout), "hi") {
    t.Fatalf("unexpected: %+v", out)
}
```

For higher-level conformance — "does this runner respect the
contract that `NotAvailable` is returned for unenforceable policy?"
— the shared suite lives in `sdk/sandbox` test files; per-backend
suites are in `sdkx/sandbox/<backend>/`.

## Further reading

- Package contract: `sdk/sandbox/doc.go` (enforcement matrix),
  `sdk/sandbox/sandbox.go` (Runner + ExecOptions + ExecResult),
  `sdk/sandbox/decorator.go`, `sdk/sandbox/approval.go`,
  `sdk/sandbox/enforcement.go`.
- Backends: `sdkx/sandbox/seatbelt/doc.go`,
  `sdkx/sandbox/bwrap/doc.go`.
- Assembly: `sdk/sandbox/config/doc.go`, the `sandbox.Registry`
  resource in [deploy.md](deploy.md#first-party-impls).
- Sibling guide: [workspace.md](workspace.md) (policy vs state).
