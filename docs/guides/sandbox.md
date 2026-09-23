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
    Close() error
    Capabilities() Capabilities
    Start(ctx context.Context, spec SessionSpec) (Session, error)
    List(ctx context.Context) ([]SessionInfo, error)
    Terminate(ctx context.Context, id string) error
}
```

Every runner is a process session manager; the one-shot
`Exec(ctx, runner, cmd, args, opts)` is a derived view over `Start`.
Backends must reject any policy they cannot enforce rather than silently
downgrade. `Close` releases everything the runner owns — sessions, a
file journal, backend state — and the decorators (`AllowCommands`,
`WithApproval`, `WithDefaults`) forward both `Close` and the journal so
wrapping never hides the backend.

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
  resource caps, opt-in Low-integrity write confinement, and
  AppContainer / WFP network policy.

Every backend that watches also takes a `journal:` settings subtree; see
[File journal](#file-journal) for what it reports and on which
platforms. The backends are decoded strictly, so asking for a journal a
platform cannot provide fails the host build rather than being ignored.

The bwrap and seatbelt backends share the same settings shape:

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

The windows backend takes `write_confine` (opt-in Low-integrity token
write confinement) and `writable_paths` (paths the confined child may
write) instead of `binary` / `readonly_root` / `extra_flags`:

```yaml
resources:
  box:
    kind: sandbox.Runner
    impl: windows
    settings:
      root: ./sandbox
      write_confine: true        # optional; Low-integrity write confinement
      writable_paths: [./out]    # optional; writable paths under confinement
```

Without `write_confine` the windows runner is lifecycle-only and
`WriteReadOnly` execs are rejected with NotAvailable; network policy
(`NetDenyAll`, `NetAllowList`, `NetProxy`) additionally requires an
elevated host, otherwise it fails closed with NotAvailable.

## Platform support

| Backend | Platform | Isolation | Net modes | Write policy | Resource caps |
|---|---|---|---|---|---|
| `local` | all (interactive sessions unix-only) | none | `NetDefault` only | none enforced | memory / cpu via group watcher (unix) |
| `bwrap` | Linux | user / mount / pid / net namespaces | `NetDefault`, `NetDenyAll`, `NetAllowList`, `NetProxy` | root + `writable_paths`, `WriteReadOnly` | memory / cpu (watcher) |
| `seatbelt` | macOS | Seatbelt (SBPL) | `NetDefault`, `NetDenyAll`, `NetAllowList`, `NetProxy` | root + writable paths, `WriteReadOnly` | memory / cpu (watcher) |
| `windows` | Windows | Job Objects + optional Low-integrity token (`WithWriteConfinement`) + AppContainer (`NetDenyAll`, `NetAllowList`, `NetProxy`) | `NetDefault`, `NetDenyAll`, `NetAllowList`, `NetProxy` | root + `writable_paths`, `WriteReadOnly` (with write confinement) | memory / cpu (job limits) |

The `windows` backend makes command execution work on Windows: every
child runs in its own job object, timeout / cancel / close terminate
the whole process tree, and memory / cpu limits are enforced by the
kernel. Write confinement is opt-in via `WithWriteConfinement`: the
child runs with a restricted Low-integrity token and only the runner
root plus explicit `writable_paths` are labeled writable (all reads
stay allowed). Network policy runs the child under an AppContainer
token with no network capabilities in every mode. The OS firewall's
AppIsolation default blocks TCP and connected flows; because it does
not constrain unconnected UDP or ICMP sockets, the backend also
installs WFP bind-layer filters for the sandbox's package SID —
`NetDenyAll` blocks every bind, while `NetAllowList` / `NetProxy`
permit only TCP binds, pin the container to a host-side enforcement
proxy, and inject the proxy environment, mirroring the seatbelt
architecture. These modes require an elevated host and fail closed
with `errdefs.NotAvailable` otherwise. Interactive sessions run through
ConPTY: stdout and stderr merge into a single TTY stream and `Resize`
applies to the pseudo console (TTY combined with write confinement or
a net policy is not available yet). Permission bits are advisory on
Windows: `chmod`-style modes
map only to the read-only attribute, and real access control comes
from the directory ACLs inherited at creation. Code that passes modes
like `0o600` / `0o755` still runs unchanged, but treat such modes as
intent, not as an enforceable boundary (for example `0o600` does not
hide a file from other users on a shared drive).

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
reports no write modes in `Capabilities`. The file journal watches the
**runner-level** boundary: a per-call `WriteReadOnly` does not
re-register watches, it only stops the call from writing.

For read-only auto-approval, `ClassifySafeReadOnly` implements the
codex-rs-style heuristic (base read-only commands plus argument-aware
checks for `find` / `rg` / `git` / `sed` / `sort`, sharing the
allowlist's shell unwrapping: a script counts only when the wrapper is
the exact supported form (`sh -c`, `cmd.exe /c`,
`pwsh -NoProfile -Command`) and the script is a single plain command;
combined and abbreviated forms such as `bash -lc`, `cmd /k` or a
PowerShell command without `-NoProfile` are never unwrapped). `date`
and `hostname` are deliberately not auto-approved: `date -s` changes
the system clock and `hostname newname` changes the host name —
non-file writes the OS sandbox cannot block. It is a caller-side
helper — the host's `ApprovalFunc` decides:

```go
if req.Opts.Write == sandbox.WriteReadOnly && sandbox.ClassifySafeReadOnly(req.Exec) {
    return sandbox.Allow, nil
}
```

It never denies and never widens policy; unrecognized commands return
`false` and route to the human approver.

## File journal

A runner can report the writes that happen inside it as a readable,
replayable event stream. That is what the journal is for: knowing which
artifacts a turn produced, without scanning trees before and after. It
exists on the backends and platforms that have a watch source, and each
source reports in the unit its kernel charges for:

| Backend | Source | Cost of one watch | Rename pairing | Identity (`Dev`/`Ino`) |
|---|---|---|---|---|
| `local` | inotify (Linux) / kqueue (macOS) | per directory / per entry | yes | yes |
| `bwrap` | inotify (Linux) | per directory | yes | yes |
| `seatbelt` | kqueue (macOS) | per entry | yes | yes |
| `windows` | ReadDirectoryChangesW (Windows) | per directory + its change buffer | within one directory | no |

Writer attribution is `no` everywhere (`WriteEvent.Session` stays
empty): inotify cannot name the writer without `CAP_SYS_ADMIN`, kqueue
cannot name it at all, and a guess from timing would be a
data-correctness bug.

The source in that table is the platform's; the backend in front of it
is what a deployment configures. All four attach theirs: `local`
(inotify, kqueue), `bwrap` (inotify), `seatbelt` (kqueue) and `windows`
(ReadDirectoryChangesW) — so a platform without a source (the BSDs
today) fails the host build when `journal:` is configured, instead of
producing a runner that reports nothing.

It is opt-in per resource — an absent `journal:` key costs nothing (no
file descriptor, no goroutine, no watch):

```yaml
resources:
  box:
    kind: sandbox.Runner
    impl: bwrap            # or local, seatbelt, windows
    settings:
      root: .
      writable_paths: [./out]
      journal:
        exclude: [.git, node_modules, dist, target, .venv]
        retention: 4096          # events kept readable for replay
        max_watch_set: 20000     # watch budget; past it, a capacity gap
        ops: [create, rename, remove]   # drop "write" events; a filtered op is not a gap
```

`exclude` lists directories relative to the runner root whose subtree is
never watched; it is the cost control (about 1 KB of kernel memory per
directory watch on Linux, one descriptor per watched entry on macOS),
and the journal applies no default list of its own — an implicit filter
would hide writes a deployment believes it is watching. `retention`
bounds the replay window; `max_watch_set` bounds the watch set (*how
many* watches, in the platform's own unit — see the table above). Both
are validated at build time, so a misconfiguration fails the deployment
instead of quietly watching less.

On macOS the budget is derived from the process's descriptor ceiling,
and attaching a journal raises the soft `RLIMIT_NOFILE` toward it (never
down, never past the hard limit) — a sandbox root is usually tens or
hundreds of entries, but a build tree that exceeds the budget is a
reported capacity gap, not a truncated watch set. Descriptor limits are
process-wide, so a host that would rather own that decision can set the
limit itself before building the runner; the journal only ever lifts it.
Per watched directory the source also keeps one snapshot in user space
(the entries' names and identities), which is what makes the diffing —
and with it the "no queue overflow to report" property — possible.

What a directory note costs there is worth knowing, because macOS is the
one platform where it is not a single syscall: the directory is re-read
and diffed, so the price is the width of the directory rather than the
size of the change. The re-read is one pass over the directory's own
records — each carries its entry's type and inode number — plus a stat
for the entries those records cannot describe on their own, which is
every directory (a directory entry may be a mounted volume, and only a
stat sees that). Measured on APFS over a directory of 4096 entries: one
re-read is ~3.5 ms, and 40 creations 15 ms apart burn about a third of a
core, against a full core before the records were read for their
identities. The read buffer is sized from what the last scan of that
directory read, because a read call here walks the whole directory
again: the same 164 KB of records cost 3.6 ms in one call and 19.8 ms in
twenty-one 8 KB calls. A wide, flat directory is the shape this costs
most in, which is what `exclude` is for.

On Windows the cost is a buffer rather than a pass: each watched
directory holds one read pending on a completion port, and that read's
16 KiB change buffer is memory the kernel keeps (and pins) until the
read completes. The default budget of 4096 watches is therefore 64 MiB of
buffers and 64 MiB more pinned — the platform's budget is about this
process's memory rather than the kernel's own limit, which is why
`max_watch_set` is worth setting there. Overflow is reported, not hidden:
a directory that produces more changes than its buffer can describe
comes back as a `overflow` gap, and the watch keeps reporting.

### What the stream contains

Net state changes, not a syscall trace: a file created and written in
one go is **one** `create`, an `mv` is **one** `rename` (with `OldPath`),
a removal is one `remove`, and attribute changes (`chmod`, `touch` on an
existing file) or reads are never events — a chip saying "written" for a
`cat` would be a false positive. A file that appears and disappears
before its writer closes it reports nothing at all, because nothing was
produced.

The sources see different edges, so the same turn can describe the same
files slightly differently. inotify reports a writer's close, so a
create and a write are dated at that edge and a write to an existing
file is reported as one `write` per close. kqueue has no close edge: an
appearance and a content change settle in the collection window and are
reported when they stop changing (a few hundred milliseconds, the same
window that turns "created and written" into one `create`), and a
truncate is recognised by the size it left behind. ReadDirectoryChangesW
has no close edge either, and two of its own: a move is one `rename` when
the platform writes both halves of it adjacently in one read — what a
rename inside one directory produces — while a move between two
directories carries no pairing information, and is reported as the
`remove` and the `create` it also is; and a content change is reported
when the filesystem records the write, so a writer that keeps its bytes
in the cache has its `write` reported late (never wrongly). What all of
them report the same way: a disappearance inside the window is nothing,
and a filtered op is never a gap.

Paths under the root are relative, with `/` separators and no leading
slash; paths under a `writable_paths` entry outside the root carry that
cleaned absolute path. `IsDir` marks directory events: a new directory
is reported once, and its contents follow as their own events.

`Session` is the sandbox session that made the change **when the backend
can name it precisely**. inotify cannot (fanotify and eBPF need
`CAP_SYS_ADMIN`/`CAP_BPF`) and kqueue has no such facility at all, so
every event is unattributed: `Session` is empty rather than guessed from
timing. Unattributed is a valid answer; a wrong attribution is a
data-correctness bug.

### Reading it

```go
j, err := sandbox.OpenJournal(ctx, runner) // errdefs.NotAvailable when there is none
if err != nil { /* not enabled, or this platform has no watch source */ }
defer j.Close()

cursor := int64(0)
batch, err := j.Read(ctx, cursor, 512)   // never blocks, never waits
cursor = batch.NextSeq
if batch.Gap != nil { /* coverage is incomplete for this window */ }
```

Reads are cursor operations: several readers hold independent positions,
and re-reading from an earlier cursor replays from the retained window.
`Read` never waits for future events, so a consumer drains on its own
schedule and persists what it needs — the window is bounded, and events
outside it are gone.

The invariant that makes the stream usable is that `(afterSeq, NextSeq]`
is always accounted for: every seq in it was either returned or declared
lost. A non-nil `Gap` is a statement about coverage, not an error and not
an empty range — when the extent of a loss is unknown (a kernel queue
overflow does not say how many events it dropped) `FirstMissing` is
larger than `NextSeq`, and the reading is "coverage after
`FirstMissing-1` is not guaranteed". Reasons: `overflow` (kernel queue),
`retention` (the reader fell behind the window), `capacity` (the watch
could not be registered — watch limits, descriptor limits, a missing
directory), `watch_lost` (a watched subtree was moved or detached),
`backend` (the source itself failed, and the journal froze). A consumer
must render a gap as "this may be incomplete", never as "nothing else
was written". `overflow` is inotify's: kqueue coalesces notes rather
than dropping them, and the macOS source re-derives state from the
filesystem, so a change it missed wakes it up later instead of
disappearing.

`JournalBatch.Closed` reports that the journal is frozen — the runner
was closed, or the source died — and no further events will arrive; the
retained residue stays readable until the reader is closed.

### What it is not

Not an enforcement boundary: the backend's write confinement still
decides what is allowed, and the journal reports what happened. Not an
audit log: events can be missing, and the window is bounded. Not a
replacement for `core/workspace`: the workspace decorator sees only
writes that go through its interface but attributes them precisely,
while the journal sees every write but cannot attribute it — prefer the
precise half when both describe the same path. On macOS a move is one
`rename` only when the entry's own watch saw the name change and an
entry with the same inode appeared in the watched tree: a hardlink
swap (`ln f g; rm f`) is a removal plus an
appearance, and a move whose destination is outside the watched tree is
a removal — the same statement inotify produces when `MOVED_TO` never
arrives. And on network or exotic mounts the kernel may report nothing
at all (inotify semantics on Linux, kqueue's notes on macOS); the
journal cannot detect "this mount reports nothing", which is the one
gap it cannot report.

On a platform without a watch source (the BSDs today, or a GOOS nobody
has written a source for), a deployment that configures `journal:` fails
at build time (`errdefs.NotAvailable`) rather than producing a runner
that reports nothing.

## Policy groups

`ExecOptions` carries:

- `WorkDir`, `Stdin`, `Timeout`;
- `Env` allow-list/inject policy;
- `Net` network policy;
- `Resources` memory/cpu/output caps.

See [workspace.md](workspace.md) for state storage.
