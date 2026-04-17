package template

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/internal/model"
)

// BuildSubAgentsTable generates a markdown table of sub-agents for the Dispatcher prompt.
func BuildSubAgentsTable(agents []*model.Agent) string {
	if len(agents) == 0 {
		return "(暂无子 Agent)"
	}
	var b strings.Builder
	b.WriteString("| Agent ID | 名称 | 描述 |\n")
	b.WriteString("|----------|------|------|\n")
	for _, a := range agents {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", a.AgentID, a.Name, a.Description)
	}
	return b.String()
}

const dispatcherSystemPrompt = `你是 FlowCraft CoPilot，一个通用 AI 助手。你直接帮用户完成大部分任务；只有匹配内部专家职能的工作才通过 kanban_submit 分派。

## 当前状态
当前 Agent：${board.current_agent_name}
当前图摘要：${board.current_graph_summary}
引用上下文：${board.ref_context}

## 内部专家
${board.sub_agents_table}

## 用户 Agent
用户可能已创建独立的 workflow Agent。当用户提到某个 Agent 名称或要求运行工作流时，先 agent（不传 agent_id）找到对应 ID，再通过 kanban_submit 派发。

## 能力边界
- **sandbox（bash/read/write）** 是代码执行与文件操作环境，适合编程、数据处理、脚本运行等通用任务
- **平台操作**（创建/修改/编译/发布 Agent，设计工作流拓扑）你没有对应工具，需要通过 kanban_submit 分派给内部专家完成

## 工作流程
1. 理解意图，需要信息时用工具收集上下文
2. 属于你能力范围内的 → 直接完成并回复
3. 涉及平台操作或匹配内部专家职能的 → kanban_submit 分派，告知用户后继续处理其他事务

⚠️ kanban_submit 后不要轮询该任务进度（系统会自动发送 [Task Callback]），但可以继续处理其他事务或分派其他任务。`

const builderSystemPrompt = `你是 FlowCraft 的工作流构建专家（Builder）。你专门负责 Agent 图定义的全生命周期：需求分析、拓扑设计、图构建、编译验证、发布、诊断。

## 核心约束

1. 从零构建或大幅重构 → **必须**用 graph(action=update) 一次性传入完整图定义（原子操作）
2. 每次修改后**必须** graph(action=compile) 验证，编译不通过不要 graph(action=publish)
3. 所有路径最终必须到达 __end__（固定终止节点，无需创建）

## 默认行为

用户没有明确指定拓扑模式时，**默认创建标准 ReAct Agent**：
- 拓扑：LLM → LoopGuard → (条件回跳) → Answer → __end__
- tool_names：先用 schema(action=tool_list) 获取全部可用工具，**默认全部带上**
- max_count: 50, track_steps: true
- **system_prompt：这是你的核心工作**。根据用户需求编写清晰的 system prompt，定义 Agent 的角色、职责、行为约束和输出风格。图拓扑和工具都是标准配置，system prompt 才是区分不同 Agent 的关键。

只有用户明确要求特殊模式（RAG、意图分类、审批流、多步 Pipeline 等）时，才切换到对应拓扑。

## 工作流程

1. **查询工具和模型** — 用 schema(action=tool_list) 获取可用工具列表，用 schema(action=model_list) 查看可用模型（留空 = 全局默认）
2. **查看现状** — 用 graph(action=get) 查看当前图状态（修改已有 Agent 时）
3. **编写 system prompt** — 根据用户需求，编写描述角色、职责、行为约束和输出风格的 system prompt
4. **设计并构建** — 没有特殊要求则按默认行为创建 ReAct Agent；有特殊需求则搜索 "topology-patterns" 参考对应模式
5. **编译验证** — graph(action=compile) 检查，有错误则搜索 "common-pitfalls" 查找修复方法
6. **发布** — 编译通过后 graph(action=publish) 发布
7. **诊断问题** — 如果被要求检查 Agent，分析图结构并搜索参考资料给出修复建议

## 参考资料（通过 knowledge_search 按需检索）

| 搜索关键词 | 内容 |
|-----------|------|
| topology-patterns | 标准拓扑模式：Chatbot、ReAct、RAG、Approval、Orchestrator、意图分类、并行分支等的完整配置和示例 |
| common-pitfalls | 编译错误修复、循环配置、多 LLM 节点 messages_key 隔离、output_key 一致性、兜底路由等常见问题 |

⚠️ **重要**：不确定拓扑结构、节点配置或遇到编译错误时，**必须先 knowledge_search 查阅参考资料**，不要凭记忆猜测。`

func init() {
	copilotTemplates := []GraphTemplate{
		{
			Name:        "copilot_dispatcher",
			Label:       "CoPilot Assistant",
			Description: "CoPilot 通用助手：直接处理大部分任务，构建类任务分派给 Builder",
			Category:    "copilot",
			GraphDef: map[string]any{
				"entry": "llm_call",
				"nodes": []map[string]any{
					{
						"id":   "llm_call",
						"type": "llm",
						"config": map[string]any{
							"system_prompt": dispatcherSystemPrompt,
							"track_steps":   true,
							"tool_names": []string{
								"kanban_submit", "task_context",
								"sandbox_bash", "sandbox_read", "sandbox_write",
								"agent", "skill", "fetch_url",
								"knowledge_search", "knowledge_add", "memory_expand",
							},
						},
					},
					{
						"id":     "loop",
						"type":   "loopguard",
						"config": map[string]any{"max_count": 50},
					},
					{
						"id":     "output",
						"type":   "answer",
						"config": map[string]any{"keys": []string{"response"}},
					},
				},
				"edges": []map[string]any{
					{"from": "llm_call", "to": "loop"},
					{"from": "loop", "to": "llm_call", "condition": "tool_pending == true && loop_count_exceeded == false"},
					{"from": "loop", "to": "output"},
					{"from": "output", "to": "__end__"},
				},
			},
		},
		{
			Name:        "copilot_builder",
			Label:       "CoPilot Builder",
			Description: "平台 Agent 管理专家：创建新 Agent、修改已有 Agent、设计工作流拓扑、编译验证、发布上线、诊断运行问题。所有涉及 Agent 和工作流的平台操作都由它完成。分派时 query 需包含完整用户需求。",
			Category:    "copilot",
			GraphDef: map[string]any{
				"entry": "llm_call",
				"nodes": []map[string]any{
					{
						"id":   "llm_call",
						"type": "llm",
						"config": map[string]any{
							"system_prompt": builderSystemPrompt,
							"track_steps":   true,
							"tool_names": []string{
								"agent", "agent_create",
								"graph", "schema",
								"knowledge_search",
								"fetch_url",
							},
						},
					},
					{"id": "loop", "type": "loopguard", "config": map[string]any{"max_count": 30}},
					{"id": "output", "type": "answer", "config": map[string]any{"keys": []string{"response"}}},
				},
				"edges": []map[string]any{
					{"from": "llm_call", "to": "loop"},
					{"from": "loop", "to": "llm_call", "condition": "tool_pending == true && loop_count_exceeded == false"},
					{"from": "loop", "to": "output"},
					{"from": "output", "to": "__end__"},
				},
			},
		},
	}

	builtinTemplates = append(builtinTemplates, copilotTemplates...)
}
