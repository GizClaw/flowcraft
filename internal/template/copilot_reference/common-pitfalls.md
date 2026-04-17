# FlowCraft 常见陷阱与解决方案

## 1. 编译错误类

### 缺少 entry 节点

**症状**: `graph entry node is required`

**原因**: GraphDefinition 未设置 `entry` 字段。

**解决**: 确保 `graph_def.entry` 指向第一个要执行的节点 ID。

```json
{
  "entry": "llm_call",
  "nodes": [{"id": "llm_call", "type": "llm", ...}]
}
```

### entry 节点不存在

**症状**: `entry node "xxx" not found in nodes`

**原因**: entry 指向的 ID 在 nodes 列表中不存在。

**解决**: 检查拼写，确保 entry 的值与某个 node 的 id 完全匹配。

### 重复节点 ID

**症状**: `duplicate node ID "xxx"`

**原因**: 两个节点使用了相同的 ID。

**解决**: 每个节点 ID 必须唯一。建议命名规范: `{type}_{功能}` 如 `llm_analysis`、`template_prompt`。

### 边引用不存在的节点

**症状**: `edge from/to unknown node "xxx"`

**原因**: 边的 from 或 to 引用了不存在的节点 ID。

**解决**: 检查边定义中的节点 ID 是否正确。终止节点使用 `__end__`。

### 无效条件表达式

**症状**: `invalid condition expression` 或编译失败

**原因**: 条件边的 expr 语法错误。

**解决**: 使用正确的 expr 语法：
- 比较: `==`, `!=`, `>`, `<`, `>=`, `<=`
- 逻辑: `&&`, `||`, `!`
- 变量直接引用 Board 中的变量名: `tool_pending == true`

---

## 2. 循环相关

### 无限循环

**症状**: 执行永不结束，达到最大迭代次数

**原因**: ReAct 循环缺少 loopguard 或条件边不正确。

**解决**:
1. 在 LLM 和回跳边之间添加 loopguard 节点
2. 条件边必须同时检查两个条件:
   ```
   tool_pending == true && loop_count_exceeded == false
   ```
3. loopguard 的默认边（无条件）应指向 answer 或下游节点

### 循环检测警告但非错误

**症状**: 编译成功但有 `has_cycles: true` 警告

**解释**: 图中存在循环是正常的（ReAct 模式需要循环）。只要有 loopguard 保护，循环不会无限执行。警告不阻塞编译。

---

## 3. LLM 节点问题

### 无 user 消息（仅限独立 messages_key 场景）

**症状**: LLM 返回 400 错误或空结果

**原因**: 使用了独立 `messages_key` 的 llm 节点，其隔离消息列表初始为空，且未设 `query_fallback: true`。注意：使用默认 `messages` 的节点**不会**遇到此问题（系统在图执行前已自动注入 query）。

**解决**: 为使用独立 `messages_key` 的节点设置 `query_fallback: true`:
```json
{"id": "llm_classify", "type": "llm", "config": {
  "messages_key": "messages_classify",
  "query_fallback": true
}}
```

### 模型未配置

**症状**: `cannot resolve LLM model "xxx"`

**原因**: 指定的模型未在 Provider 中注册，或缺少 API Key。

**解决**:
1. 确认模型格式: `provider/model`（如 `openai/gpt-4o`）
2. 确认 Provider 已配置 API Key
3. 不指定 model 时使用全局默认模型

### 工具调用不生效

**症状**: LLM 不调用已注册的工具

**原因**: `tool_names` 为空或未包含所需工具。

**解决**: `tool_names` 为空时不注入任何工具，必须显式列出需要的工具名。

### 多 LLM 节点 messages 互相污染

**症状**: 后续 LLM 节点使用了前一个节点的 system_prompt，输出完全不符合预期

**原因**: 所有 LLM 节点默认共享 `messages_key: "messages"`。第一个节点将 system_prompt 和对话历史写入 `messages`，后续节点读取同一个变量，继承了前一个节点的 system_prompt。

**解决**: 每个有独立 system_prompt 的 LLM 节点设置不同的 `messages_key`，并配合 `query_fallback: true`:
```json
{"id": "llm_classify", "type": "llm", "config": {"messages_key": "messages_classify", "query_fallback": true, ...}},
{"id": "llm_qa", "type": "llm", "config": {"messages_key": "messages_qa", "query_fallback": true, ...}}
```
注意：设了独立 messages_key 但不设 query_fallback: true，会导致"无 user 消息"错误。

### output_key 与条件变量名不匹配

**症状**: router 或条件边永远走兜底分支，其他条件从不匹配

**原因**: LLM 节点的 `output_key` 与下游 router 条件中引用的变量名不一致。例如 `output_key: "intent_result"` 但条件写 `intent == 'xxx'`。

**解决**: 确保变量名完全一致:
```json
{"id": "llm_classify", "type": "llm", "config": {"output_key": "intent", ...}}
// router 条件: intent == 'qa'  ✓
// 错误: intent_result == 'qa'（变量不存在）
```

### json_mode=true 时条件边引用方式错误

**症状**: 分类 LLM 用 json_mode 输出 `{"intent": "qa", ...}`，但条件边 `intent == 'qa'` 从不匹配，graph 在 entry 节点后直接终止（0 输出）

**原因**: `json_mode=true` 时，LLM 输出的 JSON 被**解析成 map 对象**存入 `output_key` 指定的 board 变量中。例如 `output_key: "intent_result"` 会在 board 中存为：
```
board["intent_result"] = {"intent": "qa", "continue_session": false}
```
条件边写 `intent == 'qa'` 引用的是顶层 board 变量 `intent`，该变量不存在。expr-lang 对不存在的变量返回 false（不报错），导致所有条件边静默失败。

**解决方案 A — 使用点号访问嵌套字段（推荐用于需要 JSON 结构化输出的场景）**:
```json
{"id": "llm_classify", "type": "llm", "config": {"output_key": "intent_result", "json_mode": true, ...}}
// 条件边: intent_result.intent == 'qa'  ✓
// 条件边: intent_result.continue_session == true  ✓
```

**解决方案 B — 不使用 json_mode，让 LLM 只输出纯关键词**:
```json
{"id": "llm_classify", "type": "llm", "config": {"output_key": "intent", "json_mode": false, ...}}
// system_prompt 中要求只输出一个关键词
// 条件边: intent == 'qa'  ✓
```

**解决方案 C — 使用 assigner 节点提取字段到顶层变量**:
```json
{"id": "extract", "type": "assigner", "config": {"assignments": [
  {"source": "intent_result.intent", "target": "intent"}
]}}
// 先经过 assigner，再进 router
// 条件边: intent == 'qa'  ✓
```

**选择建议**:
- 只需要分类关键词 → 方案 B 最简单
- 需要多个字段（如 intent + confidence + context） → 方案 A 或 C
- 方案 A 最少节点，但条件表达式稍长；方案 C 显式解构，条件表达式简洁

### 兜底路由 "|| true" 导致多路同时命中

**症状**: 无论路由到哪个分支，兜底分支总是同时执行

**原因**: 兜底边的 condition 写成了 `"intent == 'general' || true"`，`|| true` 使条件永远为真。

**解决**: 兜底分支使用无条件边（省略 condition 字段）:
```json
{"from": "router", "to": "llm_general"}
```
无条件边仅在所有条件边都不匹配时才走。

### 条件边引用不存在的变量不会报错

**症状**: 条件边从不匹配，graph 在某个节点后静默终止，无错误日志

**原因**: expr-lang 对 board 中不存在的变量返回 `false`（不抛错误）。如果所有条件边都引用了不存在的变量，且没有无条件兜底边，graph 将在当前节点后立即终止。

**解决**: 
1. 确保条件边中的变量名与上游节点的 output_key 完全一致
2. 使用 `schema(action=node_usage)` 查看节点的 `runtime.edge_vars` 了解可用于条件边的变量
3. 始终为多路分支设置一个无条件兜底边

---

## 3.5 Aggregator 节点

### 使用不支持的 mode

**症状**: aggregator 输出不符合预期，返回数组而不是期望的格式

**原因**: aggregator 只支持 4 种 mode: `array`、`concat`、`map`、`last`。使用其他值（如 `first_not_empty`）会静默回退到 `array` 模式。

**解决**: 只使用受支持的 mode 值。如果需要"取第一个非空值"，使用 `last` mode（适用于多路分支中只有一条分支会写入变量的场景）。

---

## 4. 端口兼容性

### 类型不匹配

**症状**: 编译警告 `port type mismatch`

**原因**: 上游节点的输出端口类型与下游输入端口类型不兼容。

**解决**: 参考端口兼容矩阵:
- `any` 兼容所有类型
- `messages` 仅与 `messages` 兼容
- `string`, `bool`, `integer` 严格匹配
- 使用 `template` 节点做类型转换

---

## 5. Knowledge 节点

### dataset_id 不存在

**症状**: 检索返回空结果

**原因**: 配置的 dataset_id 在 Knowledge Store 中不存在。

**解决**: 先通过 `knowledge_search` 或 API 确认数据集已创建并包含文档。

### 检索结果为空

**可能原因**:
1. 数据集中没有文档
2. 查询与文档内容不相关
3. top_k 设置过小

---

## 6. 审批节点

### 中断后无法恢复

**症状**: Session 状态为 `interrupted` 但 resume 失败

**原因**: resume 请求缺少必要的 decision 数据。

**解决**: 使用 `POST /api/chat/resume/stream` 时需要提供:
```json
{
  "session_id": "...",
  "decision": "approved"
}
```

---

## 7. 图编辑最佳实践

### 节点命名

- 使用有意义的 ID: `llm_analysis` 而不是 `node1`
- 保持一致的命名风格

### 构建顺序

1. 使用 `graph(action=update)` 一次性传入完整图定义（nodes + edges + entry）
2. 编译验证 `graph(action=compile)`
3. 修复问题后发布 `graph(action=publish)`

### 图编辑方式

- 使用 `graph(action=update)` 一次性传入完整图定义（整体替换）
- 每次修改后建议 `graph(action=compile)` 验证

---

## 8. CoPilot 相关

### CoPilot Agent 不能删除

设计如此。CoPilot 是系统内置 Agent，`app_delete` 对 CoPilot Agent 会返回 405 错误。
