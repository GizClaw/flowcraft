# chatbot-with-recall

A self-contained demo of how to compose `sdk/history` (conversation
transcript) with `sdk/recall` (long-term fact memory) without any
framework glue. This is the recommended replacement for the deleted
`MemoryAwareMemory` wrapper in pre-v0.2.0 builds.

## What it shows

`chat()` in `main.go` is the canonical "history + recall + LLM"
coordinator (~80 lines):

1. `history.Memory.Load` — pull the running transcript.
2. `recall.Memory.Recall` — fetch top-K facts relevant to the new user
   turn.
3. Build the LLM prompt: optional `[Long-term memory]` system block,
   transcript, then the new user message.
4. `chat.LLM.Generate` — call the model.
5. `history.Memory.Append` — durably persist both new turns at once.
6. `recall.Memory.Save` (or `SaveAsync`) — extract any new facts from
   the exchange so the next turn can recall them.

The demo runs four turns. Turn 1 establishes a user fact ("I prefer
dark mode"); turn 3 asks an indirect follow-up ("What did I tell you
about UI preferences?") and you can see recall pull the fact back into
the system prompt so the assistant answers correctly.

## Running

This example is intentionally not part of `go.work`, so build with
`GOWORK=off` (or run from inside its own module):

```bash
cd examples/chatbot-with-recall
GOWORK=off go run .
```

Expected output:

```
--- Turn 1 ---
USER: My name is Alice and I prefer dark mode.
BOT:  Sure — happy to help.

--- Turn 2 ---
USER: What is your refund policy?
BOT:  Our refund policy is full refund within 30 days.

--- Turn 3 ---
USER: What did I tell you about UI preferences?
BOT:  You told me earlier you prefer dark mode.

--- Turn 4 ---
USER: Thanks, that's all.
BOT:  Sure — happy to help.
```

## Adapting to a real provider

The demo wires two stub LLMs inside `main.go` to keep the example
network-free. Replace them with anything that satisfies `sdk/llm.LLM`:

- For OpenAI / Anthropic / Qwen / etc., import the matching package
  under `sdkx/llm/*` and pass its constructed `llm.LLM` to both
  `chat()` and `recall.WithLLM(...)`.
- For crash-recoverable async extraction, swap the default in-memory
  job queue for `sdkx/recall/jobqueue/sqlite.Open(path)` and pass it
  via `recall.WithJobQueue(...)`. Then call `mem.SaveAsync(...)`
  instead of `mem.Save(...)` in step 6.
- For per-tenant isolation pass a populated `recall.Scope`
  (`RuntimeID`, `AgentID`, `UserID`, optional `Partitions`).
