---
layout: default
title: Sandbox
---
# Sandbox Guide

`core/sandbox` is the command execution boundary, and it is three layers:

| Layer | Package | Role |
| --- | --- | --- |
| Contract | `core/sandbox` | `Runner`, `ExecOptions`, `Session`, `Capabilities`, `Journal`, `Attach`. No behaviour beyond validation helpers. |
| Policy | `core/sandbox/engine` | The one-shot `Exec`, the decorators (defaults, allowlist, approval, escalation), the safe-read-only classifier, `ValidateExecPolicy`. |
| Backends | `core/sandbox-local`, `-bwrap`, `-seatbelt`, `-windows`, `-remote` | The process boundary itself. |

The split exists so a contract package never imports `os/exec` and a backend
never re-derives the policy rules. The backends are siblings of `core/sandbox`
rather than subpackages: `core/sandbox/internal/...` is unreachable from another
directory tree, and what they do share — the session registry, the spawn path,
the group resource sampler (`core/sandbox/sessionkit`) and the journal contract
suite (`core/sandbox/journal/journaltest`) — is public for that reason. Verify a
new backend with `core/sandbox/sandboxtest`.

## Runner

```go
type Runner interface {
    Close() error
    Capabilities() Capabilities
    Start(ctx context.Context, spec SessionSpec) (Session, error)
    List(ctx context.Context) ([]SessionInfo, error)
    Terminate(ctx context.Context, id string) error
}
```

Every runner is a session manager. The one-shot form is
`engine.Exec(ctx, runner, cmd, args, opts)` — a derived view over `Start` plus
the terminal status, and the only place a call is known to be finished.

Backends must reject a policy they cannot enforce rather than silently
downgrade: `engine.ValidateExecPolicy(opts, caps)` is the shared check, and
`Capabilities.Enforcement` is what it reads (`WriteConfinement`,
`FilesystemBounds`, `MemoryCap`, `CPUCap`, `DiskCap`, `NetModes`, `EnvAllowList`,
`Socks5`, `MITM`, `UnixSocketPolicy`).

`Close` releases what the runner owns — sessions, a file journal, backend state
— and the decorators (`AllowCommands`, `WithApproval`, `WithDefaults`) forward
both `Close` and the journal, so wrapping never hides the backend.

## Sessions

Sessions declare their input surface up front. `SessionSpec.Stdin` says the
caller will feed the child through `Write` / `CloseInput`, and
`SessionCapabilities.Stdin` reports whether a started session has somewhere to
write. A non-TTY session without that declaration leaves the child's fd 0 at the
OS default (`/dev/null`), so a tool that branches on stdin — `rg` with no path
operand, `cat`, `test -p /dev/fd/0` — reads its operands instead of an empty pipe
that never ends; `Write` on such a session fails with `errdefs.NotAvailable`
instead of silently buffering.

`ExecOptions.Stdin` is one-shot input, not a declaration: `Start` neither
delivers those bytes nor pipes fd 0 for them. `engine.Exec` sets
`SessionSpec.Stdin` itself before it writes those bytes and closes the pipe.

A session that was started can be re-acquired later, by something that no longer
holds the handle — usually a different request:

```go
type SessionAttacher interface {
    Attach(ctx context.Context, id string) (Session, error)
}

sess, err := sandbox.Attach(ctx, runner, id)
```

`Attach` returns a session that replays output from the beginning and reports
the exit status, without the caller having kept the original handle. An unknown
id is `errdefs.NotFound`; a session whose spawn has not settled yet is
`errdefs.NotAvailable`. `core/sandbox-local` implements it over the session
registry, and `core/sandbox-remote` proxies it to the server, so the same call
works against a local or a remote boundary.

Capabilities describe what a started session can do: `TTY` (pty sessions, with
`Resize` and merged output), `Signal` (signal delivery), `Events` (push streams
through `Watch` — a backend that reports `Events` must be able to deliver the
terminal status, which the conformance suite checks).

## The deployment block

A backend is chosen in the runtime document, not in a resource table:

```yaml
sandbox:
  backend: seatbelt            # local | seatbelt | bwrap | windows | remote
  workdir: /workspace/forge    # default: the first files root
  writes: [/workspace/forge/out]
  net:
    mode: allow_list
    allow_hosts: [api.openai.com]
  timeout: 60s                 # ceiling; a tool may ask for less
  cpu_millicores: 2000         # rejected without timeout
  memory_mib: 2048
  output_mib: 8
```

| Field | Meaning |
| --- | --- |
| `backend` | Required, no default: a boundary picked by guessing is a boundary nobody chose. |
| `workdir` | Where commands run; empty means the first files root. |
| `writes` | Absolute paths a command may write; empty means read-only. |
| `net` | `mode` plus `allow_hosts` (allow-list only) and `proxy` (proxy only). |
| `timeout`, `cpu_millicores`, `memory_mib`, `output_mib` | The resource budget. |
| `journal` | A settings block; presence opts in. See [File journal](#file-journal). |
| `bwrap`, `seatbelt`, `windows` | The backend's own construction options. |
| `remote` | Client configuration for `backend: remote`. See [sandbox-remote.md](sandbox-remote.md). |

The budget reaches every call: the runtime wraps the backend with
`engine.WithDefaults`, which projects `timeout`, `cpu_millicores`, `memory_mib`
and `writes` onto the `ExecOptions` of each call it issues. Merging is per
field and takes the stricter side — zero defers, of two non-zero values the
smaller wins — so a tool cannot widen the deployment and the deployment cannot
silently widen a tool. Three consequences worth knowing:

- a cap the backend cannot enforce (a cpu budget on a backend without `CPUCap`,
  `writes` on one without `WriteConfinement`) fails the build, naming the field;
- `writes` on `local` and `remote` only warns: `local` has no boundary and
  `remote` enforces it on the server, so the value would be decoration;
- `cpu_millicores` without `timeout` is a validation error, because the budget
  is derived from the deadline.

### `local`

No isolation, for trusted workflows: `Capabilities.Enforcement` is empty, so a
deployment that asks for a network mode or a write boundary is rejected. Unix
hosts still get memory / cpu sampling and session support.

Interactive sessions are unix-only **today, by implementation rather than by
design**: `sessionkit` ships the session machinery for the unix spawn path (pty,
process groups, signals) and a stub elsewhere, while the Windows session stack
lives in `sandbox-windows`. The intended shape is one session layer in
`sessionkit` with the platform differences behind it (pty vs ConPTY, process
group vs job object), which makes `local` portable and shrinks `sandbox-windows`
to its policy layer. Until that lands, a Windows deployment uses
`backend: windows`, whose lifecycle-only mode is the same "no isolation beyond
the OS keeping the process tree" promise.

### `bwrap` and `seatbelt`

```yaml
sandbox:
  backend: bwrap               # or seatbelt
  bwrap:
    binary: /usr/bin/bwrap     # optional; resolved against the root
    extra_flags: [--die-with-parent]   # bwrap only
    readonly_root: true        # keep the runner root read-only
```

| Field | Rule |
| --- | --- |
| `binary` | Overrides the backend executable; resolved against the runner root. |
| `readonly_root` | Keeps the runner root read-only for every exec. Explicit `writes` stay writable, and a `writes` entry that resolves to the root is rejected instead of being dropped. |
| `extra_flags` | bwrap only. A flag that could weaken the policy (`--ro-bind`, `--args`, …) is rejected at build time. |

Both enforce namespaces (bwrap) or a Seatbelt profile (seatbelt), and both
support `NetDefault`, `NetDenyAll`, `NetAllowList` and `NetProxy`.

### `windows`

```yaml
sandbox:
  backend: windows
  windows:
    write_confine: true        # opt-in Low-integrity write confinement
```

Every child runs in its own Job Object: timeout, cancel and close terminate the
whole process tree, and memory / cpu limits are enforced by the kernel. Without
`write_confine` the runner is lifecycle-only and a call that asks for write
confinement is rejected with `NotAvailable`. Network policy runs the child in an
AppContainer with no network capabilities, plus WFP bind-layer filters for
`NetDenyAll`; `NetAllowList` / `NetProxy` permit only TCP binds, pin the
container to a host-side enforcement proxy and inject the proxy environment.
Those modes need an elevated host and fail closed otherwise. Interactive
sessions use ConPTY (merged output, `Resize`); TTY combined with write
confinement or a net policy is not available yet.

Permission bits are advisory on Windows: `chmod`-style modes map only to the
read-only attribute, and real access control comes from the directory ACLs
inherited at creation. Code passing `0o600` still runs, but treat such modes as
intent, not as a boundary.

## Per-exec permission

`ExecOptions.Permission` narrows the boundary for a single call:

```go
opts := sandbox.ExecOptions{
    Permission: &permission.Policy{
        Writes: permission.Writes{Paths: []string{"/workspace/forge/out"}},
    },
}
```

A supplied `Permission` **replaces** the runner's construction-time defaults
rather than adding to them: the caller stated the boundary, so the backend must
not quietly widen it back to the runner root. An empty writable list therefore
means read-only, which is how a call asks for it without a second enum. There is
no widening mode, and `engine.WithDefaults` follows the same rule — the stricter
side wins, per field.

The file journal watches the **runner-level** boundary: a per-call permission
does not re-register watches, it only narrows what the call may write.

## Safe read-only auto-approval

`engine.ClassifySafeReadOnly` implements the codex-rs-style heuristic (base
read-only commands plus argument-aware checks for `find` / `rg` / `git` / `sed`
/ `sort`). It shares the allowlist's shell unwrapping: a script counts only when
the wrapper is the exact supported form (`sh -c`, `cmd.exe /c`,
`pwsh -NoProfile -Command`) and the script is a single plain command; combined
and abbreviated forms such as `bash -lc`, `cmd /k` or a PowerShell command
without `-NoProfile` are never unwrapped. `date` and `hostname` are deliberately
not auto-approved: `date -s` changes the system clock and `hostname newname`
changes the host name — non-file writes no OS sandbox can block.

It is a caller-side helper; the host's `ApprovalFunc` decides:

```go
if req.Opts.Permission != nil && req.Opts.Permission.Writes.IsZero() &&
    engine.ClassifySafeReadOnly(req.Exec) {
    return sandbox.Allow, nil
}
```

It never denies and never widens policy; an unrecognized command returns `false`
and routes to the human approver.

## Escalation: one approved retry outside the confine

`WithApproval` answers "may this command run", never "under which boundary": a
command the approver allowed and the OS sandbox then refused fails like any other
command. Escalation is the second, explicit question:

```go
esc := &engine.Escalation{
    Confined: confined, // approval / defaults / OS-backend chain
    // The retry target is a chain of its own; keep the floor a retry must not drop.
    Unconfined: engine.WithDefaults(
        engine.AllowCommands(local.New(root), allowed),
        defaults),
    Escalate: policy, // the ask channel
    Rules:    policy, // remembered "run outside" rules
    Gate:     host,   // mode owner: is the retry offerable?
}
res, outcome, err := esc.Exec(ctx, cmd, args, opts)
```

The command runs through `Confined`; when `engine.Denied(res)` recognizes a
refusal in the captured output, the gate is consulted, the user is asked once,
and an approved answer re-runs the whole command through `Unconfined`, exactly
once, with byte-identical `ExecOptions`. The prompt must say that the command
repeats: a command that failed halfway may already have had side effects.

Semantics worth relying on:

- **Denial detection is a tripwire, not a wall.** `Denied` matches the fragments
  the three confinement layers leave behind (seatbelt EPERM, bwrap read-only
  binds, the Windows access-denied spellings) in either captured stream — the
  shells that phrase them disagree on stderr vs. stdout — and deliberately
  ignores plain "Permission denied" and the cannot-execute statuses 126/127:
  those are ordinary file-mode and lookup failures, and prompting for them would
  train users to approve noise. A false positive only produces a prompt the user
  can decline; the OS backend stays the wall. The table is pinned to reality by
  the seatbelt / bwrap / windows integration lanes, each of which provokes a
  genuine refusal and asserts `Denied` recognizes it.
- **Fail closed.** A prompt that errors keeps the confined result, a deployment
  with no escalator (headless, tests) keeps refusals as plain command failures,
  and a nil gate offers nothing.
- **The retry chain stands on its own.** `Unconfined` is not "the confined chain
  minus the OS backend": nothing merged into `Confined` — defaults, command
  gates, resource caps — applies to it, so build it with whatever floor must
  survive an escalation. The OS boundary is the one thing a retry gives up.
- **The gate is the mode owner's half.** A session that already runs unconfined
  has no confine to leave, and a read-only session must not trade its workspace
  guarantee for a per-command approval: `EscalationAvailable` returning false
  suppresses both the offer and the remembered-rule shortcut. `EscalationGateFunc`
  covers hosts without modes.
- **Remembered rules skip the doomed attempt.** `EscalatedAllowed` is consulted
  before the first attempt, so an "always" answer runs directly through
  `Unconfined`; persisting the rule belongs to the `Escalator` implementation. It
  observes a snapshot of the request (mutating it cannot rewrite the call, like
  an approval predicate), and it is the only check left on such a call beyond
  `Gate`. Matching normalizes like the allowlist (`RuleFor` unwraps `sh -c`
  wrappers), so the request's `Rule` is the normalized prefix the store matches
  while `Command` is the display line the prompt shows, `Reason` / `Detail` the
  refusal, and `Exec` the snapshot of the refused attempt.
- **`Escalation` is not a `Runner`.** Only a completed result tells a refusal
  from an ordinary failure, so the retry lives at the Exec level. Interactive
  sessions keep their own chains and never restart behind the user's back.
- **A delegated subagent is a separate session.** A mode store keyed on session
  identity answers for the subagent's own session, not the caller's, and core
  derives no mode for it: `delegation.Policy` bounds delegation (targets, modes,
  depth, concurrency, timeout, input size) and says nothing about the child's
  boundary. A deployment that wants one gets it from the child's own
  `sandbox` configuration or from a hook that recognises the child. See
  [delegation.md](delegation.md).

`outcome` reports `Refused` / `Approved` / `Remembered`, so a host can attach its
audit note without re-deriving what happened.

## File journal

A runner can report the writes that happen inside it as a readable, replayable
event stream: knowing which artifacts a turn produced, without scanning trees
before and after. It exists on the backends and platforms that have a watch
source, and each source reports in the unit its kernel charges for:

| Backend | Source | Cost of one watch | Rename pairing | Identity (`Dev`/`Ino`) |
|---|---|---|---|---|
| `local` | inotify (Linux) / kqueue (macOS) / ReadDirectoryChangesW (Windows) | per directory / per entry / per directory + its change buffer | yes (within one directory on Windows) | yes on the unices, no on Windows |
| `bwrap` | inotify (Linux) | per directory | yes | yes |
| `seatbelt` | kqueue (macOS) | per entry | yes | yes |
| `windows` | ReadDirectoryChangesW (Windows) | per directory + its change buffer | within one directory | no |
| `remote` | whatever the server's backend has | the server's | the server's | the server's |

Writer attribution is `no` everywhere (`WriteEvent.Session` stays empty): inotify
cannot name the writer without `CAP_SYS_ADMIN`, kqueue cannot name it at all, and
a guess from timing would be a data-correctness bug.

A platform without a source (the BSDs today) fails the build when `journal:` is
configured, instead of producing a runner that reports nothing. It is opt-in by
presence — an absent block costs nothing (no descriptor, no goroutine, no
watch), and the zero block is a fully enabled journal with every default:

```yaml
sandbox:
  journal:
    exclude: [.git, node_modules, dist, target, .venv]
    retention: 4096            # events kept for replay; DefaultJournalRetention
    max_watch_set: 20000       # watch budget; past it, a capacity gap
    ops: [create, rename, remove]   # drop "write" events; a filtered op is not a gap
```

`exclude` lists directories relative to the runner root whose subtree is never
watched; it is the cost control (about 1 KB of kernel memory per directory watch
on Linux, one descriptor per watched entry on macOS), and the journal applies no
default list of its own — an implicit filter would hide writes a deployment
believes it is watching. `retention` bounds the replay window; `max_watch_set`
bounds the watch set (*how many* watches, in the platform's unit above). Both are
validated at build time, and the cheap invariants (op names, negative values)
fail at load, where the field is still visible.

On macOS the budget is derived from the process's descriptor ceiling, and
attaching a journal raises the soft `RLIMIT_NOFILE` toward it (never down, never
past the hard limit) — a sandbox root is usually tens or hundreds of entries, but
a build tree that exceeds the budget is a reported capacity gap, not a truncated
watch set. Descriptor limits are process-wide, so a host that would rather own
that decision can set the limit itself before building the runner; the journal
only ever lifts it. Per watched directory the source also keeps one snapshot in
user space (names and identities), which is what makes diffing — and with it the
"no queue overflow to report" property — possible.

What a directory note costs on macOS is worth knowing, because it is the one
platform where it is not a single syscall: the directory is re-read and diffed,
so the price is the width of the directory rather than the size of the change.
The re-read is one pass over the directory's own records — each carries its
entry's type and inode number — plus a stat for the entries those records cannot
describe on their own, which is every directory (a directory entry may be a
mounted volume, and only a stat sees that). Measured on APFS over a directory of
4096 entries: one re-read is ~3.5 ms, and 40 creations 15 ms apart burn about a
third of a core, against a full core before the records were read for their
identities. The read buffer is sized from what the last scan of that directory
read, because a read call here walks the whole directory again: the same 164 KB
of records cost 3.6 ms in one call and 19.8 ms in twenty-one 8 KB calls. A wide,
flat directory is the shape this costs most in, which is what `exclude` is for.

On Windows the cost is a buffer rather than a pass: each watched directory holds
one read pending on a completion port, and that read's 16 KiB change buffer is
memory the kernel keeps (and pins) until the read completes. The default budget
of 4096 watches is therefore 64 MiB of buffers and 64 MiB more pinned — the
platform's budget is about this process's memory rather than a kernel limit,
which is why `max_watch_set` is worth setting there. Overflow is reported, not
hidden: a directory that produces more changes than its buffer can describe comes
back as an `overflow` gap, and the watch keeps reporting.

### What the stream contains

Net state changes, not a syscall trace: a file created and written in one go is
**one** `create`, an `mv` is **one** `rename` (with `OldPath`), a removal is one
`remove`, and attribute changes (`chmod`, `touch` on an existing file) or reads
are never events — a chip saying "written" for a `cat` would be a false positive.
A file that appears and disappears before its writer closes it reports nothing at
all, because nothing was produced.

The sources see different edges, so the same turn can describe the same files
slightly differently. inotify reports a writer's close, so a create and a write
are dated at that edge and a write to an existing file is reported as one `write`
per close. kqueue has no close edge: an appearance and a content change settle in
the collection window and are reported when they stop changing (a few hundred
milliseconds, the same window that turns "created and written" into one
`create`), and a truncate is recognized by the size it left behind.
ReadDirectoryChangesW has no close edge either, and two of its own: a move is one
`rename` when the platform writes both halves adjacently in one read — what a
rename inside one directory produces — while a move between two directories
carries no pairing information and is reported as the `remove` and the `create`
it also is; and a content change is reported when the filesystem records the
write, so a writer that keeps its bytes in the cache has its `write` reported
late (never wrongly). What all of them report the same way: a disappearance
inside the window is nothing, and a filtered op is never a gap.

Paths under the root are relative, with `/` separators and no leading slash;
paths under a `writes` entry outside the root carry that cleaned absolute path.
`IsDir` marks directory events: a new directory is reported once, and its
contents follow as their own events.

`Session` is the sandbox session that made the change **when the backend can
name it precisely**. inotify cannot (fanotify and eBPF need
`CAP_SYS_ADMIN`/`CAP_BPF`) and kqueue has no such facility at all, so every event
is unattributed: `Session` is empty rather than guessed from timing. Unattributed
is a valid answer; a wrong attribution is a data-correctness bug.

### Reading it

```go
j, err := sandbox.OpenJournal(ctx, runner) // errdefs.NotAvailable when there is none
if err != nil { /* not enabled, no watch source, or the journal's own reason */ }
defer j.Close()

cursor := int64(0)
batch, err := j.Read(ctx, cursor, 512)   // never blocks, never waits
cursor = batch.NextSeq
if batch.Gap != nil { /* coverage is incomplete for this window */ }
```

Reads are cursor operations: several readers hold independent positions, and
re-reading from an earlier cursor replays from the retained window. `Read` never
waits for future events, so a consumer drains on its own schedule and persists
what it needs — the window is bounded, and events outside it are gone.

The invariant that makes the stream usable is that `(afterSeq, NextSeq]` is
always accounted for: every seq in it was either returned or declared lost. A
non-nil `Gap` is a statement about coverage, not an error and not an empty range
— when the extent of a loss is unknown (a kernel queue overflow does not say how
many events it dropped) `FirstMissing` is larger than `NextSeq`, and the reading
is "coverage after `FirstMissing-1` is not guaranteed". Reasons: `overflow`
(kernel queue), `retention` (the reader fell behind the window), `capacity` (the
watch could not be registered — watch limits, descriptor limits, a missing
directory), `watch_lost` (a watched subtree was moved or detached), `backend`
(the source itself failed, and the journal froze). A consumer must render a gap
as "this may be incomplete", never as "nothing else was written". `overflow` is
inotify's: kqueue coalesces notes rather than dropping them, and the macOS source
re-derives state from the filesystem, so a change it missed wakes it up later
instead of disappearing.

`JournalBatch.Closed` reports that the journal is frozen — the runner was closed,
or the source died — and no further events will arrive; the retained residue
stays readable until the reader is closed.

### What it is not

Not an enforcement boundary: the backend's write confinement still decides what
is allowed, and the journal reports what happened. Not an audit log: events can
be missing, and the window is bounded. Not a replacement for `core/fsx`: the file
tools write through an OS handle rooted at a declared directory and attribute
every write precisely, while the journal sees every write but cannot attribute
it — prefer the precise half when both describe the same path.

On macOS a move is one `rename` only when the entry's own watch saw the name
change and an entry with the same inode appeared in the watched tree: a hardlink
swap (`ln f g; rm f`) is a removal plus an appearance, and a move whose
destination is outside the watched tree is a removal — the same statement inotify
produces when `MOVED_TO` never arrives. On network or exotic mounts the kernel
may report nothing at all (inotify semantics on Linux, kqueue's notes on macOS);
the journal cannot detect "this mount reports nothing", which is the one gap it
cannot report.

## Policy groups

`ExecOptions` carries:

- `WorkDir`, `Stdin`, `Timeout`;
- `Env` — allow-list / inject policy;
- `Net` — `permission.NetPolicy` (`Mode`, `AllowHosts`, `Proxy`);
- `Permission` — `*permission.Policy` (`Writes`, `Net`), nil meaning "the
  runner's own boundary";
- `Resources` — memory / cpu / output caps.

See [runtime.md](runtime.md) for how a deployment's budget becomes these values,
and [sandbox-remote.md](sandbox-remote.md) when the boundary is another machine.
