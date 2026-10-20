---
layout: default
title: Migrating the keychain secret store into core
---
# Migrating the keychain secret store into core

The `backends/secret` module was retired and its one package moved into the
platform module as `core/secret/keychain`. The store itself did not change:
it has no third-party dependencies, so folding it into `core` adds no
requirement to anything that requires `core`, and it now sits beside the
sandbox backends (`core/sandbox`), which are the same kind of built-in
platform implementation.

## What moved

| Removed module | Use instead | Package |
| --- | --- | --- |
| `backends/secret` | `core` | `github.com/GizClaw/flowcraft/core/secret/keychain` |

## Deployment documents

Nothing changes. The resource kind (`secret.Store`), the impl id
(`keychain`), the settings (`id`, `default`, `dir`) and the `${secret:...}`
references are all the same, so an existing document keeps working once the
host builds against a `core` tag that contains the package:

```yaml
resources:
  secret.keychain:
    kind: secret.Store
    impl: keychain
    settings:
      dir: ${base:secrets}
```

## Go callers

| Before | After |
| --- | --- |
| `github.com/GizClaw/flowcraft/backends/secret/keychain` | `github.com/GizClaw/flowcraft/core/secret/keychain` |

`keychain.Register`, `keychain.NewStore`, `keychain.Manager` and
`keychain.ResourceImpl` keep their names and signatures, so the edit is the
import line plus the `go.mod` requirement: the `backends/secret` require goes
away and `core` has to be on a tag that ships the package.

```go
-import "github.com/GizClaw/flowcraft/backends/secret/keychain"
+import "github.com/GizClaw/flowcraft/core/secret/keychain"
```

## Published versions

`backends/secret/v0.1.0` stays on the module proxy and keeps resolving for
builds that still pin it; it receives no further changes. New work targets
`core/secret/keychain`.
