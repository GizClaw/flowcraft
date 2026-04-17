# FlowCraft 拓扑模式详细参考

## 1. Chatbot（基础对话）

最简单的对话模式：单个 LLM 节点 → 提取输出 → 结束。

```
[entry] llm_call → answer → __end__
```

### 节点配置

- **llm_call**: 使用默认 messages，无需设 query_fallback（系统自动注入）
- **answer**: `keys: ["response"]`

### 适用场景

- 简单问答机器人
- 客服对话
- 信息查询

### 注意事项

- 不含工具调用能力
- 无循环，单轮生成

---

## 2. ReAct Agent（推理-行动循环）

LLM 配合工具调用的循环模式。LLM 决定是否调用工具，调用后结果回传 LLM 继续推理。

```
[entry] llm_call → loopguard ─┐
                               ├→ (tool_pending==true && loop_count_exceeded==false) → llm_call
                               └→ answer → __end__
```

### 节点配置

- **llm_call**: `track_steps: true`（使用默认 messages，无需 query_fallback）
  - `tool_names`: 用 `schema(action=tool_list)` 获取全部可用工具，**默认全部带上**（包括 kanban_submit、sandbox_*、knowledge_search 等），用户明确不需要某工具时才去掉
- **loopguard**: `max_count: 50`（默认 50，视任务复杂度调整）
- **answer**: `keys: ["response"]`

### 关键条件边

```
from: loopguard → to: llm_call
condition: "tool_pending == true && loop_count_exceeded == false"
```

### 适用场景

- 需要外部工具辅助的任务
- 多步骤推理（搜索+计算+确认）
- CoPilot 子 Agent

### 注意事项

- **必须有 loopguard**，否则工具调用会无限循环
- loopguard 的 `max_count` 建议 10-50，视任务复杂度
- `tool_pending` 由 LLM 节点自动设置
- 条件边必须同时检查 `tool_pending` 和 `loop_count_exceeded`

---

## 3. RAG Chat（检索增强生成）

先从知识库检索相关内容，通过模板组装 context，再交给 LLM 生成回答。

```
[entry] knowledge → template → llm_call → answer → __end__
```

### 节点配置

- **knowledge**: `datasets: [{dataset_id: "xxx", top_k: 5}]`
- **template**: `template: "Based on the following context:\n{{.results}}\n\nAnswer: {{.query}}"`
- **llm_call**: `messages_key: "messages_rag"`（隔离消息，不继承默认 messages 中的 query；template 输出已包含 query，无需 query_fallback）
- **answer**: `keys: ["response"]`

### 适用场景

- 企业知识库问答
- 文档助手
- FAQ 机器人

### 注意事项

- knowledge 节点需要 `query` 输入端口
- template 节点通过 `{{.key}}` 引用 Board 变量
- `dataset_id` 必须存在，否则检索为空

---

## 4. Approval Flow（人工审批）

LLM 生成建议后进入人工审批环节，审批通过才输出结果。

```
[entry] llm_call → approval → answer → __end__
```

### 节点配置

- **llm_call**: 生成待审批的建议
- **approval**: `prompt: "请审批以上建议"`
- **answer**: `keys: ["response", "approval_status"]`

### 行为

1. LLM 生成建议写入 Board
2. approval 节点发出 `interrupt` 信号
3. Executor 暂停，记录 `__interrupted_node`
4. 用户通过 API 提交审批决策
5. Executor 从断点恢复

### 适用场景

- 内容审核
- 敏感操作确认
- 流程审批

---

## 5. Orchestrator（多 Agent 协作）

中央 Dispatcher 接收请求，通过 Kanban 分发任务给子 Agent，收集结果后汇总。

```
[entry] dispatcher_llm → loopguard → (条件回跳) → dispatcher_llm
                                    → aggregator → answer → __end__
```

### 节点配置

- **dispatcher_llm**: 配合 `kanban_submit`/`task_context` 工具
- **loopguard**: `max_count: 50`
- **aggregator**: `input_keys: ["kanban_results"], mode: "array"`

### 适用场景

- 复杂多步骤任务
- 需要不同专业能力的协作
- CoPilot 主 Dispatcher

---

## 6. 条件路由

根据输入内容动态选择不同的处理路径。

```
[entry] router → branch_a → answer_a → __end__
              → branch_b → answer_b → __end__
```

### 实现方式

**方式 A — router 节点:**
```json
{
  "type": "router",
  "config": {
    "routes": [
      {"condition": "intent == 'query'", "target": "branch_a"},
      {"condition": "intent == 'action'", "target": "branch_b"}
    ]
  }
}
```

**方式 B — 条件边:**
```json
{"from": "start", "to": "branch_a", "condition": "intent == 'query'"},
{"from": "start", "to": "branch_b"}
```

---

## 7. 并行分支

多个节点并行执行，结果在 join 节点汇聚。

```
[entry] start → node_a ─┐
              → node_b ─┤→ join → answer → __end__
              → node_c ─┘
```

### 配置

并行由 Executor 自动检测（单节点多个无条件出边）。
通过 `AppConfig.Parallel` 控制：

```json
{
  "parallel": {
    "enabled": true,
    "max_branches": 10,
    "max_nesting": 3,
    "merge_strategy": "last_wins"
  }
}
```

### MergeStrategy

| 策略 | 说明 |
|---|---|
| last_wins | 后写入的值覆盖先写入的 |
| namespace | 每个分支写入独立命名空间 |
| error_on_conflict | 冲突时报错 |

---

## 8. 意图分类 + 多路分发

先用一个 LLM 节点做意图分类，再通过 router 将不同意图路由到各自的处理节点。

```
[entry] llm_classify → router ─→ (intent=='qa') → llm_qa → answer → __end__
                               ─→ (intent=='summarize') → llm_summarize → answer → __end__
                               ─→ (兜底，无条件) → llm_general → answer → __end__
```

### 节点配置

- **llm_classify**: `output_key: "intent"`, `messages_key: "messages_classify"`, `query_fallback: true`, `temperature: 0.3`
  - 使用独立 messages_key → **必须** query_fallback: true，否则隔离消息列表为空
  - system_prompt 要求只输出一个意图关键词
- **router**: `routes` 中的 condition 引用 `intent` 变量（与 llm_classify 的 output_key 一致）
- **各分支 llm 节点**: 各自独立的 `messages_key`（如 `"messages_qa"`、`"messages_general"`），`query_fallback: true`
  - 同理：独立 messages_key 需要 query_fallback: true
- **answer**: `keys` 包含所有分支可能的输出变量（如 `["qa_result", "summarize_result", "general_result"]`），或者所有分支使用相同的 `output_key: "response"`

### 关键要点

1. **output_key 与 router 变量一致**：llm_classify 的 `output_key` 必须与 router 条件中的变量名一致（如都用 `intent`）
2. **messages_key 隔离**：分类器和各处理节点必须使用不同的 `messages_key`，否则处理节点会继承分类器的 system_prompt
3. **独立 messages_key 必须配 query_fallback: true**：系统只自动向默认 `messages` 注入 query，独立 messages_key 的消息列表初始为空，不设 query_fallback 会导致 LLM 收不到用户输入
4. **兜底路由用无条件边**：最后一条路由不写 condition（省略即可），不要写 `"true"` 或 `"xxx || true"`
5. **answer keys 覆盖所有分支**：如果各分支的 output_key 不同，answer 的 keys 需列出所有可能的变量名

### graph_update 示例

```json
{
  "entry": "llm_classify",
  "nodes": [
    {"id": "llm_classify", "type": "llm", "config": {
      "system_prompt": "分析用户意图，只输出一个关键词：qa / summarize / general",
      "output_key": "intent", "messages_key": "messages_classify",
      "query_fallback": true, "temperature": 0.3
    }},
    {"id": "intent_router", "type": "router", "config": {
      "routes": [
        {"condition": "intent == 'qa'", "target": "llm_qa"},
        {"condition": "intent == 'summarize'", "target": "llm_summarize"}
      ]
    }},
    {"id": "llm_qa", "type": "llm", "config": {
      "system_prompt": "你是问答助手...", "output_key": "response",
      "messages_key": "messages_qa", "query_fallback": true
    }},
    {"id": "llm_summarize", "type": "llm", "config": {
      "system_prompt": "你是摘要助手...", "output_key": "response",
      "messages_key": "messages_summarize", "query_fallback": true
    }},
    {"id": "llm_general", "type": "llm", "config": {
      "system_prompt": "你是通用助手...", "output_key": "response",
      "messages_key": "messages_general", "query_fallback": true
    }},
    {"id": "output", "type": "answer", "config": {"keys": ["response"]}}
  ],
  "edges": [
    {"from": "llm_classify", "to": "intent_router"},
    {"from": "intent_router", "to": "llm_qa", "condition": "intent == 'qa'"},
    {"from": "intent_router", "to": "llm_summarize", "condition": "intent == 'summarize'"},
    {"from": "intent_router", "to": "llm_general"},
    {"from": "llm_qa", "to": "output"},
    {"from": "llm_summarize", "to": "output"},
    {"from": "llm_general", "to": "output"},
    {"from": "output", "to": "__end__"}
  ]
}
```

### 注意事项

- 所有分支 llm 节点使用相同的 `output_key: "response"`，answer 只需 `keys: ["response"]`
- 分类器的 temperature 建议设低（0.1~0.3），减少分类不稳定
- router 的最后一条 edge（无 condition）是兜底路由，仅当所有 condition 都不匹配时走

### 变体：使用 json_mode 做结构化意图分类

当需要从分类器获取多个字段（如 intent + context_summary + continue_session）时，可以使用 `json_mode: true`。

**⚠️ 关键区别**：`json_mode=true` 时，LLM 输出的 JSON 被解析成 **map 对象** 存入 `output_key`，条件边必须用 **点号** 访问嵌套字段。

```json
{"id": "llm_classify", "type": "llm", "config": {
  "system_prompt": "分析意图，输出 JSON: {\"intent\": \"qa/summarize/general\", \"context_summary\": \"...\"}",
  "output_key": "classify_result", "messages_key": "messages_classify",
  "query_fallback": true, "json_mode": true, "temperature": 0.3
}}
```

条件边写法：
```json
{"from": "llm_classify", "to": "llm_qa", "condition": "classify_result.intent == 'qa'"},
{"from": "llm_classify", "to": "llm_summarize", "condition": "classify_result.intent == 'summarize'"},
{"from": "llm_classify", "to": "llm_general"}
```

**错误写法**（变量名不匹配，所有条件静默返回 false）：
```
❌ intent == 'qa'                    → board 中无 intent 变量
❌ classify_result == 'qa'           → classify_result 是 map，不是 string
✅ classify_result.intent == 'qa'    → 正确的点号访问
```

---

## 组合技巧

### ReAct + RAG

```
[entry] knowledge → template → llm_call → loopguard → ...
```

知识检索结果注入 LLM context，同时保留工具调用循环能力。

### 条件路由 + 不同模型

```
[entry] router → (simple) → cheap_llm → answer → __end__
              → (complex) → expensive_llm → answer → __end__
```

简单问题用轻量模型，复杂问题用高能力模型。

### 多步骤 Pipeline

```
[entry] step1_llm → template → step2_llm → answer → __end__
```

第一个 LLM 分析，template 格式化，第二个 LLM 优化。
