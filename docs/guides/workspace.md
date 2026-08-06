---
layout: default
title: Workspace
---
# Workspace Guide

`sdk/workspace` is a persistent filesystem abstraction for per-run
state. Knowledge, Skills, and Memory subsystems share a single
Workspace; the execution boundary (commands, network, resources)
lives in the sibling package [`sdk/sandbox`](sandbox.md).

The split is deliberate: **workspace is state**, **sandbox is
policy**. A workspace is a file tree the agent reads and writes;
a sandbox is a `Runner` that executes commands under a policy
surface. Scripts in a graph bind a workspace for filesystem
operations and a sandbox for command execution — they are not
interchangeable.

## Concepts

### The Workspace interface

```go
type Workspace interface {
    Read    (ctx context.Context, path string) ([]byte, error)
    Write   (ctx context.Context, path string, data []byte) error
    Append  (ctx context.Context, path string, data []byte) error
    Rename  (ctx context.Context, src, dst string) error
    Delete  (ctx context.Context, path string) error
    RemoveAll(ctx context.Context, path string) error
    List    (ctx context.Context, dir string) ([]fs.DirEntry, error)
    Exists  (ctx context.Context, path string) (bool, error)
    Stat    (ctx context.Context, path string) (fs.FileInfo, error)
}
```

All paths are **relative to the workspace root**. Absolute paths and
`..` traversals are rejected with `errdefs.ErrPathTraversal` —
a workspace is a chroot, not a global filesystem.

`Rename` is the canonical "publish a finalized payload" operation:
write to a tmp path, then `Rename` to the live path so readers
never observe a half-written file. Whether the implementation can
do this atomically is reported via `CapabilitiesOf(ws).AtomicRename`.

### Capabilities

Adapters that need accurate semantics (e.g. an LSM-style retrieval
index that relies on atomic rename) read `Capabilities` via
`CapabilityReporter` (or the `CapabilitiesOf(ws)` helper) rather than
hard-coding per-implementation assumptions. All fields default to false — the conservative
"no guarantees" interpretation.

| Field              | Meaning                                                                                                    |
| ------------------ | ---------------------------------------------------------------------------------------------------------- |
| `AtomicRename`     | `Rename` is single-observer-atomically POSIX-clean                                                         |
| `ReadAfterWrite`   | a successful `Write`/`Append` is immediately visible to `Read`/`List`/`Exists`/`Stat` from the same client |
| `DurableOnWrite`   | a successful write hits stable storage before returning                                                    |
| `Distributed`      | more than one process or host can open the same workspace concurrently                                      |

Adding a new field is additive — adapters that don't know about it
keep working with the conservative default.

## First workspace

```go
ws, err := workspace.NewLocalWorkspace("./state/project")
if err != nil { panic(err) }

if err := ws.Write(ctx, "notes/today.md", []byte("# today\n")); err != nil { return err }

data, err := ws.Read(ctx, "notes/today.md")
if err != nil { return err }

// Publish a finalized payload atomically (best-effort depending on backend).
if err := ws.Rename(ctx, "drafts/v1.md", "published/v1.md"); err != nil { return err }
```

`NewLocalWorkspace` resolves the root through `filepath.Abs` and
`EvalSymlinks` so a symlink swap at the root does not become a
path-traversal hole. `LocalWorkspace` exposes a `Root()` method for
diagnostics; it is not part of the `Workspace` interface.

## Backends

| Type                      | Backing store                                   | Atomic rename         | Use when                                                                |
| ------------------------- | ----------------------------------------------- | --------------------- | ----------------------------------------------------------------------- |
| `LocalWorkspace`          | local directory                                 | yes (POSIX rename(2)) | production per-host state, durable across restarts                      |
| `MemWorkspace`            | in-memory map                                   | yes (in-process)      | tests, ephemeral runs, scratch space                                    |
| `ScopedWorkspace`         | wraps another `Workspace` with deny/allow rules | inherits inner        | least-privilege isolation: deny-by-default read, allow-by-default write |
| `Sub(ws, prefix)`         | virtual subtree of another `Workspace`          | inherits inner        | composability: a subsystem sees its own prefix as root                  |
| `sdkx/workspace/objstore` | S3 / GCS / Azure Blob                           | backend-dependent     | cross-host shared state                                                 |

`Sub` is the composability primitive: wrap a workspace with a
prefix and the inner workspace's root is hidden. A memory subsystem
can take a `Sub(ws, "memory/")` and not see `skills/` or
`knowledge/`.

`Scoped` is the permission primitive: it wraps a workspace with
two pathsets:

- `denyRead` — deny-only read; everything else is readable
- `allowWrite` — allow-only write; nothing else is writable
- `mandatoryDeny` — always blocked for both read and write

A typical setup: `denyRead: [".env", "secrets/*"]`, `allowWrite: ["drafts/*", "memory/*"]`. The mandatory-deny list is the safety
net for paths that must never be readable even if `allowWrite`
matches.

## Workspace vs Sandbox

|             | Workspace                            | Sandbox                             |
| ----------- | ------------------------------------ | ----------------------------------- |
| Models      | files (data)                         | commands (behaviour)                |
| Reaches     | persistence layer                    | kernel / namespace / microVM        |
| Policy axis | path allow / deny                    | env / net / resources / approval    |
| Bind by     | `deps.workspace: ws/<name>` in graph | `deps.sandbox: box/<name>` in graph |
| Where       | `sdk/workspace`                      | `sdk/sandbox`                       |

A graph script can read and write workspace paths; it executes
commands through the sandbox. A script that needs both binds both
`workspace` and `sandbox` deps.

## Deploy integration

`workspace.Registry` is a first-party resource in deployment
documents. The registry holds named workspaces (each with a driver
and settings) and exposes them as `ItemResolver` so graph agents
bind `ws/<name>`:

```yaml
resources:
  workspaces:
    kind: workspace.Registry
    impl: yaml
    settings:
      file: ./workspaces.yaml
```

Relative local-driver roots inside `workspaces.yaml` resolve against
the host workspace config builder's `Deps.BaseDir`, not a document
field.

```yaml
# workspaces.yaml
version: v1
workspaces:
  project:
    driver: local
    settings:
      root: ./state/project
  scratch:
    driver: mem
  skills:
    driver: local
    settings:
      root: ./skills
```

Graph binding:

```yaml
agents:
  researcher:
    engine:
      kind: graph
      settings:
        graph: ./graphs/research.json
    deps:
      workspace: workspaces/project
      sandbox: sandboxes/coding
```

The graph factory's `workspace` dep contract is "optional, used by
scripts that need filesystem access." See
[deploy.md#engines](deploy.md#engines).

Built-in drivers are `local` and `memory`; `scope` is a per-entry policy
applied on top of either driver, and `Sub()` is a programmatic composability
API, not a driver. Object-store drivers live in `sdkx/workspace/objstore`
and register as additional `(kind, impl)` pairs.

## Testing

A workspace test is hermetic: use `MemWorkspace` or a `t.TempDir()`
with `LocalWorkspace`. The same `Workspace` interface works for
both, so a tool or script that needs a workspace is testable
without a filesystem fixture.

```go
ws := workspace.NewMemWorkspace()
if err := ws.Write(ctx, "a.txt", []byte("hello")); err != nil { t.Fatal(err) }
data, _ := ws.Read(ctx, "a.txt")
if string(data) != "hello" { t.Fatal("read mismatch") }
```

For permission tests, build a `ScopedWorkspace` over a `MemWorkspace`
and assert on the exact errors for denied paths
(`workspace.ErrAccessDenied`).

## Further reading

- Package contract: `sdk/workspace/workspace.go`,
  `sdk/workspace/capabilities.go`, per-backend files
  (`local.go`, `mem.go`, `scoped.go`, `sub.go`).
- Object-store backends: `sdkx/workspace/objstore/workspace.go`,
  `sdkx/workspace/objstore/s3/register.go`.
- Assembly: `sdk/workspace/config/doc.go`, the `workspace.Registry`
  resource in [deploy.md](deploy.md#first-party-impls).
- Sibling guide: [sandbox.md](sandbox.md) (state vs policy).
