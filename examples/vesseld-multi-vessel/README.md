# vesseld multi-vessel demo

Two independently-configured vessels driven by one shared `vesseld`
process, sharing one OpenAI rate-limit bucket. Use this as the smoke
test for "is my daemon set up correctly" and as the worked example
for the YAML schema.

## What it shows

| File | Concept |
|---|---|
| `daemon.yaml` | Daemon-wide config: control socket, drain timeout, LLM rate-limit bucket. |
| `shared/openai.yaml` | One `LLMProfile` referenced by every vessel — the daemon shares the underlying client + limiter across them. |
| `vessels/triage/vessel.yaml` | A multi-agent vessel with **dispatcher + worker** + `Kanban` agent-as-tool delegation. |
| `vessels/support/vessel.yaml` | A simple single-agent vessel for plain Q&A. |

The two vessels live in the same daemon, share the LLM profile, but have independent agent rosters, history, and `Resources` caps.

## Prerequisites

```bash
export OPENAI_API_KEY=sk-...     # any OpenAI-compatible key
go build -o ./vesseld ./cmd/vesseld    # from the repo root
```

## Run

```bash
# Validate first (no IO, no daemon spin-up — just schema + ref resolution).
./vesseld validate --config examples/vesseld-multi-vessel -R

# Start the daemon (foreground; Ctrl-C to stop).
./vesseld start --config examples/vesseld-multi-vessel -R

# In another shell, dump the resolved plan as JSON.
./vesseld plan --config examples/vesseld-multi-vessel -R
```

`-R` recurses into the directory; `--config` is repeatable if you'd rather pass each YAML / sub-directory separately.

You should see two vessels (`triage`, `support`) with their agents, engines, and resolved history stores.

## Drive a run

The daemon exposes a small HTTP API over the unix socket. Any client that can `curl --unix-socket` works.

```bash
# Synchronous: returns the model's reply (waits for completion).
curl --unix-socket /tmp/vesseld-multi-vessel.sock \
  -X POST http://vesseld/v1/vessels/support/call \
  -H 'content-type: application/json' \
  -d '{"agent":"support-agent","query":"What are your business hours?"}'

# Asynchronous: returns a run_id immediately; tail the SSE log stream.
RUN=$(curl -s --unix-socket /tmp/vesseld-multi-vessel.sock \
        -X POST http://vesseld/v1/vessels/triage/submit \
        -H 'content-type: application/json' \
        -d '{"agent":"triage-dispatcher","query":"My order is two weeks late."}' \
      | jq -r .run_id)

curl --unix-socket /tmp/vesseld-multi-vessel.sock \
  "http://vesseld/v1/vessels/triage/logs?run_id=$RUN"
```

## Inspect

```bash
# Per-run terminal state + token usage.
curl --unix-socket /tmp/vesseld-multi-vessel.sock \
  "http://vesseld/v1/runs/$RUN" | jq .

# Vessel phase (running | failed | stopped | ...).
curl --unix-socket /tmp/vesseld-multi-vessel.sock \
  http://vesseld/v1/vessels/triage/phase
```

## Drain & shutdown

```bash
# Block new submits, wait for in-flight runs to settle.
curl --unix-socket /tmp/vesseld-multi-vessel.sock \
  -X POST http://vesseld/v1/vessels/triage/drain
```

`SIGTERM` to the daemon also runs drain semantics, bounded by `daemon.spec.shutdown.drainTimeout`.

## Going further

- Swap `provider: openai` → `provider: deepseek` / `anthropic` / `bytedance` / `minimax`. The same YAML works.
- Add a sidecar agent (`sidecar: true` + `subscribeTo: agent.run.completed`) to the triage vessel to log every completed turn.
- Set `auth.token: <string>` on the daemon and a `Listen: :8443` to expose the API over TCP with bearer-token auth.
- For the smaller single-vessel "hello world", strip this directory down to one `Vessel` + one `Agent` + one `LLMProfile` document — the daemon will validate and run with as few as four `kind:` blocks.
