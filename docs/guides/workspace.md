---
layout: default
title: Workspace
---
# Workspace Guide

`core/workspace` is the filesystem abstraction for per-run state. The split
is: **workspace is state**, **sandbox is policy**.

## Workspace interface

```go
type Workspace interface {
    Read(ctx context.Context, path string) ([]byte, error)
    Write(ctx context.Context, path string, data []byte) error
    Append(ctx context.Context, path string, data []byte) error
    Rename(ctx context.Context, src, dst string) error
    Delete(ctx context.Context, path string) error
    RemoveAll(ctx context.Context, path string) error
    List(ctx context.Context, dir string) ([]fs.DirEntry, error)
    Exists(ctx context.Context, path string) (bool, error)
    Stat(ctx context.Context, path string) (fs.FileInfo, error)
}
```

Paths are relative to the workspace root. Absolute paths and `..` escapes are
rejected.

## Deployment resource

```yaml
resources:
  ws:
    kind: workspace.Workspace
    impl: local
    settings:
      root: ./workspace
      scoped:
        enabled: true
        deny_read: ["**/.env"]
        allow_write: ["**"]
```

`settings.root` supports scalar settings expansion: `${env:NAME}` reads an
environment variable (an unset variable fails the build), `${base}` /
`${base:rel}` resolve against the deployment document's base dir, and `~`,
`~/...`, `${home}`, `${home:rel}` resolve against the user home directory. A
plain relative root still resolves against the deployment base dir. Expansion
applies to the whole settings subtree, so scoped patterns expand too; they
must remain relative paths. A literal `${` can be written as `\${...}` (the
backslash is consumed); this matches the graph's `${board.*}` escaping rule.

Object-store backends are app-registered and not part of the current core module.

See [sandbox.md](sandbox.md) for the execution boundary.
