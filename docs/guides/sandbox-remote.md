---
layout: default
title: Remote Sandbox
---
# Remote Sandbox Guide

`core/sandbox-remote` runs a `sandbox.Runner` across a process or host boundary.
The client **is** a `sandbox.Runner` whose processes execute on the server; the
server is a thin adapter over any other runner.

Nothing above the client changes. The tool set, the approval chain and the
escalation path see the interface they see for a local backend, because the
client is a proxy rather than a second implementation.

```go
// The server: one runner, built by whoever starts it.
inner := local.New(root)                     // or bwrap, seatbelt, windows
server, err := remote.NewServer(inner, remote.WithServerToken(token))
http.ListenAndServe(":8443", server)

// The client: a Runner, decorated exactly like a local one.
runner, err := remote.New(ctx, "https://sandbox.internal:8443",
    remote.WithToken(token))
composed := engine.ComposeLocal(runner, policy)
```

One server exposes one runner. It is not a scheduler and holds no fleet: the
server carries sessions over the wire and keeps the handles it needs to answer
by id.

## Deployment

```yaml
sandbox:
  backend: remote
  remote:
    transport: http              # http (default) | unix
    endpoint: https://sandbox.internal:8443
    token: "${SANDBOX_TOKEN}"
    timeout: 30s                 # per request; zero uses the client default
    poll_hold: 15s               # how long a read or wait asks the server to hold
    insecure_skip_verify: false
```

| Field | Rule |
| --- | --- |
| `transport` | `http` (aliases `https`, `tcp`) or `unix` (alias `socket`). |
| `endpoint` | Required for `http`, must be `http(s)`; rejected with `unix`. |
| `socket` | Required for `unix`; rejected with `http`. |
| `token` | Bearer credential. Optional over a unix socket, where the boundary is the directory's permissions. |
| `timeout` | Per-request deadline. |
| `poll_hold` | How long a read or a wait asks the server to hold before answering "nothing yet". Must stay under any proxy's idle timeout. |
| `insecure_skip_verify` | Only meaningful over TLS; rejected with `unix`, which carries none. |

The field a transport does not use is a validation error, not a comment: a
socket path beside an endpoint is configuration that silently does nothing.

Files do not cross. The built-in file tools work on a local `fsx` tree, so a
deployment that puts commands on a remote host needs that host's filesystem
mounted where the tools run, or those tools turned off — which is why the
runtime warns when `tools.files` is enabled with `backend: remote`. This package
moves the process boundary only.

## What crosses, and what does not

**Session ids are opaque and minted by the server.** The client never parses
one, and their width is the server's business — which leaves room for an
instance layer (one sandbox per session) without changing paths later.

**Output is a cursor, not a stream.** A read carries `after_seq` and answers
with a sequence range, so a dropped connection is cheap: a reconnecting client
re-reads from its last cursor instead of losing what it missed. That is also why
the transport can stay request/response.

**One operation pushes.** The session event stream is carried as Server-Sent
Events on `POST /v1/events`. It needs no resume cursor of its own: the
contract's `Watch` replays the retained log before following live, and a consumer
that fell behind recovers with a `Read` from the sequence its lag event names. A
slow consumer loses no frames — the client buffers between the socket and the
channel — which the conformance suite checks by stalling one.

## Capabilities are forwarded, never edited

The client reports the server's declaration verbatim. A capability the far end
does not claim is one this client does not claim either, and one it does claim
has to work — the conformance suite checks both directions of every advertised
feature, including the rule that a backend declaring `Events` must really deliver
the terminal status.

The server re-validates what arrives and can only **narrow** it. Policy stays on
the client side of the wire; the caller is not the authority on what the far end
can enforce.

## Security

The server refuses to be built without an authentication decision: either a
bearer token (`WithServerToken`) or an explicit `WithoutAuth()` for a listener
that already sits behind an authenticator.

| Transport | Boundary |
| --- | --- |
| `http` | Bearer token in the `Authorization` header, never in a query string — query strings end up in proxy logs. TLS is the deployment's job (`remote.WithHTTPOptions(remote.WithHTTPTLS(...))`). |
| `unix` | The socket's directory. `ListenUnix` creates the parent `0700` and the socket `0600`, and a token is optional. |

The socket path is bounded (`maxUnixPathBytes = 100`, under `UNIX_PATH_MAX` of
108 on Windows and 104 on macOS), and the check happens before the listen so a
too-long path is a clear error rather than a truncated one.

A unix socket is not automatically "local" on Windows: there `os.Chmod` only sets
a read-only attribute, so the directory ACL is the real boundary — which is why
the token stays meaningful there.

## Failure semantics

```go
// An operation that is safe to repeat is retried after a transport failure.
// defaultRetries = 3, backoff 20ms / 100ms / 500ms with ≤20% jitter,
// retryBudget = 620ms — a caller that needs a tighter bound sets its own context.
```

- **Reads, waits, lists, attaches and journal reads are retried**; they are
  cursor or query operations, so repeating one is not repeating its effect.
- **Writes are never retried.** A `Start`, `Write`, `Signal`, `Terminate` or
  `Close` that failed in flight may or may not have happened, and replaying it
  would double the effect.
- **Cancellation wins over a transport error.** A cancelled context returns
  `context.Canceled`, not "the connection broke", because a caller that gave up
  should not be told the far end failed.
- **`Close` drains best effort** for up to ten seconds and then reports the exit
  status; a server that is gone turns into a plain error, not a hang.
- **A restarted server forgets sessions.** Session state lives in the server's
  memory, so a reconnect to a new process is `NotFound` for every id — the client
  does not pretend a session survived.

## Wire

`core/sandbox-remote/wire` is the DTO layer: version `1`, one route per
operation, and an error envelope that carries `errdefs.Kind` so the client
rebuilds the same error class (`NotFound`, `NotAvailable`, `Cancelled`, …)
instead of flattening everything into one status.

| Route | Operation |
| --- | --- |
| `POST /v1/hello` | Version handshake; a mismatch fails the client before any session. |
| `POST /v1/start`, `/attach`, `/list`, `/terminate`, `/close` | Session lifecycle. |
| `POST /v1/read`, `/write`, `/close-input`, `/resize`, `/signal`, `/wait` | Session I/O, all addressed by opaque id. |
| `POST /v1/journal` | Journal read, carrying the cursor. |
| `POST /v1/events` | The SSE stream above. |

Two operations are binary, not JSON:

- **read** answers with a framed body — `FSR1 | flags | count | next_seq |
  [seq | stream | pad | len | data]…` — so a megabyte of output is not base64'd
  and re-escaped. The client exposes `ReadResponseSize` so a handler can set
  `Content-Length`.
- **write** sends `FSW1 | id_len | id | len | data`, with `Content-Type` chosen
  by the operation.

The magic prefixes exist so "this is not our frame" is decidable rather than
guessed at. There is no negotiation: the content type of a request says which
frame it is, and an unknown body is a decode error naming the version.

## Measured cost

On an 8-core laptop with the loopback transport, the remote path costs about
17% over the local one for a small echo (2.40 ms vs 2.17 ms), and a 1 MiB drain
moves at ~1569 MB/s with a 256 KiB read window. The window matters: at 64 KiB, a
megabyte cost sixteen round trips instead of four, which measured ~30% off a
1 MiB `Exec`. A single-window stall is a network problem, not a wire problem — on
the same machine it is cheaper to widen the window than to change the transport.

See [sandbox.md](sandbox.md) for the contract every backend implements, and
[runtime.md](runtime.md) for how `sandbox.remote` is validated at build time.
