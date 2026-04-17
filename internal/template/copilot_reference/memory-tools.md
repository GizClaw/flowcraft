---
title: Memory Tools
---

# Memory Retrieval Tools

When using **Lossless** memory strategy, summary index is **auto-injected** into the system prompt — you do NOT need to manually search for conversation history.

## Automatic Summary Index Injection

When a conversation has compressed summaries (via the lossless memory DAG), the system automatically appends a summary index to the LLM's system prompt. The index looks like:

```
## 对话历史摘要

以下是较早对话的压缩摘要，如需查看原始对话，调用 memory_expand(summary_id=ID)。

[s_abc123] seq 0-50: 用户要求构建 RAG 工作流...
[s_def456] seq 51-120: 调试了编译错误，发布 v1...
```

This replaces the old `memory_search` tool — you no longer need to explicitly search for past context.

## memory_expand

Expand a compressed summary to see original messages or finer-grained summaries.

**Parameters:**
- `summary_id` (string, required): The ID from the summary index (e.g. `s_abc123`)
- `max_messages` (integer, optional, default: 20): Max messages to return

**Returns:**
- For leaf summaries (depth=0): Original messages in `role: content` format
- For condensed summaries (depth>0): Child summaries with their own IDs for further expansion

**When to use:**
- When the auto-injected summary index mentions a relevant topic and you need the full details
- When a pruned summary says `[pruned — use memory_expand to load originals]`

## memory_compact (Platform only)

Manually trigger DAG compaction and message archiving.

**Parameters:**
- `conversation_id` (string, required): Target conversation
- `compact` (boolean, optional, default: true): Run DAG file compaction
- `archive` (boolean, optional, default: true): Run message archiving

**Returns:** JSON with compact/archive results.

**Note:** This tool is platform-scoped and typically triggered automatically. Manual use is for maintenance only.
