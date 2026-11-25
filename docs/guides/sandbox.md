---
layout: default
title: Sandbox
---

# Sandbox Guide

`core/sandbox` is the command execution boundary, and it is three layers:

| Layer     | Package                                   | Role                                                                                                                                                                                |
| --------- | ----------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Contract  | `core/sandbox`                            | `Runner`, `SessionStarter`, `Session`, `ExecOptions`, `Confine`, `Enforcement`, the one-shot `Exec`. It spawns nothing itself.                                                      |
| Machinery | `sandbox/sessionkit`, `sandbox/netpolicy` | The spawn path and session registry, the group resource sampler, the outbound-network policy and its proxy. Not part of the contract, so it may be reshaped between minor versions. |
| Backends  | `sandbox/local`, `sandbox/remote`         | The process boundary itself — or, for `remote`, the same contract across a host boundary.                                                                                           |

The split exists so the contract package never imports `os/exec` and a backend
never re-derives the policy rules. A backend is a sibling of the machinery, not a
subpackage of it, because it is an implementation of the contract rather than part
of the contract — and `core` is forbidden from importing it, which is why the
application registers the backends it compiles in (`deploy.BackendImpl`) and names
one per entry in the process document. Verify a new backend with
`core/sandbox/sandboxtest`: it is the contract's executable specification.

## From a grant to a command

One chain, and each link owns exactly one decision:

```text
policy.Grant              what the caller stated for this session
  → sandbox.Confine       the projection: files, net, env, resources — nothing invented
  → host.BackendFactory   func(sandbox.Confine) (sandbox.Runner, error), one instance per session
  → sandbox.ValidateConfine   refused here if the backend cannot enforce it
  → sandbox.Exec(ctx, starter, cmd, args, opts)
```

```go
// A backend's registration, in the application: the factory is process-wide, the
// instance it returns is one session's.
impls.Backend(deploy.BackendImpl("local", func(_ struct{}, confine sandbox.Confine) (sandbox.Runner, error) {
    return local.New(confine)
}))
```

At open the host projects the session's grant onto a `sandbox.Confine` — `Files`, `Net`,
`Exec.Env` and `Resources`, the grant's own values and nothing invented — builds the
runner from it, and re-runs `sandbox.ValidateConfine(confine,
runner.Capabilities().Policy)` so a backend that skipped its own check cannot hand a
session a boundary it does not enforce.

A backend instance serves exactly one session, and the confine is fixed when it is
built, so "the policy of this call" and "the policy of this session" are the same
thing. That is why `ExecOptions` carries no permission: a second channel for
something already decided would only re-open what a narrower call inside a wider
boundary means.

`host.BackendFactory` is the process-wide closure the host holds; the instance it
returns is the session's. The file boundary is built beside it — `grant.files` opens
the session's `fsx.Tree` — so a file tool is refused by a kernel handle and a command
is refused by the backend's own mounts or profile, each at the layer that can enforce
it.

## Runner and SessionStarter

```go
type Runner interface {
    Close() error
    Capabilities() Capabilities
    Start(ctx context.Context, spec SessionSpec) (Session, error)
    List(ctx context.Context) ([]SessionInfo, error)
    Terminate(ctx context.Context, id string) error
}

// The part of a runner a tool may use.
type SessionStarter interface {
    Start(ctx context.Context, spec SessionSpec) (Session, error)
}
```

The split is an authority split, not a convenience: a tool that may add a command
channel needs `Start`; only the boundary's owner may enumerate or terminate what is
running. `platform.ProcessSlot` — the capability a command tool declares in
`tool.Tool.Needs` — is filled with a `SessionStarter`, never with the `Runner`.

A backend implements `Start` and gets the one-shot form for free:
`sandbox.Exec(ctx, starter, cmd, args, opts)` drains a session, so there is no second
code path to keep in step. `Close` releases what the runner owns, it must be safe to
call twice, and a decorator that forwards the contract owes it the same forwarding —
`Capabilities` keeps advertising `SessionAttacher.Attach`, so a wrapper that swallowed
it would make the declaration a lie.

Backends must reject a confine they cannot enforce rather than silently downgrade it.
`sandbox.ValidateConfine(confine, caps)` is that gate, `caps` is the backend's own
declaration, and it is called twice on purpose: once by the backend at construction,
once by the host that owns the session.

## What a backend declares

`Capabilities` has two halves — `Policy` (what it can enforce) and `Features` (what a
started session can do). `Enforcement` is the first:

| Field              | Means                                                                                                                                             |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `EnvAllowList`     | `policy.EnvPolicy.Allow` is honoured: host variables not on the list are dropped, not forwarded.                                                  |
| `NetModes`         | The `policy.NetMode` values enforceable at the OS level. `NetDefault` is never listed — it is the absence of a posture.                           |
| `Socks5`           | The backend's proxy can dial a `socks5://` upstream, authentication included.                                                                     |
| `MITM`             | It can terminate TLS for configured CONNECT hosts and inject the temporary CA into the child environment.                                         |
| `UnixSocketPolicy` | It can confine unix socket egress to an explicit allow-list.                                                                                      |
| `MemoryCap`        | `resources.memory_bytes` is enforced (cgroup, rlimit, or a sampling watcher) rather than refused.                                                 |
| `CPUCap`           | `resources.cpu_millicores` is enforced, which also needs a per-call deadline to derive it from.                                                   |
| `DiskCap`          | `resources.disk_bytes` is enforced. No process-spawning backend in this repository reports it today.                                              |
| `FilesystemBounds` | Writes are confined to `Files` at the OS level. A backend with no OS boundary reports false, and then a confine that restricts writes is refused. |

The refusals in `ValidateConfine` follow directly from that: a network mode the backend
does not list, a TLS interception it cannot do, a unix-socket allow-list it cannot
confine, a write-restricting `Files` without `FilesystemBounds`, and an environment
allow-list without `EnvAllowList` are all `NotAvailable` at construction — at open,
where an operator sees it, rather than on the first command that needs it.

Registration declares the same value (`deploy.Factory.Enforcing`) because the loader
has to answer "does this deployment's backend bound writes" while reading a document,
before any session exists; a backend that declares nothing is a backend nothing can be
escalated on.

## One-shot execution

```go
type ExecOptions struct {
    WorkDir   string           // required: the platform defines no session directory
    Stdin     []byte           // nil or empty means no stdin side at all
    Timeout   time.Duration    // per call; zero means no sandbox-imposed deadline
    Resources policy.Resources // what to spend, at most the instance's ceiling
}

type ExecResult struct {
    ExitCode int
    Stdout   string
    Stderr   string
}
```

`Exec` takes a `SessionStarter`, applies `Timeout` to the whole call, closes every
session it started before returning, and reports a non-zero exit as a result rather
than an error. Each stream is truncated to `Resources.MaxOutputBytes` (10 MiB when the
call names none), keeping the first bytes rather than the last. A refusal before
anything ran is distinguishable from a failure: `errors.Is(err, sandbox.ErrConfineDenied)`
says the boundary refused, and `errdefs.IsPolicyDenied` says a rule refused.

## Sessions

Sessions declare their input surface up front. `SessionSpec.Stdin` says the caller will
feed the child through `Write` / `CloseInput`, and `SessionCapabilities.Stdin` reports
whether a started session has somewhere to write. A non-TTY session without that
declaration leaves the child's fd 0 at the OS default (`/dev/null`), so a tool that
branches on stdin — `rg` with no path operand, `cat`, `test -p /dev/fd/0` — reads its
operands instead of an empty pipe that never ends; `Write` on such a session fails with
`errdefs.NotAvailable` instead of silently buffering.

`ExecOptions.Stdin` is one-shot input, not that declaration: `Start` neither delivers
those bytes nor pipes fd 0 for them. `sandbox.Exec` sets `SessionSpec.Stdin` itself
before it writes those bytes and closes the pipe.

A session that was started can be re-acquired later, by something that no longer holds
the handle — usually a different request:

```go
type SessionAttacher interface {
    Attach(ctx context.Context, id string) (Session, error)
}

sess, err := sandbox.Attach(ctx, runner, id)
```

`sandbox.Attach` is the only supported way to reach the optional interface — callers ask
the runner rather than asserting the interface themselves, so a backend can keep the
capability behind a decorator chain or behind the wire. The handle is an ordinary
session: it reads the same output log from any retained cursor, and `Close` on it
terminates a still running process, so attaching is "take over", not "peek".
`sessionkit`'s registry implements it, and `sandbox/remote` proxies it to the server, so
the same call works against a local or a remote boundary. An unknown id is
`errdefs.NotFound`; a session whose spawn has not settled yet is `errdefs.NotAvailable`.

`SessionFeatures` describes what a started session can do: `TTY` (pty sessions, with
`Resize` and merged output), `Signal` (signal delivery), `Events` (push streams through
`Watch` — a backend that reports `Events` must be able to deliver the terminal status,
which the conformance suite checks).

## A call may ask for less

`Confine.Narrow(opts)` resolves one call's `Resources` against the instance's ceiling
and returns the options the backend should act on. A zero field inherits the ceiling; a
non-zero field must fit under it, and asking for more is an error rather than a silent
truncation — a budget that quietly lost its value is indistinguishable from one that
never had it. There is no widening direction anywhere in this package.

Two rules come out of that:

- a cpu budget needs a per-call deadline to derive from, so `cpu_millicores` with no
  `Timeout` is refused rather than run unmeasured;
- `ValidateBudgets(resources, caps)` is the per-call half of `ValidateConfine`: a
  ceiling of zero means "unbounded", so a call may name a budget the instance never
  claimed, and a backend refuses what it cannot enforce instead of running the command
  uncapped.

A cap that stops being measurable mid-session is a different answer: every backend that
samples a budget reaps the session as `sandbox.SessionUnenforceable` with a
`NotAvailable` error once its sampler has failed three consecutive times, rather than
letting a child run unguarded under a capability that still claims the cap.
`sessionkit.StartGroupCapsWatcher` is that sampler, and `GroupCapsAvailable` is what a
backend must gate its `MemoryCap`/`CPUCap` declaration on instead of hardcoding true.

## What a deployment says

A backend is chosen in the process document, by name; a session selects one with its
grant's `sandbox` field (empty picks the process's only backend). There is no per-agent
sandbox block: what one session may touch is its grant.

```yaml
# the process document: the backends this binary carries
version: 1
sandbox:
  - name: local
    impl: local
```

```go
// what the application states when it opens a session
authority := policy.Grant{
    Sandbox: "local", // a name the process must configure
    Files:   policy.Files{Read: []string{"/srv/agent"}, Write: []string{"/srv/agent/out"}},
    Net:     policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.openai.com"}},
    Exec:    policy.ExecPolicy{Timeout: policy.Duration(60 * time.Second)},
    Resources: policy.Resources{CPUMillicores: 2000, MemoryBytes: 2147483648},
}
```

| Grant field | Becomes, in the confine                                                                                                        |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `sandbox`   | The name the host resolves to a `BackendFactory`. A name the process does not carry is `NotAvailable` at open.                 |
| `files`     | `Confine.Files` — `read[]`, `write[]`, `readonly`. Absolute roots; a write-restricting set needs a backend that bounds writes. |
| `net`       | `Confine.Net` — `mode`, `allow_hosts[]`, `rules[]`, `proxy`, `unix_sockets[]`, `mitm`.                                         |
| `exec.env`  | `Confine.Env` — `allow[]` (an empty list means "nothing", absent means "inherit") and `inject{}`.                              |
| `resources` | `Confine.Resources` — the per-command ceiling: `cpu_millicores`, `memory_bytes`, `disk_bytes`, `max_output_bytes`.             |

Everything else in the grant is somebody else's: the tools are the projection's, the
models are the provider's, the timeout is a per-call argument, and the delegation block
belongs to [delegation.md](delegation.md). The settings block of a `sandbox` entry is the
backend implementation's own and is decoded strictly at load.

## Backends

### `local`

No isolation, for trusted workflows: commands run through `os/exec` on the host,
`Enforcement` reports only `EnvAllowList` plus whatever the shared sampler can measure,
and a confine that restricts writes is **refused at construction** rather than ignored —
there is no OS boundary here to confine a write set with. Interactive sessions, memory
and cpu sampling and `Attach` all come from `sessionkit`.

### `remote`

`sandbox/remote` is a `sandbox.Runner` whose processes execute on another host: the
client is a proxy, `remote.NewServer` wraps any other runner, and nothing above the
client changes — the tool set, the approval path and the confine see the interface they
see for a local backend. It reports what its server declared, which is what keeps
`ValidateConfine` honest across the wire. See [sandbox-remote.md](sandbox-remote.md).

### Which shell

`sandbox.DefaultShell(goos)` and `sandbox.ShellFor(name, path, args)` are the only place
the platform decides which shell exists on a host, and `ShellSpec.Argv` is how a command
line is wrapped in it. The exec tool uses them; a backend never does.

## Not the sandbox's job

Two command rules belong to the host, because asking whether a command may run — and
asking again whether a refused one may leave the boundary — is policy rather than a
capability: approval (`host.Approval`) and escalation (`host.Escalator`, only ever offered
for a `sandbox.ErrConfineDenied`). They decorate
the narrow `SessionStarter`, so a tool gets the operator's decision for free. See
[runtime.md](runtime.md).

A delegated subagent is a session of its own: its confine comes from the authority the
delegation resolves for it — by default the parent's, which is the only rule that cannot
escalate — and core derives no mode for it. Its command questions go to the parent's
model before a person is asked: see [delegation.md](delegation.md#approvals).

## What this replaced

The previous `core/sandbox/engine` package is gone: the `Exec` decorator stack
(`WithDefaults`, `AllowCommands`, approval, `Escalation`/`EscalationRunner`,
`ClassifySafeReadOnly`), `ValidateExecPolicy` and the safe-read-only classifier. So is
`core/permission` — its `Policy`/`Writes` per-call permission is now the grant's `files`
and the session's confine — and the `sandbox.backend/writes/net/journal` deployment
block, which became `sandbox[]` in the process document plus `files`/`net`/`exec`/
`resources` on the session's grant. The file journal (`OpenJournal`, the watch set, the gap
vocabulary) went with it. Approval and escalation did not disappear, they moved to
`core/host`; the one-shot form did not disappear either, it is now
`sandbox.Exec` over a `SessionStarter`, with no decorator chain to keep in step.
